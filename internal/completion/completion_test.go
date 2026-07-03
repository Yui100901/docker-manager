package completion

import (
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

func newCompletionTestRoot() *cobra.Command {
	cmd := &cobra.Command{Use: "dm"}
	cmd.PersistentFlags().String("config", defaultConfigPath, "")
	cmd.PersistentFlags().String("docker-host", "", "")
	cmd.PersistentFlags().Bool("docker-tls-verify", false, "")
	cmd.PersistentFlags().String("docker-cert-path", "", "")
	cmd.PersistentFlags().String("docker-api-version", "", "")
	return cmd
}
