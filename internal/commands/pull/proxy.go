package pull

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Yui100901/MyGo/network/http_utils"
	"golang.org/x/net/http/httpproxy"
)

func newPullHTTPClient(proxy string, timeout time.Duration) (*http_utils.HTTPClient, error) {
	proxyFunc, err := proxyFuncFromSetting(proxy)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = defaultPullTimeout
	}

	dialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		Proxy:                 proxyFunc,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}
	return &http_utils.HTTPClient{
		Client: &http.Client{
			Transport:     transport,
			CheckRedirect: secureRegistryRedirectPolicy,
		},
	}, nil
}

func proxyFuncFromSetting(proxy string) (func(*http.Request) (*url.URL, error), error) {
	if proxy == "" {
		return proxyFromEnvironment, nil
	}

	proxyURL, err := url.Parse(proxy)
	if err != nil {
		return nil, fmt.Errorf("无效代理地址 %q: %w", proxy, err)
	}
	if proxyURL.Scheme == "" || proxyURL.Host == "" {
		return nil, fmt.Errorf("无效代理地址 %q: 必须包含 scheme 和 host，例如 http://127.0.0.1:7890", proxy)
	}
	return http.ProxyURL(proxyURL), nil
}

func proxyFromEnvironment(req *http.Request) (*url.URL, error) {
	if req == nil || req.URL == nil {
		return nil, nil
	}
	return environmentProxyConfig().ProxyFunc()(req.URL)
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}

func environmentProxyConfig() *httpproxy.Config {
	return &httpproxy.Config{
		HTTPProxy:  firstEnv("HTTP_PROXY", "http_proxy"),
		HTTPSProxy: firstEnv("HTTPS_PROXY", "https_proxy"),
		NoProxy:    firstEnv("NO_PROXY", "no_proxy"),
		CGI:        os.Getenv("REQUEST_METHOD") != "",
	}
}

func secureRegistryRedirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	if len(via) == 0 {
		return nil
	}
	previous := via[len(via)-1].URL
	if strings.EqualFold(previous.Scheme, "https") && !strings.EqualFold(req.URL.Scheme, "https") {
		return fmt.Errorf("refusing HTTPS redirect downgrade to %s", req.URL.Redacted())
	}
	if !sameOriginURL(via[0].URL, req.URL) {
		stripSensitiveRedirectHeaders(req.Header)
	}
	return nil
}

func sameOriginRedirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	if len(via) == 0 {
		return nil
	}
	if !sameOriginURL(via[0].URL, req.URL) {
		return fmt.Errorf("refusing cross-origin authentication redirect from %s to %s", via[0].URL.Redacted(), req.URL.Redacted())
	}
	return secureRegistryRedirectPolicy(req, via)
}

func sameOriginURL(left, right *url.URL) bool {
	if left == nil || right == nil || !strings.EqualFold(left.Scheme, right.Scheme) {
		return false
	}
	return canonicalURLHost(left) == canonicalURLHost(right)
}

func canonicalURLHost(value *url.URL) string {
	host := strings.ToLower(value.Hostname())
	port := value.Port()
	if port == "" {
		switch strings.ToLower(value.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return net.JoinHostPort(host, port)
}

func stripSensitiveRedirectHeaders(header http.Header) {
	for _, name := range []string{"Authorization", "Proxy-Authorization", "Cookie", "X-Registry-Auth"} {
		header.Del(name)
	}
}
