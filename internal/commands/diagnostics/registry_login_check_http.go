package diagnostics

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

var registryCheckHTTPClient httpDoer = http.DefaultClient

type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

func pingRegistryV2(ctx context.Context, registryName string, plainHTTP bool, cred registryCredential) CheckResult {
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
	resp, err := registryCheckHTTPClient.Do(req)
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
