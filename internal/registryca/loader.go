package registryca

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

const (
	MaxFileBytes   int64 = 16 << 20
	MaxPathEntries       = 256
	MaxPathBytes   int64 = 32 << 20

	windowsReparsePointAttribute = 0x400
)

var (
	certificatePEMBegin = []byte("-----BEGIN CERTIFICATE-----")
	certificatePEMEnd   = []byte("-----END CERTIFICATE-----")
)

// Load returns the system trust roots extended with the configured registry
// CA file and directory. Custom roots are added only after every configured
// file has passed validation.
func Load(caFile, caPath string) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load system CA pool: %w", err)
	}
	if pool == nil {
		return nil, fmt.Errorf("load system CA pool: empty pool")
	}

	filePath := strings.TrimSpace(caFile)
	path := strings.TrimSpace(caPath)
	var certificates []*x509.Certificate
	if filePath != "" {
		loaded, _, err := readCertificateFile(filePath, MaxFileBytes)
		if err != nil {
			return nil, fmt.Errorf("load ca_file %q: %w", filePath, err)
		}
		certificates = append(certificates, loaded...)
	}
	if path != "" {
		loaded, err := readCertificatePath(path, MaxPathEntries, MaxPathBytes)
		if err != nil {
			return nil, err
		}
		certificates = append(certificates, loaded...)
	}
	addCertificates(pool, certificates)
	return pool, nil
}

// AppendFile validates and appends certificates from path. maxBytes may lower
// the package limit for callers and tests, but cannot raise it.
func AppendFile(pool *x509.CertPool, path string, maxBytes int64) (int64, error) {
	if pool == nil {
		return 0, fmt.Errorf("certificate pool is nil")
	}
	limit, err := boundedByteLimit(maxBytes, MaxFileBytes)
	if err != nil {
		return 0, err
	}
	certificates, readBytes, err := readCertificateFile(path, limit)
	if err != nil {
		return 0, err
	}
	addCertificates(pool, certificates)
	return readBytes, nil
}

// AppendPath validates and appends all certificate files in path. Limits may
// be lowered by callers and tests, but cannot exceed the package hard limits.
func AppendPath(pool *x509.CertPool, path string, maxEntries int, maxBytes int64) error {
	if pool == nil {
		return fmt.Errorf("certificate pool is nil")
	}
	entryLimit, err := boundedEntryLimit(maxEntries)
	if err != nil {
		return err
	}
	byteLimit, err := boundedByteLimit(maxBytes, MaxPathBytes)
	if err != nil {
		return err
	}
	certificates, err := readCertificatePath(path, entryLimit, byteLimit)
	if err != nil {
		return err
	}
	addCertificates(pool, certificates)
	return nil
}

func readCertificateFile(path string, maxBytes int64) ([]*x509.Certificate, int64, error) {
	cleanPath := filepath.Clean(path)
	originalInfo, err := os.Lstat(cleanPath)
	if err != nil {
		return nil, 0, err
	}
	if err := validateRegularFile(originalInfo); err != nil {
		return nil, 0, err
	}

	root, err := os.OpenRoot(filepath.Dir(cleanPath))
	if err != nil {
		return nil, 0, err
	}
	defer root.Close()
	name := filepath.Base(cleanPath)
	rootedInfo, err := root.Lstat(name)
	if err != nil {
		return nil, 0, err
	}
	if err := validateRegularFile(rootedInfo); err != nil {
		return nil, 0, err
	}
	if !os.SameFile(originalInfo, rootedInfo) {
		return nil, 0, fmt.Errorf("file changed while opening")
	}
	return readRootedCertificateFile(root, name, cleanPath, rootedInfo, maxBytes)
}

func readCertificatePath(path string, maxEntries int, maxBytes int64) ([]*x509.Certificate, error) {
	cleanPath := filepath.Clean(path)
	originalInfo, err := os.Lstat(cleanPath)
	if err != nil {
		return nil, fmt.Errorf("read ca_path %q: %w", path, err)
	}
	if err := validateDirectory(originalInfo); err != nil {
		return nil, fmt.Errorf("ca_path %q %w", path, err)
	}

	root, err := os.OpenRoot(cleanPath)
	if err != nil {
		return nil, fmt.Errorf("read ca_path %q: %w", path, err)
	}
	defer root.Close()
	openedInfo, err := root.Stat(".")
	if err != nil {
		return nil, fmt.Errorf("inspect ca_path %q: %w", path, err)
	}
	if err := validateDirectory(openedInfo); err != nil || !os.SameFile(originalInfo, openedInfo) {
		return nil, fmt.Errorf("ca_path %q changed while opening", path)
	}
	if err := verifyDirectoryPath(cleanPath, originalInfo); err != nil {
		return nil, fmt.Errorf("ca_path %q changed while opening: %w", path, err)
	}

	directory, err := root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("read ca_path %q: %w", path, err)
	}
	defer directory.Close()
	entries, err := directory.ReadDir(maxEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read ca_path %q: %w", path, err)
	}
	if len(entries) > maxEntries {
		return nil, fmt.Errorf("ca_path %q exceeds %d certificate files", path, maxEntries)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("ca_path %q contains no certificate files", path)
	}

	type pathEntry struct {
		name string
		info os.FileInfo
	}
	files := make([]pathEntry, 0, len(entries))
	var totalBytes int64
	for _, entry := range entries {
		entryPath := filepath.Join(path, entry.Name())
		info, err := root.Lstat(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("inspect ca_path entry %q: %w", entryPath, err)
		}
		if err := validateRegularFile(info); err != nil {
			return nil, fmt.Errorf("ca_path entry %q %w", entryPath, err)
		}
		if info.Size() <= 0 || info.Size() > MaxFileBytes {
			return nil, fmt.Errorf("ca_path entry %q size must be between 1 and %d bytes", entryPath, MaxFileBytes)
		}
		if info.Size() > maxBytes-totalBytes {
			return nil, fmt.Errorf("ca_path %q exceeds %d total bytes", path, maxBytes)
		}
		totalBytes += info.Size()
		files = append(files, pathEntry{name: entry.Name(), info: info})
	}

	certificates := make([]*x509.Certificate, 0, len(files))
	var readBytes int64
	for _, entry := range files {
		entryPath := filepath.Join(path, entry.name)
		remaining := maxBytes - readBytes
		limit := min(remaining, MaxFileBytes)
		loaded, size, err := readRootedCertificateFile(root, entry.name, entryPath, entry.info, limit)
		if err != nil {
			return nil, fmt.Errorf("load ca_path entry %q: %w", entryPath, err)
		}
		readBytes += size
		certificates = append(certificates, loaded...)
	}
	if err := verifyDirectoryPath(cleanPath, originalInfo); err != nil {
		return nil, fmt.Errorf("ca_path %q changed while reading: %w", path, err)
	}
	return certificates, nil
}

func readRootedCertificateFile(root *os.Root, name, displayPath string, expected os.FileInfo, maxBytes int64) ([]*x509.Certificate, int64, error) {
	currentInfo, err := root.Lstat(name)
	if err != nil {
		return nil, 0, err
	}
	if err := validateRegularFile(currentInfo); err != nil {
		return nil, 0, err
	}
	if !os.SameFile(expected, currentInfo) {
		return nil, 0, fmt.Errorf("file changed while opening")
	}
	if currentInfo.Size() <= 0 || currentInfo.Size() > maxBytes {
		return nil, 0, fmt.Errorf("size must be between 1 and %d bytes", maxBytes)
	}

	file, err := root.Open(name)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	if err := validateRegularFile(openedInfo); err != nil {
		return nil, 0, err
	}
	currentInfo, err = root.Lstat(name)
	if err != nil {
		return nil, 0, err
	}
	if err := validateRegularFile(currentInfo); err != nil {
		return nil, 0, err
	}
	if !os.SameFile(expected, openedInfo) || !os.SameFile(expected, currentInfo) {
		return nil, 0, fmt.Errorf("file changed while opening")
	}

	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(data)) > maxBytes {
		return nil, 0, fmt.Errorf("exceeds %d bytes", maxBytes)
	}
	finalInfo, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	currentInfo, err = root.Lstat(name)
	if err != nil {
		return nil, 0, err
	}
	if err := validateRegularFile(currentInfo); err != nil {
		return nil, 0, err
	}
	if !os.SameFile(expected, finalInfo) || !os.SameFile(expected, currentInfo) ||
		finalInfo.Size() != expected.Size() || int64(len(data)) != finalInfo.Size() {
		return nil, 0, fmt.Errorf("file changed while reading")
	}

	certificates, err := parseCertificatePEM(data)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", displayPath, err)
	}
	return certificates, int64(len(data)), nil
}

func parseCertificatePEM(data []byte) ([]*x509.Certificate, error) {
	remaining := trimPEMWhitespace(data)
	certificates := make([]*x509.Certificate, 0, 1)
	for len(remaining) > 0 {
		if !bytes.HasPrefix(remaining, certificatePEMBegin) {
			return nil, fmt.Errorf("does not contain only valid PEM certificate blocks")
		}
		end := bytes.Index(remaining, certificatePEMEnd)
		if end < 0 {
			return nil, fmt.Errorf("does not contain only valid PEM certificate blocks")
		}
		end += len(certificatePEMEnd)
		candidate := remaining[:end]
		block, rest := pem.Decode(candidate)
		if block == nil || len(trimPEMWhitespace(rest)) != 0 || block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("does not contain only valid PEM certificate blocks")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("invalid CERTIFICATE PEM block: %w", err)
		}
		certificates = append(certificates, certificate)
		remaining = trimPEMWhitespace(remaining[end:])
	}
	if len(certificates) == 0 {
		return nil, fmt.Errorf("does not contain a valid PEM certificate")
	}
	return certificates, nil
}

func trimPEMWhitespace(data []byte) []byte {
	return bytes.Trim(data, " \t\r\n\v\f")
}

func validateDirectory(info os.FileInfo) error {
	if info == nil || info.Mode()&os.ModeSymlink != 0 || fileInfoIsReparsePoint(info) || !info.IsDir() {
		return fmt.Errorf("must be a non-symlink directory without reparse points")
	}
	return nil
}

func validateRegularFile(info os.FileInfo) error {
	if info == nil || info.Mode()&os.ModeSymlink != 0 || fileInfoIsReparsePoint(info) || !info.Mode().IsRegular() {
		return fmt.Errorf("must be a non-symlink regular file without reparse points")
	}
	return nil
}

func fileInfoIsReparsePoint(info os.FileInfo) bool {
	if info == nil {
		return false
	}
	value := reflect.ValueOf(info.Sys())
	for value.IsValid() && (value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer) {
		if value.IsNil() {
			return false
		}
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return false
	}
	attributes := value.FieldByName("FileAttributes")
	return attributes.IsValid() && attributes.CanUint() && attributes.Uint()&windowsReparsePointAttribute != 0
}

func verifyDirectoryPath(path string, expected os.FileInfo) error {
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if err := validateDirectory(current); err != nil {
		return err
	}
	if !os.SameFile(expected, current) {
		return fmt.Errorf("directory was replaced")
	}
	return nil
}

func boundedEntryLimit(requested int) (int, error) {
	if requested <= 0 {
		return 0, fmt.Errorf("certificate file limit must be positive")
	}
	return min(requested, MaxPathEntries), nil
}

func boundedByteLimit(requested, hardLimit int64) (int64, error) {
	if requested <= 0 {
		return 0, fmt.Errorf("certificate byte limit must be positive")
	}
	return min(requested, hardLimit), nil
}

func addCertificates(pool *x509.CertPool, certificates []*x509.Certificate) {
	for _, certificate := range certificates {
		pool.AddCert(certificate)
	}
}
