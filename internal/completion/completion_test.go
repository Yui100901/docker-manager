package completion

import (
	"docker-manager/internal/appconfig"
	"docker-manager/internal/docker"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestFilterCompletionValuesSortsDeduplicatesAndMatchesPrefix(t *testing.T) {
	got := filterCompletionValues([]string{"worker", "api", "api", "db"}, "a")
	if strings.Join(got, ",") != "api" {
		t.Fatalf("filterCompletionValues() = %#v, want api", got)
	}
}

func TestCompleteFixedValuesDisablesFileCompletion(t *testing.T) {
	fn := FixedValues("json", "text")
	values, directive := fn(&cobra.Command{}, nil, "j")
	if strings.Join(values, ",") != "json" {
		t.Fatalf("values = %#v, want json", values)
	}
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("directive = %v, want NoFileComp", directive)
	}
}

func TestPrepareDockerCompletionUsesConfig(t *testing.T) {
	t.Cleanup(func() { docker.Configure(docker.Options{}) })
	dir := t.TempDir()
	configPath := filepath.Join(dir, "dm.yaml")
	if err := os.WriteFile(configPath, []byte("docker_host: tcp://configured.example:2376\ndocker_tls_verify: true\ndocker_cert_path: /configured/certs\ndocker_api_version: \"1.45\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := newCompletionTestRoot()
	if err := cmd.PersistentFlags().Set("config", configPath); err != nil {
		t.Fatal(err)
	}

	if err := prepareDockerCompletion(cmd); err != nil {
		t.Fatalf("prepareDockerCompletion() error = %v", err)
	}

	got := docker.CurrentOptions()
	if got.Host != "tcp://configured.example:2376" || got.CertPath != "/configured/certs" || got.APIVersion != "1.45" {
		t.Fatalf("docker options = %#v, want configured values", got)
	}
	if got.TLSVerify == nil || !*got.TLSVerify {
		t.Fatalf("docker tls verify = %#v, want true", got.TLSVerify)
	}
}

func TestPrepareDockerCompletionDockerFlagsOverrideConfig(t *testing.T) {
	t.Cleanup(func() { docker.Configure(docker.Options{}) })
	dir := t.TempDir()
	configPath := filepath.Join(dir, "dm.yaml")
	if err := os.WriteFile(configPath, []byte("docker_host: tcp://configured.example:2376\ndocker_tls_verify: true\ndocker_cert_path: /configured/certs\ndocker_api_version: \"1.45\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := newCompletionTestRoot()
	for name, value := range map[string]string{
		"config":             configPath,
		"docker-host":        "tcp://flag.example:2376",
		"docker-tls-verify":  "false",
		"docker-cert-path":   "/flag/certs",
		"docker-api-version": "1.46",
	} {
		if err := cmd.PersistentFlags().Set(name, value); err != nil {
			t.Fatal(err)
		}
	}

	if err := prepareDockerCompletion(cmd); err != nil {
		t.Fatalf("prepareDockerCompletion() error = %v", err)
	}

	got := docker.CurrentOptions()
	if got.Host != "tcp://flag.example:2376" || got.CertPath != "/flag/certs" || got.APIVersion != "1.46" {
		t.Fatalf("docker options = %#v, want flag values", got)
	}
	if got.TLSVerify == nil || *got.TLSVerify {
		t.Fatalf("docker tls verify = %#v, want false", got.TLSVerify)
	}
}

func TestPrepareDockerCompletionUsesDMConfigWhenConfigFlagUnset(t *testing.T) {
	t.Cleanup(func() { docker.Configure(docker.Options{}) })
	dir := t.TempDir()
	configPath := filepath.Join(dir, "dm.yaml")
	if err := os.WriteFile(configPath, []byte("docker_host: tcp://env-config.example:2376\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(configEnvName, configPath)
	cmd := newCompletionTestRoot()

	if err := prepareDockerCompletion(cmd); err != nil {
		t.Fatalf("prepareDockerCompletion() error = %v", err)
	}

	if got := docker.CurrentOptions().Host; got != "tcp://env-config.example:2376" {
		t.Fatalf("docker host = %q, want DM_CONFIG host", got)
	}
}

func TestPrepareDockerCompletionUsesSelectedProfile(t *testing.T) {
	t.Cleanup(func() { docker.Configure(docker.Options{}) })
	configPath := filepath.Join(t.TempDir(), "dm.yaml")
	data := "docker_host: tcp://base.example:2375\nprofiles:\n  production:\n    docker_host: tcp://production.example:2376\n"
	if err := os.WriteFile(configPath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newCompletionTestRoot()
	if err := cmd.PersistentFlags().Set("config", configPath); err != nil {
		t.Fatal(err)
	}
	if err := cmd.PersistentFlags().Set("profile", "production"); err != nil {
		t.Fatal(err)
	}
	if err := prepareDockerCompletion(cmd); err != nil {
		t.Fatalf("prepareDockerCompletion() error = %v", err)
	}
	if got := docker.CurrentOptions().Host; got != "tcp://production.example:2376" {
		t.Fatalf("docker host = %q, want selected profile endpoint", got)
	}
}

func TestPrepareDockerCompletionUsesDMProfile(t *testing.T) {
	t.Cleanup(func() { docker.Configure(docker.Options{}) })
	configPath := filepath.Join(t.TempDir(), "dm.yaml")
	data := "profiles:\n  staging:\n    docker_host: tcp://staging.example:2375\n"
	if err := os.WriteFile(configPath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(configEnvName, configPath)
	t.Setenv(appconfig.ProfileEnvName, "staging")
	cmd := newCompletionTestRoot()
	if err := prepareDockerCompletion(cmd); err != nil {
		t.Fatalf("prepareDockerCompletion() error = %v", err)
	}
	if got := docker.CurrentOptions().Host; got != "tcp://staging.example:2375" {
		t.Fatalf("docker host = %q, want DM_PROFILE endpoint", got)
	}
}

func TestPrepareDockerCompletionUsesDefaultProfile(t *testing.T) {
	t.Cleanup(func() { docker.Configure(docker.Options{}) })
	configPath := filepath.Join(t.TempDir(), "dm.yaml")
	data := "default_profile: staging\ndocker_host: tcp://base.example:2375\nprofiles:\n  staging:\n    docker_host: tcp://staging.example:2376\n"
	if err := os.WriteFile(configPath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newCompletionTestRoot()
	if err := cmd.PersistentFlags().Set("config", configPath); err != nil {
		t.Fatal(err)
	}
	if err := prepareDockerCompletion(cmd); err != nil {
		t.Fatalf("prepareDockerCompletion() error = %v", err)
	}
	if got := docker.CurrentOptions().Host; got != "tcp://staging.example:2376" {
		t.Fatalf("docker host = %q, want default profile endpoint", got)
	}
}

func TestPrepareDockerCompletionExplicitEmptyProfileDisablesEnvironmentAndDefault(t *testing.T) {
	t.Cleanup(func() { docker.Configure(docker.Options{}) })
	configPath := filepath.Join(t.TempDir(), "dm.yaml")
	data := "default_profile: staging\ndocker_host: tcp://base.example:2375\nprofiles:\n  staging:\n    docker_host: tcp://staging.example:2376\n"
	if err := os.WriteFile(configPath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(appconfig.ProfileEnvName, "staging")
	cmd := newCompletionTestRoot()
	if err := cmd.PersistentFlags().Set("config", configPath); err != nil {
		t.Fatal(err)
	}
	if err := cmd.PersistentFlags().Set("profile", ""); err != nil {
		t.Fatal(err)
	}
	if err := prepareDockerCompletion(cmd); err != nil {
		t.Fatalf("prepareDockerCompletion() error = %v", err)
	}
	if got := docker.CurrentOptions().Host; got != "tcp://base.example:2375" {
		t.Fatalf("docker host = %q, want base endpoint", got)
	}
}

func TestConfigProfilesCompletesConfiguredNames(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "dm.yaml")
	data := "profiles:\n  production: {}\n  preview: {}\n  staging: {}\n"
	if err := os.WriteFile(configPath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newCompletionTestRoot()
	if err := cmd.PersistentFlags().Set("config", configPath); err != nil {
		t.Fatal(err)
	}
	values, directive := ConfigProfiles(cmd, nil, "pr")
	if strings.Join(values, ",") != "preview,production" {
		t.Fatalf("ConfigProfiles() = %#v", values)
	}
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("directive = %v, want NoFileComp", directive)
	}
}

func TestCompletionRejectsExplicitMissingConfig(t *testing.T) {
	t.Cleanup(func() { docker.Configure(docker.Options{}) })
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	cmd := newCompletionTestRoot()
	if err := cmd.PersistentFlags().Set("config", missing); err != nil {
		t.Fatal(err)
	}
	if err := prepareDockerCompletion(cmd); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("prepareDockerCompletion() error = %v, want explicit missing config failure", err)
	}
	if values, directive := ConfigProfiles(cmd, nil, ""); len(values) != 0 || directive != cobra.ShellCompDirectiveError {
		t.Fatalf("ConfigProfiles() = (%#v, %v), want error directive", values, directive)
	}
}

func newCompletionTestRoot() *cobra.Command {
	cmd := &cobra.Command{Use: "dm"}
	cmd.PersistentFlags().String("config", defaultConfigPath, "")
	cmd.PersistentFlags().String("profile", "", "")
	cmd.PersistentFlags().String("docker-host", "", "")
	cmd.PersistentFlags().Bool("docker-tls-verify", false, "")
	cmd.PersistentFlags().String("docker-cert-path", "", "")
	cmd.PersistentFlags().String("docker-api-version", "", "")
	return cmd
}
