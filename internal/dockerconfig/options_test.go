package dockerconfig

import (
	"os"
	"path/filepath"
	"testing"

	"docker-manager/internal/appconfig"
	"docker-manager/internal/docker"
)

func TestExplicitEmptyConfigClearsDockerEnvironmentFallbacks(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://environment.example:2375")
	t.Setenv("DOCKER_CERT_PATH", filepath.Join(t.TempDir(), "environment-certs"))
	t.Setenv("DOCKER_API_VERSION", "1.49")
	t.Setenv("DOCKER_TLS_VERIFY", "1")

	path := filepath.Join(t.TempDir(), "dm.yaml")
	if err := os.WriteFile(path, []byte("docker_host: \"\"\ndocker_tls_verify:\ndocker_cert_path: \"\"\ndocker_api_version: \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := appconfig.LoadWithOptions(path, appconfig.LoadOptions{Required: true})
	if err != nil {
		t.Fatal(err)
	}

	docker.Configure(OptionsFromConfig(loaded.Config, FlagValues{}))
	t.Cleanup(func() { docker.Configure(docker.Options{}) })
	effective := docker.EffectiveOptions()
	if effective.Host != "" || effective.CertPath != "" || effective.APIVersion != "" {
		t.Fatalf("effective Docker options = %#v, want explicit empty strings", effective)
	}
	if effective.TLSVerify == nil || *effective.TLSVerify {
		t.Fatalf("effective TLS verify = %#v, want explicit false", effective.TLSVerify)
	}
}

func TestExplicitEmptyFlagsClearConfiguredDockerValues(t *testing.T) {
	opts := OptionsFromConfig(appconfig.Config{
		DockerHost:       "tcp://config.example:2375",
		DockerCertPath:   "/config/certs",
		DockerAPIVersion: "1.48",
	}, FlagValues{
		HostChanged:       true,
		CertPathChanged:   true,
		APIVersionChanged: true,
	})
	if opts.Host != "" || !opts.HostSet || opts.CertPath != "" || !opts.CertPathSet ||
		opts.APIVersion != "" || !opts.APIVersionSet {
		t.Fatalf("options = %#v, want explicit empty flag overrides", opts)
	}
}
