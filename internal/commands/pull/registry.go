package pull

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Yui100901/MyGo/struct_utils"
	"github.com/distribution/reference"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
)

type registryPingStatus string

const (
	registryPingOK           registryPingStatus = "ok"
	registryPingAuthRequired registryPingStatus = "auth-required"
	registryPingFailed       registryPingStatus = "failed"
)

type registryPingResult struct {
	status     registryPingStatus
	message    string
	httpStatus int
}

func (r *PullRunner) pingRegistryV2(ctx context.Context, registryName string, opts PullOptions, cred pullRegistryCredential, info *ImageInfo) registryPingResult {
	scheme := "https"
	if opts.PlainHTTP {
		scheme = "http"
	}
	rawURL := fmt.Sprintf("%s://%s/v2/", scheme, registryName)
	result := r.pingRegistryV2Once(ctx, rawURL, nil)
	if result.status == registryPingOK {
		return result
	}
	if result.httpStatus != http.StatusUnauthorized {
		return result
	}
	if !cred.Found {
		return registryPingResult{status: registryPingAuthRequired, httpStatus: result.httpStatus, message: "registry 可访问但需要认证"}
	}
	auth, err := r.resolveRegistryAuth(ctx, result.message, info, opts)
	if err != nil {
		return registryPingResult{status: registryPingFailed, httpStatus: result.httpStatus, message: err.Error()}
	}
	retry := r.pingRegistryV2Once(ctx, rawURL, auth)
	if retry.status == registryPingOK {
		return retry
	}
	return registryPingResult{status: registryPingFailed, httpStatus: retry.httpStatus, message: "已配置凭据未被 registry /v2/ 接受: " + retry.message}
}

func (r *PullRunner) pingRegistryV2Once(ctx context.Context, rawURL string, auth *pullRegistryAuth) registryPingResult {
	req, err := buildGETRequest(ctx, rawURL, authHeaders(nil, auth), nil)
	if err != nil {
		return registryPingResult{status: registryPingFailed, message: err.Error()}
	}
	resp, err := r.httpClient.Client.Do(req)
	if err != nil {
		return registryPingResult{status: registryPingFailed, message: err.Error()}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return registryPingResult{status: registryPingOK, httpStatus: resp.StatusCode, message: resp.Status}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return registryPingResult{status: registryPingAuthRequired, httpStatus: resp.StatusCode, message: resp.Header.Get("WWW-Authenticate")}
	}
	return registryPingResult{status: registryPingFailed, httpStatus: resp.StatusCode, message: resp.Status}
}

func parseImageInfo(imageName string) (*ImageInfo, error) {
	spec := &ImageInfo{
		Registry: defaultRegistry,
		Tag:      "latest",
	}

	named, err := reference.ParseNormalizedNamed(imageName)
	if err != nil {
		return nil, err
	}

	domain := reference.Domain(named)
	if domain != "" && domain != dockerHubDomain {
		spec.Registry = domain
	}

	path := reference.Path(named)
	spec.Repository, spec.Image = splitRepositoryImage(path)

	if tagged, ok := named.(reference.Tagged); ok {
		spec.Tag = tagged.Tag()
	}
	if digested, ok := named.(reference.Digested); ok {
		spec.Digest = digested.Digest().String()
	}

	return spec, nil
}

func (r *PullRunner) fetchManifest(ctx context.Context, info *ImageInfo, opts PullOptions) (*ocispec.Manifest, *pullRegistryAuth, error) {
	manifestURL := registryAPIURL(opts, info, "manifests", getReference(info))
	headers := map[string]string{
		"Accept": strings.Join([]string{
			dockerManifestV2,
			dockerManifestListV2,
			ocispec.MediaTypeImageManifest,
			ocispec.MediaTypeImageIndex,
		}, ", "),
	}

	respBytes, auth, err := r.fetchRegistryBytesWithRetry(ctx, manifestURL, headers, nil, info, opts, nil)
	if err != nil {
		return nil, auth, fmt.Errorf("获取清单失败: %w", err)
	}

	isIndex, err := isManifestIndex(respBytes)
	if err != nil {
		return nil, auth, fmt.Errorf("解析清单类型失败: %w", err)
	}
	if isIndex {
		index, err := struct_utils.UnmarshalData[ocispec.Index](respBytes, struct_utils.JSON)
		if err != nil {
			return nil, auth, fmt.Errorf("解析多架构清单失败: %w", err)
		}
		return r.handleOCIIndex(ctx, info, index, auth, opts)
	}

	manifest, err := struct_utils.UnmarshalData[ocispec.Manifest](respBytes, struct_utils.JSON)
	return manifest, auth, err
}

func getReference(info *ImageInfo) string {
	if info.Digest != "" {
		return info.Digest
	}
	return info.Tag
}

func (r *PullRunner) handleOCIIndex(ctx context.Context, info *ImageInfo, index *ocispec.Index, auth *pullRegistryAuth, opts PullOptions) (*ocispec.Manifest, *pullRegistryAuth, error) {
	log.Println("[+] 检测到多架构镜像索引")
	var selectedDigest string

	for _, m := range index.Manifests {
		if m.Platform != nil &&
			m.Platform.OS == r.platform.targetOS &&
			m.Platform.Architecture == r.platform.targetArch {
			selectedDigest = string(m.Digest)
			break
		}
	}

	if selectedDigest == "" {
		return nil, auth, fmt.Errorf("未找到匹配的平台: %s/%s", r.platform.targetOS, r.platform.targetArch)
	}

	manifestURL := registryAPIURL(opts, info, "manifests", selectedDigest)
	headers := map[string]string{
		"Accept": strings.Join([]string{
			dockerManifestV2,
			ocispec.MediaTypeImageManifest,
		}, ", "),
	}

	resp, auth, err := r.fetchRegistryBytesWithRetry(ctx, manifestURL, headers, nil, info, opts, auth)
	if err != nil {
		return nil, auth, fmt.Errorf("获取架构清单失败: %w", err)
	}

	manifest, err := struct_utils.UnmarshalData[ocispec.Manifest](resp, struct_utils.JSON)
	return manifest, auth, err
}

func registryAPIURL(opts PullOptions, info *ImageInfo, kind, ref string) string {
	scheme := "https"
	if opts.PlainHTTP {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s/v2/%s/%s/%s", scheme, info.Registry, imagePath(info), kind, ref)
}

func (r *PullRunner) fetchRegistryBytesWithRetry(ctx context.Context, rawURL string, headers map[string]string, query map[string]string, info *ImageInfo, opts PullOptions, auth *pullRegistryAuth) ([]byte, *pullRegistryAuth, error) {
	var lastErr error
	currentAuth := auth
	backoff := initialBackoff
	for i := 0; i < maxHTTPRetries; i++ {
		if err := ctx.Err(); err != nil {
			return nil, currentAuth, err
		}
		body, nextAuth, err := r.fetchRegistryBytesOnce(ctx, rawURL, headers, query, info, opts, currentAuth)
		if err == nil {
			return body, nextAuth, nil
		}
		currentAuth = nextAuth
		lastErr = err
		log.Printf("请求 %s 失败（尝试 %d/%d）: %v，稍后重试...", rawURL, i+1, maxHTTPRetries, err)
		if err := sleepWithContext(ctx, backoff); err != nil {
			return nil, currentAuth, err
		}
		backoff *= 2
	}
	return nil, currentAuth, lastErr
}

func (r *PullRunner) fetchRegistryBytesOnce(ctx context.Context, rawURL string, headers map[string]string, query map[string]string, info *ImageInfo, opts PullOptions, auth *pullRegistryAuth) ([]byte, *pullRegistryAuth, error) {
	body, err := r.httpGetBytesWithStatus(ctx, rawURL, authHeaders(headers, auth), query)
	if err == nil {
		return body, auth, nil
	}
	statusErr, ok := err.(*httpStatusError)
	if !ok || statusErr.StatusCode != http.StatusUnauthorized {
		return nil, auth, err
	}
	nextAuth, err := r.resolveRegistryAuth(ctx, statusErr.Header.Get("WWW-Authenticate"), info, opts)
	if err != nil {
		return nil, auth, err
	}
	body, err = r.httpGetBytesWithStatus(ctx, rawURL, authHeaders(headers, nextAuth), query)
	if err != nil {
		return nil, nextAuth, err
	}
	return body, nextAuth, nil
}

// fetchWithRetry 会带简单的重试和指数退避，返回 body bytes
func fetchWithRetry(ctx context.Context, url string, headers map[string]string, query map[string]string) ([]byte, error) {
	runner, err := NewPullRunner("", "linux", "amd64")
	if err != nil {
		return nil, err
	}
	return runner.fetchWithRetry(ctx, url, headers, query)
}

// saveWithRetry 将 GET 请求的响应直接保存到文件，带重试
func saveWithRetry(ctx context.Context, url string, headers map[string]string, query map[string]string, outputPath string) error {
	runner, err := NewPullRunner("", "linux", "amd64")
	if err != nil {
		return err
	}
	return runner.saveWithRetry(ctx, url, headers, query, outputPath)
}

func (r *PullRunner) fetchWithRetry(ctx context.Context, url string, headers map[string]string, query map[string]string) ([]byte, error) {
	var lastErr error
	backoff := initialBackoff
	for i := 0; i < maxHTTPRetries; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		resp, err := r.httpGetBytes(ctx, url, headers, query)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		log.Printf("请求 %s 失败（尝试 %d/%d）: %v，稍后重试...", url, i+1, maxHTTPRetries, err)
		if err := sleepWithContext(ctx, backoff); err != nil {
			return nil, err
		}
		backoff *= 2
	}
	return nil, lastErr
}

func (r *PullRunner) saveWithRetry(ctx context.Context, url string, headers map[string]string, query map[string]string, outputPath string) error {
	var lastErr error
	backoff := initialBackoff
	for i := 0; i < maxHTTPRetries; i++ {
		if err := ctx.Err(); err != nil {
			_ = removePartialDownload(outputPath)
			return err
		}
		if err := r.httpSaveToFile(ctx, url, headers, query, outputPath); err == nil {
			return nil
		} else {
			_ = removePartialDownload(outputPath)
			lastErr = err
			log.Printf("保存 %s 到 %s 失败（尝试 %d/%d）: %v，稍后重试...", url, outputPath, i+1, maxHTTPRetries, err)
			if err := sleepWithContext(ctx, backoff); err != nil {
				_ = removePartialDownload(outputPath)
				return err
			}
			backoff *= 2
		}
	}
	return lastErr
}

func (r *PullRunner) httpGetBytes(ctx context.Context, rawURL string, headers map[string]string, query map[string]string) ([]byte, error) {
	return r.httpGetBytesWithStatus(ctx, rawURL, headers, query)
}

func (r *PullRunner) httpGetBytesWithStatus(ctx context.Context, rawURL string, headers map[string]string, query map[string]string) ([]byte, error) {
	req, err := buildGETRequest(ctx, rawURL, headers, query)
	if err != nil {
		return nil, err
	}
	resp, err := r.httpClient.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			log.Printf("警告: 关闭 HTTP response body 失败: %v", cerr)
		}
	}()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, &httpStatusError{StatusCode: resp.StatusCode, Status: resp.Status, Header: resp.Header.Clone()}
	}
	return io.ReadAll(resp.Body)
}

func buildGETRequest(ctx context.Context, rawURL string, headers map[string]string, query map[string]string) (*http.Request, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	values := parsedURL.Query()
	for key, value := range query {
		values.Set(key, value)
	}
	parsedURL.RawQuery = values.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return req, nil
}

func splitRepositoryImage(path string) (string, string) {
	parts := strings.Split(path, "/")
	if len(parts) == 1 {
		return "", parts[0]
	}
	return strings.Join(parts[:len(parts)-1], "/"), parts[len(parts)-1]
}

func imagePath(info *ImageInfo) string {
	if info.Repository == "" {
		return info.Image
	}
	return info.Repository + "/" + info.Image
}

func isManifestIndex(data []byte) (bool, error) {
	var probe struct {
		MediaType string            `json:"mediaType"`
		Manifests []json.RawMessage `json:"manifests"`
		Config    *json.RawMessage  `json:"config"`
		Layers    []json.RawMessage `json:"layers"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return false, err
	}

	switch probe.MediaType {
	case ocispec.MediaTypeImageIndex, dockerManifestListV2:
		return true, nil
	case ocispec.MediaTypeImageManifest, dockerManifestV2:
		return false, nil
	}

	if len(probe.Manifests) > 0 {
		return true, nil
	}
	if probe.Config != nil || len(probe.Layers) > 0 {
		return false, nil
	}
	return false, nil
}
