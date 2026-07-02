package docker

import "testing"

func TestEffectiveOptionsReadsDockerEnv(t *testing.T) {
	t.Cleanup(func() { Configure(Options{}) })
	Configure(Options{})
	t.Setenv("DOCKER_HOST", "tcp://env.example:2376")
	t.Setenv("DOCKER_TLS_VERIFY", "1")
	t.Setenv("DOCKER_CERT_PATH", "/env/certs")
	t.Setenv("DOCKER_API_VERSION", "1.44")

	got := EffectiveOptions()

	if got.Host != "tcp://env.example:2376" || got.CertPath != "/env/certs" || got.APIVersion != "1.44" {
		t.Fatalf("EffectiveOptions() = %#v, want env values", got)
	}
	if got.TLSVerify == nil || !*got.TLSVerify {
		t.Fatalf("TLSVerify = %#v, want true from env", got.TLSVerify)
	}
}

func TestEffectiveOptionsExplicitOverridesEnv(t *testing.T) {
	t.Cleanup(func() { Configure(Options{}) })
	t.Setenv("DOCKER_HOST", "tcp://env.example:2376")
	t.Setenv("DOCKER_TLS_VERIFY", "1")
	t.Setenv("DOCKER_CERT_PATH", "/env/certs")
	t.Setenv("DOCKER_API_VERSION", "1.44")
	tlsVerify := false
	Configure(Options{
		Host:       "tcp://configured.example:2376",
		TLSVerify:  &tlsVerify,
		CertPath:   "/configured/certs",
		APIVersion: "1.46",
	})

	got := EffectiveOptions()

	if got.Host != "tcp://configured.example:2376" || got.CertPath != "/configured/certs" || got.APIVersion != "1.46" {
		t.Fatalf("EffectiveOptions() = %#v, want explicit values", got)
	}
	if got.TLSVerify == nil || *got.TLSVerify {
		t.Fatalf("TLSVerify = %#v, want false from explicit config", got.TLSVerify)
	}
}
