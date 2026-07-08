package backup

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
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
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	units := map[byte]int64{
		'k': 1024,
		'm': 1024 * 1024,
		'g': 1024 * 1024 * 1024,
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
		return 0, fmt.Errorf("invalid split size %q", value)
	}
	return size * multiplier, nil
}

func openBackupArchiveWriter(ctx context.Context, archivePath string, opts backupArchiveOptions) (io.WriteCloser, error) {
	if err := os.MkdirAll(filepath.Dir(archivePath), 0755); err != nil {
		return nil, err
	}
	var writer io.WriteCloser
	var err error
	if opts.SplitSize > 0 {
		writer, err = newSplitFileWriter(ctx, archivePath, opts.SplitSize)
	} else {
		writer, err = os.Create(archivePath)
	}
	if err != nil {
		return nil, err
	}
	if !opts.Encrypt {
		return writer, nil
	}
	passphrase, err := readBackupPassphrase(opts.PassphraseFile)
	if err != nil {
		_ = writer.Close()
		return nil, err
	}
	encrypted, err := newEncryptWriter(writer, passphrase)
	if err != nil {
		_ = writer.Close()
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
	dst     io.WriteCloser
	aead    cipher.AEAD
	prefix  []byte
	counter uint64
	buf     []byte
	closed  bool
}

func newEncryptWriter(dst io.WriteCloser, passphrase []byte) (*encryptWriter, error) {
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

func (w *encryptWriter) Write(p []byte) (int, error) {
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
	return written, nil
}

func (w *encryptWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	var err error
	if len(w.buf) > 0 {
		err = w.flushChunk()
	}
	if err == nil {
		err = binary.Write(w.dst, binary.BigEndian, uint32(0))
	}
	if closeErr := w.dst.Close(); err == nil {
		err = closeErr
	}
	return err
}

func (w *encryptWriter) flushChunk() error {
	nonce := make([]byte, backupEncryptionNonceSize)
	copy(nonce, w.prefix)
	binary.BigEndian.PutUint64(nonce[backupEncryptionPrefixSize:], w.counter)
	w.counter++
	sealed := w.aead.Seal(nil, nonce, w.buf, nil)
	if len(sealed) > int(^uint32(0)) {
		return fmt.Errorf("encrypted chunk too large")
	}
	if err := binary.Write(w.dst, binary.BigEndian, uint32(len(sealed))); err != nil {
		return err
	}
	if _, err := w.dst.Write(sealed); err != nil {
		return err
	}
	w.buf = w.buf[:0]
	return nil
}

type splitFileWriter struct {
	ctx       context.Context
	basePath  string
	partSize  int64
	partIndex int
	written   int64
	current   *os.File
}

func newSplitFileWriter(ctx context.Context, basePath string, partSize int64) (*splitFileWriter, error) {
	if partSize <= 0 {
		return nil, fmt.Errorf("split size must be positive")
	}
	w := &splitFileWriter{ctx: ctx, basePath: basePath, partSize: partSize}
	if err := w.openNextPart(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *splitFileWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		if err := checkBackupContext(w.ctx); err != nil {
			return 0, err
		}
		remain := w.partSize - w.written
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
		w.written += int64(written)
		p = p[written:]
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}

func (w *splitFileWriter) Close() error {
	if w.current == nil {
		return nil
	}
	err := w.current.Close()
	w.current = nil
	return err
}

func (w *splitFileWriter) openNextPart() error {
	if w.current != nil {
		if err := w.current.Close(); err != nil {
			return err
		}
	}
	w.partIndex++
	w.written = 0
	path := splitPartPath(w.basePath, w.partIndex)
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	w.current = file
	return nil
}

func splitPartPath(basePath string, index int) string {
	return fmt.Sprintf("%s.part-%03d", basePath, index)
}

func backupArchiveOutputPath(path string, opts backupArchiveOptions) string {
	if opts.Encrypt && !strings.HasSuffix(strings.ToLower(path), ".enc") {
		return path + ".enc"
	}
	return path
}

func joinBackupArchivePartsWithContext(ctx context.Context, firstPart, outputPath string) error {
	base := strings.TrimSuffix(firstPart, ".part-001")
	out, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer out.Close()
	for i := 1; ; i++ {
		if err := checkBackupContext(ctx); err != nil {
			return err
		}
		partPath := splitPartPath(base, i)
		in, err := os.Open(partPath)
		if err != nil {
			if i == 1 || !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		err = backupCopyWithContext(ctx, out, in)
		closeErr := in.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
}

func decryptBackupArchiveWithContext(ctx context.Context, encryptedPath, outputPath, passphraseFile string) error {
	if strings.TrimSpace(passphraseFile) == "" {
		return fmt.Errorf("encrypted backup requires --passphrase-file")
	}
	passphrase, err := readBackupPassphrase(passphraseFile)
	if err != nil {
		return err
	}
	in, err := os.Open(encryptedPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer out.Close()
	return decryptBackupArchiveStream(ctx, in, out, passphrase)
}

func decryptBackupArchiveStream(ctx context.Context, src io.Reader, dst io.Writer, passphrase []byte) error {
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
	for {
		if err := checkBackupContext(ctx); err != nil {
			return err
		}
		var chunkLen uint32
		if err := binary.Read(src, binary.BigEndian, &chunkLen); err != nil {
			return err
		}
		if chunkLen == 0 {
			return nil
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
