package dockerconfig

import (
	"strings"
	"testing"
	"time"

	"docker-manager/internal/appconfig"
	"docker-manager/internal/docker"

	"github.com/spf13/cobra"
)

func TestOptionsFromConfigAppliesConfigAndFlagPrecedence(t *testing.T) {
	tlsVerify := true
	cfg := appconfig.Config{
		DockerHost:          "tcp://config.example:2376",
		DockerHostSet:       true,
		DockerTLSVerify:     &tlsVerify,
		DockerTLSVerifySet:  true,
		DockerCertPath:      "/config/certs",
		DockerCertPathSet:   true,
		DockerAPIVersion:    "1.48",
		DockerAPIVersionSet: true,
		DockerTimeout:       "45s",
	}
	flags := FlagValues{
		Host:              "tcp://flag.example:2375",
		HostChanged:       true,
		TLSVerify:         false,
		TLSVerifyChanged:  true,
		CertPath:          "",
		CertPathChanged:   true,
		APIVersion:        "1.49",
		APIVersionChanged: true,
		Timeout:           9 * time.Second,
		TimeoutChanged:    true,
	}

	got := OptionsFromConfig(cfg, flags)
	if got.Host != flags.Host || !got.HostSet || got.CertPath != "" || !got.CertPathSet ||
		got.APIVersion != flags.APIVersion || !got.APIVersionSet || got.Timeout != flags.Timeout {
		t.Fatalf("OptionsFromConfig() = %#v, want flag overrides", got)
	}
	if got.TLSVerify == nil || *got.TLSVerify {
		t.Fatalf("TLSVerify = %#v, want explicit false", got.TLSVerify)
	}
}

func TestOptionsFromConfigDefaultsInvalidTimeoutAndPreservesExplicitNullTLS(t *testing.T) {
	got := OptionsFromConfig(appconfig.Config{
		DockerTLSVerifySet: true,
		DockerTimeout:      "invalid",
	}, FlagValues{})
	if got.Timeout != docker.DefaultRequestTimeout {
		t.Fatalf("Timeout = %s, want default %s", got.Timeout, docker.DefaultRequestTimeout)
	}
	if got.TLSVerify == nil || *got.TLSVerify {
		t.Fatalf("TLSVerify = %#v, want explicit false for configured null", got.TLSVerify)
	}

	got = OptionsFromConfig(appconfig.Config{DockerTimeout: "0s"}, FlagValues{})
	if got.Timeout != docker.DefaultRequestTimeout {
		t.Fatalf("non-positive config timeout = %s, want default", got.Timeout)
	}
}

func TestOptionsFromRootFlagsReadsRootPersistentFlags(t *testing.T) {
	root := newDockerConfigTestRoot()
	child := &cobra.Command{Use: "child"}
	root.AddCommand(child)
	for name, value := range map[string]string{
		"docker-host":        "tcp://flag.example:2375",
		"docker-tls-verify":  "false",
		"docker-cert-path":   "",
		"docker-api-version": "1.49",
		"docker-timeout":     "7s",
	} {
		if err := root.PersistentFlags().Set(name, value); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}
	tlsVerify := true
	cfg := appconfig.Config{
		DockerHost:          "tcp://config.example:2376",
		DockerHostSet:       true,
		DockerTLSVerify:     &tlsVerify,
		DockerTLSVerifySet:  true,
		DockerCertPath:      "/config/certs",
		DockerCertPathSet:   true,
		DockerAPIVersion:    "1.48",
		DockerAPIVersionSet: true,
		DockerTimeout:       "1m",
	}

	got, err := OptionsFromRootFlags(cfg, child)
	if err != nil {
		t.Fatalf("OptionsFromRootFlags() error = %v", err)
	}
	if got.Host != "tcp://flag.example:2375" || !got.HostSet || got.CertPath != "" || !got.CertPathSet ||
		got.APIVersion != "1.49" || !got.APIVersionSet || got.Timeout != 7*time.Second {
		t.Fatalf("OptionsFromRootFlags() = %#v", got)
	}
	if got.TLSVerify == nil || *got.TLSVerify {
		t.Fatalf("TLSVerify = %#v, want explicit false flag", got.TLSVerify)
	}
}

func TestOptionsFromRootFlagsHandlesNilAndMissingFlags(t *testing.T) {
	cfg := appconfig.Config{DockerHost: "tcp://config.example:2375", DockerTimeout: "12s"}

	fromNil, err := OptionsFromRootFlags(cfg, nil)
	if err != nil {
		t.Fatalf("OptionsFromRootFlags(nil) error = %v", err)
	}
	fromEmpty, err := OptionsFromRootFlags(cfg, &cobra.Command{Use: "dm"})
	if err != nil {
		t.Fatalf("OptionsFromRootFlags(empty) error = %v", err)
	}
	if fromNil.Host != cfg.DockerHost || fromEmpty.Host != cfg.DockerHost ||
		fromNil.Timeout != 12*time.Second || fromEmpty.Timeout != 12*time.Second {
		t.Fatalf("nil/empty options = %#v / %#v", fromNil, fromEmpty)
	}
}

func TestOptionsFromRootFlagsRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name      string
		flagName  string
		flagValue string
		want      string
	}{
		{name: "invalid tls bool", flagName: "docker-tls-verify", flagValue: "sometimes", want: "invalid syntax"},
		{name: "invalid timeout", flagName: "docker-timeout", flagValue: "later", want: "invalid duration"},
		{name: "zero timeout", flagName: "docker-timeout", flagValue: "0s", want: "greater than zero"},
		{name: "negative timeout", flagName: "docker-timeout", flagValue: "-1s", want: "greater than zero"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := &cobra.Command{Use: "dm"}
			root.PersistentFlags().String(tt.flagName, "", "")
			if err := root.PersistentFlags().Set(tt.flagName, tt.flagValue); err != nil {
				t.Fatal(err)
			}
			_, err := OptionsFromRootFlags(appconfig.Config{}, root)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("OptionsFromRootFlags() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func newDockerConfigTestRoot() *cobra.Command {
	root := &cobra.Command{Use: "dm"}
	root.PersistentFlags().String("docker-host", "", "")
	root.PersistentFlags().Bool("docker-tls-verify", false, "")
	root.PersistentFlags().String("docker-cert-path", "", "")
	root.PersistentFlags().String("docker-api-version", "", "")
	root.PersistentFlags().Duration("docker-timeout", docker.DefaultRequestTimeout, "")
	return root
}
