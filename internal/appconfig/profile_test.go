package appconfig

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestResolveProfilePrecedenceAndExplicitEmpty(t *testing.T) {
	t.Setenv(ProfileEnvName, " env ")

	if name, source := ResolveProfile(" flag ", true, "configured"); name != "flag" || source != "flag:--profile" {
		t.Fatalf("explicit ResolveProfile() = %q, %q", name, source)
	}
	if name, source := ResolveProfile(" ", true, "configured"); name != "" || source != "flag:--profile" {
		t.Fatalf("explicit empty ResolveProfile() = %q, %q", name, source)
	}
	if name, source := ResolveProfile("ignored", false, "configured"); name != "env" || source != "env:"+ProfileEnvName {
		t.Fatalf("environment ResolveProfile() = %q, %q", name, source)
	}

	t.Setenv(ProfileEnvName, " ")
	if name, source := ResolveProfile("ignored", false, " configured "); name != "configured" || source != "config:default_profile" {
		t.Fatalf("configured ResolveProfile() = %q, %q", name, source)
	}
	if name, source := ResolveProfile("", false, ""); name != "" || source != "default" {
		t.Fatalf("default ResolveProfile() = %q, %q", name, source)
	}
}

func TestLoadProfileMergesExplicitZeroValuesAndSources(t *testing.T) {
	path := writeConfig(t, `
proxy: http://base-proxy.example:8080
os: linux
arch: amd64
output_dir: base-output
docker_host: tcp://base.example:2376
docker_tls_verify: true
docker_cert_path: /base/certs
docker_api_version: "1.49"
registry_auth_realms:
  - https://base-auth.example
credential_helpers_disabled: true
verbose: true
default_profile: prod
profiles:
  dev:
    docker_host: tcp://dev.example:2375
  prod:
    proxy: ""
    output_dir: ""
    docker_host: ""
    docker_tls_verify: false
    docker_cert_path: ""
    docker_api_version: ""
    registry_auth_realms: []
    credential_helpers_disabled: false
    verbose: false
`)

	loaded, err := LoadWithOptions(path, LoadOptions{Required: true})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Profile != "prod" || loaded.ProfileSource != "config:default_profile" {
		t.Fatalf("selected profile = %q source=%q", loaded.Profile, loaded.ProfileSource)
	}
	if !reflect.DeepEqual(loaded.AvailableProfiles, []string{"dev", "prod"}) {
		t.Fatalf("AvailableProfiles = %#v", loaded.AvailableProfiles)
	}
	cfg := loaded.Config
	if cfg.Proxy != "" || cfg.OutputDir != "" || cfg.DockerHost != "" || cfg.DockerCertPath != "" || cfg.DockerAPIVersion != "" {
		t.Fatalf("explicit empty values were not merged: %#v", cfg)
	}
	if cfg.DockerTLSVerify == nil || *cfg.DockerTLSVerify || cfg.Verbose || cfg.CredentialHelpersDisabled {
		t.Fatalf("explicit false values were not merged: %#v", cfg)
	}
	if cfg.RegistryAuthRealms == nil || len(cfg.RegistryAuthRealms) != 0 {
		t.Fatalf("explicit empty registry_auth_realms = %#v, want non-nil empty", cfg.RegistryAuthRealms)
	}
	if !cfg.DockerHostSet || !cfg.DockerTLSVerifySet || !cfg.DockerCertPathSet || !cfg.DockerAPIVersionSet {
		t.Fatalf("effective Docker presence markers = %#v", cfg)
	}
	for _, field := range []string{"proxy", "output_dir", "docker_host", "docker_tls_verify", "registry_auth_realms", "verbose"} {
		if !loaded.Fields[field] {
			t.Fatalf("Fields[%q] = false", field)
		}
		if got := loaded.FieldSources[field]; got != "profile:prod@"+path {
			t.Fatalf("FieldSources[%q] = %q", field, got)
		}
	}
	if got := loaded.FieldSources["arch"]; got != "config:"+path {
		t.Fatalf("arch source = %q", got)
	}
}

func TestLoadProfileSelectionPrecedence(t *testing.T) {
	path := writeConfig(t, `
docker_host: tcp://base.example:2375
default_profile: configured
profiles:
  configured: {docker_host: tcp://configured.example:2375}
  env: {docker_host: tcp://env.example:2375}
  flag: {docker_host: tcp://flag.example:2375}
`)

	t.Setenv(ProfileEnvName, "env")
	loaded, err := LoadWithOptions(path, LoadOptions{Required: true})
	if err != nil || loaded.Profile != "env" || loaded.Config.DockerHost != "tcp://env.example:2375" || loaded.ProfileSource != "env:"+ProfileEnvName {
		t.Fatalf("environment selection = %#v, %v", loaded, err)
	}
	loaded, err = LoadWithOptions(path, LoadOptions{Required: true, Profile: "flag", ProfileExplicit: true})
	if err != nil || loaded.Profile != "flag" || loaded.Config.DockerHost != "tcp://flag.example:2375" || loaded.ProfileSource != "flag:--profile" {
		t.Fatalf("flag selection = %#v, %v", loaded, err)
	}
	loaded, err = LoadWithOptions(path, LoadOptions{Required: true, ProfileExplicit: true})
	if err != nil || loaded.Profile != "" || loaded.Config.DockerHost != "tcp://base.example:2375" || loaded.ProfileSource != "flag:--profile" {
		t.Fatalf("explicit empty selection = %#v, %v", loaded, err)
	}

	t.Setenv(ProfileEnvName, "")
	loaded, err = LoadWithOptions(path, LoadOptions{Required: true})
	if err != nil || loaded.Profile != "configured" || loaded.Config.DockerHost != "tcp://configured.example:2375" {
		t.Fatalf("configured selection = %#v, %v", loaded, err)
	}
}

func TestLoadRejectsUnknownOrUnavailableSelectedProfile(t *testing.T) {
	path := writeConfig(t, "profiles:\n  dev: {}\n")
	_, err := LoadWithOptions(path, LoadOptions{Required: true, Profile: "prod", ProfileExplicit: true})
	if err == nil || !strings.Contains(err.Error(), `profile "prod" is not defined`) {
		t.Fatalf("unknown profile error = %v", err)
	}

	missing := filepath.Join(t.TempDir(), "missing.yaml")
	_, err = LoadWithOptions(missing, LoadOptions{Profile: "dev", ProfileExplicit: true})
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("profile with missing optional config error = %v", err)
	}
	loaded, err := LoadWithOptions(missing, LoadOptions{ProfileExplicit: true})
	if err != nil || loaded.ProfileSource != "flag:--profile" || loaded.Profile != "" {
		t.Fatalf("explicit empty profile with missing config = %#v, %v", loaded, err)
	}
}

func TestLoadStrictlyValidatesAllProfiles(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "unknown default", data: "default_profile: prod\nprofiles:\n  dev: {}\n", want: "not defined"},
		{name: "invalid name", data: "profiles:\n  'bad name': {}\n", want: "profile name"},
		{name: "invalid unselected profile", data: "profiles:\n  good: {}\n  bad:\n    docker_timeout: never\n", want: `profile "bad"`},
		{name: "nested profiles", data: "profiles:\n  prod:\n    profiles: {}\n", want: "nested profiles"},
		{name: "nested empty default", data: "profiles:\n  prod:\n    default_profile: ''\n", want: "default_profile"},
		{name: "profile unknown field", data: "profiles:\n  prod:\n    unknown_option: true\n", want: "unknown_option"},
		{name: "profiles sequence", data: "profiles: []\n", want: "profiles"},
		{name: "profile null", data: "profiles:\n  prod:\n", want: "YAML mapping"},
		{name: "default non-string", data: "default_profile: 123\nprofiles:\n  '123': {}\n", want: "default_profile"},
		{name: "effective output conflict", data: "verbose: true\nprofiles:\n  prod:\n    quiet: true\n", want: "effective config"},
		{name: "profile merge key", data: "profiles:\n  base: &base\n    output_dir: images\n  prod:\n    <<: *base\n", want: "merge keys"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadWithOptions(writeConfig(t, tt.data), LoadOptions{Required: true})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("LoadWithOptions() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLoadMergesRegistryPoliciesByCanonicalKey(t *testing.T) {
	path := writeConfig(t, `
registries:
  index.docker.io:
    proxy: http://base-proxy.example:8080
    no_proxy: false
    timeout: 30s
    credential_scope: [pull, push]
    auth_realms: [https://auth.example]
    plain_http: true
profiles:
  prod:
    registries:
      registry-1.docker.io:
        proxy: ""
        no_proxy: true
        timeout: ""
        credential_scope: []
        auth_realms: []
        plain_http: false
`)
	loaded, err := LoadWithOptions(path, LoadOptions{Required: true, Profile: "prod", ProfileExplicit: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Config.Registries) != 1 {
		t.Fatalf("Registries = %#v", loaded.Config.Registries)
	}
	policy, ok := ResolveRegistryPolicy(loaded.Config.Registries, "registry-1.docker.io")
	if !ok {
		t.Fatal("Docker Hub policy was not resolved")
	}
	if policy.Proxy != "" || !policy.NoProxy || policy.Timeout != "" || policy.PlainHTTP {
		t.Fatalf("effective policy scalar fields = %#v", policy)
	}
	if policy.CredentialScope == nil || len(policy.CredentialScope) != 0 || policy.AuthRealms == nil || len(policy.AuthRealms) != 0 {
		t.Fatalf("effective policy empty lists = %#v", policy)
	}
	if !policy.ProxySet || !policy.NoProxySet || !policy.TimeoutSet || !policy.CredentialScopeSet || !policy.AuthRealmsSet || !policy.PlainHTTPSet {
		t.Fatalf("effective policy presence = %#v", policy)
	}
	if policy.AllowsCredential("pull") || policy.AllowsCredential("push") || policy.AllowsCredential("login") {
		t.Fatalf("empty credential_scope unexpectedly allows credentials: %#v", policy)
	}
	for _, field := range []string{"proxy", "no_proxy", "timeout", "credential_scope", "auth_realms", "plain_http"} {
		key := "registries.docker.io." + field
		if got := loaded.FieldSources[key]; got != "profile:prod@"+path {
			t.Fatalf("FieldSources[%q] = %q", key, got)
		}
	}
	profilePolicy := loaded.Config.Profiles["prod"].Registries["docker.io"]
	if !profilePolicy.CredentialScopeSet || !profilePolicy.PlainHTTPSet || !profilePolicy.NoProxySet {
		t.Fatalf("profile policy presence = %#v", profilePolicy)
	}
}

func TestLoadRejectsInvalidRegistryPolicies(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "plain HTTP with CA", data: "registries:\n  registry.example:\n    plain_http: true\n    ca_file: ca.pem\n", want: "cannot be combined"},
		{name: "effective plain HTTP with profile CA", data: "registries:\n  registry.example:\n    plain_http: true\nprofiles:\n  prod:\n    registries:\n      registry.example:\n        ca_file: ca.pem\n", want: "effective config"},
		{name: "invalid timeout", data: "registries:\n  registry.example:\n    timeout: 0s\n", want: "greater than zero"},
		{name: "invalid scope", data: "registries:\n  registry.example:\n    credential_scope: [catalog]\n", want: "credential_scope"},
		{name: "duplicate scope", data: "registries:\n  registry.example:\n    credential_scope: [pull, PULL]\n", want: "duplicate"},
		{name: "insecure realm", data: "registries:\n  registry.example:\n    auth_realms: [http://auth.example]\n", want: "HTTPS origin"},
		{name: "invalid proxy", data: "registries:\n  registry.example:\n    proxy: proxy.example\n", want: "scheme and host"},
		{name: "scheme in key", data: "registries:\n  'https://registry.example': {}\n", want: "without a scheme"},
		{name: "path in key", data: "registries:\n  'registry.example/team': {}\n", want: "host[:port]"},
		{name: "wildcard key", data: "registries:\n  '*.example': {}\n", want: "invalid host"},
		{name: "invalid port", data: "registries:\n  'registry.example:70000': {}\n", want: "invalid port"},
		{name: "normalized collision", data: "registries:\n  docker.io: {}\n  index.docker.io: {}\n", want: "same key"},
		{name: "policy null", data: "registries:\n  registry.example:\n", want: "policy must be a YAML mapping"},
		{name: "scope null", data: "registries:\n  registry.example:\n    credential_scope:\n", want: "YAML sequence"},
		{name: "realms scalar", data: "registries:\n  registry.example:\n    auth_realms: https://auth.example\n", want: "cannot unmarshal"},
		{name: "unknown policy field", data: "registries:\n  registry.example:\n    insecure: true\n", want: "insecure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadWithOptions(writeConfig(t, tt.data), LoadOptions{Required: true})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("LoadWithOptions() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestNormalizeRegistryKey(t *testing.T) {
	tests := []struct {
		value string
		want  string
	}{
		{value: " Registry.Example.COM. ", want: "registry.example.com"},
		{value: "registry.example.com:5000", want: "registry.example.com:5000"},
		{value: "registry.example.com:05000", want: "registry.example.com:5000"},
		{value: "index.docker.io", want: "docker.io"},
		{value: "registry-1.docker.io:443", want: "docker.io:443"},
		{value: "[2001:0db8::1]:5000", want: "[2001:db8::1]:5000"},
		{value: "[::1]", want: "[::1]"},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got, err := NormalizeRegistryKey(tt.value)
			if err != nil || got != tt.want {
				t.Fatalf("NormalizeRegistryKey(%q) = %q, %v, want %q", tt.value, got, err, tt.want)
			}
		})
	}

	for _, value := range []string{"", "https://registry.example", "user@registry.example", "registry.example/path", "registry.example?x=1", "*.example", "registry_example", "registry.example:", "registry.example:0", "registry.example:65536"} {
		t.Run("invalid "+value, func(t *testing.T) {
			if got, err := NormalizeRegistryKey(value); err == nil || got != "" {
				t.Fatalf("NormalizeRegistryKey(%q) = %q, %v, want error", value, got, err)
			}
		})
	}
}

func TestNormalizeRegistryEndpointPreservesDockerHubAliases(t *testing.T) {
	tests := []struct {
		value string
		want  string
	}{
		{value: "REGISTRY-1.DOCKER.IO", want: "registry-1.docker.io"},
		{value: "index.docker.io:00443", want: "index.docker.io:443"},
		{value: "registry.example.com.", want: "registry.example.com"},
	}
	for _, tt := range tests {
		got, err := NormalizeRegistryEndpoint(tt.value)
		if err != nil || got != tt.want {
			t.Fatalf("NormalizeRegistryEndpoint(%q) = %q, %v, want %q", tt.value, got, err, tt.want)
		}
	}
}

func TestResolveRegistryPolicyUsesExactCanonicalKey(t *testing.T) {
	policies := map[string]RegistryPolicy{
		"index.docker.io":       {Timeout: "10s"},
		"registry.example:5000": {Timeout: "20s"},
	}
	if policy, ok := ResolveRegistryPolicy(policies, "registry-1.docker.io"); !ok || policy.Timeout != "10s" {
		t.Fatalf("Docker Hub ResolveRegistryPolicy() = %#v, %v", policy, ok)
	}
	if policy, ok := ResolveRegistryPolicy(policies, "REGISTRY.EXAMPLE:5000"); !ok || policy.Timeout != "20s" {
		t.Fatalf("exact ResolveRegistryPolicy() = %#v, %v", policy, ok)
	}
	for _, registry := range []string{"registry.example", "other.example:5000", "registry.example:5000/team"} {
		if _, ok := ResolveRegistryPolicy(policies, registry); ok {
			t.Fatalf("ResolveRegistryPolicy(%q) unexpectedly matched", registry)
		}
	}
}

func TestRegistryPolicyAllowsCredential(t *testing.T) {
	all := RegistryPolicy{}
	for _, operation := range []string{"pull", "push", "login", " PULL "} {
		if !all.AllowsCredential(operation) {
			t.Fatalf("nil credential scope rejected %q", operation)
		}
	}
	none := RegistryPolicy{CredentialScope: []string{}}
	if none.AllowsCredential("pull") {
		t.Fatal("empty credential scope allowed pull")
	}
	selected := RegistryPolicy{CredentialScope: []string{"pull", "LOGIN"}}
	if !selected.AllowsCredential("pull") || !selected.AllowsCredential("login") || selected.AllowsCredential("push") || selected.AllowsCredential("catalog") {
		t.Fatalf("selected credential scope behavior is wrong: %#v", selected)
	}
}

func TestRegistryPolicyJSONUsesSchemaNamesAndHidesPresence(t *testing.T) {
	data, err := json.Marshal(RegistryPolicy{
		CAFile:             "ca.pem",
		NoProxy:            true,
		CredentialScope:    []string{},
		PlainHTTPSet:       true,
		CredentialScopeSet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{`"ca_file":"ca.pem"`, `"no_proxy":true`, `"credential_scope":[]`, `"plain_http":false`} {
		if !strings.Contains(text, want) {
			t.Fatalf("JSON %s does not contain %s", text, want)
		}
	}
	if strings.Contains(text, "Set") || strings.Contains(text, "set") {
		t.Fatalf("JSON leaked presence markers: %s", text)
	}
}
