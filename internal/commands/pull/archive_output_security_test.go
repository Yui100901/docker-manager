package pull

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestCreateTarArchiveRejectsPresetOutputSymlinks(t *testing.T) {
	t.Run("ancestor", func(t *testing.T) {
		root := t.TempDir()
		realOutputDir := filepath.Join(root, "real-output")
		if err := os.Mkdir(realOutputDir, 0755); err != nil {
			t.Fatal(err)
		}
		linkedOutputDir := filepath.Join(root, "linked-output")
		if err := os.Symlink(realOutputDir, linkedOutputDir); err != nil {
			t.Skipf("symlink creation unavailable: %v", err)
		}
		existingPath := filepath.Join(realOutputDir, "image.tar")
		if err := os.WriteFile(existingPath, []byte("existing"), 0600); err != nil {
			t.Fatal(err)
		}

		err := createTarArchiveWithContext(context.Background(), writePullArchiveSource(t, "new"), filepath.Join(linkedOutputDir, "image.tar"))
		if err == nil {
			t.Fatal("createTarArchiveWithContext() error = nil, want symlink ancestor rejection")
		}
		assertPullArchiveFileContent(t, existingPath, "existing")
		assertNoPullArchiveStaging(t, realOutputDir)
	})

	t.Run("destination", func(t *testing.T) {
		outputDir := t.TempDir()
		targetPath := filepath.Join(outputDir, "target.tar")
		if err := os.WriteFile(targetPath, []byte("existing"), 0600); err != nil {
			t.Fatal(err)
		}
		outputPath := filepath.Join(outputDir, "image.tar")
		if err := os.Symlink(targetPath, outputPath); err != nil {
			t.Skipf("symlink creation unavailable: %v", err)
		}

		err := createTarArchiveWithContext(context.Background(), writePullArchiveSource(t, "new"), outputPath)
		if err == nil {
			t.Fatal("createTarArchiveWithContext() error = nil, want symlink destination rejection")
		}
		assertPullArchiveFileContent(t, targetPath, "existing")
		info, statErr := os.Lstat(outputPath)
		if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("destination symlink changed: info=%v error=%v", info, statErr)
		}
		assertNoPullArchiveStaging(t, outputDir)
	})
}

func TestCreateTarArchiveCancellationCleansPrivateStaging(t *testing.T) {
	outputDir := t.TempDir()
	outputPath := filepath.Join(outputDir, "image.tar")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := createTarArchiveWithContext(ctx, writePullArchiveSource(t, "new"), outputPath)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("createTarArchiveWithContext() error = %v, want context.Canceled", err)
	}
	if _, statErr := os.Lstat(outputPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("output stat error = %v, want not exist", statErr)
	}
	assertNoPullArchiveStaging(t, outputDir)
}

func TestCreateTarArchiveFailurePreservesExistingOutput(t *testing.T) {
	outputDir := t.TempDir()
	outputPath := filepath.Join(outputDir, "image.tar")
	if err := os.WriteFile(outputPath, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}

	err := createTarArchiveWithContext(context.Background(), filepath.Join(t.TempDir(), "missing-source"), outputPath)
	if err == nil {
		t.Fatal("createTarArchiveWithContext() error = nil, want missing source error")
	}
	assertPullArchiveFileContent(t, outputPath, "existing")
	assertNoPullArchiveStaging(t, outputDir)
}

func TestCreateTarArchiveReplacesExistingOutput(t *testing.T) {
	outputDir := t.TempDir()
	outputPath := filepath.Join(outputDir, "image.tar")
	if err := os.WriteFile(outputPath, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := createTarArchiveWithContext(context.Background(), writePullArchiveSource(t, "replacement"), outputPath); err != nil {
		t.Fatalf("createTarArchiveWithContext() error = %v", err)
	}
	if marker := readPullArchiveMarker(t, outputPath); marker != "replacement" {
		t.Fatalf("published marker = %q, want replacement", marker)
	}
	assertNoPullArchiveStaging(t, outputDir)
}

func TestCreateTarArchiveConcurrentReplacementPublishesOwnStaging(t *testing.T) {
	const writers = 8
	outputDir := t.TempDir()
	outputPath := filepath.Join(outputDir, "image.tar")
	sources := make([]string, writers)
	wants := make(map[string]bool, writers)
	for i := range writers {
		marker := strings.Repeat(string(rune('a'+i)), 4096+i)
		sources[i] = writePullArchiveSource(t, marker)
		wants[marker] = true
	}

	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func(source string) {
			defer wg.Done()
			<-start
			errs <- createTarArchiveWithContext(context.Background(), source, outputPath)
		}(sources[i])
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent createTarArchiveWithContext() error = %v", err)
		}
	}

	marker := readPullArchiveMarker(t, outputPath)
	if !wants[marker] {
		t.Fatalf("published marker is not from a complete writer: length=%d", len(marker))
	}
	assertNoPullArchiveStaging(t, outputDir)
}

func TestPullArchiveRejectsAndPreservesReplacedStaging(t *testing.T) {
	outputDir := t.TempDir()
	outputPath := filepath.Join(outputDir, "image.tar")
	root, outputName, absoluteOutput, parentInfo, err := openPullArchiveOutput(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	stagingName, stagingFile, ownerInfo, err := createPullArchiveStaging(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := stagingFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := root.Remove(stagingName); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(stagingName, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}

	err = verifyPullArchivePublication(root, stagingName, outputName, absoluteOutput, parentInfo, ownerInfo)
	if err == nil {
		t.Fatal("verifyPullArchivePublication() error = nil, want replaced staging rejection")
	}
	if err := removeOwnedPullArchiveStaging(root, stagingName, ownerInfo); err == nil {
		t.Fatal("removeOwnedPullArchiveStaging() error = nil, want ownership rejection")
	}
	data, err := root.ReadFile(stagingName)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "foreign" {
		t.Fatalf("replacement staging content = %q, want foreign", data)
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output stat error = %v, want not exist", err)
	}
}

func writePullArchiveSource(t *testing.T, marker string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte(marker), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func readPullArchiveMarker(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader := tar.NewReader(file)
	for {
		header, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			t.Fatal("archive marker not found")
		}
		if nextErr != nil {
			t.Fatalf("read archive error: %v", nextErr)
		}
		if header.Name != "marker" {
			continue
		}
		data, readErr := io.ReadAll(reader)
		if readErr != nil {
			t.Fatal(readErr)
		}
		return string(data)
	}
}

func assertPullArchiveFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("file content = %q, want %q", data, want)
	}
}

func assertNoPullArchiveStaging(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), pullArchiveStagingPrefix) {
			t.Fatalf("private staging was not cleaned: %s", entry.Name())
		}
	}
}
