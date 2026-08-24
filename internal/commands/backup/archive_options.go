package backup

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	backupEncryptionMagic      = "DMBKENC1\n"
	backupEncryptionSaltSize   = 16
	backupEncryptionNonceSize  = 12
	backupEncryptionPrefixSize = 4
	backupEncryptionChunkSize  = 1024 * 1024
	backupEncryptionKDFIter    = 200_000
)

type backupArchiveOptions struct {
	Encrypt        bool
	PassphraseFile string
	SplitSize      int64
}

type backupArchiveWriter interface {
	io.WriteCloser
	Abort() error
}

type singleBackupArchiveWriter struct {
	ctx        context.Context
	file       *os.File
	finalPath  string
	stagedPath string
	stagingDir string
	hash       hash.Hash
	written    int64
	closed     bool
	committed  bool
}

func newSingleBackupArchiveWriter(ctx context.Context, finalPath string) (*singleBackupArchiveWriter, error) {
	stagingDir, err := createBackupArtifactStaging(finalPath)
	if err != nil {
		return nil, err
	}
	stagedPath := filepath.Join(stagingDir, "archive")
	file, err := os.OpenFile(stagedPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		_ = os.RemoveAll(stagingDir)
		return nil, err
	}
	return &singleBackupArchiveWriter{
		ctx:        backupContext(ctx),
		file:       file,
		finalPath:  finalPath,
		stagedPath: stagedPath,
		stagingDir: stagingDir,
		hash:       sha256.New(),
	}, nil
}

func (w *singleBackupArchiveWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, os.ErrClosed
	}
	if int64(len(p)) > maxBackupArchiveBytes-w.written {
		return 0, fmt.Errorf("backup archive exceeds the %d-byte limit", maxBackupArchiveBytes)
	}
	n, err := w.file.Write(p)
	if n > 0 {
		_, _ = w.hash.Write(p[:n])
		w.written += int64(n)
	}
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

func (w *singleBackupArchiveWriter) Close() error {
	if w.closed {
		if w.committed {
			return nil
		}
		return os.ErrClosed
	}
	w.closed = true
	digest := hex.EncodeToString(w.hash.Sum(nil))
	if err := w.file.Sync(); err != nil {
		_ = w.file.Close()
		return err
	}
	if err := w.file.Close(); err != nil {
		return err
	}
	if err := verifyBackupStagedFile(w.ctx, w.stagedPath, w.written, digest); err != nil {
		return err
	}
	if err := checkBackupContext(w.ctx); err != nil {
		return err
	}
	if err := publishBackupStagedFile(w.stagedPath, w.finalPath); err != nil {
		return err
	}
	w.committed = true
	_ = os.RemoveAll(w.stagingDir)
	return nil
}

func (w *singleBackupArchiveWriter) Abort() error {
	if w.committed {
		return nil
	}
	w.closed = true
	closeErr := w.file.Close()
	removeErr := os.RemoveAll(w.stagingDir)
	return errors.Join(closeErr, removeErr)
}

func archiveOptionsFromBackup(opts BackupOptions) (backupArchiveOptions, error) {
	splitSize, err := parseBackupSize(opts.SplitSize)
	if err != nil {
		return backupArchiveOptions{}, err
	}
	if opts.Encrypt && strings.TrimSpace(opts.PassphraseFile) == "" {
		return backupArchiveOptions{}, fmt.Errorf("--encrypt requires --passphrase-file")
	}
	return backupArchiveOptions{
		Encrypt:        opts.Encrypt,
		PassphraseFile: opts.PassphraseFile,
		SplitSize:      splitSize,
	}, nil
}

func parseBackupSize(value string) (int64, error) {
	return parseBackupByteSize("split size", value)
}

func parseBackupByteSize(name, value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	units := map[byte]int64{
		'k': 1024,
		'm': 1024 * 1024,
		'g': 1024 * 1024 * 1024,
		't': 1024 * 1024 * 1024 * 1024,
	}
	multiplier := int64(1)
	last := value[len(value)-1]
	if last >= 'A' && last <= 'Z' {
		last = last - 'A' + 'a'
	}
	if unit, ok := units[last]; ok {
		multiplier = unit
		value = strings.TrimSpace(value[:len(value)-1])
	}
	size, err := strconv.ParseInt(value, 10, 64)
	if err != nil || size <= 0 {
		return 0, fmt.Errorf("invalid %s %q", name, value)
	}
	if size > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("%s exceeds the supported byte range", name)
	}
	return size * multiplier, nil
}

func openBackupArchiveWriter(ctx context.Context, archivePath string, opts backupArchiveOptions) (backupArchiveWriter, error) {
	var passphrase []byte
	var err error
	if opts.Encrypt {
		passphrase, err = readBackupPassphrase(opts.PassphraseFile)
		if err != nil {
			return nil, err
		}
	}
	var writer backupArchiveWriter
	if opts.SplitSize > 0 {
		writer, err = newSplitFileWriter(ctx, archivePath, opts.SplitSize)
	} else {
		writer, err = newSingleBackupArchiveWriter(ctx, archivePath)
	}
	if err != nil {
		return nil, err
	}
	if !opts.Encrypt {
		return writer, nil
	}
	encrypted, err := newEncryptWriter(writer, passphrase)
	if err != nil {
		_ = writer.Abort()
		return nil, err
	}
	return encrypted, nil
}

func readBackupPassphrase(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read passphrase file: %w", err)
	}
	passphrase := []byte(strings.TrimRight(string(data), "\r\n"))
	if len(passphrase) == 0 {
		return nil, fmt.Errorf("passphrase file is empty")
	}
	return passphrase, nil
}

type encryptWriter struct {
	dst       backupArchiveWriter
	aead      cipher.AEAD
	prefix    []byte
	counter   uint64
	buf       []byte
	plaintext int64
	closed    bool
}

func newEncryptWriter(dst backupArchiveWriter, passphrase []byte) (*encryptWriter, error) {
	salt := make([]byte, backupEncryptionSaltSize)
	prefix := make([]byte, backupEncryptionPrefixSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(prefix); err != nil {
		return nil, err
	}
	key, err := pbkdf2.Key(sha256.New, string(passphrase), salt, backupEncryptionKDFIter, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if _, err := dst.Write([]byte(backupEncryptionMagic)); err != nil {
		return nil, err
	}
	if _, err := dst.Write(salt); err != nil {
		return nil, err
	}
	if _, err := dst.Write(prefix); err != nil {
		return nil, err
	}
	return &encryptWriter{dst: dst, aead: aead, prefix: prefix, buf: make([]byte, 0, backupEncryptionChunkSize)}, nil
}

func (w *encryptWriter) Abort() error {
	w.closed = true
	return w.dst.Abort()
}

func (w *encryptWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > maxBackupEncryptionPlaintextBytes-w.plaintext {
		return 0, fmt.Errorf("encrypted backup plaintext exceeds the %d-byte limit", maxBackupEncryptionPlaintextBytes)
	}
	written := len(p)
	for len(p) > 0 {
		space := backupEncryptionChunkSize - len(w.buf)
		if space > len(p) {
			space = len(p)
		}
		w.buf = append(w.buf, p[:space]...)
		p = p[space:]
		if len(w.buf) == backupEncryptionChunkSize {
			if err := w.flushChunk(); err != nil {
				return 0, err
			}
		}
	}
	w.plaintext += int64(written)
	return written, nil
}

func (w *encryptWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if len(w.buf) > 0 {
		if err := w.flushChunk(); err != nil {
			return errors.Join(err, w.dst.Abort())
		}
	}
	if err := binary.Write(w.dst, binary.BigEndian, uint32(0)); err != nil {
		return errors.Join(err, w.dst.Abort())
	}
	return w.dst.Close()
}

func (w *encryptWriter) flushChunk() error {
	nonce := make([]byte, backupEncryptionNonceSize)
	copy(nonce, w.prefix)
	binary.BigEndian.PutUint64(nonce[backupEncryptionPrefixSize:], w.counter)
	w.counter++
	sealed := w.aead.Seal(nil, nonce, w.buf, nil)
	if uint64(len(sealed)) > uint64(math.MaxUint32) {
		return fmt.Errorf("encrypted chunk too large")
	}
	chunkLen := uint32(len(sealed)) // #nosec G115 -- bounded by math.MaxUint32 above.
	if err := binary.Write(w.dst, binary.BigEndian, chunkLen); err != nil {
		return err
	}
	if _, err := w.dst.Write(sealed); err != nil {
		return err
	}
	w.buf = w.buf[:0]
	return nil
}

type splitFileWriter struct {
	ctx          context.Context
	basePath     string
	partSize     int64
	partIndex    int
	partWritten  int64
	totalWritten int64
	current      *os.File
	currentPath  string
	currentHash  hash.Hash
	overallHash  hash.Hash
	stagingDir   string
	stagedPaths  []string
	finalPaths   []string
	partSizes    []int64
	partDigests  []string
	published    []backupPublishedArtifact
	transaction  *os.File
	closed       bool
	committed    bool
}

type backupPublishedArtifact struct {
	stagedPath string
	finalPath  string
}

func newSplitFileWriter(ctx context.Context, basePath string, partSize int64) (*splitFileWriter, error) {
	if partSize <= 0 {
		return nil, fmt.Errorf("split size must be positive")
	}
	if partSize > maxBackupArchivePartBytes {
		return nil, fmt.Errorf("split size exceeds the %d-byte per-part limit", maxBackupArchivePartBytes)
	}
	stagingDir, err := createBackupArtifactStaging(basePath)
	if err != nil {
		return nil, err
	}
	if err := recoverInterruptedSplitArchive(ctx, basePath); err != nil {
		_ = os.RemoveAll(stagingDir)
		return nil, err
	}
	if err := rejectExistingBackupSplitArtifacts(basePath); err != nil {
		_ = os.RemoveAll(stagingDir)
		return nil, err
	}
	transaction, err := createBackupSplitTransaction(ctx, basePath, stagingDir)
	if err != nil {
		_ = os.RemoveAll(stagingDir)
		return nil, err
	}
	w := &splitFileWriter{
		ctx:         ctx,
		basePath:    basePath,
		partSize:    partSize,
		stagingDir:  stagingDir,
		overallHash: sha256.New(),
		transaction: transaction,
	}
	if err := w.openNextPart(); err != nil {
		return nil, errors.Join(err, w.cleanupTransaction())
	}
	return w, nil
}

func (w *splitFileWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, os.ErrClosed
	}
	if int64(len(p)) > maxBackupArchiveBytes-w.totalWritten {
		return 0, fmt.Errorf("split backup archive exceeds the %d-byte total limit", maxBackupArchiveBytes)
	}
	total := len(p)
	for len(p) > 0 {
		if err := checkBackupContext(w.ctx); err != nil {
			return 0, err
		}
		remain := w.partSize - w.partWritten
		if remain <= 0 {
			if err := w.openNextPart(); err != nil {
				return 0, err
			}
			remain = w.partSize
		}
		n := len(p)
		if int64(n) > remain {
			n = int(remain)
		}
		written, err := w.current.Write(p[:n])
		if written > 0 {
			_, _ = w.currentHash.Write(p[:written])
			_, _ = w.overallHash.Write(p[:written])
			w.partWritten += int64(written)
			w.totalWritten += int64(written)
		}
		p = p[written:]
		if err != nil {
			return 0, err
		}
		if written != n {
			return 0, io.ErrShortWrite
		}
	}
	return total, nil
}

func (w *splitFileWriter) Close() error {
	if w.closed {
		if w.committed {
			return nil
		}
		return os.ErrClosed
	}
	w.closed = true
	if err := w.finalizeCurrentPart(); err != nil {
		return err
	}
	if err := checkBackupContext(w.ctx); err != nil {
		return err
	}
	manifestPath, err := w.writePartsManifest()
	if err != nil {
		return err
	}
	for i, stagedPath := range w.stagedPaths {
		if err := checkBackupContext(w.ctx); err != nil {
			return errors.Join(err, w.rollbackPublished())
		}
		finalPath := w.finalPaths[i]
		if err := publishBackupStagedFile(stagedPath, finalPath); err != nil {
			return errors.Join(err, w.rollbackPublished())
		}
		w.published = append(w.published, backupPublishedArtifact{stagedPath: stagedPath, finalPath: finalPath})
	}
	finalManifestPath := backupPartsManifestPath(w.basePath)
	if err := checkBackupContext(w.ctx); err != nil {
		return errors.Join(err, w.rollbackPublished())
	}
	if err := publishBackupStagedFile(manifestPath, finalManifestPath); err != nil {
		return errors.Join(err, w.rollbackPublished())
	}
	w.published = append(w.published, backupPublishedArtifact{stagedPath: manifestPath, finalPath: finalManifestPath})
	w.committed = true
	if err := w.cleanupTransaction(); err != nil {
		return fmt.Errorf("split backup was committed but transaction cleanup failed: %w", err)
	}
	return nil
}

func (w *splitFileWriter) Abort() error {
	if w.committed {
		return w.cleanupTransaction()
	}
	var cleanupErrors []error
	if w.current != nil {
		cleanupErrors = append(cleanupErrors, w.current.Close())
		w.current = nil
	}
	cleanupErrors = append(cleanupErrors, w.rollbackPublished(), w.cleanupTransaction())
	w.closed = true
	return errors.Join(cleanupErrors...)
}

func (w *splitFileWriter) cleanupTransaction() error {
	if w.transaction == nil {
		return nil
	}
	transaction := w.transaction
	w.transaction = nil
	payloadErr := removeSplitStagingPayload(context.Background(), w.stagingDir)
	if payloadErr != nil {
		unlockErr := unlockBackupTransactionFile(transaction)
		closeErr := transaction.Close()
		return errors.Join(payloadErr, unlockErr, closeErr)
	}
	removeErr := removeBackupSplitTransactionMarker(transaction, w.basePath)
	unlockErr := unlockBackupTransactionFile(transaction)
	closeErr := transaction.Close()
	if removeErr != nil {
		return errors.Join(removeErr, unlockErr, closeErr)
	}
	return errors.Join(unlockErr, closeErr, removeSplitStagingMarker(w.stagingDir))
}

func (w *splitFileWriter) openNextPart() error {
	if w.current != nil {
		if err := w.finalizeCurrentPart(); err != nil {
			return err
		}
	}
	if w.partIndex >= maxBackupArchivePartCount {
		return fmt.Errorf("split backup exceeds the %d-part limit", maxBackupArchivePartCount)
	}
	w.partIndex++
	w.partWritten = 0
	w.currentHash = sha256.New()
	finalPath := splitPartPath(w.basePath, w.partIndex)
	if err := rejectBackupPathSymlinks(finalPath); err != nil {
		return err
	}
	if _, err := os.Lstat(finalPath); err == nil {
		return fmt.Errorf("backup archive part already exists: %s", finalPath)
	} else if !os.IsNotExist(err) {
		return err
	}
	stagedPath := filepath.Join(w.stagingDir, filepath.Base(finalPath))
	file, err := os.OpenFile(stagedPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	w.current = file
	w.currentPath = stagedPath
	w.stagedPaths = append(w.stagedPaths, stagedPath)
	w.finalPaths = append(w.finalPaths, finalPath)
	return nil
}

func (w *splitFileWriter) finalizeCurrentPart() error {
	if w.current == nil {
		return nil
	}
	digest := hex.EncodeToString(w.currentHash.Sum(nil))
	if err := w.current.Sync(); err != nil {
		_ = w.current.Close()
		w.current = nil
		return err
	}
	if err := w.current.Close(); err != nil {
		w.current = nil
		return err
	}
	w.current = nil
	if err := verifyBackupStagedFile(w.ctx, w.currentPath, w.partWritten, digest); err != nil {
		return err
	}
	w.partSizes = append(w.partSizes, w.partWritten)
	w.partDigests = append(w.partDigests, digest)
	return nil
}

type backupArchivePartManifest struct {
	Version       int                       `json:"version"`
	Archive       string                    `json:"archive"`
	TotalSize     int64                     `json:"total_size"`
	ArchiveSHA256 string                    `json:"archive_sha256"`
	Parts         []backupArchivePartRecord `json:"parts"`
	Commit        string                    `json:"commit"`
}

type backupArchivePartRecord struct {
	Index  int    `json:"index"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func (w *splitFileWriter) writePartsManifest() (string, error) {
	manifest := backupArchivePartManifest{
		Version:       1,
		Archive:       filepath.Base(w.basePath),
		TotalSize:     w.totalWritten,
		ArchiveSHA256: hex.EncodeToString(w.overallHash.Sum(nil)),
		Commit:        "complete",
		Parts:         make([]backupArchivePartRecord, len(w.stagedPaths)),
	}
	for i := range w.stagedPaths {
		manifest.Parts[i] = backupArchivePartRecord{
			Index:  i + 1,
			Name:   filepath.Base(w.finalPaths[i]),
			Size:   w.partSizes[i],
			SHA256: w.partDigests[i],
		}
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	if int64(len(data)) > maxBackupPartsManifestBytes {
		return "", fmt.Errorf("backup parts manifest exceeds the %d-byte limit", maxBackupPartsManifestBytes)
	}
	path := filepath.Join(w.stagingDir, "parts.json")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	written, err := file.Write(data)
	if err != nil {
		_ = file.Close()
		return "", err
	}
	if written != len(data) {
		_ = file.Close()
		return "", io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	if err := verifyBackupStagedFile(w.ctx, path, int64(len(data)), hex.EncodeToString(digest[:])); err != nil {
		return "", err
	}
	return path, nil
}

func (w *splitFileWriter) rollbackPublished() error {
	var errs []error
	hadPublished := len(w.published) > 0
	for i := len(w.published) - 1; i >= 0; i-- {
		artifact := w.published[i]
		if err := removePublishedBackupStagedFile(artifact.stagedPath, artifact.finalPath); err != nil {
			errs = append(errs, err)
		}
	}
	w.published = nil
	if hadPublished {
		if err := syncBackupDirectory(filepath.Dir(w.basePath)); err != nil {
			errs = append(errs, fmt.Errorf("sync rolled back backup artifact directory: %w", err))
		}
	}
	return errors.Join(errs...)
}

func splitPartPath(basePath string, index int) string {
	return fmt.Sprintf("%s.part-%03d", basePath, index)
}

func backupPartsManifestPath(basePath string) string {
	return basePath + ".parts.json"
}

func backupArchiveOutputPath(path string, opts backupArchiveOptions) string {
	if opts.Encrypt && !strings.HasSuffix(strings.ToLower(path), ".enc") {
		return path + ".enc"
	}
	return path
}

func joinBackupArchivePartsWithContext(ctx context.Context, firstPart, outputPath string) error {
	return joinBackupArchivePartsWithBudget(ctx, firstPart, outputPath, newBackupByteBudget("restore temporary files", maxBackupTemporaryBytes))
}

func joinBackupArchivePartsWithBudget(ctx context.Context, firstPart, outputPath string, diskBudget *backupByteBudget) (resultErr error) {
	return joinBackupArchivePartsWithLimits(ctx, firstPart, outputPath, diskBudget, defaultBackupLimits())
}

func joinBackupArchivePartsWithLimits(ctx context.Context, firstPart, outputPath string, diskBudget *backupByteBudget, limits backupLimits) (resultErr error) {
	ctx, cancel := context.WithTimeout(backupContext(ctx), maxBackupArchiveOperationTime)
	defer cancel()
	base := strings.TrimSuffix(firstPart, ".part-001")
	if base == firstPart || firstPart != splitPartPath(base, 1) {
		return fmt.Errorf("restore must start with the canonical .part-001 path")
	}
	manifestPath := backupPartsManifestPath(base)
	manifestInfo, err := os.Lstat(manifestPath)
	if err != nil {
		return fmt.Errorf("read committed backup parts manifest: %w", err)
	}
	if !manifestInfo.Mode().IsRegular() || manifestInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("backup parts manifest must be a regular file")
	}
	manifestData, err := readLimitedBackupFile(ctx, manifestPath, maxBackupPartsManifestBytes)
	if err != nil {
		return fmt.Errorf("read backup parts manifest: %w", err)
	}
	var manifest backupArchivePartManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return fmt.Errorf("parse backup parts manifest: %w", err)
	}
	if err := validateBackupPartsManifestWithLimits(base, manifest, limits); err != nil {
		return err
	}
	if err := rejectUnexpectedBackupParts(base, len(manifest.Parts)); err != nil {
		return err
	}
	if err := rejectBackupPathSymlinks(outputPath); err != nil {
		return err
	}
	out, err := os.OpenFile(outputPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil {
			_ = out.Close()
			_ = os.Remove(outputPath)
		}
	}()
	overallHash := sha256.New()
	for i, record := range manifest.Parts {
		if err := checkBackupContext(ctx); err != nil {
			return err
		}
		partPath := splitPartPath(base, i+1)
		info, err := os.Lstat(partPath)
		if err != nil {
			return fmt.Errorf("read backup archive part %d: %w", i+1, err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("backup archive part %d is not a regular file", i+1)
		}
		if info.Size() != record.Size {
			return fmt.Errorf("backup archive part %d size mismatch: expected %d, found %d", i+1, record.Size, info.Size())
		}
		if err := diskBudget.Add(record.Size); err != nil {
			return err
		}
		in, err := os.Open(partPath)
		if err != nil {
			return err
		}
		partHash := sha256.New()
		err = backupCopyNWithContext(ctx, io.MultiWriter(out, partHash, overallHash), in, record.Size)
		if err == nil {
			var trailing [1]byte
			if n, readErr := in.Read(trailing[:]); n != 0 || (readErr != nil && readErr != io.EOF) {
				err = fmt.Errorf("backup archive part %d changed while it was read", i+1)
			}
		}
		closeErr := in.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		actualDigest := hex.EncodeToString(partHash.Sum(nil))
		if actualDigest != record.SHA256 {
			return fmt.Errorf("backup archive part %d digest mismatch: expected %s, found %s", i+1, record.SHA256, actualDigest)
		}
	}
	if actual := hex.EncodeToString(overallHash.Sum(nil)); actual != manifest.ArchiveSHA256 {
		return fmt.Errorf("backup archive overall digest mismatch: expected %s, found %s", manifest.ArchiveSHA256, actual)
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return verifyBackupStagedFile(ctx, outputPath, manifest.TotalSize, manifest.ArchiveSHA256)
}

func validateBackupPartsManifest(base string, manifest backupArchivePartManifest) error {
	return validateBackupPartsManifestWithLimits(base, manifest, defaultBackupLimits())
}

func validateBackupPartsManifestWithLimits(base string, manifest backupArchivePartManifest, limits backupLimits) error {
	if manifest.Version != 1 || manifest.Commit != "complete" {
		return fmt.Errorf("backup parts manifest is not committed")
	}
	if manifest.Archive != filepath.Base(base) {
		return fmt.Errorf("backup parts manifest archive mismatch")
	}
	if len(manifest.Parts) == 0 || len(manifest.Parts) > limits.parts {
		return fmt.Errorf("backup parts manifest exceeds the %d-part limit", limits.parts)
	}
	if manifest.TotalSize < 0 || manifest.TotalSize > limits.archiveBytes {
		return fmt.Errorf("backup parts manifest total size exceeds the %d-byte limit", limits.archiveBytes)
	}
	if !validBackupSHA256(manifest.ArchiveSHA256) {
		return fmt.Errorf("backup parts manifest contains an invalid overall digest")
	}
	var total int64
	for i, record := range manifest.Parts {
		expectedName := filepath.Base(splitPartPath(base, i+1))
		if record.Index != i+1 || record.Name != expectedName {
			return fmt.Errorf("backup parts manifest contains a non-contiguous part at position %d", i+1)
		}
		if record.Size <= 0 || record.Size > maxBackupArchivePartBytes {
			return fmt.Errorf("backup archive part %d exceeds the %d-byte limit", i+1, maxBackupArchivePartBytes)
		}
		if record.Size > limits.archiveBytes-total {
			return fmt.Errorf("backup parts manifest total size exceeds the %d-byte limit", limits.archiveBytes)
		}
		total += record.Size
		if !validBackupSHA256(record.SHA256) {
			return fmt.Errorf("backup archive part %d contains an invalid digest", i+1)
		}
	}
	if total != manifest.TotalSize {
		return fmt.Errorf("backup parts manifest total size mismatch: expected %d, found %d", manifest.TotalSize, total)
	}
	return nil
}

func validBackupSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func rejectUnexpectedBackupParts(base string, expectedCount int) error {
	entries, err := os.ReadDir(filepath.Dir(base))
	if err != nil {
		return err
	}
	prefix := filepath.Base(base) + ".part-"
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		suffix := strings.TrimPrefix(entry.Name(), prefix)
		index, err := strconv.Atoi(suffix)
		if err != nil || index <= 0 {
			continue
		}
		if entry.Name() != filepath.Base(splitPartPath(base, index)) || index > expectedCount {
			return fmt.Errorf("unexpected backup archive part: %s", entry.Name())
		}
	}
	return nil
}

func backupCopyNWithContext(ctx context.Context, dst io.Writer, src io.Reader, remaining int64) error {
	buffer := make([]byte, 32*1024)
	for remaining > 0 {
		if err := checkBackupContext(ctx); err != nil {
			return err
		}
		readSize := int64(len(buffer))
		if readSize > remaining {
			readSize = remaining
		}
		n, err := io.ReadFull(src, buffer[:int(readSize)])
		if n > 0 {
			written, writeErr := dst.Write(buffer[:n])
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
			remaining -= int64(n)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func decryptBackupArchiveWithContext(ctx context.Context, encryptedPath, outputPath, passphraseFile string) error {
	return decryptBackupArchiveWithBudget(ctx, encryptedPath, outputPath, passphraseFile, newBackupByteBudget("restore temporary files", maxBackupTemporaryBytes))
}

func decryptBackupArchiveWithBudget(ctx context.Context, encryptedPath, outputPath, passphraseFile string, diskBudget *backupByteBudget) (resultErr error) {
	return decryptBackupArchiveWithLimits(ctx, encryptedPath, outputPath, passphraseFile, diskBudget, defaultBackupLimits())
}

func decryptBackupArchiveWithLimits(ctx context.Context, encryptedPath, outputPath, passphraseFile string, diskBudget *backupByteBudget, limits backupLimits) (resultErr error) {
	ctx, cancel := context.WithTimeout(backupContext(ctx), maxBackupArchiveOperationTime)
	defer cancel()
	if strings.TrimSpace(passphraseFile) == "" {
		return fmt.Errorf("encrypted backup requires --passphrase-file")
	}
	passphrase, err := readBackupPassphrase(passphraseFile)
	if err != nil {
		return err
	}
	info, err := os.Lstat(encryptedPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("encrypted backup must be a regular file")
	}
	if info.Size() > limits.archiveBytes {
		return fmt.Errorf("encrypted backup exceeds the %d-byte limit", limits.archiveBytes)
	}
	in, err := os.Open(encryptedPath)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := rejectBackupPathSymlinks(outputPath); err != nil {
		return err
	}
	out, err := os.OpenFile(outputPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil {
			_ = out.Close()
			_ = os.Remove(outputPath)
		}
	}()
	if err := decryptBackupArchiveStreamWithLimit(ctx, in, out, passphrase, diskBudget, limits.archiveBytes); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	return out.Close()
}

func decryptBackupArchiveStream(ctx context.Context, src io.Reader, dst io.Writer, passphrase []byte) error {
	return decryptBackupArchiveStreamWithBudget(ctx, src, dst, passphrase, newBackupByteBudget("decrypted backup", maxBackupEncryptionPlaintextBytes))
}

func decryptBackupArchiveStreamWithBudget(ctx context.Context, src io.Reader, dst io.Writer, passphrase []byte, diskBudget *backupByteBudget) error {
	return decryptBackupArchiveStreamWithLimit(ctx, src, dst, passphrase, diskBudget, maxBackupEncryptionPlaintextBytes)
}

func decryptBackupArchiveStreamWithLimit(ctx context.Context, src io.Reader, dst io.Writer, passphrase []byte, diskBudget *backupByteBudget, plaintextLimit int64) error {
	header := make([]byte, len(backupEncryptionMagic)+backupEncryptionSaltSize+backupEncryptionPrefixSize)
	if _, err := io.ReadFull(src, header); err != nil {
		return err
	}
	if string(header[:len(backupEncryptionMagic)]) != backupEncryptionMagic {
		return fmt.Errorf("invalid encrypted backup header")
	}
	saltStart := len(backupEncryptionMagic)
	saltEnd := saltStart + backupEncryptionSaltSize
	salt := header[saltStart:saltEnd]
	prefix := header[saltEnd:]
	key, err := pbkdf2.Key(sha256.New, string(passphrase), salt, backupEncryptionKDFIter, 32)
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	var counter uint64
	var plaintextTotal int64
	for {
		if err := checkBackupContext(ctx); err != nil {
			return err
		}
		var chunkLen uint32
		if err := binary.Read(src, binary.BigEndian, &chunkLen); err != nil {
			return err
		}
		if chunkLen == 0 {
			var trailing [1]byte
			if n, err := src.Read(trailing[:]); n != 0 || (err != nil && err != io.EOF) {
				return fmt.Errorf("encrypted backup contains trailing data")
			}
			return nil
		}
		maxCiphertextChunk := int64(backupEncryptionChunkSize) + int64(aead.Overhead())
		if int64(chunkLen) > maxCiphertextChunk {
			return fmt.Errorf("encrypted backup chunk length %d exceeds the %d-byte limit", chunkLen, maxCiphertextChunk)
		}
		if int64(chunkLen) < int64(aead.Overhead()) {
			return fmt.Errorf("encrypted backup chunk length %d is too small", chunkLen)
		}
		plaintextSize := int64(chunkLen) - int64(aead.Overhead())
		if plaintextSize > plaintextLimit-plaintextTotal {
			return fmt.Errorf("decrypted backup exceeds the %d-byte limit", plaintextLimit)
		}
		plaintextTotal += plaintextSize
		if err := diskBudget.Add(plaintextSize); err != nil {
			return err
		}
		ciphertext := make([]byte, chunkLen)
		if _, err := io.ReadFull(src, ciphertext); err != nil {
			return err
		}
		nonce := make([]byte, backupEncryptionNonceSize)
		copy(nonce, prefix)
		binary.BigEndian.PutUint64(nonce[backupEncryptionPrefixSize:], counter)
		counter++
		plaintext, err := aead.Open(nil, nonce, ciphertext, nil)
		if err != nil {
			return fmt.Errorf("decrypt backup chunk: %w", err)
		}
		if _, err := dst.Write(plaintext); err != nil {
			return err
		}
	}
}
