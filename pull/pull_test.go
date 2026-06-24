package pull

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Yui100901/MyGo/network/http_utils"
	digest "github.com/opencontainers/go-digest"
)

// 测试parseImageInfo函数，验证不同格式的镜像字符串是否被正确解析为Registry、Repository、Image、Tag和Digest等字段。
func TestParseImageInfo(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		registry   string
		repository string
		image      string
		tag        string
		digest     string
	}{
		{
			name:       "docker hub official image",
			input:      "nginx",
			registry:   defaultRegistry,
			repository: "library",
			image:      "nginx",
			tag:        "latest",
		},
		{
			name:       "docker hub namespace with tag",
			input:      "yui/app:v1",
			registry:   defaultRegistry,
			repository: "yui",
			image:      "app",
			tag:        "v1",
		},
		{
			name:       "registry with port and tag",
			input:      "localhost:5000/team/app:v2",
			registry:   "localhost:5000",
			repository: "team",
			image:      "app",
			tag:        "v2",
		},
		{
			name:       "registry with port and no namespace",
			input:      "localhost:5000/app:v2",
			registry:   "localhost:5000",
			repository: "",
			image:      "app",
			tag:        "v2",
		},
		{
			name:       "digest reference keeps digest",
			input:      "nginx@sha256:" + strings.Repeat("a", 64),
			registry:   defaultRegistry,
			repository: "library",
			image:      "nginx",
			tag:        "latest",
			digest:     "sha256:" + strings.Repeat("a", 64),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseImageInfo(tt.input)
			if err != nil {
				t.Fatalf("parseImageInfo() error = %v", err)
			}
			if got.Registry != tt.registry {
				t.Fatalf("Registry = %q, want %q", got.Registry, tt.registry)
			}
			if got.Repository != tt.repository {
				t.Fatalf("Repository = %q, want %q", got.Repository, tt.repository)
			}
			if got.Image != tt.image {
				t.Fatalf("Image = %q, want %q", got.Image, tt.image)
			}
			if got.Tag != tt.tag {
				t.Fatalf("Tag = %q, want %q", got.Tag, tt.tag)
			}
			if got.Digest != tt.digest {
				t.Fatalf("Digest = %q, want %q", got.Digest, tt.digest)
			}
		})
	}
}

func TestIsManifestIndex(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{
			name: "docker schema v2 manifest is not index",
			input: `{
				"schemaVersion": 2,
				"mediaType": "application/vnd.docker.distribution.manifest.v2+json",
				"config": {"mediaType": "application/vnd.docker.container.image.v1+json", "digest": "sha256:abc", "size": 1},
				"layers": []
			}`,
			want: false,
		},
		{
			name: "oci index is index",
			input: `{
				"schemaVersion": 2,
				"mediaType": "application/vnd.oci.image.index.v1+json",
				"manifests": [{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": "sha256:abc", "size": 1}]
			}`,
			want: true,
		},
		{
			name: "manifest without media type is not index",
			input: `{
				"schemaVersion": 2,
				"config": {"mediaType": "application/vnd.docker.container.image.v1+json", "digest": "sha256:abc", "size": 1},
				"layers": []
			}`,
			want: false,
		},
		{
			name: "index without media type is index",
			input: `{
				"schemaVersion": 2,
				"manifests": [{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": "sha256:abc", "size": 1}]
			}`,
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := isManifestIndex([]byte(tt.input))
			if err != nil {
				t.Fatalf("isManifestIndex() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("isManifestIndex() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProxyFuncFromSettingUsesEnvironmentByDefault(t *testing.T) {
	t.Setenv("http_proxy", "")
	t.Setenv("https_proxy", "")
	t.Setenv("no_proxy", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NO_PROXY", "")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:7890")

	proxyFunc, err := proxyFuncFromSetting("")
	if err != nil {
		t.Fatalf("proxyFuncFromSetting() error = %v", err)
	}

	req, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	got, err := proxyFunc(req)
	if err != nil {
		t.Fatalf("proxyFunc() error = %v", err)
	}
	if got == nil || got.String() != "http://127.0.0.1:7890" {
		t.Fatalf("proxy = %v, want http://127.0.0.1:7890", got)
	}
}

func TestProxyFuncFromSettingNoEnvironmentUsesNoProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NO_PROXY", "")
	t.Setenv("http_proxy", "")
	t.Setenv("https_proxy", "")
	t.Setenv("no_proxy", "")

	proxyFunc, err := proxyFuncFromSetting("")
	if err != nil {
		t.Fatalf("proxyFuncFromSetting() error = %v", err)
	}

	req, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	got, err := proxyFunc(req)
	if err != nil {
		t.Fatalf("proxyFunc() error = %v", err)
	}
	if got != nil {
		t.Fatalf("proxy = %v, want nil", got)
	}
}

func TestProxyFuncFromSettingExplicitProxyOverridesEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:7890")

	proxyFunc, err := proxyFuncFromSetting("http://10.0.0.1:8080")
	if err != nil {
		t.Fatalf("proxyFuncFromSetting() error = %v", err)
	}

	req, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	got, err := proxyFunc(req)
	if err != nil {
		t.Fatalf("proxyFunc() error = %v", err)
	}
	if got == nil || got.String() != "http://10.0.0.1:8080" {
		t.Fatalf("proxy = %v, want http://10.0.0.1:8080", got)
	}
}

func TestProxyFuncFromSettingRejectsInvalidProxy(t *testing.T) {
	if _, err := proxyFuncFromSetting("127.0.0.1:7890"); err == nil {
		t.Fatal("proxyFuncFromSetting() error = nil, want invalid proxy error")
	}
}

func TestProxyFuncFromSettingRespectsNoProxy(t *testing.T) {
	t.Setenv("http_proxy", "")
	t.Setenv("no_proxy", "")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:7890")
	t.Setenv("NO_PROXY", "example.com")

	proxyFunc, err := proxyFuncFromSetting("")
	if err != nil {
		t.Fatalf("proxyFuncFromSetting() error = %v", err)
	}

	req, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	got, err := proxyFunc(req)
	if err != nil {
		t.Fatalf("proxyFunc() error = %v", err)
	}
	if got != nil {
		t.Fatalf("proxy = %v, want nil", got)
	}
}

func TestVerifyFileDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "layer.tar.gz")
	content := []byte("layer-content")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := verifyFileDigest(path, digest.FromBytes(content)); err != nil {
		t.Fatalf("verifyFileDigest() error = %v", err)
	}
	if err := verifyFileDigest(path, digest.FromBytes([]byte("other-content"))); err == nil {
		t.Fatal("verifyFileDigest() error = nil, want mismatch error")
	}
}

func TestResolveOutputFile(t *testing.T) {
	info := &ImageInfo{
		Repository: "library",
		Image:      "nginx",
		Tag:        "latest",
	}

	got, err := resolveOutputFile(info, PullOptions{OutputDir: "dist"})
	if err != nil {
		t.Fatalf("resolveOutputFile() error = %v", err)
	}
	want := filepath.Join("dist", "library_nginx_latest.tar")
	if got != want {
		t.Fatalf("resolveOutputFile() = %q, want %q", got, want)
	}

	got, err = resolveOutputFile(info, PullOptions{Output: filepath.Join("out", "nginx.tar"), OutputDir: "dist"})
	if err != nil {
		t.Fatalf("resolveOutputFile() error = %v", err)
	}
	want = filepath.Join("out", "nginx.tar")
	if got != want {
		t.Fatalf("resolveOutputFile() = %q, want %q", got, want)
	}
}

func TestDefaultOutputFileNameSanitizesTag(t *testing.T) {
	info := &ImageInfo{
		Repository: "team",
		Image:      "app",
		Tag:        "feature/test",
	}

	got := defaultOutputFileName(info)
	want := "team_app_feature_test.tar"
	if got != want {
		t.Fatalf("defaultOutputFileName() = %q, want %q", got, want)
	}
}

func TestPullCommandReturnsInvalidProxyError(t *testing.T) {
	cmd := NewPullCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--proxy", "127.0.0.1:7890", "busybox:latest"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want invalid proxy error")
	}
	if !strings.Contains(err.Error(), "配置代理失败") {
		t.Fatalf("Execute() error = %q, want proxy error", err.Error())
	}
}

func TestPullCommandUsesConfiguredDefaultProxy(t *testing.T) {
	cmd := NewPullCommandWithDefaults(func() CommandDefaults {
		return CommandDefaults{Proxy: "127.0.0.1:7890"}
	})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"busybox:latest"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want invalid configured proxy error")
	}
	if !strings.Contains(err.Error(), "代理失败") {
		t.Fatalf("Execute() error = %q, want proxy error", err.Error())
	}
}

func TestPullCommandRejectsOutputWithMultipleImages(t *testing.T) {
	cmd := NewPullCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--output", "busybox.tar", "busybox:latest", "alpine:latest"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want --output multi-image error")
	}
	if !strings.Contains(err.Error(), "--output") {
		t.Fatalf("Execute() error = %q, want --output error", err.Error())
	}
}

func TestPullCommandReturnsImageParseError(t *testing.T) {
	cmd := NewPullCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"UPPERCASE_IMAGE_NAME"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want image parse error")
	}
	if !strings.Contains(err.Error(), "镜像名称解析失败") {
		t.Fatalf("Execute() error = %q, want image parse error", err.Error())
	}
}

func TestCompletePulledImageLoadsWhenRequested(t *testing.T) {
	var loadedPath string
	previous := loadPulledImage
	loadPulledImage = func(path string) error {
		loadedPath = path
		return nil
	}
	t.Cleanup(func() {
		loadPulledImage = previous
	})

	if err := completePulledImage("busybox.tar", testBusyboxInfo(), PullOptions{Load: true}); err != nil {
		t.Fatalf("completePulledImage() error = %v", err)
	}
	if loadedPath != "busybox.tar" {
		t.Fatalf("loadedPath = %q, want busybox.tar", loadedPath)
	}
}

func TestCompletePulledImageReturnsLoadError(t *testing.T) {
	loadErr := errors.New("load failed")
	previous := loadPulledImage
	loadPulledImage = func(path string) error {
		return loadErr
	}
	t.Cleanup(func() {
		loadPulledImage = previous
	})

	err := completePulledImage("busybox.tar", testBusyboxInfo(), PullOptions{Load: true})
	if err == nil {
		t.Fatal("completePulledImage() error = nil, want load error")
	}
	if !errors.Is(err, loadErr) {
		t.Fatalf("completePulledImage() error = %v, want wrapped %v", err, loadErr)
	}
}

func TestResolvePushTarget(t *testing.T) {
	tests := []struct {
		name   string
		info   *ImageInfo
		target string
		want   string
	}{
		{
			name:   "registry keeps source repository path",
			info:   &ImageInfo{Repository: "team", Image: "app", Tag: "v1"},
			target: "registry.local:5000",
			want:   "registry.local:5000/team/app:v1",
		},
		{
			name:   "namespace prefix uses source image name",
			info:   testBusyboxInfo(),
			target: "registry.local/mirror",
			want:   "registry.local/mirror/busybox:latest",
		},
		{
			name:   "explicit tagged target is used as is",
			info:   testBusyboxInfo(),
			target: "registry.local/mirror/busybox:v2",
			want:   "registry.local/mirror/busybox:v2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolvePushTarget(tt.info, tt.target)
			if err != nil {
				t.Fatalf("resolvePushTarget() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("resolvePushTarget() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolvePushTargetRejectsInvalidTarget(t *testing.T) {
	if _, err := resolvePushTarget(testBusyboxInfo(), ""); err == nil {
		t.Fatal("resolvePushTarget() error = nil, want blank target error")
	}
	if _, err := resolvePushTarget(testBusyboxInfo(), "registry.local/team@sha256:abc"); err == nil {
		t.Fatal("resolvePushTarget() error = nil, want digest target error")
	}
}

func TestCompletePulledImageMirrorsWhenToSet(t *testing.T) {
	var calls []string
	previousLoad := loadPulledImage
	previousTag := tagPulledImage
	previousPush := pushPulledImage
	loadPulledImage = func(path string) error {
		calls = append(calls, "load:"+path)
		return nil
	}
	tagPulledImage = func(ctx context.Context, source, target string) error {
		calls = append(calls, "tag:"+source+"->"+target)
		return nil
	}
	pushPulledImage = func(ctx context.Context, target string) error {
		calls = append(calls, "push:"+target)
		return nil
	}
	t.Cleanup(func() {
		loadPulledImage = previousLoad
		tagPulledImage = previousTag
		pushPulledImage = previousPush
	})

	err := completePulledImage("busybox.tar", testBusyboxInfo(), PullOptions{To: "registry.local:5000"})
	if err != nil {
		t.Fatalf("completePulledImage() error = %v", err)
	}
	want := []string{
		"load:busybox.tar",
		"tag:library/busybox:latest->registry.local:5000/library/busybox:latest",
		"push:registry.local:5000/library/busybox:latest",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
}

func TestCompletePulledImageReturnsPushError(t *testing.T) {
	pushErr := errors.New("push failed")
	previousLoad := loadPulledImage
	previousTag := tagPulledImage
	previousPush := pushPulledImage
	loadPulledImage = func(path string) error { return nil }
	tagPulledImage = func(ctx context.Context, source, target string) error { return nil }
	pushPulledImage = func(ctx context.Context, target string) error { return pushErr }
	t.Cleanup(func() {
		loadPulledImage = previousLoad
		tagPulledImage = previousTag
		pushPulledImage = previousPush
	})

	err := completePulledImage("busybox.tar", testBusyboxInfo(), PullOptions{To: "registry.local:5000"})
	if err == nil {
		t.Fatal("completePulledImage() error = nil, want push error")
	}
	if !errors.Is(err, pushErr) {
		t.Fatalf("completePulledImage() error = %v, want wrapped %v", err, pushErr)
	}
}

func testBusyboxInfo() *ImageInfo {
	return &ImageInfo{Repository: "library", Image: "busybox", Tag: "latest"}
}

func TestConfigureHTTPLogging(t *testing.T) {
	var buf bytes.Buffer
	configureHTTPLogging(false)
	http_utils.Logger.Print("hidden")
	if buf.Len() != 0 {
		t.Fatalf("buffer length = %d, want 0", buf.Len())
	}

	http_utils.Logger.SetOutput(&buf)
	configureHTTPLogging(false)
	http_utils.Logger.Print("hidden")
	if buf.Len() != 0 {
		t.Fatalf("buffer length = %d, want 0 after quiet logging", buf.Len())
	}
}

func TestSleepWithContextReturnsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := sleepWithContext(ctx, time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("sleepWithContext() error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("sleepWithContext() took %s, want immediate cancel", elapsed)
	}
}

func TestFetchWithRetryReturnsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := fetchWithRetry(ctx, "http://127.0.0.1/not-called", nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("fetchWithRetry() error = %v, want context.Canceled", err)
	}
}

func TestBuildGETRequestAppliesHeadersAndQuery(t *testing.T) {
	req, err := buildGETRequest(
		context.Background(),
		"https://example.com/v2/token?service=registry",
		map[string]string{"Authorization": "Bearer token"},
		map[string]string{"scope": "repository:library/busybox:pull"},
	)
	if err != nil {
		t.Fatalf("buildGETRequest() error = %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer token" {
		t.Fatalf("Authorization = %q, want Bearer token", got)
	}
	if got := req.URL.Query().Get("service"); got != "registry" {
		t.Fatalf("service query = %q, want registry", got)
	}
	if got := req.URL.Query().Get("scope"); got != "repository:library/busybox:pull" {
		t.Fatalf("scope query = %q, want repository:library/busybox:pull", got)
	}
}

func TestCreateTarArchiveWithContextRemovesPartialOnCancel(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("[]"), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	output := filepath.Join(t.TempDir(), "image.tar")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := createTarArchiveWithContext(ctx, dir, output)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("createTarArchiveWithContext() error = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(partialDownloadPath(output)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial file stat error = %v, want not exist", err)
	}
}
