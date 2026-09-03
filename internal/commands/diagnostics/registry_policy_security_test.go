package diagnostics

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"docker-manager/internal/appconfig"
)

func TestRegistryPolicyExplicitProxyOverridesConfiguredNoProxy(t *testing.T) {
	const explicitProxy = "http://flag-proxy.example:8080"
	resolved, _, err := applyRegistryPolicy("registry.example", RegistryLoginCheckOptions{
		Proxy:         explicitProxy,
		proxyExplicit: true,
		ResolveRegistryPolicy: func(string) (appconfig.RegistryPolicy, bool) {
			return appconfig.RegistryPolicy{NoProxy: true}, true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Proxy != explicitProxy || resolved.NoProxy {
		t.Fatalf("resolved policy = %#v, want explicit proxy with no_proxy=false", resolved)
	}
}

func TestParseRegistryProxyURLRejectsEmptyHostname(t *testing.T) {
	if _, err := parseRegistryProxyURL("http://:8080"); err == nil {
		t.Fatal("parseRegistryProxyURL() error = nil, want empty-hostname rejection")
	}
}

func TestValidatedRegistryProxyFuncRejectsEmptyHostname(t *testing.T) {
	proxyFunc := validatedRegistryProxyFunc(func(*http.Request) (*url.URL, error) {
		return &url.URL{Scheme: "http", Host: ":8080"}, nil
	})
	if proxyURL, err := proxyFunc(&http.Request{}); err == nil || proxyURL != nil {
		t.Fatalf("validatedRegistryProxyFunc() = %v, %v, want empty-hostname rejection", proxyURL, err)
	}
}

func TestRegistryPolicyDeniedLoginDoesNotRecommendDockerLoginWithoutConfig(t *testing.T) {
	report := RegistryLoginCheckReport{
		Registry:     "registry.example",
		ConfigFound:  false,
		Credential:   CredentialReport{Source: "registry-policy"},
		RegistryPing: CheckResult{Status: "warning"},
		DockerLogin:  CheckResult{Status: "skipped"},
	}
	for _, recommendation := range registryLoginRecommendations(report) {
		if strings.Contains(recommendation, "docker login") {
			t.Fatalf("unexpected login recommendation %q", recommendation)
		}
	}
}

func TestNormalizeRegistryNameRejectsPolicyBypassInputs(t *testing.T) {
	for _, value := range []string{
		"user:pass@registry.example",
		"registry.example/path",
		"registry.example?scope=push",
		"registry.example#fragment",
		"https://registry.example",
		"*.registry.example",
	} {
		t.Run(value, func(t *testing.T) {
			if normalized, err := normalizeRegistryName(value); err == nil {
				t.Fatalf("normalizeRegistryName(%q) = %q, want rejection", value, normalized)
			}
		})
	}
	if normalized, err := normalizeRegistryName("REGISTRY.Example:05000"); err != nil || normalized != "registry.example:5000" {
		t.Fatalf("normalizeRegistryName canonical result = %q, %v", normalized, err)
	}
	if normalized, err := normalizeRegistryName("REGISTRY-1.DOCKER.IO"); err != nil || normalized != "registry-1.docker.io" {
		t.Fatalf("normalizeRegistryName Docker Hub endpoint = %q, %v", normalized, err)
	}
}
