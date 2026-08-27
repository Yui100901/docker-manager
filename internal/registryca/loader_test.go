package registryca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppendFileRequiresOnlyCertificatePEM(t *testing.T) {
	certificate := testCertificatePEM(t)
	tests := []struct {
		name string
		data []byte
		ok   bool
	}{
		{name: "one certificate", data: certificate, ok: true},
		{name: "multiple certificates", data: append(append([]byte(nil), certificate...), certificate...), ok: true},
		{name: "surrounding whitespace", data: append(append([]byte("\r\n\t"), certificate...), []byte(" \n")...), ok: true},
		{name: "leading garbage", data: append([]byte("garbage\n"), certificate...)},
		{name: "trailing garbage", data: append(append([]byte(nil), certificate...), []byte("garbage")...)},
		{name: "garbage between certificates", data: append(append(append([]byte(nil), certificate...), []byte("garbage")...), certificate...)},
		{name: "other PEM block", data: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("key")})},
		{name: "invalid certificate DER", data: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("invalid")})},
		{name: "empty", data: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ca.pem")
			if err := os.WriteFile(path, tt.data, 0o600); err != nil {
				t.Fatal(err)
			}
			pool := x509.NewCertPool()
			before := pool.Clone()
			_, err := AppendFile(pool, path, MaxFileBytes)
			if tt.ok && err != nil {
				t.Fatalf("AppendFile() error = %v", err)
			}
			if !tt.ok && err == nil {
				t.Fatal("AppendFile() error = nil, want fail-closed rejection")
			}
			if !tt.ok && !pool.Equal(before) {
				t.Fatal("AppendFile mutated the pool after validation failed")
			}
		})
	}
}

func TestAppendFileRejectsUnsafeAndOversizedFiles(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		_, err := AppendFile(x509.NewCertPool(), t.TempDir(), MaxFileBytes)
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("AppendFile() error = %v", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target.pem")
		if err := os.WriteFile(target, testCertificatePEM(t), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link.pem")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink creation unavailable: %v", err)
		}
		_, err := AppendFile(x509.NewCertPool(), link, MaxFileBytes)
		if err == nil || !strings.Contains(err.Error(), "non-symlink regular file") {
			t.Fatalf("AppendFile() error = %v", err)
		}
	})

	t.Run("caller limit", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ca.pem")
		if err := os.WriteFile(path, testCertificatePEM(t), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := AppendFile(x509.NewCertPool(), path, 1)
		if err == nil || !strings.Contains(err.Error(), "between 1 and 1 bytes") {
			t.Fatalf("AppendFile() error = %v", err)
		}
	})
}

func TestAppendPathEnforcesLimitsAndFailsAtomically(t *testing.T) {
	certificate := testCertificatePEM(t)

	t.Run("empty", func(t *testing.T) {
		err := AppendPath(x509.NewCertPool(), t.TempDir(), MaxPathEntries, MaxPathBytes)
		if err == nil || !strings.Contains(err.Error(), "no certificate files") {
			t.Fatalf("AppendPath() error = %v", err)
		}
	})

	t.Run("entry count", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range []string{"one.pem", "two.pem"} {
			if err := os.WriteFile(filepath.Join(dir, name), certificate, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		err := AppendPath(x509.NewCertPool(), dir, 1, MaxPathBytes)
		if err == nil || !strings.Contains(err.Error(), "exceeds 1 certificate files") {
			t.Fatalf("AppendPath() error = %v", err)
		}
	})

	t.Run("total bytes", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "ca.pem"), certificate, 0o600); err != nil {
			t.Fatal(err)
		}
		err := AppendPath(x509.NewCertPool(), dir, MaxPathEntries, int64(len(certificate)-1))
		if err == nil || !strings.Contains(err.Error(), "total bytes") {
			t.Fatalf("AppendPath() error = %v", err)
		}
	})

	t.Run("invalid entry leaves pool unchanged", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "good.pem"), certificate, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "bad.pem"), []byte("not a certificate"), 0o600); err != nil {
			t.Fatal(err)
		}
		pool := x509.NewCertPool()
		before := pool.Clone()
		err := AppendPath(pool, dir, MaxPathEntries, MaxPathBytes)
		if err == nil || !pool.Equal(before) {
			t.Fatalf("AppendPath() error = %v, pool changed after failure", err)
		}
	})
}

func TestAppendPathRejectsUnsafeDirectoryAndEntries(t *testing.T) {
	t.Run("path is file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ca.pem")
		if err := os.WriteFile(path, testCertificatePEM(t), 0o600); err != nil {
			t.Fatal(err)
		}
		err := AppendPath(x509.NewCertPool(), path, MaxPathEntries, MaxPathBytes)
		if err == nil || !strings.Contains(err.Error(), "directory") {
			t.Fatalf("AppendPath() error = %v", err)
		}
	})

	t.Run("directory entry", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "nested"), 0o700); err != nil {
			t.Fatal(err)
		}
		err := AppendPath(x509.NewCertPool(), dir, MaxPathEntries, MaxPathBytes)
		if err == nil || !strings.Contains(err.Error(), "non-symlink regular file") {
			t.Fatalf("AppendPath() error = %v", err)
		}
	})

	t.Run("symlink entry", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "target.pem")
		if err := os.WriteFile(target, testCertificatePEM(t), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, "linked.pem")); err != nil {
			t.Skipf("symlink creation unavailable: %v", err)
		}
		err := AppendPath(x509.NewCertPool(), dir, MaxPathEntries, MaxPathBytes)
		if err == nil || !strings.Contains(err.Error(), "non-symlink regular file") {
			t.Fatalf("AppendPath() error = %v", err)
		}
	})
}

func TestFileInfoIsReparsePoint(t *testing.T) {
	info := fakeFileInfo{sys: &fakeWindowsFileAttributes{FileAttributes: windowsReparsePointAttribute}}
	if !fileInfoIsReparsePoint(info) {
		t.Fatal("Windows reparse attribute was not detected")
	}
	info.sys = &fakeWindowsFileAttributes{}
	if fileInfoIsReparsePoint(info) {
		t.Fatal("ordinary Windows attributes were reported as a reparse point")
	}
}

func testCertificatePEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "registryca-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		BasicConstraintsValid: true,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

type fakeWindowsFileAttributes struct {
	FileAttributes uint32
}

type fakeFileInfo struct {
	sys any
}

func (f fakeFileInfo) Name() string       { return "fake" }
func (f fakeFileInfo) Size() int64        { return 1 }
func (f fakeFileInfo) Mode() os.FileMode  { return 0o600 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return f.sys }
