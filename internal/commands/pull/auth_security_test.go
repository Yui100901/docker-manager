package pull

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"docker-manager/internal/sensitive"
	"github.com/Yui100901/MyGo/network/http_utils"
)

func TestResolveRegistryAuthRejectsCrossOriginRealmBeforeSendingCredential(t *testing.T) {
	var tokenRequests atomic.Int32
	evil := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenRequests.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("evil realm received credential: %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"token":"evil"}`))
	}))
	defer evil.Close()

	runner := newTestPullRunner()
	runner.httpClient = &http_utils.HTTPClient{Client: evil.Client()}
	configPath := writePullDockerConfig(t, "registry.example", "admin", "realm-secret")
	_, err := runner.resolveRegistryAuth(
		context.Background(),
		`Bearer realm="`+evil.URL+`/token"`,
		&ImageInfo{Registry: "registry.example", Repository: "team", Image: "app", Tag: "latest"},
		PullOptions{DockerConfig: configPath},
	)
	if err == nil || !strings.Contains(err.Error(), "不在 --auth-realm allowlist") {
		t.Fatalf("resolveRegistryAuth() error = %v, want realm policy rejection", err)
	}
	if tokenRequests.Load() != 0 {
		t.Fatalf("evil realm requests = %d, want 0", tokenRequests.Load())
	}
}

func TestResolveRegistryAuthAllowsExplicitHTTPSRealmAndSendsCredential(t *testing.T) {
	wantAuthorization := basicAuthHeader("admin", "realm-secret")
	var gotAuthorization string
	authServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"token":"allowed-token"}`))
	}))
	defer authServer.Close()

	runner := newTestPullRunner()
	runner.httpClient = &http_utils.HTTPClient{Client: authServer.Client()}
	configPath := writePullDockerConfig(t, "registry.example", "admin", "realm-secret")
	auth, err := runner.resolveRegistryAuth(
		context.Background(),
		`Bearer realm="`+authServer.URL+`/token"`,
		&ImageInfo{Registry: "registry.example", Repository: "team", Image: "app", Tag: "latest"},
		PullOptions{DockerConfig: configPath, AuthRealmAllowlist: []string{authServer.URL}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if auth == nil || auth.Authorization != "Bearer allowed-token" {
		t.Fatalf("auth = %#v", auth)
	}
	if gotAuthorization != wantAuthorization {
		t.Fatalf("realm Authorization = %q, want configured Basic credential", gotAuthorization)
	}
}

func TestBearerRealmRequiresHTTPSEvenWhenHostIsAllowlisted(t *testing.T) {
	info := &ImageInfo{Registry: "registry.example", Repository: "team", Image: "app", Tag: "latest"}
	_, err := validateBearerRealm("http://registry.example/token", info, PullOptions{AuthRealmAllowlist: []string{"registry.example"}})
	if err == nil || !strings.Contains(err.Error(), "必须使用 HTTPS") {
		t.Fatalf("validateBearerRealm() error = %v", err)
	}
}

func TestBearerRealmRejectsEmptyHostname(t *testing.T) {
	info := &ImageInfo{Registry: "registry.example", Repository: "team", Image: "app", Tag: "latest"}
	_, err := validateBearerRealm("https://:443/token", info, PullOptions{AuthRealmAllowlist: []string{"https://:443"}})
	if err == nil || !strings.Contains(err.Error(), "绝对 HTTPS URL") {
		t.Fatalf("validateBearerRealm() error = %v, want empty-hostname rejection", err)
	}
}

func TestDockerHubBuiltInRealmIsAllowed(t *testing.T) {
	info := &ImageInfo{Registry: defaultRegistry, Repository: "library", Image: "busybox", Tag: "latest"}
	got, err := validateBearerRealm("https://auth.docker.io/token", info, PullOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Hostname() != "auth.docker.io" {
		t.Fatalf("realm = %s", got)
	}
}

func TestAuthRealmAllowlistRejectsNonOriginValues(t *testing.T) {
	for _, value := range []string{"", "http://auth.example", "https://user:pass@auth.example", "https://auth.example/token", "*.example.com", "https://:443", ":443"} {
		if err := validateAuthRealmAllowlist([]string{value}); err == nil {
			t.Fatalf("allowlist value %q accepted", value)
		}
	}
}

func TestSameOriginRedirectClientDoesNotForwardCredential(t *testing.T) {
	var evilRequests atomic.Int32
	evil := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		evilRequests.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("redirect target received Authorization %q", r.Header.Get("Authorization"))
		}
	}))
	defer evil.Close()
	authServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/stolen", http.StatusFound)
	}))
	defer authServer.Close()

	client := authServer.Client()
	client.CheckRedirect = sameOriginRedirectPolicy
	req, err := http.NewRequest(http.MethodGet, authServer.URL+"/token", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("admin:redirect-secret")))
	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "cross-origin authentication redirect") {
		t.Fatalf("Do() error = %v, want redirect rejection", err)
	}
	if evilRequests.Load() != 0 {
		t.Fatalf("redirect target requests = %d, want 0", evilRequests.Load())
	}
}

func TestPullCredentialHelpersCanBeDisabled(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	content := fmt.Sprintf(`{"credHelpers":{"registry.example":"pass"},"auths":{"registry.example":{"auth":%q}}}`,
		base64.StdEncoding.EncodeToString([]byte("fallback:secret")))
	if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	runner := newTestPullRunner()
	called := false
	runner.runCredentialHelper = func(context.Context, string, string) (pullRegistryCredential, error) {
		called = true
		return pullRegistryCredential{}, nil
	}
	cred, err := runner.loadPullRegistryCredential(context.Background(), "registry.example", PullOptions{
		DockerConfig:             configPath,
		DisableCredentialHelpers: true,
		CredentialHelperTimeout:  time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("credential helper ran while disabled")
	}
	if !cred.Found || cred.Source != "auths" || cred.Username != "fallback" {
		t.Fatalf("credential = %#v", cred)
	}
}

func TestPullCredentialHelperFailureIsNotSilentlyTreatedAsMissing(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"credsStore":"pass"}`), 0600); err != nil {
		t.Fatal(err)
	}
	runner := newTestPullRunner()
	runner.runCredentialHelper = func(context.Context, string, string) (pullRegistryCredential, error) {
		return pullRegistryCredential{}, errors.New("helper unavailable")
	}
	_, err := runner.loadPullRegistryCredential(context.Background(), "registry.example", PullOptions{
		DockerConfig: configPath,
	})
	if err == nil || !strings.Contains(err.Error(), "helper unavailable") {
		t.Fatalf("loadPullRegistryCredential() error = %v, want helper failure", err)
	}
}

func TestPullCredentialHelperFailureRedactsOpaqueMessage(t *testing.T) {
	previous := sensitive.DefaultProfile()
	sensitive.SetDefaultProfile(sensitive.ProfileBasic)
	t.Cleanup(func() { sensitive.SetDefaultProfile(previous) })

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"credsStore":"pass"}`), 0600); err != nil {
		t.Fatal(err)
	}
	const opaqueSecret = "pull-helper-value-5b8c"
	runner := newTestPullRunner()
	runner.runCredentialHelper = func(context.Context, string, string) (pullRegistryCredential, error) {
		return pullRegistryCredential{}, errors.New("fatal helper response: " + opaqueSecret)
	}
	_, err := runner.loadPullRegistryCredential(context.Background(), "registry.example", PullOptions{DockerConfig: configPath})
	if err == nil {
		t.Fatal("loadPullRegistryCredential() error = nil, want helper failure")
	}
	if strings.Contains(err.Error(), opaqueSecret) || !strings.Contains(err.Error(), sensitive.RedactedValue) {
		t.Fatalf("loadPullRegistryCredential() error = %q, want opaque detail redacted", err)
	}
}

func TestPullCommandExposesCredentialBoundaryFlags(t *testing.T) {
	cmd := NewPullCommand()
	for _, name := range []string{"disable-credential-helpers", "credential-helper-timeout", "auth-realm"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("missing --%s", name)
		}
	}
}

func TestVerboseHTTPLoggingHonorsExplicitRedaction(t *testing.T) {
	t.Cleanup(func() {
		sensitive.SetDefaultProfile(sensitive.ProfileNone)
		configureHTTPLogging(false)
	})
	for _, profile := range []sensitive.Profile{sensitive.ProfileBasic, sensitive.ProfileStrict} {
		t.Run(string(profile), func(t *testing.T) {
			var output bytes.Buffer
			sensitive.SetDefaultProfile(profile)
			configureHTTPLoggingTo(true, &output)
			http_utils.Logger.Print("Authorization: Bearer verbose-secret token=http-secret")
			got := output.String()
			for _, leaked := range []string{"verbose-secret", "http-secret"} {
				if strings.Contains(got, leaked) {
					t.Fatalf("verbose HTTP output leaked %q: %s", leaked, got)
				}
			}
		})
	}
}
