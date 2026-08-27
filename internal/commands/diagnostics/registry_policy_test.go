package diagnostics

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"docker-manager/internal/appconfig"
	"docker-manager/internal/registryca"
)

func TestApplyRegistryPolicyAndExplicitOverrides(t *testing.T) {
	policy := appconfig.RegistryPolicy{
		CAFile:    "policy-ca.pem",
		CAPath:    "policy-cas",
		Proxy:     "http://policy-proxy.example:8080",
		NoProxy:   true,
		Timeout:   "250ms",
		PlainHTTP: true,
	}
	resolver := func(registry string) (appconfig.RegistryPolicy, bool) {
		if registry != "registry.example:5000" {
			t.Fatalf("resolver registry = %q", registry)
		}
		return policy, true
	}

	resolved, allowed, err := applyRegistryPolicy("registry.example:5000", RegistryLoginCheckOptions{
		Timeout:               5 * time.Second,
		ResolveRegistryPolicy: resolver,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !allowed || !resolved.PlainHTTP || !resolved.NoProxy || resolved.Proxy != policy.Proxy ||
		resolved.RegistryCAFile != policy.CAFile || resolved.RegistryCAPath != policy.CAPath ||
		resolved.Timeout != 250*time.Millisecond {
		t.Fatalf("resolved policy = %#v allowed=%v", resolved, allowed)
	}

	explicit := RegistryLoginCheckOptions{
		PlainHTTP:              false,
		Proxy:                  "http://flag-proxy.example:8080",
		NoProxy:                false,
		RegistryCAFile:         "flag-ca.pem",
		RegistryCAPath:         "flag-cas",
		Timeout:                9 * time.Second,
		ResolveRegistryPolicy:  resolver,
		plainHTTPExplicit:      true,
		proxyExplicit:          true,
		noProxyExplicit:        true,
		registryCAFileExplicit: true,
		registryCAPathExplicit: true,
		timeoutExplicit:        true,
	}
	resolved, _, err = applyRegistryPolicy("registry.example:5000", explicit)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.PlainHTTP || resolved.NoProxy || resolved.Proxy != explicit.Proxy ||
		resolved.RegistryCAFile != explicit.RegistryCAFile || resolved.RegistryCAPath != explicit.RegistryCAPath ||
		resolved.Timeout != explicit.Timeout {
		t.Fatalf("explicit options were overridden: %#v", resolved)
	}
}

func TestApplyRegistryPolicyCredentialScope(t *testing.T) {
	for _, tt := range []struct {
		name  string
		scope []string
		want  bool
	}{
		{name: "nil allows all", want: true},
		{name: "login allowed", scope: []string{"login"}, want: true},
		{name: "explicit empty denies all", scope: []string{}, want: false},
		{name: "other operation denied", scope: []string{"pull"}, want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, allowed, err := applyRegistryPolicy("registry.example", RegistryLoginCheckOptions{
				ResolveRegistryPolicy: func(string) (appconfig.RegistryPolicy, bool) {
					return appconfig.RegistryPolicy{CredentialScope: tt.scope}, true
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if allowed != tt.want {
				t.Fatalf("credential allowed = %v, want %v", allowed, tt.want)
			}
		})
	}
}

func TestRegistryPolicyCAFileAndPathTrustTLSRegistry(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	registryName := strings.TrimPrefix(server.URL, "https://")
	caPEM := testServerCertificatePEM(t, server)

	for _, tt := range []struct {
		name   string
		policy func(string) appconfig.RegistryPolicy
	}{
		{
			name: "CA file",
			policy: func(dir string) appconfig.RegistryPolicy {
				path := filepath.Join(dir, "registry-ca.pem")
				if err := os.WriteFile(path, caPEM, 0o600); err != nil {
					t.Fatal(err)
				}
				return appconfig.RegistryPolicy{CAFile: path, NoProxy: true}
			},
		},
		{
			name: "CA path",
			policy: func(dir string) appconfig.RegistryPolicy {
				caDir := filepath.Join(dir, "registry-cas")
				if err := os.Mkdir(caDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(caDir, "ca.pem"), caPEM, 0o600); err != nil {
					t.Fatal(err)
				}
				return appconfig.RegistryPolicy{CAPath: caDir, NoProxy: true}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			policy := tt.policy(t.TempDir())
			report, err := runRegistryLoginCheck(context.Background(), registryName, RegistryLoginCheckOptions{
				DockerConfig: filepath.Join(t.TempDir(), "missing-config.json"),
				Timeout:      time.Second,
				ResolveRegistryPolicy: func(got string) (appconfig.RegistryPolicy, bool) {
					return policy, got == registryName
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if report.RegistryPing.Status != "ok" {
				t.Fatalf("registry ping = %#v", report.RegistryPing)
			}
		})
	}
}

func TestRegistryPolicyCAConfigurationFailsClosedAtBoundaries(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	certificate := testServerCertificatePEM(t, server)

	t.Run("mixed directory content", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "ca.pem"), certificate, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "README.txt"), []byte("not a certificate"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := registryRootCAs("", dir)
		if err == nil || !strings.Contains(err.Error(), "valid PEM certificate") {
			t.Fatalf("registryRootCAs() error = %v, want mixed-content rejection", err)
		}
	})

	t.Run("mixed file content", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ca.pem")
		data := append(append([]byte(nil), certificate...), []byte("trailing garbage")...)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := registryRootCAs(path, "")
		if err == nil || !strings.Contains(err.Error(), "valid PEM certificate") {
			t.Fatalf("registryRootCAs() error = %v, want mixed-content rejection", err)
		}
	})

	t.Run("empty directory", func(t *testing.T) {
		_, err := registryRootCAs("", t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "no certificate files") {
			t.Fatalf("registryRootCAs() error = %v, want empty-directory rejection", err)
		}
	})

	t.Run("entry count", func(t *testing.T) {
		dir := t.TempDir()
		for index := range registryca.MaxPathEntries + 1 {
			name := filepath.Join(dir, fmt.Sprintf("ca-%03d.pem", index))
			if err := os.WriteFile(name, certificate, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		_, err := registryRootCAs("", dir)
		if err == nil || !strings.Contains(err.Error(), "exceeds 256 certificate files") {
			t.Fatalf("registryRootCAs() error = %v, want entry-limit rejection", err)
		}
	})

	t.Run("single file bytes", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ca.pem")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, registryca.MaxFileBytes+1); err != nil {
			t.Fatal(err)
		}
		_, err := registryRootCAs(path, "")
		if err == nil || !strings.Contains(err.Error(), "16777216 bytes") {
			t.Fatalf("registryRootCAs() error = %v, want file-size rejection", err)
		}
	})

	t.Run("cumulative bytes", func(t *testing.T) {
		dir := t.TempDir()
		fileSize := registryca.MaxPathBytes/3 + 1
		for index := range 3 {
			path := filepath.Join(dir, fmt.Sprintf("ca-%d.pem", index))
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(path, fileSize); err != nil {
				t.Fatal(err)
			}
		}
		_, err := registryRootCAs("", dir)
		if err == nil || !strings.Contains(err.Error(), "total bytes") {
			t.Fatalf("registryRootCAs() error = %v, want cumulative-size rejection", err)
		}
	})
}

func TestRegistryPolicyProxyAndNoProxyAreIsolated(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		if !r.URL.IsAbs() {
			t.Errorf("proxy request URL = %q, want absolute URL", r.URL.String())
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()
	registryName := strings.TrimPrefix(target.URL, "http://")

	policy := appconfig.RegistryPolicy{PlainHTTP: true, Proxy: proxy.URL}
	report, err := runRegistryLoginCheck(context.Background(), registryName, RegistryLoginCheckOptions{
		DockerConfig: filepath.Join(t.TempDir(), "missing-config.json"),
		Timeout:      time.Second,
		ResolveRegistryPolicy: func(string) (appconfig.RegistryPolicy, bool) {
			return policy, true
		},
	})
	if err != nil || report.RegistryPing.Status != "ok" {
		t.Fatalf("proxied check = %#v, %v", report.RegistryPing, err)
	}
	if proxyHits.Load() != 1 || targetHits.Load() != 0 {
		t.Fatalf("proxied hits proxy=%d target=%d", proxyHits.Load(), targetHits.Load())
	}

	proxyHits.Store(0)
	targetHits.Store(0)
	policy.NoProxy = true
	report, err = runRegistryLoginCheck(context.Background(), registryName, RegistryLoginCheckOptions{
		DockerConfig: filepath.Join(t.TempDir(), "missing-config.json"),
		Timeout:      time.Second,
		ResolveRegistryPolicy: func(string) (appconfig.RegistryPolicy, bool) {
			return policy, true
		},
	})
	if err != nil || report.RegistryPing.Status != "ok" {
		t.Fatalf("direct check = %#v, %v", report.RegistryPing, err)
	}
	if proxyHits.Load() != 0 || targetHits.Load() != 1 {
		t.Fatalf("direct hits proxy=%d target=%d", proxyHits.Load(), targetHits.Load())
	}
}

func TestRegistryPolicyCredentialScopeBlocksHTTPAndDaemonCredentials(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	registryName := strings.TrimPrefix(server.URL, "http://")
	configPath := writeDockerConfig(t, fmt.Sprintf(`{"credHelpers":{%q:"pass"},"auths":{%q:{"username":"admin","password":"policy-secret"}}}`, registryName, registryName))
	previousHelper := runDockerCredentialHelper
	helperCalled := false
	runDockerCredentialHelper = func(context.Context, string, string) (registryCredential, error) {
		helperCalled = true
		return registryCredential{Found: true, Username: "helper-user", Password: "helper-secret"}, nil
	}
	t.Cleanup(func() { runDockerCredentialHelper = previousHelper })
	fakeDocker := &fakeRegistryLoginDockerService{}
	restoreDocker := replaceRegistryLoginServiceFactory(fakeDocker)
	defer restoreDocker()

	report, err := runRegistryLoginCheck(context.Background(), registryName, RegistryLoginCheckOptions{
		DockerConfig: configPath,
		Timeout:      time.Second,
		ResolveRegistryPolicy: func(string) (appconfig.RegistryPolicy, bool) {
			return appconfig.RegistryPolicy{PlainHTTP: true, NoProxy: true, CredentialScope: []string{}}, true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		t.Fatalf("direct registry received Authorization %q", authorization)
	}
	if helperCalled {
		t.Fatal("credential helper ran while registry policy denied login")
	}
	if report.Credential.Found || report.Credential.Source != "registry-policy" || !strings.Contains(report.Credential.Message, "未读取或发送") {
		t.Fatalf("credential report = %#v", report.Credential)
	}
	if report.RegistryPing.Status != "warning" || report.DockerLogin.Status != "skipped" || !strings.Contains(report.DockerLogin.Message, "未向 Docker daemon") {
		t.Fatalf("report = %#v", report)
	}
	if fakeDocker.auth.Username != "" || fakeDocker.auth.Password != "" {
		t.Fatalf("daemon received auth = %#v", fakeDocker.auth)
	}
	if containsSubstring(report.Recommendations, "docker login") {
		t.Fatalf("recommendations = %#v, must not recommend login when policy forbids it", report.Recommendations)
	}
}

func TestRegistryPolicyTimeoutAndExplicitTimeoutOverride(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(60 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	registryName := strings.TrimPrefix(server.URL, "http://")
	resolver := func(string) (appconfig.RegistryPolicy, bool) {
		return appconfig.RegistryPolicy{PlainHTTP: true, NoProxy: true, Timeout: "10ms"}, true
	}

	report, err := runRegistryLoginCheck(context.Background(), registryName, RegistryLoginCheckOptions{
		DockerConfig:          filepath.Join(t.TempDir(), "missing-config.json"),
		Timeout:               time.Second,
		ResolveRegistryPolicy: resolver,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.RegistryPing.Status != "failed" {
		t.Fatalf("policy timeout ping = %#v, want failed", report.RegistryPing)
	}

	report, err = runRegistryLoginCheck(context.Background(), registryName, RegistryLoginCheckOptions{
		DockerConfig:          filepath.Join(t.TempDir(), "missing-config.json"),
		Timeout:               250 * time.Millisecond,
		ResolveRegistryPolicy: resolver,
		timeoutExplicit:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.RegistryPing.Status != "ok" {
		t.Fatalf("explicit timeout ping = %#v, want ok", report.RegistryPing)
	}
}

func TestRegistryCommandExplicitFalseOverridesPlainHTTPPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	registryName := strings.TrimPrefix(server.URL, "http://")

	cmd := NewRegistryReportCommandWithDefaults(func() RegistryLoginCheckDefaults {
		return RegistryLoginCheckDefaults{ResolveRegistryPolicy: func(string) (appconfig.RegistryPolicy, bool) {
			return appconfig.RegistryPolicy{PlainHTTP: true, NoProxy: true}, true
		}}
	})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--plain-http=false",
		"--fail-on-error=false",
		"--docker-config", filepath.Join(t.TempDir(), "missing-config.json"),
		"--format", "json",
		registryName,
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var report RegistryLoginCheckReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v output=%q", err, output.String())
	}
	if report.RegistryPing.Status != "failed" {
		t.Fatalf("registry ping = %#v, want HTTPS failure against HTTP-only server", report.RegistryPing)
	}
}

func TestDoctorRegistryUsesResolvedPolicy(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	registryName := strings.TrimPrefix(server.URL, "https://")
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, testServerCertificatePEM(t, server), 0o600); err != nil {
		t.Fatal(err)
	}

	checks := checkDoctorRegistry(context.Background(), registryName, DoctorOptions{
		DockerConfig: filepath.Join(t.TempDir(), "missing-config.json"),
		Timeout:      time.Second,
		ResolveRegistryPolicy: func(string) (appconfig.RegistryPolicy, bool) {
			return appconfig.RegistryPolicy{CAFile: caFile, NoProxy: true}, true
		},
	})
	if !hasDoctorCheck(checks, "registry:"+registryName, "ok") {
		t.Fatalf("doctor registry checks = %#v", checks)
	}
}

func TestRegistryAndDoctorExposeRegistryPolicyOverrideFlags(t *testing.T) {
	for _, cmd := range []struct {
		name  string
		flags func(string) bool
	}{
		{name: "registry", flags: func(name string) bool { return NewRegistryReportCommand().Flags().Lookup(name) != nil }},
		{name: "doctor", flags: func(name string) bool { return NewDoctorCommand().Flags().Lookup(name) != nil }},
	} {
		for _, name := range []string{"proxy", "no-proxy", "registry-ca-file", "registry-ca-path", "plain-http", "timeout"} {
			if !cmd.flags(name) {
				t.Errorf("%s missing --%s", cmd.name, name)
			}
		}
	}
}

func testServerCertificatePEM(t *testing.T, server *httptest.Server) []byte {
	t.Helper()
	certificate := server.Certificate()
	if certificate == nil || len(certificate.Raw) == 0 {
		t.Fatal("TLS test server certificate is unavailable")
	}
	if _, err := x509.ParseCertificate(certificate.Raw); err != nil {
		t.Fatalf("parse TLS test certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
}
