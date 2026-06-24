package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/docker/api/types/image"
)

type fakeImageManager struct {
	images    []image.Summary
	saveErrs  map[string]error
	loadErrs  map[string]error
	saveCalls []saveCall
	loadCalls []string
}

type saveCall struct {
	images     []string
	outputFile string
}

func (m *fakeImageManager) List(all bool) ([]image.Summary, error) {
	return m.images, nil
}

func (m *fakeImageManager) Save(images []string, outputFile string) error {
	m.saveCalls = append(m.saveCalls, saveCall{
		images:     append([]string(nil), images...),
		outputFile: outputFile,
	})
	if len(images) == 1 {
		return m.saveErrs[images[0]]
	}
	return nil
}

func (m *fakeImageManager) Load(inputFile string) error {
	m.loadCalls = append(m.loadCalls, inputFile)
	return m.loadErrs[inputFile]
}

func withFakeImageManager(t *testing.T, manager *fakeImageManager) {
	t.Helper()
	previous := imageManager
	imageManager = manager
	t.Cleanup(func() {
		imageManager = previous
	})
}

func TestSaveImagesMergeUsesRequestedPath(t *testing.T) {
	manager := &fakeImageManager{
		images: []image.Summary{
			{ID: "sha256:one", RepoTags: []string{"repo/app:v1"}},
		},
	}
	withFakeImageManager(t, manager)

	if err := saveImages("backup", true, false); err != nil {
		t.Fatalf("saveImages() error = %v", err)
	}
	if len(manager.saveCalls) != 1 {
		t.Fatalf("Save called %d times, want 1", len(manager.saveCalls))
	}
	want := filepath.Join("backup", "images.tar")
	if manager.saveCalls[0].outputFile != want {
		t.Fatalf("outputFile = %q, want %q", manager.saveCalls[0].outputFile, want)
	}
}

func TestSaveImagesReturnsExportErrors(t *testing.T) {
	exportErr := errors.New("save failed")
	manager := &fakeImageManager{
		images: []image.Summary{
			{ID: "sha256:one", RepoTags: []string{"repo/app:v1"}},
			{ID: "sha256:two", RepoTags: []string{"repo/other:v2"}},
		},
		saveErrs: map[string]error{
			"sha256:two": exportErr,
		},
	}
	withFakeImageManager(t, manager)

	err := saveImages("backup", false, false)
	if err == nil {
		t.Fatal("saveImages() error = nil, want export error")
	}
	if !errors.Is(err, exportErr) {
		t.Fatalf("saveImages() error = %v, want wrapped %v", err, exportErr)
	}
	if len(manager.saveCalls) != 2 {
		t.Fatalf("Save called %d times, want 2", len(manager.saveCalls))
	}
}

func TestSaveImagesFiltersByWildcard(t *testing.T) {
	manager := &fakeImageManager{
		images: []image.Summary{
			{ID: "sha256:one", RepoTags: []string{"repo/app:v1"}},
			{ID: "sha256:two", RepoTags: []string{"repo/other:v2"}},
			{ID: "sha256:three", RepoTags: []string{"library/busybox:latest"}},
		},
	}
	withFakeImageManager(t, manager)

	err := saveImagesWithOptions("backup", SaveOptions{
		Filters: []string{"repo/*:v?"},
	})
	if err != nil {
		t.Fatalf("saveImagesWithOptions() error = %v", err)
	}
	if len(manager.saveCalls) != 2 {
		t.Fatalf("Save called %d times, want 2", len(manager.saveCalls))
	}
	got := map[string]bool{}
	for _, call := range manager.saveCalls {
		got[call.images[0]] = true
	}
	if !got["sha256:one"] || !got["sha256:two"] {
		t.Fatalf("saved images = %#v, want sha256:one and sha256:two", got)
	}
}

func TestSaveImagesFiltersByShortIDAndRepositoryName(t *testing.T) {
	manager := &fakeImageManager{
		images: []image.Summary{
			{ID: "sha256:abcdef1234567890", RepoTags: []string{"repo/app:v1"}},
			{ID: "sha256:two", RepoTags: []string{"library/busybox:latest"}},
		},
	}
	withFakeImageManager(t, manager)

	err := saveImagesWithOptions("backup", SaveOptions{
		Filters: []string{"abcdef123456", "busybox"},
	})
	if err != nil {
		t.Fatalf("saveImagesWithOptions() error = %v", err)
	}
	if len(manager.saveCalls) != 2 {
		t.Fatalf("Save called %d times, want 2", len(manager.saveCalls))
	}
}

func TestSaveImagesDryRunDoesNotSave(t *testing.T) {
	manager := &fakeImageManager{
		images: []image.Summary{
			{ID: "sha256:one", RepoTags: []string{"repo/app:v1"}},
		},
	}
	withFakeImageManager(t, manager)

	err := saveImagesWithOptions("backup", SaveOptions{DryRun: true})
	if err != nil {
		t.Fatalf("saveImagesWithOptions() error = %v", err)
	}
	if len(manager.saveCalls) != 0 {
		t.Fatalf("Save called %d times, want 0", len(manager.saveCalls))
	}
}

func TestIsDockerImageArchive(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: "image.tar", want: true},
		{path: "image.TAR", want: true},
		{path: "image.tar.gz", want: true},
		{path: "image.tgz", want: true},
		{path: "image.zip", want: false},
		{path: "README.md", want: false},
		{path: "tar.txt", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := isDockerImageArchive(tt.path); got != tt.want {
				t.Fatalf("isDockerImageArchive(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestLoadImagesSkipsNonImageArchives(t *testing.T) {
	dir := t.TempDir()
	files := []string{
		"image.tar",
		"nested/image.tar.gz",
		"nested/image.tgz",
		"README.md",
		"nested/image.zip",
	}
	for _, name := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		if err := os.WriteFile(path, []byte("test"), 0644); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	}

	manager := &fakeImageManager{}
	withFakeImageManager(t, manager)

	if err := loadImages(dir); err != nil {
		t.Fatalf("loadImages() error = %v", err)
	}

	if len(manager.loadCalls) != 3 {
		t.Fatalf("Load called %d times, want 3: %v", len(manager.loadCalls), manager.loadCalls)
	}
	for _, loaded := range manager.loadCalls {
		if !isDockerImageArchive(loaded) {
			t.Fatalf("loaded non-image archive %q", loaded)
		}
	}
}

func TestLoadImagesSupportsSingleArchiveFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image.tar")
	if err := os.WriteFile(path, []byte("test"), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	manager := &fakeImageManager{}
	withFakeImageManager(t, manager)

	if err := loadImages(path); err != nil {
		t.Fatalf("loadImages() error = %v", err)
	}
	if len(manager.loadCalls) != 1 {
		t.Fatalf("Load called %d times, want 1", len(manager.loadCalls))
	}
	if manager.loadCalls[0] != path {
		t.Fatalf("Load called with %q, want %q", manager.loadCalls[0], path)
	}
}

func TestLoadImagesReturnsAggregatedErrors(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.tar")
	second := filepath.Join(dir, "second.tar")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("test"), 0644); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	}

	loadErr := errors.New("load failed")
	manager := &fakeImageManager{
		loadErrs: map[string]error{
			first: loadErr,
		},
	}
	withFakeImageManager(t, manager)

	err := loadImages(dir)
	if err == nil {
		t.Fatal("loadImages() error = nil, want load error")
	}
	if !errors.Is(err, loadErr) {
		t.Fatalf("loadImages() error = %v, want wrapped %v", err, loadErr)
	}
	if len(manager.loadCalls) != 2 {
		t.Fatalf("Load called %d times, want 2", len(manager.loadCalls))
	}
}
