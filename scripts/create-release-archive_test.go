package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReleaseArchiveHelperModesAndPaths(t *testing.T) {
	source := createArchiveTestPackage(t)
	repoRoot := archiveTestRepoRoot(t)
	for _, format := range []string{"tar.gz", "zip"} {
		t.Run(format, func(t *testing.T) {
			archivePath := filepath.Join(t.TempDir(), "release."+format)
			runArchiveHelper(t, repoRoot, source, archivePath, format, false)
			rootName := filepath.Base(source)
			var modes map[string]os.FileMode
			if format == "tar.gz" {
				modes = readTarModes(t, archivePath)
			} else {
				modes = readZipModes(t, archivePath)
			}
			for name, want := range map[string]os.FileMode{
				rootName + "/":                   0o755,
				rootName + "/dm":                 0o755,
				rootName + "/notes.txt":          0o644,
				rootName + "/scripts/":           0o755,
				rootName + "/scripts/install.sh": 0o755,
			} {
				got, ok := modes[name]
				if !ok {
					t.Fatalf("archive entry %q is missing; entries=%v", name, modes)
				}
				if got != want {
					t.Errorf("archive mode for %q = %o, want %o", name, got, want)
				}
			}
			for name := range modes {
				clean := strings.ReplaceAll(name, "\\", "/")
				if strings.HasPrefix(clean, "/") || clean == ".." || strings.Contains(clean, "/../") {
					t.Errorf("archive contains unsafe path %q", name)
				}
			}
		})
	}
}

func TestReleaseArchiveHelperRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "package")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "real.txt"), []byte("real"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(source, "link.txt")
	if err := os.Symlink("real.txt", link); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	archivePath := filepath.Join(root, "release.tar.gz")
	runArchiveHelper(t, archiveTestRepoRoot(t), source, archivePath, "tar.gz", true)
	if _, err := os.Lstat(archivePath); !os.IsNotExist(err) {
		t.Fatalf("rejected archive should not remain, lstat error=%v", err)
	}
}

func createArchiveTestPackage(t *testing.T) string {
	t.Helper()
	source := filepath.Join(t.TempDir(), "package")
	if err := os.MkdirAll(filepath.Join(source, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "dm"), []byte("binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "notes.txt"), []byte("notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "scripts", "install.sh"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return source
}

func archiveTestRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(filepath.Dir(file))
}

func runArchiveHelper(t *testing.T, repoRoot, source, archivePath, format string, wantFailure bool) {
	t.Helper()
	helper := filepath.Join(repoRoot, "scripts", "create-release-archive.go")
	// #nosec G204 -- the test invokes the repository-local helper with fixed subcommands and temporary paths.
	cmd := exec.Command("go", "run", helper, "--source-dir", source, "--archive", archivePath, "--format", format)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if wantFailure {
		if err == nil {
			t.Fatalf("archive helper unexpectedly succeeded: %s", out)
		}
		return
	}
	if err != nil {
		t.Fatalf("archive helper failed: %v\n%s", err, out)
	}
}

func readTarModes(t *testing.T, archivePath string) map[string]os.FileMode {
	t.Helper()
	file, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	modes := map[string]os.FileMode{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		modes[header.Name] = os.FileMode(header.Mode) & 0o777
	}
	return modes
}

func readZipModes(t *testing.T, archivePath string) map[string]os.FileMode {
	t.Helper()
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	modes := map[string]os.FileMode{}
	for _, entry := range reader.File {
		modes[entry.Name] = entry.Mode().Perm()
	}
	if len(modes) == 0 {
		t.Fatal(fmt.Errorf("zip archive is empty"))
	}
	return modes
}
