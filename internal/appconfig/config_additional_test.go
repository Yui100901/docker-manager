package appconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadUsesDefaultPathAndReturnsConfig(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(DefaultPath, []byte("arch: arm64\nverbose: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Arch != "arm64" || !cfg.Verbose {
		t.Fatalf("Load() = %#v, want default-path config", cfg)
	}
}

func TestLoadWithOptionsAcceptsEmptyMapping(t *testing.T) {
	path := writeConfig(t, "{}\n")
	loaded, err := LoadWithOptions(path, LoadOptions{Required: true})
	if err != nil {
		t.Fatalf("LoadWithOptions() error = %v", err)
	}
	if !loaded.Exists || loaded.Path != path || len(loaded.Fields) != 0 {
		t.Fatalf("LoadWithOptions(empty mapping) = %#v", loaded)
	}
	if !reflect.DeepEqual(loaded.Config, Config{}) {
		t.Fatalf("LoadWithOptions(empty mapping).Config = %#v, want zero config", loaded.Config)
	}
}

func TestResolvePathPrecedenceAndExplicitness(t *testing.T) {
	t.Setenv(EnvName, filepath.Join("env", "dm.yaml"))

	tests := []struct {
		name        string
		path        string
		flagChanged bool
		want        string
	}{
		{name: "explicit flag wins", path: filepath.Join("flag", "dm.yaml"), flagChanged: true, want: filepath.Join("flag", "dm.yaml")},
		{name: "explicit blank uses default", path: "  ", flagChanged: true, want: DefaultPath},
		{name: "environment wins over default flag value", path: DefaultPath, want: filepath.Join("env", "dm.yaml")},
		{name: "environment wins over implicit path", path: filepath.Join("implicit", "dm.yaml"), want: filepath.Join("env", "dm.yaml")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolvePath(tt.path, tt.flagChanged); got != tt.want {
				t.Fatalf("ResolvePath(%q, %t) = %q, want %q", tt.path, tt.flagChanged, got, tt.want)
			}
		})
	}
	if !IsExplicitPath(false) || !IsExplicitPath(true) {
		t.Fatal("IsExplicitPath() = false with configured environment/flag")
	}

	t.Setenv(EnvName, "  ")
	if got := ResolvePath("", false); got != DefaultPath {
		t.Fatalf("ResolvePath(blank) = %q, want %q", got, DefaultPath)
	}
	if IsExplicitPath(false) {
		t.Fatal("IsExplicitPath(false) = true with blank environment")
	}
}

func TestConfigValidateAcceptsHTTPSOrigins(t *testing.T) {
	cfg := Config{
		DockerTimeout:           " 15s ",
		ReadyTimeout:            "2m",
		CredentialHelperTimeout: "500ms",
		RedactProfile:           " STRICT ",
		RegistryAuthRealms: []string{
			"https://auth.example",
			" https://auth.example:8443/ ",
			"https://[2001:db8::1]:443",
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Config.Validate() error = %v", err)
	}
	if got := cfg.EffectiveRedactProfile(); got != "strict" {
		t.Fatalf("EffectiveRedactProfile() = %q, want strict", got)
	}
}

func TestConfigValidateRejectsAdditionalRealmAndProfileEdges(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "invalid profile", cfg: Config{RedactProfile: "maximum"}, want: "redact_profile"},
		{name: "realm credentials", cfg: Config{RegistryAuthRealms: []string{"https://user:pass@auth.example"}}, want: "HTTPS origin"},
		{name: "realm query", cfg: Config{RegistryAuthRealms: []string{"https://auth.example?service=registry"}}, want: "HTTPS origin"},
		{name: "realm fragment", cfg: Config{RegistryAuthRealms: []string{"https://auth.example#token"}}, want: "HTTPS origin"},
		{name: "realm missing host", cfg: Config{RegistryAuthRealms: []string{"https:///token"}}, want: "HTTPS origin"},
		{name: "credential helper timeout", cfg: Config{CredentialHelperTimeout: "-1s"}, want: "greater than zero"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Config.Validate() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestPositiveDurationRejectsInvalidValues(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "invalid", value: "later", want: "must be a duration"},
		{name: "zero", value: "0s", want: "greater than zero"},
		{name: "negative", value: "-1s", want: "greater than zero"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PositiveDuration("timeout", tt.value, time.Minute)
			if err == nil || got != 0 || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("PositiveDuration(%q) = %v, %v, want %q", tt.value, got, err, tt.want)
			}
		})
	}
}

func TestConfigFieldsHandlesEmptyMappingAndRejectsSequence(t *testing.T) {
	fields, err := configFields([]byte("{}\n"))
	if err != nil || len(fields) != 0 {
		t.Fatalf("configFields(empty mapping) = %#v, %v", fields, err)
	}
	fields, err = configFields(nil)
	if err != nil || len(fields) != 0 {
		t.Fatalf("configFields(nil) = %#v, %v", fields, err)
	}
	if _, err := configFields([]byte("- one\n- two\n")); err == nil || !strings.Contains(err.Error(), "YAML mapping") {
		t.Fatalf("configFields(sequence) error = %v, want mapping rejection", err)
	}
}
