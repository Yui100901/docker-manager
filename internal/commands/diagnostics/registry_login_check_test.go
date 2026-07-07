package diagnostics

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/registry"
)

type fakeRegistryLoginDockerService struct {
	auth registry.AuthConfig
	err  error
}

func (f *fakeRegistryLoginDockerService) RegistryLogin(ctx context.Context, auth registry.AuthConfig) (registry.AuthResponse, error) {
	f.auth = auth
	if f.err != nil {
		return registry.AuthResponse{}, f.err
	}
	return registry.AuthResponse{Status: "Login Succeeded"}, nil
}

func TestCredentialFromAuthEntryDecodesAuth(t *testing.T) {
	auth := base64.StdEncoding.EncodeToString([]byte("demo:secret"))
	cred := credentialFromAuthEntry(dockerAuthEntry{Auth: auth})
	if cred.Username != "demo" || cred.Password != "secret" {
		t.Fatalf("credential = %#v, want decoded username/password", cred)
	}
}

func TestResolveRegistryCredentialUsesAuths(t *testing.T) {
	cfg := dockerConfigFile{
		Auths: map[string]dockerAuthEntry{
			"registry.local:5000": {
				Username: "demo",
				Password: "secret",
			},
		},
	}
	cred := resolveRegistryCredential(context.Background(), cfg, "registry.local:5000")
	if !cred.Found || cred.Source != "auths" || cred.Username != "demo" {
		t.Fatalf("credential = %#v, want auths credential", cred)
	}
}

func TestResolveRegistryCredentialUsesCredentialHelper(t *testing.T) {
	previous := runDockerCredentialHelper
	runDockerCredentialHelper = func(ctx context.Context, helper, server string) (registryCredential, error) {
		if helper != "pass" || server != "registry.local:5000" {
			t.Fatalf("helper=%q server=%q", helper, server)
		}
		return registryCredential{Username: "helper-user", Password: "secret"}, nil
	}
	t.Cleanup(func() { runDockerCredentialHelper = previous })

	cfg := dockerConfigFile{CredHelpers: map[string]string{"registry.local:5000": "pass"}}
	cred := resolveRegistryCredential(context.Background(), cfg, "registry.local:5000")
	if !cred.Found || cred.Source != "credential-helper" || cred.Helper != "pass" || cred.Username != "helper-user" {
		t.Fatalf("credential = %#v, want helper credential", cred)
	}
}

func TestPingRegistryV2ReportsAuthRequirement(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/" {
			t.Fatalf("path = %q, want /v2/", r.URL.Path)
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	registryName := strings.TrimPrefix(server.URL, "http://")
	got := pingRegistryV2(context.Background(), registryName, true, registryCredential{})
	if got.Status != "warning" || got.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("ping = %#v, want auth warning", got)
	}
}

func TestRunRegistryLoginCheckWithInlineCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "demo" || password != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	fakeDocker := &fakeRegistryLoginDockerService{}
	restoreDocker := replaceRegistryLoginServiceFactory(fakeDocker)
	defer restoreDocker()
	restoreHTTP := replaceRegistryCheckHTTPClient(server.Client())
	defer restoreHTTP()

	registryName := strings.TrimPrefix(server.URL, "http://")
	configPath := writeDockerConfig(t, fmt.Sprintf(`{
		"auths": {
			"%s": {"username": "demo", "password": "secret"}
		}
	}`, registryName))

	report, err := runRegistryLoginCheck(context.Background(), registryName, RegistryLoginCheckOptions{
		DockerConfig: configPath,
		PlainHTTP:    true,
	})
	if err != nil {
		t.Fatalf("runRegistryLoginCheck() error = %v", err)
	}
	if !report.Credential.Found || report.Credential.Source != "auths" {
		t.Fatalf("credential = %#v, want auths credential", report.Credential)
	}
	if report.RegistryPing.Status != "ok" {
		t.Fatalf("RegistryPing = %#v, want ok", report.RegistryPing)
	}
	if report.DockerLogin.Status != "ok" {
		t.Fatalf("DockerLogin = %#v, want ok", report.DockerLogin)
	}
	if fakeDocker.auth.Username != "demo" || fakeDocker.auth.ServerAddress != registryName {
		t.Fatalf("RegistryLogin auth = %#v", fakeDocker.auth)
	}
}

func TestRegistryReportCommandShape(t *testing.T) {
	cmd := NewRegistryReportCommand()
	if cmd.Name() != "registry" {
		t.Fatalf("Name() = %q, want registry", cmd.Name())
	}
	if flag := cmd.Flags().Lookup("plain-http"); flag == nil {
		t.Fatal("missing --plain-http flag")
	}
	if flag := cmd.Flags().Lookup("fail-on-error"); flag == nil {
		t.Fatal("missing --fail-on-error flag")
	}
	if flag := cmd.Flags().Lookup("fail-on-warning"); flag == nil {
		t.Fatal("missing --fail-on-warning flag")
	}
}

func TestRegistryLoginCheckExitErrorDefaultsToFailedOnly(t *testing.T) {
	report := RegistryLoginCheckReport{
		Registry:     "registry.local:5000",
		RegistryPing: CheckResult{Status: "warning"},
		DockerLogin:  CheckResult{Status: "skipped"},
	}
	if err := registryLoginCheckExitError(report, RegistryLoginCheckOptions{FailOnError: true}); err != nil {
		t.Fatalf("registryLoginCheckExitError() warning default error = %v, want nil", err)
	}
	report.DockerLogin = CheckResult{Status: "failed", Message: "bad credentials"}
	if err := registryLoginCheckExitError(report, RegistryLoginCheckOptions{FailOnError: true}); err == nil {
		t.Fatal("registryLoginCheckExitError() error = nil, want failed status error")
	}
}

func TestRegistryLoginCheckExitErrorCanFailOnWarning(t *testing.T) {
	report := RegistryLoginCheckReport{
		Registry:     "registry.local:5000",
		RegistryPing: CheckResult{Status: "warning"},
		DockerLogin:  CheckResult{Status: "skipped"},
	}
	if err := registryLoginCheckExitError(report, RegistryLoginCheckOptions{FailOnError: true, FailOnWarning: true}); err == nil {
		t.Fatal("registryLoginCheckExitError() error = nil, want warning status error")
	}
}

func TestRegistryLoginRecommendationsIncludeArtifactoryRouterHint(t *testing.T) {
	report := RegistryLoginCheckReport{
		Registry:     "artifactory.local:8082",
		ConfigFound:  true,
		Credential:   CredentialReport{Found: true},
		RegistryPing: CheckResult{Status: "ok", HTTPStatus: http.StatusOK},
		DockerLogin:  CheckResult{Status: "ok"},
	}
	tips := registryLoginRecommendations(report)
	if !containsSubstring(tips, "Artifactory/JCR Router 8082") {
		t.Fatalf("recommendations = %#v, want Artifactory Router hint", tips)
	}
}

func TestRegistryLoginRecommendationsSkipArtifactoryRouterHintForGenericRegistry(t *testing.T) {
	report := RegistryLoginCheckReport{
		Registry:     "registry.local:5000",
		ConfigFound:  true,
		Credential:   CredentialReport{Found: true},
		RegistryPing: CheckResult{Status: "ok", HTTPStatus: http.StatusOK},
		DockerLogin:  CheckResult{Status: "ok"},
	}
	tips := registryLoginRecommendations(report)
	if containsSubstring(tips, "Artifactory/JCR Router 8082") {
		t.Fatalf("recommendations = %#v, do not want Artifactory Router hint", tips)
	}
}

func TestRegistryLoginRecommendationsSkipArtifactoryRouterHintForTomcatPort(t *testing.T) {
	report := RegistryLoginCheckReport{
		Registry:     "artifactory.local:8081",
		ConfigFound:  true,
		Credential:   CredentialReport{Found: true},
		RegistryPing: CheckResult{Status: "ok", HTTPStatus: http.StatusOK},
		DockerLogin:  CheckResult{Status: "ok"},
	}
	tips := registryLoginRecommendations(report)
	if containsSubstring(tips, "Artifactory/JCR Router 8082") {
		t.Fatalf("recommendations = %#v, do not want Artifactory Router hint for 8081", tips)
	}
}

func TestPrintRegistryLoginCheckReportIncludesSections(t *testing.T) {
	var out bytes.Buffer
	printRegistryLoginCheckReport(&out, RegistryLoginCheckReport{
		Registry:     "registry.local:5000",
		DockerConfig: "/tmp/config.json",
		ConfigFound:  true,
		Credential:   CredentialReport{Found: true, Source: "auths", Username: "demo"},
		RegistryPing: CheckResult{Status: "ok", HTTPStatus: 200, Message: "reachable"},
		DockerLogin:  CheckResult{Status: "ok", Message: "Login Succeeded"},
	})
	got := out.String()
	for _, want := range []string{"Docker registry 登录检查", "registry.local:5000", "凭据:", "Registry 连通性:", "Docker 登录:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output = %q, want %q", got, want)
		}
	}
}

func writeDockerConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func containsSubstring(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}

func replaceRegistryLoginServiceFactory(fake *fakeRegistryLoginDockerService) func() {
	previous := newRegistryLoginDockerService
	newRegistryLoginDockerService = func() (registryLoginDockerService, error) {
		return fake, nil
	}
	return func() {
		newRegistryLoginDockerService = previous
	}
}

func replaceRegistryCheckHTTPClient(client httpDoer) func() {
	previous := registryCheckHTTPClient
	registryCheckHTTPClient = client
	return func() {
		registryCheckHTTPClient = previous
	}
}
