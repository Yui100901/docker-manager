package pull

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"docker-manager/internal/appconfig"
)

func TestRegistryPolicyUsesIsolatedCustomCAs(t *testing.T) {
	first := newRegistryTLSServer(t)
	second := newRegistryTLSServer(t)
	firstCA := writeRegistryServerCA(t, first)
	secondCA := writeRegistryServerCA(t, second)
	firstRegistry := registryServerHost(t, first.URL)
	secondRegistry := registryServerHost(t, second.URL)

	runner, err := NewPullRunner("", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	opts := PullOptions{RegistryPolicies: map[string]appconfig.RegistryPolicy{
		firstRegistry:  {CAFile: firstCA, CAFileSet: true},
		secondRegistry: {CAFile: secondCA, CAFileSet: true},
	}}

	firstRunner, firstOpts, err := runner.bindRegistryPolicy(firstRegistry, registryCredentialPull, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := firstRunner.pingRegistryV2(context.Background(), firstRegistry, firstOpts, pullRegistryCredential{}, &ImageInfo{Registry: firstRegistry}); got.status != registryPingOK {
		t.Fatalf("first registry ping = %#v, want ok", got)
	}
	secondRunner, secondOpts, err := runner.bindRegistryPolicy(secondRegistry, registryCredentialPull, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := secondRunner.pingRegistryV2(context.Background(), secondRegistry, secondOpts, pullRegistryCredential{}, &ImageInfo{Registry: secondRegistry}); got.status != registryPingOK {
		t.Fatalf("second registry ping = %#v, want ok", got)
	}
	if got := firstRunner.pingRegistryV2(context.Background(), secondRegistry, secondOpts, pullRegistryCredential{}, &ImageInfo{Registry: secondRegistry}); got.status != registryPingFailed {
		t.Fatalf("first policy client trusted second registry: %#v", got)
	}
	if firstRunner.httpClient == secondRunner.httpClient {
		t.Fatal("registries with different CA policies shared one HTTP client")
	}
}

func TestRegistryPolicyCAPathAndInvalidCAFailClosed(t *testing.T) {
	server := newRegistryTLSServer(t)
	registry := registryServerHost(t, server.URL)
	caDir := t.TempDir()
	caFile := writeRegistryServerCA(t, server)
	data, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(caDir, "registry.crt"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	runner, err := NewPullRunner("", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	bound, effective, err := runner.bindRegistryPolicy(registry, registryCredentialPull, PullOptions{RegistryPolicies: map[string]appconfig.RegistryPolicy{
		registry: {CAPath: caDir, CAPathSet: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := bound.pingRegistryV2(context.Background(), registry, effective, pullRegistryCredential{}, &ImageInfo{Registry: registry}); got.status != registryPingOK {
		t.Fatalf("CA path registry ping = %#v, want ok", got)
	}

	badCA := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(badCA, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = runner.bindRegistryPolicy(registry, registryCredentialPull, PullOptions{RegistryPolicies: map[string]appconfig.RegistryPolicy{
		registry: {CAFile: badCA, CAFileSet: true},
	}})
	if err == nil || !strings.Contains(err.Error(), "valid PEM certificate") {
		t.Fatalf("invalid CA error = %v, want fail-closed PEM rejection", err)
	}

	mixedCA := filepath.Join(t.TempDir(), "mixed.pem")
	mixedData := append(append([]byte(nil), data...), []byte("trailing garbage")...)
	if err := os.WriteFile(mixedCA, mixedData, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = runner.bindRegistryPolicy(registry, registryCredentialPull, PullOptions{RegistryPolicies: map[string]appconfig.RegistryPolicy{
		registry: {CAFile: mixedCA, CAFileSet: true},
	}})
	if err == nil || !strings.Contains(err.Error(), "valid PEM certificate") {
		t.Fatalf("mixed CA error = %v, want fail-closed trailing-data rejection", err)
	}
}

func TestRegistryPolicyCAPathEnforcesEntryAndByteLimits(t *testing.T) {
	server := newRegistryTLSServer(t)
	caFile := writeRegistryServerCA(t, server)
	certificate, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("entry count", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range []string{"one.pem", "two.pem"} {
			if err := os.WriteFile(filepath.Join(dir, name), certificate, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		err := appendRegistryCAPath(x509.NewCertPool(), dir, 1, int64(len(certificate))*2)
		if err == nil || !strings.Contains(err.Error(), "exceeds 1 certificate files") {
			t.Fatalf("entry limit error = %v", err)
		}
	})

	t.Run("cumulative bytes", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "ca.pem"), certificate, 0o600); err != nil {
			t.Fatal(err)
		}
		err := appendRegistryCAPath(x509.NewCertPool(), dir, 1, int64(len(certificate)-1))
		if err == nil || !strings.Contains(err.Error(), "total bytes") {
			t.Fatalf("byte limit error = %v", err)
		}
	})
}

func TestRegistryPolicyCAPathRejectsNonRegularEntries(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "nested"), 0o700); err != nil {
			t.Fatal(err)
		}
		err := appendRegistryCAPath(x509.NewCertPool(), dir, maxRegistryCAPathEntries, maxRegistryCAPathBytes)
		if err == nil || !strings.Contains(err.Error(), "non-symlink regular file") {
			t.Fatalf("directory entry error = %v", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "target.pem")
		if err := os.WriteFile(target, []byte("certificate"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, "linked.pem")); err != nil {
			t.Skipf("symlink creation unavailable: %v", err)
		}
		err := appendRegistryCAPath(x509.NewCertPool(), dir, maxRegistryCAPathEntries, maxRegistryCAPathBytes)
		if err == nil || !strings.Contains(err.Error(), "non-symlink regular file") {
			t.Fatalf("symlink entry error = %v", err)
		}
	})
}

func TestRegistryPolicyProxyAndNoProxyAreAppliedPerRegistry(t *testing.T) {
	var proxyHits atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(proxy.Close)

	runner, err := NewPullRunner("", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	proxiedRegistry := "registry-policy-proxy.invalid"
	proxied, proxiedOpts, err := runner.bindRegistryPolicy(proxiedRegistry, registryCredentialPull, PullOptions{RegistryPolicies: map[string]appconfig.RegistryPolicy{
		proxiedRegistry: {Proxy: proxy.URL, ProxySet: true, PlainHTTP: true, PlainHTTPSet: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := proxied.pingRegistryV2(context.Background(), proxiedRegistry, proxiedOpts, pullRegistryCredential{}, &ImageInfo{Registry: proxiedRegistry}); got.status != registryPingOK {
		t.Fatalf("proxied registry ping = %#v, want ok", got)
	}
	if proxyHits.Load() != 1 {
		t.Fatalf("proxy hits = %d, want 1", proxyHits.Load())
	}

	var directHits atomic.Int64
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		directHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(direct.Close)
	directRegistry := registryServerHost(t, direct.URL)
	baseWithProxy, err := NewPullRunner(proxy.URL, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	directRunner, directOpts, err := baseWithProxy.bindRegistryPolicy(directRegistry, registryCredentialPull, PullOptions{RegistryPolicies: map[string]appconfig.RegistryPolicy{
		directRegistry: {NoProxy: true, NoProxySet: true, PlainHTTP: true, PlainHTTPSet: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := directRunner.pingRegistryV2(context.Background(), directRegistry, directOpts, pullRegistryCredential{}, &ImageInfo{Registry: directRegistry}); got.status != registryPingOK {
		t.Fatalf("direct registry ping = %#v, want ok", got)
	}
	if directHits.Load() != 1 || proxyHits.Load() != 1 {
		t.Fatalf("direct hits=%d proxy hits=%d, want 1/1", directHits.Load(), proxyHits.Load())
	}
}

func TestRegistryPolicyExplicitProxyAndTimeoutOverridesWin(t *testing.T) {
	var targetHits atomic.Int64
	var proxyHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		time.Sleep(40 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(proxy.Close)
	registry := registryServerHost(t, target.URL)
	runner, err := NewPullRunnerWithTimeout(proxy.URL, "linux", "amd64", 250*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	opts := PullOptions{
		RegistryPolicies: map[string]appconfig.RegistryPolicy{
			registry: {NoProxy: true, NoProxySet: true, Timeout: "5ms", TimeoutSet: true, PlainHTTP: true, PlainHTTPSet: true},
		},
		policyOverrides: registryPolicyOverrides{Proxy: true, Timeout: true},
	}
	bound, effective, err := runner.bindRegistryPolicy(registry, registryCredentialPull, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := bound.pingRegistryV2(context.Background(), registry, effective, pullRegistryCredential{}, &ImageInfo{Registry: registry}); got.status != registryPingOK {
		t.Fatalf("explicit override ping = %#v, want proxy success", got)
	}
	if proxyHits.Load() != 1 || targetHits.Load() != 0 {
		t.Fatalf("proxy hits=%d target hits=%d, explicit proxy did not win", proxyHits.Load(), targetHits.Load())
	}

	directRunner, err := NewPullRunnerWithTimeout("", "linux", "amd64", 250*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	timeoutOpts := PullOptions{
		RegistryPolicies: map[string]appconfig.RegistryPolicy{
			registry: {Timeout: "5ms", TimeoutSet: true, PlainHTTP: true, PlainHTTPSet: true},
		},
		policyOverrides: registryPolicyOverrides{Timeout: true},
	}
	timeoutRunner, timeoutEffective, err := directRunner.bindRegistryPolicy(registry, registryCredentialPull, timeoutOpts)
	if err != nil {
		t.Fatal(err)
	}
	if got := timeoutRunner.pingRegistryV2(context.Background(), registry, timeoutEffective, pullRegistryCredential{}, &ImageInfo{Registry: registry}); got.status != registryPingOK {
		t.Fatalf("explicit timeout override ping = %#v, want slow target success", got)
	}
	if targetHits.Load() != 1 {
		t.Fatalf("target hits after explicit timeout = %d, want 1", targetHits.Load())
	}
}

func TestRegistryPolicyTimeoutAppliesWithoutExplicitOverride(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	registry := registryServerHost(t, server.URL)
	runner, err := NewPullRunnerWithTimeout("", "linux", "amd64", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	bound, effective, err := runner.bindRegistryPolicy(registry, registryCredentialPull, PullOptions{RegistryPolicies: map[string]appconfig.RegistryPolicy{
		registry: {Timeout: "10ms", TimeoutSet: true, PlainHTTP: true, PlainHTTPSet: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := bound.pingRegistryV2(context.Background(), registry, effective, pullRegistryCredential{}, &ImageInfo{Registry: registry}); got.status != registryPingFailed || !strings.Contains(strings.ToLower(got.message), "timeout") {
		t.Fatalf("policy timeout ping = %#v, want timeout failure", got)
	}
}

func TestRegistryPolicySourceAndTargetValuesDoNotLeak(t *testing.T) {
	policies := map[string]appconfig.RegistryPolicy{
		"source.example": {
			PlainHTTP: true, PlainHTTPSet: true,
			AuthRealms: []string{"https://source-auth.example"}, AuthRealmsSet: true,
		},
		"target.example": {
			AuthRealms: []string{"https://target-auth.example"}, AuthRealmsSet: true,
		},
	}
	runner, err := NewPullRunner("", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	base := PullOptions{RegistryPolicies: policies, AuthRealmAllowlist: []string{"https://global-auth.example"}}
	_, source, err := runner.bindRegistryPolicy("source.example", registryCredentialPull, base)
	if err != nil {
		t.Fatal(err)
	}
	_, target, err := runner.bindRegistryPolicy("target.example", registryCredentialPush, base)
	if err != nil {
		t.Fatal(err)
	}
	if !source.PlainHTTP || target.PlainHTTP {
		t.Fatalf("source plain=%t target plain=%t, want true/false", source.PlainHTTP, target.PlainHTTP)
	}
	if strings.Join(source.AuthRealmAllowlist, ",") != "https://source-auth.example" || strings.Join(target.AuthRealmAllowlist, ",") != "https://target-auth.example" {
		t.Fatalf("source realms=%v target realms=%v", source.AuthRealmAllowlist, target.AuthRealmAllowlist)
	}
	if base.PlainHTTP || strings.Join(base.AuthRealmAllowlist, ",") != "https://global-auth.example" {
		t.Fatalf("base options mutated: %#v", base)
	}

	explicit := base
	explicit.PlainHTTP = false
	explicit.AuthRealmAllowlist = []string{"https://cli-auth.example"}
	explicit.policyOverrides = registryPolicyOverrides{PlainHTTP: true, AuthRealms: true}
	_, got, err := runner.bindRegistryPolicy("source.example", registryCredentialPull, explicit)
	if err != nil {
		t.Fatal(err)
	}
	if got.PlainHTTP || strings.Join(got.AuthRealmAllowlist, ",") != "https://cli-auth.example" {
		t.Fatalf("explicit values did not win: %#v", got)
	}
}

func TestRegistryPolicyOnlyOverridesConfiguredRealmAndPlainHTTPFields(t *testing.T) {
	runner, err := NewPullRunner("", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	base := PullOptions{
		PlainHTTP:          true,
		AuthRealmAllowlist: []string{"https://global-auth.example"},
		RegistryPolicies: map[string]appconfig.RegistryPolicy{
			"timeout.example": {Timeout: "15s", TimeoutSet: true},
			"empty.example": {
				PlainHTTPSet: true,
				AuthRealms:   []string{}, AuthRealmsSet: true,
			},
		},
	}
	_, inherited, err := runner.bindRegistryPolicy("timeout.example", registryCredentialPull, base)
	if err != nil {
		t.Fatal(err)
	}
	if !inherited.PlainHTTP || strings.Join(inherited.AuthRealmAllowlist, ",") != "https://global-auth.example" {
		t.Fatalf("unconfigured fields did not inherit global values: %#v", inherited)
	}
	_, cleared, err := runner.bindRegistryPolicy("empty.example", registryCredentialPull, base)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.PlainHTTP || len(cleared.AuthRealmAllowlist) != 0 {
		t.Fatalf("explicit false/empty fields did not clear global values: %#v", cleared)
	}
}

func TestRegistryPolicyCommandTracksExplicitFlagOverrides(t *testing.T) {
	cmd := NewPullCommand()
	for name, value := range map[string]string{
		"proxy":      "",
		"timeout":    "45s",
		"plain-http": "false",
		"auth-realm": "https://auth.cli.example",
	} {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatalf("set --%s: %v", name, err)
		}
	}
	got := commandRegistryPolicyOverrides(cmd)
	if !got.Proxy || !got.Timeout || !got.PlainHTTP || !got.AuthRealms {
		t.Fatalf("explicit overrides = %#v, want all true", got)
	}
	if got := commandRegistryPolicyOverrides(nil); got != (registryPolicyOverrides{}) {
		t.Fatalf("nil command overrides = %#v", got)
	}
}

func TestRegistryPolicyPushTargetExplicitSchemeWins(t *testing.T) {
	policy := map[string]appconfig.RegistryPolicy{
		"target.example": {PlainHTTP: true, PlainHTTPSet: true},
	}
	runner, err := NewPullRunner("", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		to   string
		want bool
	}{
		{to: "https://target.example/team", want: false},
		{to: "http://target.example/team", want: true},
		{to: "target.example/team", want: true},
	} {
		_, effective, err := runner.bindRegistryPolicy("target.example", registryCredentialPush, PullOptions{To: tt.to, RegistryPolicies: policy})
		if err != nil {
			t.Fatal(err)
		}
		if got := pushTargetUsesPlainHTTP(effective); got != tt.want {
			t.Fatalf("pushTargetUsesPlainHTTP(%q) = %t, want %t", tt.to, got, tt.want)
		}
	}
}

func TestRegistryPolicyCredentialScopeBlocksConfigRead(t *testing.T) {
	invalidConfig := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(invalidConfig, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := "registry.example"
	runner, err := NewPullRunner("", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	policies := map[string]appconfig.RegistryPolicy{
		registry: {CredentialScope: []string{}, CredentialScopeSet: true},
	}
	clone := cloneRegistryPolicies(policies)
	if clone[registry].AllowsCredential(registryCredentialPull) {
		t.Fatal("clone changed explicit empty credential_scope into implicit allow-all")
	}
	for _, operation := range []string{registryCredentialPull, registryCredentialPush} {
		cred, err := runner.loadPullRegistryCredential(context.Background(), registry, PullOptions{
			DockerConfig:        invalidConfig,
			RegistryPolicies:    clone,
			credentialOperation: operation,
		})
		if err != nil || cred.Found {
			t.Fatalf("scope-blocked %s credential = %#v, %v", operation, cred, err)
		}
	}

	_, err = runner.loadPullRegistryCredential(context.Background(), registry, PullOptions{
		DockerConfig: invalidConfig,
		RegistryPolicies: map[string]appconfig.RegistryPolicy{
			registry: {CredentialScope: []string{registryCredentialPull}, CredentialScopeSet: true},
		},
		credentialOperation: registryCredentialPull,
	})
	if err == nil {
		t.Fatal("allowed credential scope did not read the invalid Docker config")
	}
}

func TestRegistryPolicyCredentialScopeExplainsAuthenticationFailures(t *testing.T) {
	registryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="private"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(registryServer.Close)
	registry := registryServerHost(t, registryServer.URL)
	runner, err := NewPullRunner("", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	policy := appconfig.RegistryPolicy{
		PlainHTTP: true, PlainHTTPSet: true,
		CredentialScope: []string{}, CredentialScopeSet: true,
	}
	opts := PullOptions{
		To:               "http://" + registry + "/team",
		RegistryPolicies: map[string]appconfig.RegistryPolicy{registry: policy},
		Limits:           effectivePullResourceLimits(PullResourceLimits{}),
	}
	err = runner.checkPushTargetRegistry(context.Background(), registry+"/team/app:latest", opts)
	if err == nil || !strings.Contains(err.Error(), "credential_scope 禁止 push") || strings.Contains(err.Error(), "docker login") {
		t.Fatalf("push preflight error = %v", err)
	}
	_, err = runner.targetManifestExists(context.Background(), "source/app:latest", registry+"/team/app:latest", opts)
	if err == nil || !strings.Contains(err.Error(), "credential_scope 禁止 push") {
		t.Fatalf("skip-existing auth error = %v", err)
	}

	info := &ImageInfo{Registry: registry, Repository: "team", Image: "app", Tag: "latest"}
	_, err = runner.resolveRegistryAuth(context.Background(), `Basic realm="private"`, info, PullOptions{
		RegistryPolicies:    map[string]appconfig.RegistryPolicy{registry: policy},
		credentialOperation: registryCredentialPull,
	})
	if err == nil || !strings.Contains(err.Error(), "credential_scope 禁止 pull") {
		t.Fatalf("source basic auth error = %v", err)
	}
}

func TestRegistryPolicyCredentialScopeAllowsAnonymousPushButExplainsDaemonFailure(t *testing.T) {
	registryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(registryServer.Close)
	registry := registryServerHost(t, registryServer.URL)
	runner, err := NewPullRunner("", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	runner.loadPulledImage = func(context.Context, string, io.Writer) error { return nil }
	runner.tagPulledImage = func(context.Context, string, string) error { return nil }
	runner.pushPulledImage = func(context.Context, string, string, io.Writer) error {
		return context.DeadlineExceeded
	}
	policy := appconfig.RegistryPolicy{
		PlainHTTP: true, PlainHTTPSet: true,
		CredentialScope: []string{}, CredentialScopeSet: true,
	}
	err = runner.completePulledImage("image.tar", &ImageInfo{Repository: "team", Image: "app", Tag: "latest"}, PullOptions{
		Context:          context.Background(),
		To:               "http://" + registry + "/mirror",
		RegistryPolicies: map[string]appconfig.RegistryPolicy{registry: policy},
	})
	if err == nil || !strings.Contains(err.Error(), "credential_scope 禁止 push") {
		t.Fatalf("daemon push error = %v", err)
	}
}

func TestRegistryPolicyBatchOptionsPreservePoliciesPerItem(t *testing.T) {
	policies := map[string]appconfig.RegistryPolicy{
		"first.example":  {PlainHTTP: true, PlainHTTPSet: true},
		"second.example": {AuthRealms: []string{"https://auth.second.example"}, AuthRealmsSet: true},
	}
	runner, err := NewPullRunner("", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan string, 2)
	_, err = runPullBatchWithDeps(context.Background(), PullBatchOptions{
		Images:           []string{"first.example/team/app:v1", "second.example/team/app:v2"},
		OutputDir:        t.TempDir(),
		Concurrency:      2,
		RegistryPolicies: policies,
	}, func(image string, opts PullOptions) error {
		info, err := parseImageInfo(image)
		if err != nil {
			return err
		}
		_, effective, err := runner.bindRegistryPolicy(info.Registry, registryCredentialPull, opts)
		if err != nil {
			return err
		}
		seen <- info.Registry + ":" + strings.Join(effective.AuthRealmAllowlist, ",") + ":" + boolText(effective.PlainHTTP)
		return nil
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{<-seen: true, <-seen: true}
	if !got["first.example::true"] || !got["second.example:https://auth.second.example:false"] {
		t.Fatalf("batch policy results = %#v", got)
	}
}

func newRegistryTLSServer(t *testing.T) *httptest.Server {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "docker-manager registry policy test"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}),
	)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v2/" {
			http.NotFound(w, request)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func writeRegistryServerCA(t *testing.T, server *httptest.Server) string {
	t.Helper()
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	path := filepath.Join(t.TempDir(), "registry-ca.pem")
	if err := os.WriteFile(path, block, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func registryServerHost(t *testing.T, rawURL string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Host
}

func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
