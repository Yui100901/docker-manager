package diagnostics

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"docker-manager/internal/docker"

	mobyclient "github.com/moby/moby/client"
)

func TestDockerEndpointCheckUsesConnectionInfo(t *testing.T) {
	configuredTLS := true
	docker.Configure(docker.Options{
		Host:       "tcp://configured.invalid:2376",
		TLSVerify:  &configuredTLS,
		CertPath:   "/configured/certs",
		APIVersion: "9.99",
	})
	t.Cleanup(func() { docker.Configure(docker.Options{}) })
	t.Setenv("DOCKER_HOST", "tcp://environment.invalid:2376")
	t.Setenv("DOCKER_TLS_VERIFY", "1")
	t.Setenv("DOCKER_CERT_PATH", "/environment/certs")
	t.Setenv("DOCKER_API_VERSION", "8.88")

	tests := []struct {
		name       string
		info       docker.ConnectionInfo
		wantStatus string
		wantDetail []string
	}{
		{
			name: "plain HTTP is a warning",
			info: docker.ConnectionInfo{
				Host:       "tcp://actual.example:2375",
				Transport:  "http",
				APIVersion: "1.47",
			},
			wantStatus: "warning",
			wantDetail: []string{
				"host=tcp://actual.example:2375",
				"transport=http",
				"tls=false",
				"tls_verify=false",
				"client_api_version=1.47",
			},
		},
		{
			name: "HTTPS without verification is a warning",
			info: docker.ConnectionInfo{
				Host:              "tcp://actual.example:2376",
				Transport:         "https",
				TLS:               true,
				TLSVerify:         false,
				CertPath:          "/actual/certs",
				CASource:          "docker-cert-path",
				ClientCertificate: true,
				APIVersion:        "1.48",
			},
			wantStatus: "warning",
			wantDetail: []string{
				"host=tcp://actual.example:2376",
				"transport=https",
				"tls=true",
				"tls_verify=false",
				"cert_path=/actual/certs",
				"ca_source=docker-cert-path",
				"client_certificate=true",
				"client_api_version=1.48",
			},
		},
		{
			name: "verified HTTPS is ok",
			info: docker.ConnectionInfo{
				Host:              "tcp://secure.example:2376",
				Transport:         "https",
				TLS:               true,
				TLSVerify:         true,
				CertPath:          "/secure/certs",
				CASource:          "docker-cert-path",
				ClientCertificate: true,
				APIVersion:        "1.49",
			},
			wantStatus: "ok",
			wantDetail: []string{
				"host=tcp://secure.example:2376",
				"transport=https",
				"tls=true",
				"tls_verify=true",
				"cert_path=/secure/certs",
				"ca_source=docker-cert-path",
				"client_certificate=true",
				"client_api_version=1.49",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check := dockerEndpointCheck(tt.info, tt.info.APIVersion, nil)
			if check.Name != "docker-endpoint" || check.Status != tt.wantStatus {
				t.Fatalf("dockerEndpointCheck() = %#v, want docker-endpoint status %q", check, tt.wantStatus)
			}
			for _, value := range tt.wantDetail {
				if !strings.Contains(check.Detail, value) {
					t.Errorf("dockerEndpointCheck() detail = %q, want %q", check.Detail, value)
				}
			}
			for _, stale := range []string{
				"configured.invalid",
				"environment.invalid",
				"/configured/certs",
				"/environment/certs",
				"9.99",
				"8.88",
			} {
				if strings.Contains(check.Detail, stale) {
					t.Errorf("dockerEndpointCheck() detail = %q, contains stale configured value %q", check.Detail, stale)
				}
			}
		})
	}
}

func TestCheckDoctorDockerReportsEndpointInitializationFailure(t *testing.T) {
	old := newDoctorDockerService
	initErr := errors.New("docker TLS configuration is invalid")
	info := docker.ConnectionInfo{
		Host:       "tcp://secure.example:2376",
		Transport:  "https",
		TLS:        true,
		TLSVerify:  true,
		CertPath:   "/missing/certs",
		CASource:   "docker-cert-path",
		APIVersion: "1.48",
	}
	newDoctorDockerService = func() (doctorDockerService, docker.ConnectionInfo, error) {
		return nil, info, initErr
	}
	t.Cleanup(func() { newDoctorDockerService = old })

	checks := checkDoctorDocker(context.Background(), time.Second)
	endpoint, ok := doctorCheckByName(checks, "docker-endpoint")
	if !ok {
		t.Fatalf("checkDoctorDocker() checks = %#v, want docker-endpoint", checks)
	}
	if endpoint.Status != "failed" {
		t.Fatalf("docker endpoint check = %#v, want failed", endpoint)
	}
	if !strings.Contains(endpoint.Message, initErr.Error()) {
		t.Errorf("docker endpoint message = %q, want initialization error %q", endpoint.Message, initErr)
	}
	for _, value := range []string{
		"host=tcp://secure.example:2376",
		"transport=https",
		"tls=true",
		"tls_verify=true",
		"cert_path=/missing/certs",
	} {
		if !strings.Contains(endpoint.Detail, value) {
			t.Errorf("docker endpoint detail = %q, want %q", endpoint.Detail, value)
		}
	}

	daemon, ok := doctorCheckByName(checks, "docker-daemon")
	if !ok || daemon.Status != "failed" || !strings.Contains(daemon.Message, initErr.Error()) {
		t.Fatalf("docker daemon check = %#v, want failed initialization check", daemon)
	}
}

func TestCheckDoctorDockerReportsNegotiatedClientVersion(t *testing.T) {
	old := newDoctorDockerService
	svc := &negotiatingDoctorDockerService{}
	newDoctorDockerService = func() (doctorDockerService, docker.ConnectionInfo, error) {
		return svc, docker.ConnectionInfo{
			Host:      "tcp://docker.example:2375",
			Transport: "http",
		}, nil
	}
	t.Cleanup(func() { newDoctorDockerService = old })

	checks := checkDoctorDocker(context.Background(), time.Second)
	endpoint, ok := doctorCheckByName(checks, "docker-endpoint")
	if !ok {
		t.Fatalf("checkDoctorDocker() checks = %#v, want docker-endpoint", checks)
	}
	if !strings.Contains(endpoint.Detail, "client_api_version=1.40") {
		t.Fatalf("docker endpoint detail = %q, want negotiated API version", endpoint.Detail)
	}
	if strings.Contains(endpoint.Detail, "client_api_version=9.99") {
		t.Fatalf("docker endpoint detail = %q, contains pre-Ping API version", endpoint.Detail)
	}
}

func TestDoctorDockerServiceNegotiatesAgainstDockerAPI(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_TLS_VERIFY", "")
	t.Setenv("DOCKER_CERT_PATH", "")
	t.Setenv("DOCKER_API_VERSION", "")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_ping":
			w.Header().Set("API-Version", "1.40")
			w.Header().Set("OSType", "linux")
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = io.WriteString(w, "OK")
			}
		case strings.HasSuffix(r.URL.Path, "/version"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"Version":"19.03","ApiVersion":"1.40","MinAPIVersion":"1.24","Os":"linux","Arch":"amd64"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	verify := false
	docker.Configure(docker.Options{Host: "tcp://" + serverURL.Host, TLSVerify: &verify})
	t.Cleanup(func() { docker.Configure(docker.Options{}) })

	checks := checkDoctorDocker(context.Background(), 2*time.Second)
	endpoint, ok := doctorCheckByName(checks, "docker-endpoint")
	if !ok {
		t.Fatalf("checkDoctorDocker() checks = %#v, want docker-endpoint", checks)
	}
	if !strings.Contains(endpoint.Detail, "client_api_version=1.40") {
		t.Fatalf("docker endpoint detail = %q, want API version negotiated from real Moby ping", endpoint.Detail)
	}
	if !hasDoctorCheck(checks, "docker-daemon", "ok") || !hasDoctorCheck(checks, "docker-version", "ok") {
		t.Fatalf("checkDoctorDocker() checks = %#v, want daemon and version ok", checks)
	}
}

type negotiatingDoctorDockerService struct {
	pinged bool
}

func (s *negotiatingDoctorDockerService) Ping(context.Context) (mobyclient.PingResult, error) {
	s.pinged = true
	return mobyclient.PingResult{APIVersion: "1.40", OSType: "linux"}, nil
}

func (s *negotiatingDoctorDockerService) ServerVersion(context.Context) (mobyclient.ServerVersionResult, error) {
	return mobyclient.ServerVersionResult{Version: "19.03", APIVersion: "1.40", Os: "linux", Arch: "amd64"}, nil
}

func (s *negotiatingDoctorDockerService) DaemonHost() string {
	return "tcp://docker.example:2375"
}

func (s *negotiatingDoctorDockerService) ClientVersion() string {
	if s.pinged {
		return "1.40"
	}
	return "9.99"
}

func doctorCheckByName(checks []DoctorCheck, name string) (DoctorCheck, bool) {
	for _, check := range checks {
		if check.Name == name {
			return check, true
		}
	}
	return DoctorCheck{}, false
}
