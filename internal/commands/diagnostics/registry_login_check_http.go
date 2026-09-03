package diagnostics

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"docker-manager/internal/registryca"
)

var registryCheckHTTPClient httpDoer = &http.Client{CheckRedirect: registryCredentialRedirectPolicy}

type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

func registryCredentialRedirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	if len(via) == 0 {
		return nil
	}
	if strings.EqualFold(via[len(via)-1].URL.Scheme, "https") && !strings.EqualFold(req.URL.Scheme, "https") {
		return fmt.Errorf("refusing registry HTTPS redirect downgrade to %s", req.URL.Redacted())
	}
	if !sameRegistryOrigin(via[0].URL, req.URL) {
		return fmt.Errorf("refusing registry credential redirect from %s to %s", via[0].URL.Redacted(), req.URL.Redacted())
	}
	return nil
}

func sameRegistryOrigin(left, right *url.URL) bool {
	if left == nil || right == nil || !strings.EqualFold(left.Scheme, right.Scheme) {
		return false
	}
	return registryURLHost(left) == registryURLHost(right)
}

func registryURLHost(value *url.URL) string {
	port := value.Port()
	if port == "" {
		if strings.EqualFold(value.Scheme, "https") {
			port = "443"
		} else if strings.EqualFold(value.Scheme, "http") {
			port = "80"
		}
	}
	return net.JoinHostPort(strings.ToLower(value.Hostname()), port)
}

func pingRegistryV2(ctx context.Context, registryName string, plainHTTP bool, cred registryCredential) CheckResult {
	return pingRegistryV2WithClient(ctx, registryCheckHTTPClient, registryName, plainHTTP, cred)
}

func pingRegistryV2WithClient(ctx context.Context, client httpDoer, registryName string, plainHTTP bool, cred registryCredential) CheckResult {
	scheme := "https"
	if plainHTTP {
		scheme = "http"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s://%s/v2/", scheme, registryName), nil)
	if err != nil {
		return CheckResult{Status: "failed", Message: err.Error()}
	}
	if cred.Username != "" && cred.Password != "" {
		req.SetBasicAuth(cred.Username, cred.Password)
	}
	if client == nil {
		client = registryCheckHTTPClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return CheckResult{Status: "failed", Message: err.Error()}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	switch resp.StatusCode {
	case http.StatusOK:
		return CheckResult{Status: "ok", HTTPStatus: resp.StatusCode, Message: "registry /v2/ 可访问"}
	case http.StatusUnauthorized:
		if cred.Found {
			return CheckResult{Status: "failed", HTTPStatus: resp.StatusCode, Message: "registry 需要认证，但已配置凭据未被 /v2/ 接受"}
		}
		return CheckResult{Status: "warning", HTTPStatus: resp.StatusCode, Message: "registry 可访问但需要认证"}
	case http.StatusForbidden:
		return CheckResult{Status: "failed", HTTPStatus: resp.StatusCode, Message: "registry 拒绝访问"}
	default:
		if resp.StatusCode >= 200 && resp.StatusCode < 400 {
			return CheckResult{Status: "ok", HTTPStatus: resp.StatusCode, Message: resp.Status}
		}
		return CheckResult{Status: "failed", HTTPStatus: resp.StatusCode, Message: resp.Status}
	}
}

func newRegistryCheckHTTPClient(opts RegistryLoginCheckOptions) (httpDoer, func(), error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, func() {}, fmt.Errorf("default HTTP transport has unexpected type %T", http.DefaultTransport)
	}
	transport := base.Clone()
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	transport.DialContext = dialer.DialContext
	transport.TLSHandshakeTimeout = timeout
	transport.ResponseHeaderTimeout = timeout

	switch {
	case opts.NoProxy:
		transport.Proxy = nil
	case strings.TrimSpace(opts.Proxy) != "":
		proxyURL, err := parseRegistryProxyURL(opts.Proxy)
		if err != nil {
			return nil, func() {}, err
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	default:
		transport.Proxy = validatedRegistryProxyFunc(http.ProxyFromEnvironment)
	}

	if strings.TrimSpace(opts.RegistryCAFile) != "" || strings.TrimSpace(opts.RegistryCAPath) != "" {
		roots, err := registryRootCAs(opts.RegistryCAFile, opts.RegistryCAPath)
		if err != nil {
			return nil, func() {}, err
		}
		tlsConfig := &tls.Config{RootCAs: roots}
		if transport.TLSClientConfig != nil {
			tlsConfig = transport.TLSClientConfig.Clone()
			tlsConfig.RootCAs = roots
		}
		transport.TLSClientConfig = tlsConfig
	}

	client := &http.Client{
		Transport:     transport,
		CheckRedirect: registryCredentialRedirectPolicy,
	}
	return client, transport.CloseIdleConnections, nil
}

func validatedRegistryProxyFunc(next func(*http.Request) (*url.URL, error)) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		proxyURL, err := next(req)
		if err != nil || proxyURL == nil {
			return proxyURL, err
		}
		if proxyURL.Scheme == "" || proxyURL.Host == "" || proxyURL.Hostname() == "" {
			return nil, fmt.Errorf("无效环境 registry proxy: 必须包含 scheme 和 host")
		}
		return proxyURL, nil
	}
}

func parseRegistryProxyURL(raw string) (*url.URL, error) {
	value := strings.TrimSpace(raw)
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Hostname() == "" {
		return nil, fmt.Errorf("无效 registry proxy %q: 必须包含 scheme 和 host", raw)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5":
		return parsed, nil
	default:
		return nil, fmt.Errorf("无效 registry proxy %q: 不支持 scheme %q", raw, parsed.Scheme)
	}
}

func registryRootCAs(caFile, caPath string) (*x509.CertPool, error) {
	return registryca.Load(caFile, caPath)
}
