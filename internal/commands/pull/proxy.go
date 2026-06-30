package pull

import (
	"fmt"
	"github.com/Yui100901/MyGo/network/http_utils"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
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
			Transport: transport,
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
	if shouldBypassProxy(req.URL.Hostname()) {
		return nil, nil
	}

	proxy := proxyEnvForScheme(req.URL.Scheme)
	if proxy == "" {
		return nil, nil
	}

	proxyURL, err := url.Parse(proxy)
	if err != nil {
		return nil, err
	}
	if proxyURL.Scheme == "" || proxyURL.Host == "" {
		return nil, fmt.Errorf("无效环境变量代理地址 %q: 必须包含 scheme 和 host", proxy)
	}
	return proxyURL, nil
}

func proxyEnvForScheme(scheme string) string {
	switch strings.ToLower(scheme) {
	case "https":
		return firstEnv("HTTPS_PROXY", "https_proxy")
	case "http":
		return firstEnv("HTTP_PROXY", "http_proxy")
	default:
		return ""
	}
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}

func shouldBypassProxy(host string) bool {
	noProxy := firstEnv("NO_PROXY", "no_proxy")
	if noProxy == "" || host == "" {
		return false
	}

	for _, item := range strings.Split(noProxy, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if item == "*" || item == host {
			return true
		}
		if strings.HasPrefix(item, ".") && strings.HasSuffix(host, item) {
			return true
		}
		if strings.HasPrefix(host, item+".") {
			return true
		}
	}
	return false
}
