package pull

import (
	"net/http"
	"net/url"
	"testing"
)

func TestProxyFromEnvironmentSupportsStandardNoProxyRules(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		noProxy string
		bypass  bool
	}{
		{name: "IPv4 CIDR", target: "https://10.42.7.9/v2/", noProxy: "10.42.0.0/16", bypass: true},
		{name: "IPv4 outside CIDR", target: "https://10.43.7.9/v2/", noProxy: "10.42.0.0/16"},
		{name: "host and port", target: "https://registry.example:5443/v2/", noProxy: "registry.example:5443", bypass: true},
		{name: "different port", target: "https://registry.example:443/v2/", noProxy: "registry.example:5443"},
		{name: "IPv6 CIDR", target: "https://[2001:db8::10]:5443/v2/", noProxy: "2001:db8::/32", bypass: true},
		{name: "IPv6 host and port", target: "https://[2001:db8::10]:5443/v2/", noProxy: "[2001:db8::10]:5443", bypass: true},
		{name: "domain wildcard", target: "https://REGISTRY.EXAMPLE.COM/v2/", noProxy: ".example.com", bypass: true},
		{name: "global wildcard", target: "https://registry.example/v2/", noProxy: "*", bypass: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearProxyEnv(t)
			t.Setenv("HTTPS_PROXY", "http://proxy.example:8080")
			t.Setenv("NO_PROXY", tt.noProxy)
			req, err := http.NewRequest(http.MethodGet, tt.target, nil)
			if err != nil {
				t.Fatal(err)
			}
			proxyURL, err := proxyFromEnvironment(req)
			if err != nil {
				t.Fatal(err)
			}
			if tt.bypass && proxyURL != nil {
				t.Fatalf("proxy = %v, want bypass", proxyURL)
			}
			if !tt.bypass && (proxyURL == nil || proxyURL.Host != "proxy.example:8080") {
				t.Fatalf("proxy = %v, want configured proxy", proxyURL)
			}
		})
	}
}

func TestProxyFromEnvironmentSupportsLowercaseVariables(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("https_proxy", "http://lower-proxy.example:8080")
	t.Setenv("no_proxy", "registry.internal")

	for target, wantProxy := range map[string]bool{
		"https://registry.internal/v2/": false,
		"https://registry.example/v2/":  true,
	} {
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		proxyURL, err := proxyFromEnvironment(req)
		if err != nil {
			t.Fatal(err)
		}
		if (proxyURL != nil) != wantProxy {
			t.Fatalf("target=%s proxy=%v wantProxy=%v", target, proxyURL, wantProxy)
		}
	}
}

func TestSecureRegistryRedirectPolicyStripsCredentialsAcrossOrigins(t *testing.T) {
	original := &http.Request{URL: mustParseURL(t, "https://registry.example/v2/")}
	redirected := &http.Request{
		URL: mustParseURL(t, "https://cdn.example/blobs/layer"),
		Header: http.Header{
			"Authorization":       []string{"Bearer registry-token"},
			"Proxy-Authorization": []string{"Basic proxy-token"},
			"Cookie":              []string{"session=secret"},
			"X-Registry-Auth":     []string{"docker-secret"},
			"Accept":              []string{"application/json"},
		},
	}
	if err := secureRegistryRedirectPolicy(redirected, []*http.Request{original}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Authorization", "Proxy-Authorization", "Cookie", "X-Registry-Auth"} {
		if got := redirected.Header.Get(name); got != "" {
			t.Fatalf("%s survived redirect: %q", name, got)
		}
	}
	if redirected.Header.Get("Accept") == "" {
		t.Fatal("non-sensitive Accept header was removed")
	}
}

func TestRedirectPoliciesRejectDowngradeAndCrossOriginAuthentication(t *testing.T) {
	httpsRequest := &http.Request{URL: mustParseURL(t, "https://auth.example/token")}
	downgrade := &http.Request{URL: mustParseURL(t, "http://auth.example/token")}
	if err := secureRegistryRedirectPolicy(downgrade, []*http.Request{httpsRequest}); err == nil {
		t.Fatal("HTTPS downgrade was accepted")
	}
	crossOrigin := &http.Request{URL: mustParseURL(t, "https://evil.example/token")}
	if err := sameOriginRedirectPolicy(crossOrigin, []*http.Request{httpsRequest}); err == nil {
		t.Fatal("cross-origin authentication redirect was accepted")
	}
	sameOrigin := &http.Request{URL: mustParseURL(t, "https://auth.example/oauth/token")}
	if err := sameOriginRedirectPolicy(sameOrigin, []*http.Request{httpsRequest}); err != nil {
		t.Fatalf("same-origin HTTPS redirect rejected: %v", err)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
