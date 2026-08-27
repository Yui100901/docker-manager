package pull

import (
	"crypto/x509"
	"fmt"
	"strings"
	"sync"
	"time"

	"docker-manager/internal/appconfig"
	"docker-manager/internal/registryca"

	"github.com/Yui100901/MyGo/network/http_utils"
)

const (
	registryCredentialPull         = "pull"
	registryCredentialPush         = "push"
	maxRegistryCAPathBytes   int64 = registryca.MaxPathBytes
	maxRegistryCAPathEntries       = registryca.MaxPathEntries
)

type registryPolicyClientKey struct {
	proxy   string
	noProxy bool
	timeout time.Duration
	caFile  string
	caPath  string
}

type registryPolicyClientCache struct {
	mu      sync.Mutex
	clients map[registryPolicyClientKey]*http_utils.HTTPClient
}

func cloneRegistryPolicies(values map[string]appconfig.RegistryPolicy) map[string]appconfig.RegistryPolicy {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]appconfig.RegistryPolicy, len(values))
	for key, policy := range values {
		policy.CredentialScope = clonePolicyStrings(policy.CredentialScope)
		policy.AuthRealms = clonePolicyStrings(policy.AuthRealms)
		result[key] = policy
	}
	return result
}

func clonePolicyStrings(values []string) []string {
	if values == nil {
		return nil
	}
	result := make([]string, len(values))
	copy(result, values)
	return result
}

func (r *PullRunner) bindRegistryPolicy(registry, operation string, opts PullOptions) (*PullRunner, PullOptions, error) {
	policy, matched := appconfig.ResolveRegistryPolicy(opts.RegistryPolicies, registry)
	if !matched {
		return r, opts, nil
	}

	effective := opts
	effective.credentialOperation = operation
	if !opts.policyOverrides.PlainHTTP && (policy.PlainHTTPSet || policy.PlainHTTP) {
		effective.PlainHTTP = policy.PlainHTTP
	}
	if !opts.policyOverrides.AuthRealms && (policy.AuthRealmsSet || policy.AuthRealms != nil) {
		effective.AuthRealmAllowlist = append([]string(nil), policy.AuthRealms...)
	}

	proxy := r.baseProxy
	noProxy := false
	if !opts.policyOverrides.Proxy {
		switch {
		case policy.NoProxy:
			proxy = ""
			noProxy = true
		case policy.ProxySet || strings.TrimSpace(policy.Proxy) != "":
			proxy = policy.Proxy
		}
	}
	baseTimeout := r.baseTimeout
	if baseTimeout <= 0 {
		baseTimeout = defaultPullTimeout
	}
	timeout := baseTimeout
	if !opts.policyOverrides.Timeout && strings.TrimSpace(policy.Timeout) != "" {
		parsed, err := time.ParseDuration(strings.TrimSpace(policy.Timeout))
		if err != nil || parsed <= 0 {
			return nil, PullOptions{}, fmt.Errorf("registry %q policy timeout must be a positive duration: %q", registry, policy.Timeout)
		}
		timeout = parsed
	}

	caFile := strings.TrimSpace(policy.CAFile)
	caPath := strings.TrimSpace(policy.CAPath)
	if !noProxy && proxy == r.baseProxy && timeout == baseTimeout && caFile == "" && caPath == "" {
		return r, effective, nil
	}
	key := registryPolicyClientKey{
		proxy:   proxy,
		noProxy: noProxy,
		timeout: timeout,
		caFile:  caFile,
		caPath:  caPath,
	}
	client, err := r.registryPolicyClient(key)
	if err != nil {
		return nil, PullOptions{}, fmt.Errorf("registry %q policy: %w", registry, err)
	}
	return r.withHTTPClient(client), effective, nil
}

func (r *PullRunner) registryPolicyClient(key registryPolicyClientKey) (*http_utils.HTTPClient, error) {
	cache := r.policyClients
	if cache == nil {
		cache = &registryPolicyClientCache{clients: make(map[registryPolicyClientKey]*http_utils.HTTPClient)}
		r.policyClients = cache
	}
	cache.mu.Lock()
	client := cache.clients[key]
	cache.mu.Unlock()
	if client != nil {
		return client, nil
	}

	var roots *x509.CertPool
	var err error
	if key.caFile != "" || key.caPath != "" {
		roots, err = registryPolicyCertPool(key.caFile, key.caPath)
		if err != nil {
			return nil, err
		}
	}
	client, err = newPullHTTPClientWithOptions(pullHTTPClientOptions{
		Proxy:   key.proxy,
		NoProxy: key.noProxy,
		Timeout: key.timeout,
		RootCAs: roots,
	})
	if err != nil {
		return nil, err
	}
	cache.mu.Lock()
	if cached := cache.clients[key]; cached != nil {
		client = cached
	} else {
		cache.clients[key] = client
	}
	cache.mu.Unlock()
	return client, nil
}

func (r *PullRunner) withHTTPClient(client *http_utils.HTTPClient) *PullRunner {
	return &PullRunner{
		platform:            r.platform,
		httpClient:          client,
		baseProxy:           r.baseProxy,
		baseTimeout:         r.baseTimeout,
		policyClients:       r.policyClients,
		loadPulledImage:     r.loadPulledImage,
		tagPulledImage:      r.tagPulledImage,
		pushPulledImage:     r.pushPulledImage,
		runCredentialHelper: r.runCredentialHelper,
	}
}

func registryPolicyCertPool(caFile, caPath string) (*x509.CertPool, error) {
	return registryca.Load(caFile, caPath)
}

func appendRegistryCAPath(pool *x509.CertPool, path string, maxEntries int, maxBytes int64) error {
	return registryca.AppendPath(pool, path, maxEntries, maxBytes)
}

func registryCredentialsAllowed(registry string, opts PullOptions) bool {
	policy, matched := appconfig.ResolveRegistryPolicy(opts.RegistryPolicies, registry)
	if !matched {
		return true
	}
	operation := opts.credentialOperation
	if operation == "" {
		operation = registryCredentialPull
	}
	return policy.AllowsCredential(operation)
}

func registryCredentialsDenied(registry, operation string, opts PullOptions) bool {
	policy, matched := appconfig.ResolveRegistryPolicy(opts.RegistryPolicies, registry)
	return matched && !policy.AllowsCredential(operation)
}

func registryCredentialPolicyDescription(operation string) string {
	return fmt.Sprintf("registry policy credential_scope 禁止 %s 凭据，未读取或发送 Docker 凭据", operation)
}
