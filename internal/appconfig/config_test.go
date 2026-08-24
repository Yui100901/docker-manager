package appconfig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadWithOptionsRejectsUnknownField(t *testing.T) {
	path := writeConfig(t, "docker_host: unix:///var/run/docker.sock\nunknown_option: true\n")

	_, err := LoadWithOptions(path, LoadOptions{Required: true})
	if err == nil || !strings.Contains(err.Error(), "field unknown_option not found") {
		t.Fatalf("LoadWithOptions() error = %v, want unknown field", err)
	}
}

func TestLoadWithOptionsDistinguishesExplicitMissingConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	loaded, err := LoadWithOptions(missing, LoadOptions{})
	if err != nil || loaded.Exists || loaded.Path != missing {
		t.Fatalf("optional missing config = %#v, %v", loaded, err)
	}

	_, err = LoadWithOptions(missing, LoadOptions{Required: true})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("required missing config error = %v, want os.ErrNotExist", err)
	}
}

func TestLoadWithOptionsRejectsMultipleDocuments(t *testing.T) {
	path := writeConfig(t, "proxy: http://proxy.example\n---\narch: arm64\n")

	_, err := LoadWithOptions(path, LoadOptions{Required: true})
	if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("LoadWithOptions() error = %v, want multiple document rejection", err)
	}
}

func TestLoadWithOptionsRejectsOversizedConfig(t *testing.T) {
	path := writeConfig(t, strings.Repeat("#", maxConfigFileSize+1))

	_, err := LoadWithOptions(path, LoadOptions{Required: true})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("LoadWithOptions() error = %v, want config size rejection", err)
	}
}

func TestLoadWithOptionsRejectsInvalidTypesAndRoot(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "field type", data: "verbose: [true]\n", want: "cannot unmarshal"},
		{name: "root type", data: "null\n", want: "config root must be a YAML mapping"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadWithOptions(writeConfig(t, tt.data), LoadOptions{Required: true})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("LoadWithOptions() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLoadWithOptionsTracksConfiguredFields(t *testing.T) {
	path := writeConfig(t, "proxy: http://proxy.example\nredact_profile: strict\nready_timeout: 45s\n")

	loaded, err := LoadWithOptions(path, LoadOptions{Required: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"proxy", "redact_profile", "ready_timeout"} {
		if !loaded.Fields[field] {
			t.Fatalf("Fields = %#v, want %s", loaded.Fields, field)
		}
	}
	if loaded.Config.EffectiveRedactProfile() != "strict" {
		t.Fatalf("EffectiveRedactProfile() = %q", loaded.Config.EffectiveRedactProfile())
	}
}

func TestLoadWithOptionsPreservesExplicitEmptyDockerFields(t *testing.T) {
	path := writeConfig(t, "docker_host: \"\"\ndocker_tls_verify:\ndocker_cert_path: \"\"\ndocker_api_version: \"\"\n")

	loaded, err := LoadWithOptions(path, LoadOptions{Required: true})
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Config.DockerHostSet || !loaded.Config.DockerTLSVerifySet ||
		!loaded.Config.DockerCertPathSet || !loaded.Config.DockerAPIVersionSet {
		t.Fatalf("Docker presence markers = %#v", loaded.Config)
	}
}

func TestLoadWithOptionsAcceptsDocumentedCADiagnosticFields(t *testing.T) {
	path := writeConfig(t, "ca_file: /etc/ssl/company.pem\nca_path: /etc/ssl/certs\nregistry_ca_file: /etc/docker/certs.d/example/ca.crt\nregistry_ca_path: /etc/docker/certs.d/example\n")
	loaded, err := LoadWithOptions(path, LoadOptions{Required: true})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Config.CAFile != "/etc/ssl/company.pem" || loaded.Config.RegistryCAPath != "/etc/docker/certs.d/example" {
		t.Fatalf("CA diagnostic config = %#v", loaded.Config)
	}
}

func TestConfigValidationRejectsUnsafeOrAmbiguousValues(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "invalid duration", data: "docker_timeout: never\n", want: "docker_timeout"},
		{name: "non-positive duration", data: "ready_timeout: 0s\n", want: "greater than zero"},
		{name: "insecure realm", data: "registry_auth_realms:\n  - http://auth.example\n", want: "HTTPS origin"},
		{name: "realm path", data: "registry_auth_realms:\n  - https://auth.example/token\n", want: "HTTPS origin"},
		{name: "realm wildcard", data: "registry_auth_realms:\n  - https://*.auth.example\n", want: "HTTPS origin"},
		{name: "redaction conflict", data: "redact_profile: none\nredact_secrets: true\n", want: "conflicts"},
		{name: "output conflict", data: "verbose: true\nquiet: true\n", want: "cannot both"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadWithOptions(writeConfig(t, tt.data), LoadOptions{Required: true})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("LoadWithOptions() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestConfigEffectiveRedactProfileDefaultsToVisibleOutput(t *testing.T) {
	if got := (Config{}).EffectiveRedactProfile(); got != "none" {
		t.Fatalf("EffectiveRedactProfile() = %q, want none", got)
	}
	if got := (Config{RedactSecrets: true}).EffectiveRedactProfile(); got != "basic" {
		t.Fatalf("legacy EffectiveRedactProfile() = %q, want basic", got)
	}
}

func TestPositiveDuration(t *testing.T) {
	got, err := PositiveDuration("ready_timeout", "45s", 30*time.Second)
	if err != nil || got != 45*time.Second {
		t.Fatalf("PositiveDuration() = %v, %v", got, err)
	}
	got, err = PositiveDuration("ready_timeout", "", 30*time.Second)
	if err != nil || got != 30*time.Second {
		t.Fatalf("PositiveDuration(fallback) = %v, %v", got, err)
	}
}

func writeConfig(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dm.yaml")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
