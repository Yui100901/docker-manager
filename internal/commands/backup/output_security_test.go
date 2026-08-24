package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPrivateBackupDirectoryCleanupPreservesReplacementAndClearsOwnedTree(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "plaintext")
	owned, err := createPrivateBackupDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "secret"), []byte("plaintext"), 0600); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(parent, "moved-plaintext")
	if err := os.Rename(path, moved); err != nil {
		cleanupErr := owned.removeAll()
		if runtime.GOOS == "windows" {
			if cleanupErr != nil {
				t.Fatalf("cleanup after blocked Windows replacement: %v", cleanupErr)
			}
			t.Skipf("open directory handle blocked replacement on Windows: %v", err)
		}
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(path, "keep")
	if err := os.WriteFile(replacement, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}

	err = owned.removeAll()
	if err == nil || !strings.Contains(err.Error(), "refusing to remove replaced") {
		t.Fatalf("removeAll() error = %v, want replacement rejection", err)
	}
	if data, readErr := os.ReadFile(replacement); readErr != nil || string(data) != "foreign" {
		t.Fatalf("replacement changed: data=%q error=%v", data, readErr)
	}
	entries, readErr := os.ReadDir(moved)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("owned plaintext tree was not cleared: %#v", entries)
	}
}

func TestPrivateBackupDirectoryCleanupPreservesSymlinkReplacement(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "plaintext")
	owned, err := createPrivateBackupDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "secret"), []byte("plaintext"), 0600); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(parent, "moved-plaintext")
	if err := os.Rename(path, moved); err != nil {
		cleanupErr := owned.removeAll()
		if runtime.GOOS == "windows" {
			if cleanupErr != nil {
				t.Fatalf("cleanup after blocked Windows replacement: %v", cleanupErr)
			}
			t.Skipf("open directory handle blocked replacement on Windows: %v", err)
		}
		t.Fatal(err)
	}
	target := filepath.Join(parent, "foreign")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(target, "keep")
	if err := os.WriteFile(keep, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}

	err = owned.removeAll()
	if err == nil || !strings.Contains(err.Error(), "refusing to remove replaced") {
		t.Fatalf("removeAll() error = %v, want replacement rejection", err)
	}
	if info, statErr := os.Lstat(path); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("replacement symbolic link changed: info=%v error=%v", info, statErr)
	}
	if data, readErr := os.ReadFile(keep); readErr != nil || string(data) != "foreign" {
		t.Fatalf("replacement target changed: data=%q error=%v", data, readErr)
	}
	entries, readErr := os.ReadDir(moved)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("owned plaintext tree was not cleared: %#v", entries)
	}
}

func TestEncryptedBackupCleanupUsesOwnedDirectoryIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the held directory handle prevents the replacement required by this scenario")
	}
	parent := t.TempDir()
	outputDir := filepath.Join(parent, "plaintext")
	moved := filepath.Join(parent, "moved-plaintext")
	replacement := filepath.Join(outputDir, "keep")
	passphrase := filepath.Join(parent, "passphrase")
	if err := os.WriteFile(passphrase, []byte("correct horse battery staple\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeBackupDockerService{inspect: basicRestoreInspect("demo")}
	fake.inspect.Config.Image = "busybox:latest"
	fake.afterSave = func(string) error {
		if err := os.Rename(outputDir, moved); err != nil {
			return err
		}
		if err := os.Mkdir(outputDir, 0700); err != nil {
			return err
		}
		if err := os.WriteFile(replacement, []byte("foreign"), 0600); err != nil {
			return err
		}
		return errors.New("forced save failure after directory replacement")
	}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	_, err := backupContainer(context.Background(), "demo", BackupOptions{
		OutputDir: outputDir, IncludeImage: true, Bundle: true, Encrypt: true,
		PassphraseFile: passphrase, BundleOutput: filepath.Join(parent, "bundle.tar.gz.enc"),
	})
	if err == nil || !strings.Contains(err.Error(), "forced save failure") || !strings.Contains(err.Error(), "refusing to remove replaced") {
		t.Fatalf("backupContainer() error = %v, want operation and cleanup errors", err)
	}
	if data, readErr := os.ReadFile(replacement); readErr != nil || string(data) != "foreign" {
		t.Fatalf("replacement changed: data=%q error=%v", data, readErr)
	}
	entries, readErr := os.ReadDir(moved)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("owned plaintext tree was not cleared: %#v", entries)
	}
}
