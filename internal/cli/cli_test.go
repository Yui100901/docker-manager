package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAppConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dm.yaml")
	data := []byte("proxy: http://127.0.0.1:7890\nos: linux\narch: arm64\noutput_dir: dist\nverbose: true\nlog_json: true\n")
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

func TestResolveConfigPathUsesDMConfigWhenFlagUnset(t *testing.T) {
	t.Setenv(configEnvName, filepath.Join(t.TempDir(), "dm.yaml"))

	got := resolveConfigPath(defaultConfigPath, false)

	if got != os.Getenv(configEnvName) {
		t.Fatalf("resolveConfigPath() = %q, want DM_CONFIG", got)
	}
}

func TestResolveConfigPathKeepsExplicitConfig(t *testing.T) {
	t.Setenv(configEnvName, filepath.Join(t.TempDir(), "dm.yaml"))

	got := resolveConfigPath("explicit.yaml", true)

	if got != "explicit.yaml" {
		t.Fatalf("resolveConfigPath() = %q, want explicit path", got)
	}
}

func TestRootCommandLoadsDMConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "dm.yaml")
	if err := os.WriteFile(configPath, []byte("output_dir: from-env\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(configEnvName, configPath)

	cfg := appConfig{}
	opts := outputOptions{}
	cmd := newRootCommand(&cfg, &opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"doctor", "--format", "json", "--check-e2e=false"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(out.String(), "from-env") {
		t.Fatalf("doctor output did not use DM_CONFIG output_dir, output=%s", out.String())
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

func TestRootCommandLogJSONFlagAlias(t *testing.T) {
	cfg := appConfig{}
	opts := outputOptions{}
	cmd := newRootCommand(&cfg, &opts)

	if flag := cmd.PersistentFlags().Lookup("log-json"); flag == nil {
		t.Fatal("missing --log-json flag")
	}
	if flag := cmd.PersistentFlags().Lookup("json"); flag != nil {
		t.Fatal("--json compatibility flag should be removed")
	}

	cmd.SetArgs([]string{"--log-json", "version"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !opts.JSON {
		t.Fatal("--log-json did not enable JSON logs/errors")
	}
}

func TestPreseedJSONErrorMode(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "log json", args: []string{"--log-json", "missing"}, want: true},
		{name: "log json false", args: []string{"--log-json=false", "missing"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := outputOptions{}
			preseedJSONErrorMode(&opts, tt.args)
			if opts.JSON != tt.want {
				t.Fatalf("opts.JSON = %v, want %v", opts.JSON, tt.want)
			}
		})
	}
}
