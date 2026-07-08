package cli

import (
	"bytes"
	"context"
	"docker-manager/internal/docker"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if err := os.WriteFile(configPath, []byte("docker_host: tcp://configured.example:2376\ndocker_tls_verify: true\ndocker_cert_path: /configured/certs\ndocker_api_version: \"1.45\"\n"), 0644); err != nil {
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
