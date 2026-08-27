package audit

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
)

const (
	DefaultAuditMaxBytes    int64 = 64 * 1024 * 1024
	DefaultAuditMaxFiles          = 5
	DefaultAuditLockTimeout       = 5 * time.Second
	maxAuditKeyFileBytes    int64 = 256
	auditWindowsReparseAttr       = 0x400
)

var auditPathMutexes sync.Map

type FileSink struct {
	mu sync.Mutex

	path          string
	keyPath       string
	lockPath      string
	lockFile      *os.File
	pathMutex     *sync.Mutex
	key           []byte
	maxBytes      int64
	maxFiles      int
	maxEventBytes int
	lockTimeout   time.Duration
	closed        bool
}

func OpenFileSink(opts FileOptions) (*FileSink, error) {
	path, err := normalizeAuditFilePath(opts.Path, "audit file")
	if err != nil {
		return nil, err
	}
	keyPath := opts.KeyPath
	if strings.TrimSpace(keyPath) == "" {
		keyPath = path + ".key"
	}
	keyPath, err = normalizeAuditFilePath(keyPath, "audit key file")
	if err != nil {
		return nil, err
	}
	lockPath, err := normalizeAuditFilePath(path+".lock", "audit lock file")
	if err != nil {
		return nil, err
	}
	if sameAuditPath(path, keyPath) || sameAuditPath(path, lockPath) || sameAuditPath(keyPath, lockPath) {
		return nil, errors.New("audit data, key, and lock paths must be distinct")
	}

	maxEventBytes := opts.MaxEventBytes
	if maxEventBytes == 0 {
		maxEventBytes = DefaultMaxEventBytes
	}
	if maxEventBytes < 1024 || maxEventBytes > DefaultMaxEventBytes {
		return nil, fmt.Errorf("audit max event bytes must be between 1024 and %d", DefaultMaxEventBytes)
	}
	maxBytes := opts.MaxBytes
	if maxBytes == 0 {
		maxBytes = DefaultAuditMaxBytes
	}
	if maxBytes < int64(maxEventBytes) {
		return nil, fmt.Errorf("audit max bytes must be at least max event bytes (%d)", maxEventBytes)
	}
	maxFiles := opts.MaxFiles
	if maxFiles == 0 {
		maxFiles = DefaultAuditMaxFiles
	}
	if maxFiles < 1 || maxFiles > 100 {
		return nil, errors.New("audit max files must be between 1 and 100")
	}
	lockTimeout := opts.LockTimeout
	if lockTimeout == 0 {
		lockTimeout = DefaultAuditLockTimeout
	}
	if lockTimeout < 0 {
		return nil, errors.New("audit lock timeout cannot be negative")
	}

	for _, parent := range uniqueAuditParents(path, keyPath, lockPath) {
		if err := ensureAuditParent(parent); err != nil {
			return nil, err
		}
	}
	for _, candidate := range []string{path, keyPath, lockPath} {
		if err := validateAuditRegularPath(candidate, true); err != nil {
			return nil, err
		}
	}

	lockFile, err := openAuditLockFile(lockPath)
	if err != nil {
		return nil, fmt.Errorf("open audit lock file: %w", err)
	}
	if err := secureOpenedAuditFile(lockPath, lockFile); err != nil {
		_ = lockFile.Close()
		return nil, err
	}

	pathMutexValue, _ := auditPathMutexes.LoadOrStore(path, &sync.Mutex{})
	sink := &FileSink{
		path:          path,
		keyPath:       keyPath,
		lockPath:      lockPath,
		lockFile:      lockFile,
		pathMutex:     pathMutexValue.(*sync.Mutex),
		maxBytes:      maxBytes,
		maxFiles:      maxFiles,
		maxEventBytes: maxEventBytes,
		lockTimeout:   lockTimeout,
	}

	sink.pathMutex.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), lockTimeout)
	if lockTimeout == 0 {
		cancel()
		ctx = context.Background()
		cancel = func() {}
	}
	lockErr := acquireAuditFileLock(ctx, sink.lockFile)
	if lockErr == nil {
		sink.key, err = loadOrCreateAuditKey(keyPath)
		unlockErr := unlockAuditFile(sink.lockFile)
		err = errors.Join(err, unlockErr)
	} else {
		err = lockErr
	}
	cancel()
	sink.pathMutex.Unlock()
	if err != nil {
		_ = lockFile.Close()
		return nil, err
	}
	return sink, nil
}

func (sink *FileSink) Append(ctx context.Context, event Event) error {
	if sink == nil {
		return errors.New("audit file sink is nil")
	}
	if err := validateAuditEvent(event); err != nil {
		return err
	}
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal audit event: %w", err)
	}
	data = append(data, '\n')
	if len(data) > sink.maxEventBytes {
		return fmt.Errorf("audit event contains %d bytes, limit is %d", len(data), sink.maxEventBytes)
	}
	if int64(len(data)) > sink.maxBytes {
		return fmt.Errorf("audit event contains %d bytes, file rotation limit is %d", len(data), sink.maxBytes)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.closed {
		return errors.New("audit file sink is closed")
	}
	sink.pathMutex.Lock()
	defer sink.pathMutex.Unlock()

	lockCtx := ctx
	var cancel context.CancelFunc
	if sink.lockTimeout > 0 {
		lockCtx, cancel = context.WithTimeout(ctx, sink.lockTimeout)
		defer cancel()
	}
	if err := acquireAuditFileLock(lockCtx, sink.lockFile); err != nil {
		return err
	}
	writeErr := sink.appendLocked(data)
	unlockErr := unlockAuditFile(sink.lockFile)
	return errors.Join(writeErr, unlockErr)
}

func (sink *FileSink) IdentifierKey() []byte {
	if sink == nil {
		return nil
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]byte(nil), sink.key...)
}

func (sink *FileSink) Path() string {
	if sink == nil {
		return ""
	}
	return sink.path
}

func (sink *FileSink) Close() error {
	if sink == nil {
		return nil
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.closed {
		return nil
	}
	sink.closed = true
	return sink.lockFile.Close()
}

func (sink *FileSink) appendLocked(data []byte) error {
	if err := validateAuditRegularPath(sink.path, true); err != nil {
		return err
	}
	info, err := os.Lstat(sink.path)
	switch {
	case err == nil && info.Size() > 0 && info.Size()+int64(len(data)) > sink.maxBytes:
		if err := sink.rotateLocked(); err != nil {
			return err
		}
	case err != nil && !os.IsNotExist(err):
		return fmt.Errorf("inspect audit file before append: %w", err)
	}

	file, err := os.OpenFile(sink.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("open audit file for append: %w", err)
	}
	if err := secureOpenedAuditFile(sink.path, file); err != nil {
		_ = file.Close()
		return err
	}
	written, writeErr := file.Write(data)
	if writeErr == nil && written != len(data) {
		writeErr = io.ErrShortWrite
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}

func (sink *FileSink) rotateLocked() error {
	if err := validateAuditRegularPath(sink.path, false); err != nil {
		return err
	}
	oldest := rotatedAuditPath(sink.path, sink.maxFiles)
	if err := removeAuditRotationFile(oldest); err != nil {
		return err
	}
	for index := sink.maxFiles - 1; index >= 1; index-- {
		source := rotatedAuditPath(sink.path, index)
		destination := rotatedAuditPath(sink.path, index+1)
		if err := moveAuditRotationFile(source, destination); err != nil {
			return err
		}
	}
	if err := os.Rename(sink.path, rotatedAuditPath(sink.path, 1)); err != nil {
		return fmt.Errorf("rotate audit file: %w", err)
	}
	return nil
}

func rotatedAuditPath(path string, index int) string {
	return fmt.Sprintf("%s.%d", path, index)
}

func removeAuditRotationFile(path string) error {
	if err := validateAuditRegularPath(path, true); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove old audit rotation %s: %w", path, err)
	}
	return nil
}

func moveAuditRotationFile(source, destination string) error {
	if err := validateAuditRegularPath(source, true); err != nil {
		return err
	}
	if _, err := os.Lstat(source); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if err := validateAuditRegularPath(destination, true); err != nil {
		return err
	}
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("audit rotation destination already exists: %s", destination)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(source, destination); err != nil {
		return fmt.Errorf("rotate audit file %s to %s: %w", source, destination, err)
	}
	return nil
}

func loadOrCreateAuditKey(path string) ([]byte, error) {
	if err := validateAuditRegularPath(path, true); err != nil {
		return nil, err
	}
	// #nosec G304 -- the normalized audit key path is link/reparse checked before open and identity checked after open.
	file, err := os.Open(path)
	if err == nil {
		defer file.Close()
		if err := secureOpenedAuditFile(path, file); err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(file, maxAuditKeyFileBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read audit key: %w", err)
		}
		if len(data) > int(maxAuditKeyFileBytes) {
			return nil, errors.New("audit key file is too large")
		}
		key, err := hex.DecodeString(strings.TrimSpace(string(data)))
		if err != nil || len(key) != identifierKeyBytes {
			return nil, fmt.Errorf("audit key file must contain exactly %d hex-encoded bytes", identifierKeyBytes)
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("open audit key: %w", err)
	}

	key := make([]byte, identifierKeyBytes)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate audit key: %w", err)
	}
	// #nosec G304 -- the normalized audit key path is link/reparse checked before create and identity checked after open.
	file, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, fmt.Errorf("create audit key: %w", err)
	}
	if err := secureOpenedAuditFile(path, file); err != nil {
		_ = file.Close()
		return nil, err
	}
	data := []byte(hex.EncodeToString(key) + "\n")
	written, writeErr := file.Write(data)
	if writeErr == nil && written != len(data) {
		writeErr = io.ErrShortWrite
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return nil, fmt.Errorf("persist audit key: %w", err)
	}
	return key, nil
}

func normalizeAuditFilePath(path, description string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" || path == "-" {
		return "", fmt.Errorf("%s path must name a regular file", description)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s path: %w", description, err)
	}
	return filepath.Clean(absolute), nil
}

func sameAuditPath(left, right string) bool {
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}

func uniqueAuditParents(paths ...string) []string {
	seen := make(map[string]struct{}, len(paths))
	var result []string
	for _, path := range paths {
		parent := filepath.Dir(path)
		key := strings.ToLower(filepath.Clean(parent))
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, parent)
	}
	return result
}

func ensureAuditParent(path string) error {
	if err := rejectAuditPathLinks(path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return fmt.Errorf("create audit directory: %w", err)
	}
	if err := rejectAuditPathLinks(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect audit directory: %w", err)
	}
	if !info.IsDir() || auditInfoIsReparsePoint(info) {
		return fmt.Errorf("audit parent is not a regular directory: %s", path)
	}
	return nil
}

func rejectAuditPathLinks(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for current := filepath.Clean(absolute); ; current = filepath.Dir(current) {
		info, statErr := os.Lstat(current)
		if statErr == nil && auditInfoIsReparsePoint(info) {
			return fmt.Errorf("audit path contains a symbolic link or reparse point: %s", current)
		}
		if statErr != nil && !os.IsNotExist(statErr) {
			return fmt.Errorf("inspect audit path %s: %w", current, statErr)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
	}
}

func validateAuditRegularPath(path string, allowMissing bool) error {
	if err := rejectAuditPathLinks(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) && allowMissing {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect audit file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || auditInfoIsReparsePoint(info) {
		return fmt.Errorf("audit path is not a regular file: %s", path)
	}
	return nil
}

func secureOpenedAuditFile(path string, file *os.File) error {
	if file == nil {
		return errors.New("opened audit file is nil")
	}
	opened, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect opened audit file: %w", err)
	}
	current, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("reinspect audit file path: %w", err)
	}
	if !opened.Mode().IsRegular() || !current.Mode().IsRegular() ||
		auditInfoIsReparsePoint(opened) || auditInfoIsReparsePoint(current) || !os.SameFile(opened, current) {
		return fmt.Errorf("audit file changed while opening or is not regular: %s", path)
	}
	if err := file.Chmod(0600); err != nil {
		return fmt.Errorf("set private audit file permissions: %w", err)
	}
	return nil
}

func auditInfoIsReparsePoint(info os.FileInfo) bool {
	if info == nil || info.Mode()&os.ModeSymlink != 0 {
		return info != nil
	}
	value := reflect.ValueOf(info.Sys())
	if value.IsValid() && value.Kind() == reflect.Pointer && !value.IsNil() {
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return false
	}
	attributes := value.FieldByName("FileAttributes")
	return attributes.IsValid() && attributes.CanUint() && attributes.Uint()&auditWindowsReparseAttr != 0
}

func validateAuditEvent(event Event) error {
	if event.Schema != SchemaVersion {
		return fmt.Errorf("audit event schema must be %q", SchemaVersion)
	}
	if event.Type == "" || event.RunID == "" || event.Sequence == 0 || event.Operation == "" || event.Time == "" {
		return errors.New("audit event is missing required envelope fields")
	}
	if _, err := time.Parse(time.RFC3339Nano, event.Time); err != nil {
		return fmt.Errorf("audit event time is invalid: %w", err)
	}
	return nil
}

func acquireAuditFileLock(ctx context.Context, file *os.File) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		locked, err := tryLockAuditFile(file)
		if err != nil {
			return fmt.Errorf("lock audit file: %w", err)
		}
		if locked {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("lock audit file: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func auditJSONLines(data []byte) [][]byte {
	var lines [][]byte
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) > 0 {
			lines = append(lines, line)
		}
	}
	return lines
}
