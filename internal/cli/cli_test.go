package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
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
