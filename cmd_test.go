package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/docker/docker/api/types/image"
)

type fakeImageManager struct {
	images    []image.Summary
	saveErrs  map[string]error
	saveCalls []saveCall
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
	return nil
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
