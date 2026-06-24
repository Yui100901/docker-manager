package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/docker/api/types/image"
)

func TestLoadAppConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dm.yaml")
	data := []byte("proxy: http://127.0.0.1:7890\nos: linux\narch: arm64\noutput_dir: dist\nverbose: true\njson: true\n")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := loadAppConfig(path)
	if err != nil {
		t.Fatalf("loadAppConfig() error = %v", err)
	}
	if cfg.Proxy != "http://127.0.0.1:7890" || cfg.TargetOS != "linux" || cfg.Arch != "arm64" || cfg.OutputDir != "dist" {
		t.Fatalf("config = %#v, want proxy/os/arch/output_dir", cfg)
	}
	if !cfg.Verbose || !cfg.JSON {
		t.Fatalf("config flags = %#v, want verbose and json", cfg)
	}
}

func TestWriteCommandErrorJSON(t *testing.T) {
	var buf bytes.Buffer
	writeCommandError(&buf, errors.New("boom"), outputOptions{JSON: true})

	var got map[string]string
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal() error = %v, output=%q", err, buf.String())
	}
	if got["level"] != "error" || got["error"] != "boom" {
		t.Fatalf("error json = %#v, want level=error error=boom", got)
	}
}

func TestSaveCommandUsesConfiguredDefaultOutputDir(t *testing.T) {
	outputDir := t.TempDir()
	manager := &fakeImageManager{
		images: []image.Summary{
			{ID: "sha256:one", RepoTags: []string{"repo/app:v1"}},
		},
	}
	withFakeImageManager(t, manager)

	cmd := newSaveCommandWithDefaults(func() string { return outputDir })
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(manager.saveCalls) != 1 {
		t.Fatalf("Save called %d times, want 1", len(manager.saveCalls))
	}
	want := filepath.Join(outputDir, "repo_app-v1.tar")
	if manager.saveCalls[0].outputFile != want {
		t.Fatalf("outputFile = %q, want %q", manager.saveCalls[0].outputFile, want)
	}
}
