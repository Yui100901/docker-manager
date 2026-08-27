package diagnostics

import (
	"fmt"

	"docker-manager/internal/appconfig"
)

func applyRegistryPolicy(registry string, opts RegistryLoginCheckOptions) (RegistryLoginCheckOptions, bool, error) {
	credentialAllowed := true
	if opts.ResolveRegistryPolicy == nil {
		return opts, credentialAllowed, nil
	}
	policy, found := opts.ResolveRegistryPolicy(registry)
	if !found {
		return opts, credentialAllowed, nil
	}
	if !opts.plainHTTPExplicit {
		opts.PlainHTTP = policy.PlainHTTP
	}
	if !opts.proxyExplicit {
		opts.Proxy = policy.Proxy
	}
	if !opts.noProxyExplicit {
		if opts.proxyExplicit {
			opts.NoProxy = false
		} else {
			opts.NoProxy = policy.NoProxy
		}
	}
	if !opts.registryCAFileExplicit {
		opts.RegistryCAFile = policy.CAFile
	}
	if !opts.registryCAPathExplicit {
		opts.RegistryCAPath = policy.CAPath
	}
	if !opts.timeoutExplicit && policy.Timeout != "" {
		timeout, err := appconfig.PositiveDuration("registry policy timeout", policy.Timeout, opts.Timeout)
		if err != nil {
			return opts, false, fmt.Errorf("registry %s: %w", registry, err)
		}
		opts.Timeout = timeout
	}
	return opts, policy.AllowsCredential("login"), nil
}

func credentialPolicyBlockedReport() CredentialReport {
	return CredentialReport{
		Source:  "registry-policy",
		Message: "registry policy credential_scope 不允许 login，未读取或发送 Docker 凭据",
	}
}
