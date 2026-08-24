package cli

import (
	"bytes"
	"context"
	"docker-manager/internal/appconfig"
	"docker-manager/internal/docker"
	"docker-manager/internal/sensitive"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func TestLoadAppConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dm.yaml")
	data := []byte("proxy: http://127.0.0.1:7890\nos: linux\narch: arm64\noutput_dir: dist\ndocker_host: tcp://docker.example.com:2376\ndocker_tls_verify: true\ndocker_cert_path: /tmp/certs\ndocker_api_version: \"1.46\"\nverbose: true\nlog_json: true\n")
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
	if cfg.DockerHost != "tcp://docker.example.com:2376" || cfg.DockerCertPath != "/tmp/certs" || cfg.DockerAPIVersion != "1.46" {
		t.Fatalf("docker config = %#v, want host/cert/api", cfg)
	}
	if cfg.DockerTLSVerify == nil || !*cfg.DockerTLSVerify {
		t.Fatalf("docker tls verify = %#v, want true", cfg.DockerTLSVerify)
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

func TestRootCommandRejectsExplicitMissingConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	cfg := appConfig{}
	opts := outputOptions{}
	cmd := newRootCommand(&cfg, &opts)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--config", missing, "version"})

	err := cmd.Execute()
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Execute() error = %v, want explicit missing config failure", err)
	}
}

func TestRootCommandRejectsMissingDMConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	t.Setenv(configEnvName, missing)
	cfg := appConfig{}
	opts := outputOptions{}
	cmd := newRootCommand(&cfg, &opts)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"version"})

	err := cmd.Execute()
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Execute() error = %v, want missing DM_CONFIG failure", err)
	}
}

func TestRootCommandRejectsUnknownConfigField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dm.yaml")
	if err := os.WriteFile(path, []byte("unknown_option: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := appConfig{}
	opts := outputOptions{}
	cmd := newRootCommand(&cfg, &opts)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--config", path, "version"})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "field unknown_option not found") {
		t.Fatalf("Execute() error = %v, want strict field failure", err)
	}
}

func TestExplicitRedactionAppliesBeforeConfigLoadError(t *testing.T) {
	t.Cleanup(func() { sensitive.SetDefaultProfile(sensitive.ProfileNone) })
	path := filepath.Join(t.TempDir(), "dm.yaml")
	if err := os.WriteFile(path, []byte("docker_timeout: token=load-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := appConfig{}
	opts := outputOptions{}
	cmd := newRootCommand(&cfg, &opts)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--config", path, "--redact-profile", "strict", "version"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want invalid duration")
	}
	var out bytes.Buffer
	writeCommandError(&out, err, opts)
	if strings.Contains(out.String(), "load-secret") || !strings.Contains(out.String(), sensitive.RedactedValue) {
		t.Fatalf("error output = %q, want pre-load redaction", out.String())
	}
}

func TestConfigValidateAndShowSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dm.yaml")
	if err := os.WriteFile(path, []byte("proxy: http://admin:secret@proxy.example\nready_timeout: 45s\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("validate", func(t *testing.T) {
		cfg := appConfig{}
		opts := outputOptions{}
		cmd := newRootCommand(&cfg, &opts)
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"--config", path, "config", "validate"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "配置有效") || !strings.Contains(out.String(), path) {
			t.Fatalf("validate output = %q", out.String())
		}
	})

	t.Run("effective sources retain sensitive values by default", func(t *testing.T) {
		cfg := appConfig{}
		opts := outputOptions{}
		cmd := newRootCommand(&cfg, &opts)
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"--config", path, "--docker-host", "tcp://flag.example:2375", "--docker-timeout", "7s", "config", "show", "--effective", "--show-source", "--format", "json"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		var report configShowReport
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatalf("json.Unmarshal() error = %v, output=%q", err, out.String())
		}
		if report.Values["proxy"] != "http://admin:secret@proxy.example" {
			t.Fatalf("proxy = %#v, want default unredacted administrator output", report.Values["proxy"])
		}
		if report.Values["ready_timeout"] != "45s" || report.Sources["ready_timeout"] != "config:"+path {
			t.Fatalf("ready timeout report = %#v %#v", report.Values, report.Sources)
		}
		if report.Values["docker_host"] != "tcp://flag.example:2375" || report.Sources["docker_host"] != "flag:--docker-host" {
			t.Fatalf("docker host report = %#v %#v", report.Values, report.Sources)
		}
		if report.Values["docker_timeout"] != "7s" || report.Sources["docker_timeout"] != "flag:--docker-timeout" {
			t.Fatalf("docker timeout report = %#v %#v", report.Values, report.Sources)
		}
		if report.Values["credential_helper_timeout"] != configuredCredentialHelperTimeout(&appConfig{}).String() {
			t.Fatalf("credential helper timeout = %#v", report.Values["credential_helper_timeout"])
		}
	})
}

func TestConfigShowReportsEffectivePullPlatformDefaultsAndSources(t *testing.T) {
	tests := []struct {
		name       string
		config     string
		wantOS     string
		wantArch   string
		wantSource string
	}{
		{name: "defaults", wantOS: defaultPullOS, wantArch: defaultPullArch, wantSource: "default"},
		{name: "configured", config: "os: windows\narch: arm64\n", wantOS: "windows", wantArch: "arm64", wantSource: "config"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "dm.yaml")
			if err := os.WriteFile(path, []byte(tt.config), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := appConfig{}
			opts := outputOptions{}
			cmd := newRootCommand(&cfg, &opts)
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{"--config", path, "config", "show", "--effective", "--show-source", "--format", "json"})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}

			var report configShowReport
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatalf("json.Unmarshal() error = %v, output=%q", err, out.String())
			}
			if report.Values["os"] != tt.wantOS || report.Values["arch"] != tt.wantArch {
				t.Fatalf("platform = os:%#v arch:%#v, want %s/%s", report.Values["os"], report.Values["arch"], tt.wantOS, tt.wantArch)
			}
			wantSource := tt.wantSource
			if wantSource == "config" {
				wantSource += ":" + path
			}
			if report.Sources["os"] != wantSource || report.Sources["arch"] != wantSource {
				t.Fatalf("platform sources = os:%q arch:%q, want %q", report.Sources["os"], report.Sources["arch"], wantSource)
			}
		})
	}
}

func TestConfigShowReportsRedactSecretsCompatibilityValueAndSource(t *testing.T) {
	tests := []struct {
		name              string
		effective         bool
		cfg               appConfig
		fields            map[string]bool
		redactSecretsFlag string
		redactProfileFlag string
		wantSecrets       bool
		wantSecretsSource string
		wantProfile       string
		wantProfileSource string
	}{
		{name: "raw default", wantProfile: "none", wantSecretsSource: "default", wantProfileSource: "default"},
		{name: "raw legacy config", cfg: appConfig{RedactSecrets: true}, fields: map[string]bool{"redact_secrets": true}, wantSecrets: true, wantSecretsSource: "config:test.yaml", wantProfile: "basic", wantProfileSource: "config:test.yaml"},
		{name: "effective legacy config", effective: true, cfg: appConfig{RedactSecrets: true}, fields: map[string]bool{"redact_secrets": true}, wantSecrets: true, wantSecretsSource: "config:test.yaml", wantProfile: "basic", wantProfileSource: "config:test.yaml"},
		{name: "effective legacy flag", effective: true, redactSecretsFlag: "true", wantSecrets: true, wantSecretsSource: "flag:--redact-secrets", wantProfile: "basic", wantProfileSource: "flag:--redact-secrets"},
		{name: "effective profile flag keeps compatibility input separate", effective: true, redactProfileFlag: "strict", wantSecretsSource: "default", wantProfile: "strict", wantProfileSource: "flag:--redact-profile"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := &cobra.Command{Use: "dm"}
			root.PersistentFlags().Bool("redact-secrets", false, "")
			root.PersistentFlags().String("redact-profile", "", "")
			show := &cobra.Command{Use: "show"}
			root.AddCommand(show)
			if tt.redactSecretsFlag != "" {
				if err := root.PersistentFlags().Set("redact-secrets", tt.redactSecretsFlag); err != nil {
					t.Fatal(err)
				}
			}
			if tt.redactProfileFlag != "" {
				if err := root.PersistentFlags().Set("redact-profile", tt.redactProfileFlag); err != nil {
					t.Fatal(err)
				}
			}
			loaded := appconfig.Loaded{Path: "test.yaml", Exists: true, Fields: tt.fields}
			report := buildConfigShowReport(show, &tt.cfg, &loaded, tt.effective, true)
			if report.Values["redact_secrets"] != tt.wantSecrets || report.Sources["redact_secrets"] != tt.wantSecretsSource {
				t.Fatalf("redact_secrets = %#v source=%q, want %v source=%q", report.Values["redact_secrets"], report.Sources["redact_secrets"], tt.wantSecrets, tt.wantSecretsSource)
			}
			if report.Values["redact_profile"] != tt.wantProfile || report.Sources["redact_profile"] != tt.wantProfileSource {
				t.Fatalf("redact_profile = %#v source=%q, want %q source=%q", report.Values["redact_profile"], report.Sources["redact_profile"], tt.wantProfile, tt.wantProfileSource)
			}
		})
	}
}

func TestConfigShowReportsExplicitEmptyDockerHostOverride(t *testing.T) {
	t.Cleanup(func() { docker.Configure(docker.Options{}) })
	t.Setenv("DOCKER_HOST", "tcp://environment.example:2375")
	path := filepath.Join(t.TempDir(), "dm.yaml")
	if err := os.WriteFile(path, []byte("docker_host: \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := appConfig{}
	opts := outputOptions{}
	cmd := newRootCommand(&cfg, &opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--config", path, "config", "show", "--effective", "--show-source", "--format", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	var report configShowReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("json.Unmarshal() error = %v, output=%q", err, out.String())
	}
	if got := report.Values["docker_host"]; got == "tcp://environment.example:2375" || got != docker.Endpoint() {
		t.Fatalf("docker_host = %#v, endpoint=%q", got, docker.Endpoint())
	}
	if got := report.Sources["docker_host"]; got != "config:"+path {
		t.Fatalf("docker_host source = %q", got)
	}
}

func TestConfigShowReportsRedactionFlagOverrideSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dm.yaml")
	if err := os.WriteFile(path, []byte("redact_profile: strict\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := appConfig{}
	opts := outputOptions{}
	cmd := newRootCommand(&cfg, &opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--config", path, "--redact-profile", "none", "config", "show", "--effective", "--show-source", "--format", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	var report configShowReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("json.Unmarshal() error = %v, output=%q", err, out.String())
	}
	if report.Values["redact_profile"] != "none" || report.Sources["redact_profile"] != "flag:--redact-profile" {
		t.Fatalf("redact profile report = %#v %#v", report.Values, report.Sources)
	}
}

func TestConfigShowReportsEffectiveOutputModeAndSource(t *testing.T) {
	tests := []struct {
		name          string
		config        string
		flags         []string
		wantVerbose   bool
		wantQuiet     bool
		verboseSource string
		quietSource   string
	}{
		{name: "quiet overrides configured verbose", config: "verbose: true\n", flags: []string{"--quiet"}, wantQuiet: true, verboseSource: "flag:--quiet", quietSource: "flag:--quiet"},
		{name: "verbose overrides configured quiet", config: "quiet: true\n", flags: []string{"--verbose"}, wantVerbose: true, verboseSource: "flag:--verbose", quietSource: "flag:--verbose"},
		{name: "explicit false quiet keeps configured verbose", config: "verbose: true\n", flags: []string{"--quiet=false"}, wantVerbose: true, verboseSource: "config", quietSource: "flag:--quiet"},
		{name: "explicit false verbose keeps configured quiet", config: "quiet: true\n", flags: []string{"--verbose=false"}, wantQuiet: true, verboseSource: "flag:--verbose", quietSource: "config"},
		{name: "verbose wins when both true flags are present", flags: []string{"--verbose", "--quiet"}, wantVerbose: true, verboseSource: "flag:--verbose", quietSource: "flag:--verbose"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "dm.yaml")
			if err := os.WriteFile(path, []byte(tt.config), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := appConfig{}
			opts := outputOptions{}
			cmd := newRootCommand(&cfg, &opts)
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			args := []string{"--config", path}
			args = append(args, tt.flags...)
			args = append(args, "config", "show", "--effective", "--show-source", "--format", "json")
			cmd.SetArgs(args)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}

			var report configShowReport
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatalf("json.Unmarshal() error = %v, output=%q", err, out.String())
			}
			if report.Values["verbose"] != tt.wantVerbose || report.Values["quiet"] != tt.wantQuiet {
				t.Fatalf("output modes = verbose:%#v quiet:%#v", report.Values["verbose"], report.Values["quiet"])
			}
			wantVerboseSource := tt.verboseSource
			if wantVerboseSource == "config" {
				wantVerboseSource += ":" + path
			}
			wantQuietSource := tt.quietSource
			if wantQuietSource == "config" {
				wantQuietSource += ":" + path
			}
			if report.Sources["verbose"] != wantVerboseSource || report.Sources["quiet"] != wantQuietSource {
				t.Fatalf("output sources = verbose:%q quiet:%q, want verbose:%q quiet:%q", report.Sources["verbose"], report.Sources["quiet"], wantVerboseSource, wantQuietSource)
			}
		})
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

func TestWriteCommandErrorCanceledText(t *testing.T) {
	var buf bytes.Buffer
	writeCommandError(&buf, context.Canceled, outputOptions{})

	if got := buf.String(); !strings.Contains(got, "操作已取消") || strings.Contains(got, "context canceled") {
		t.Fatalf("cancel text error = %q, want friendly cancellation message", got)
	}
}

func TestWriteCommandErrorCanceledJSON(t *testing.T) {
	var buf bytes.Buffer
	writeCommandError(&buf, context.Canceled, outputOptions{JSON: true})

	var got map[string]string
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal() error = %v, output=%q", err, buf.String())
	}
	if got["level"] != "error" || got["error"] != "操作已取消" {
		t.Fatalf("cancel error json = %#v, want friendly cancellation message", got)
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

func TestRootCommandAppliesDockerConfigDefaults(t *testing.T) {
	t.Cleanup(func() { docker.Configure(docker.Options{}) })
	dir := t.TempDir()
	configPath := filepath.Join(dir, "dm.yaml")
	if err := os.WriteFile(configPath, []byte("docker_host: tcp://configured.example:2376\ndocker_tls_verify: true\ndocker_cert_path: /configured/certs\ndocker_api_version: \"1.45\"\ndocker_timeout: 42s\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := appConfig{}
	opts := outputOptions{}
	cmd := newRootCommand(&cfg, &opts)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--config", configPath, "version"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	got := docker.CurrentOptions()
	if got.Host != "tcp://configured.example:2376" || got.CertPath != "/configured/certs" || got.APIVersion != "1.45" {
		t.Fatalf("docker options = %#v, want configured values", got)
	}
	if got.TLSVerify == nil || !*got.TLSVerify {
		t.Fatalf("docker tls verify = %#v, want true", got.TLSVerify)
	}
	if got.Timeout != 42*time.Second {
		t.Fatalf("docker timeout = %v, want 42s", got.Timeout)
	}
}

func TestRootCommandDockerFlagsOverrideConfig(t *testing.T) {
	t.Cleanup(func() { docker.Configure(docker.Options{}) })
	dir := t.TempDir()
	configPath := filepath.Join(dir, "dm.yaml")
	if err := os.WriteFile(configPath, []byte("docker_host: tcp://configured.example:2376\ndocker_tls_verify: true\ndocker_cert_path: /configured/certs\ndocker_api_version: \"1.45\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := appConfig{}
	opts := outputOptions{}
	cmd := newRootCommand(&cfg, &opts)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--config", configPath,
		"--docker-host", "tcp://flag.example:2376",
		"--docker-tls-verify=false",
		"--docker-cert-path", "/flag/certs",
		"--docker-api-version", "1.46",
		"--docker-timeout", "3s",
		"version",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	got := docker.CurrentOptions()
	if got.Host != "tcp://flag.example:2376" || got.CertPath != "/flag/certs" || got.APIVersion != "1.46" {
		t.Fatalf("docker options = %#v, want flag values", got)
	}
	if got.TLSVerify == nil || *got.TLSVerify {
		t.Fatalf("docker tls verify = %#v, want false", got.TLSVerify)
	}
	if got.Timeout != 3*time.Second {
		t.Fatalf("docker timeout = %v, want 3s", got.Timeout)
	}
}

func TestDoctorAppliesExplicitDockerFlagsWhenConfigIsInvalid(t *testing.T) {
	var plainHits atomic.Int64
	plainServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		plainHits.Add(1)
		w.Header().Set("API-Version", "1.49")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(plainServer.Close)

	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(plainServer.URL, "http://"))
	t.Setenv("DOCKER_TLS_VERIFY", "")
	t.Setenv("DOCKER_CERT_PATH", "")
	t.Setenv("DOCKER_API_VERSION", "")
	docker.Configure(docker.Options{})
	if _, err := docker.NewMobyClient(); err != nil {
		t.Fatalf("NewMobyClient() for ambient HTTP endpoint error = %v", err)
	}
	t.Cleanup(func() {
		docker.Configure(docker.Options{Host: "tcp://doctor-test-reset.invalid:1"})
		docker.Configure(docker.Options{})
	})

	configPath := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(configPath, []byte("docker_host: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missingCertPath := filepath.Join(t.TempDir(), "missing-certs")
	cfg := appConfig{DockerHost: "tcp://stale.invalid:2375"}
	opts := outputOptions{}
	cmd := newRootCommand(&cfg, &opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--config", configPath,
		"--docker-host", "tcp://secure.example:2376",
		"--docker-tls-verify=true",
		"--docker-cert-path", missingCertPath,
		"doctor", "--format", "json", "--check-e2e=false", "--output-dir", t.TempDir(),
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	got := docker.CurrentOptions()
	if got.Host != "tcp://secure.example:2376" || got.CertPath != missingCertPath || got.TLSVerify == nil || !*got.TLSVerify {
		t.Fatalf("CurrentOptions() = %#v, want explicit TLS flags despite invalid YAML", got)
	}
	newClient, _, err := docker.NewMobyClientWithInfo()
	if err == nil || newClient != nil {
		t.Fatalf("NewMobyClientWithInfo() = (%p, %v), want missing TLS certificate error", newClient, err)
	}
	if got := plainHits.Load(); got != 0 {
		t.Fatalf("ambient plaintext endpoint requests = %d, want 0", got)
	}
	if !strings.Contains(out.String(), "secure.example:2376") || !strings.Contains(out.String(), "Docker endpoint 初始化失败") {
		t.Fatalf("doctor output = %q, want explicit endpoint initialization failure", out.String())
	}
}

func TestRootCommandExposesLeafShortcuts(t *testing.T) {
	cfg := appConfig{}
	opts := outputOptions{}
	cmd := newRootCommand(&cfg, &opts)

	for _, name := range []string{"pull", "load", "save", "tree", "health", "network", "logs", "diff", "prune", "volumes", "registry"} {
		sub, _, err := cmd.Find([]string{name})
		if err != nil {
			t.Fatalf("Find(%s) error = %v", name, err)
		}
		if sub == nil || sub.Name() != name {
			t.Fatalf("Find(%s) = %#v, want root shortcut", name, sub)
		}
		if len(sub.Commands()) != 0 {
			t.Fatalf("%s should be a leaf shortcut, got subcommands %#v", name, sub.Commands())
		}
	}
	report, _, err := cmd.Find([]string{"report", "registry"})
	if err != nil {
		t.Fatalf("Find(report registry) error = %v", err)
	}
	if report == nil || report.Name() != "registry" {
		t.Fatalf("Find(report registry) = %#v, want registry report command", report)
	}
	reportAll, _, err := cmd.Find([]string{"report", "all"})
	if err != nil {
		t.Fatalf("Find(report all) error = %v", err)
	}
	if reportAll == nil || reportAll.Name() != "all" {
		t.Fatalf("Find(report all) = %#v, want all report command", reportAll)
	}
	imagePull, _, err := cmd.Find([]string{"image", "pull"})
	if err != nil {
		t.Fatalf("Find(image pull) error = %v", err)
	}
	if imagePull == nil || imagePull.Name() != "pull" {
		t.Fatalf("Find(image pull) = %#v, want pull command", imagePull)
	}
}

func TestShortcutCommandsMatchGroupedCommandFlags(t *testing.T) {
	cfg := appConfig{}
	opts := outputOptions{}
	root := newRootCommand(&cfg, &opts)

	tests := []struct {
		shortcut []string
		grouped  []string
	}{
		{shortcut: []string{"pull"}, grouped: []string{"image", "pull"}},
		{shortcut: []string{"save"}, grouped: []string{"image", "save"}},
		{shortcut: []string{"load"}, grouped: []string{"image", "load"}},
		{shortcut: []string{"tree"}, grouped: []string{"image", "tree"}},
		{shortcut: []string{"health"}, grouped: []string{"report", "health"}},
		{shortcut: []string{"network"}, grouped: []string{"report", "network"}},
		{shortcut: []string{"logs"}, grouped: []string{"report", "logs"}},
		{shortcut: []string{"diff"}, grouped: []string{"report", "diff"}},
		{shortcut: []string{"prune"}, grouped: []string{"report", "prune"}},
		{shortcut: []string{"volumes"}, grouped: []string{"report", "volumes"}},
		{shortcut: []string{"registry"}, grouped: []string{"report", "registry"}},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.shortcut, " "), func(t *testing.T) {
			shortcut := mustFindCommand(t, root, tt.shortcut)
			grouped := mustFindCommand(t, root, tt.grouped)
			got := commandFlagSignatures(shortcut)
			want := commandFlagSignatures(grouped)
			if !equalStringMaps(got, want) {
				t.Fatalf("flag signatures differ\nshortcut %v: %#v\ngrouped %v: %#v", tt.shortcut, got, tt.grouped, want)
			}
		})
	}
}

func mustFindCommand(t *testing.T, root *cobra.Command, args []string) *cobra.Command {
	t.Helper()
	cmd, _, err := root.Find(args)
	if err != nil {
		t.Fatalf("Find(%v) error = %v", args, err)
	}
	if cmd == nil {
		t.Fatalf("Find(%v) = nil", args)
	}
	return cmd
}

func commandFlagSignatures(cmd *cobra.Command) map[string]string {
	result := map[string]string{}
	cmd.Flags().VisitAll(func(flag *pflag.Flag) {
		result[flag.Name] = strings.Join([]string{flag.Shorthand, flag.DefValue, flag.Value.Type()}, "\x00")
	})
	return result
}

func equalStringMaps(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, leftValue := range left {
		if right[key] != leftValue {
			return false
		}
	}
	return true
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
