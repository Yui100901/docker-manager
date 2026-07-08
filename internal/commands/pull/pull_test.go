package pull

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Yui100901/MyGo/network/http_utils"
	"github.com/klauspost/compress/zstd"
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
	clearProxyEnv(t)
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
	clearProxyEnv(t)

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
	clearProxyEnv(t)
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

func TestProxyFuncFromSettingProxyPrecedenceMatrix(t *testing.T) {
	tests := []struct {
		name          string
		target        string
		explicitProxy string
		env           map[string]string
		want          string
	}{
		{
			name:   "https uses HTTPS_PROXY",
			target: "https://registry.example.com/v2/",
			env: map[string]string{
				"HTTP_PROXY":  "http://http-proxy:8080",
				"HTTPS_PROXY": "http://https-proxy:8443",
			},
			want: "http://https-proxy:8443",
		},
		{
			name:   "http uses HTTP_PROXY",
			target: "http://registry.example.com/v2/",
			env: map[string]string{
				"HTTP_PROXY":  "http://http-proxy:8080",
				"HTTPS_PROXY": "http://https-proxy:8443",
			},
			want: "http://http-proxy:8080",
		},
		{
			name:   "uppercase env wins over lowercase env",
			target: "http://registry.example.com/v2/",
			env: map[string]string{
				"HTTP_PROXY": "http://upper-proxy:8080",
				"http_proxy": "http://lower-proxy:8080",
			},
			want: "http://upper-proxy:8080",
		},
		{
			name:   "lowercase env used when uppercase env is empty",
			target: "https://registry.example.com/v2/",
			env: map[string]string{
				"https_proxy": "http://lower-https-proxy:8443",
			},
			want: "http://lower-https-proxy:8443",
		},
		{
			name:   "NO_PROXY bypasses env proxy",
			target: "https://registry.example.com/v2/",
			env: map[string]string{
				"HTTPS_PROXY": "http://https-proxy:8443",
				"NO_PROXY":    "registry.example.com",
			},
			want: "",
		},
		{
			name:   "lowercase no_proxy bypasses env proxy",
			target: "https://registry.example.com/v2/",
			env: map[string]string{
				"HTTPS_PROXY": "http://https-proxy:8443",
				"no_proxy":    ".example.com",
			},
			want: "",
		},
		{
			name:          "explicit proxy ignores env and NO_PROXY",
			target:        "https://registry.example.com/v2/",
			explicitProxy: "http://explicit-proxy:9000",
			env: map[string]string{
				"HTTPS_PROXY": "http://https-proxy:8443",
				"NO_PROXY":    "registry.example.com",
			},
			want: "http://explicit-proxy:9000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if runtime.GOOS == "windows" && tt.name == "uppercase env wins over lowercase env" {
				t.Skip("Windows environment variables are case-insensitive")
			}
			clearProxyEnv(t)
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			proxyFunc, err := proxyFuncFromSetting(tt.explicitProxy)
			if err != nil {
				t.Fatalf("proxyFuncFromSetting() error = %v", err)
			}
			req, err := http.NewRequest(http.MethodGet, tt.target, nil)
			if err != nil {
				t.Fatalf("NewRequest() error = %v", err)
			}
			got, err := proxyFunc(req)
			if err != nil {
				t.Fatalf("proxyFunc() error = %v", err)
			}
			if tt.want == "" {
				if got != nil {
					t.Fatalf("proxy = %v, want nil", got)
				}
				return
			}
			if got == nil || got.String() != tt.want {
				t.Fatalf("proxy = %v, want %s", got, tt.want)
			}
		})
	}
}

func TestProxyFuncFromSettingRejectsInvalidProxy(t *testing.T) {
	if _, err := proxyFuncFromSetting("127.0.0.1:7890"); err == nil {
		t.Fatal("proxyFuncFromSetting() error = nil, want invalid proxy error")
	}
}

func TestProxyFuncFromSettingRespectsNoProxy(t *testing.T) {
	clearProxyEnv(t)
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

func clearProxyEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"} {
		t.Setenv(name, "")
	}
}

func TestNewPullHTTPClientUsesConfiguredTransportTimeouts(t *testing.T) {
	client, err := newPullHTTPClient("", 2*time.Second)
	if err != nil {
		t.Fatalf("newPullHTTPClient() error = %v", err)
	}
	if client.Client.Timeout != 0 {
		t.Fatalf("client timeout = %v, want no whole-response timeout", client.Client.Timeout)
	}
	transport, ok := client.Client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", client.Client.Transport)
	}
	if transport.TLSHandshakeTimeout != 2*time.Second {
		t.Fatalf("TLSHandshakeTimeout = %v, want 2s", transport.TLSHandshakeTimeout)
	}
	if transport.ResponseHeaderTimeout != 2*time.Second {
		t.Fatalf("ResponseHeaderTimeout = %v, want 2s", transport.ResponseHeaderTimeout)
	}
}

func TestDownloadProgressReaderReportsLargeDownload(t *testing.T) {
	var output bytes.Buffer
	reader := newDownloadProgressReader(strings.NewReader(strings.Repeat("x", 2*1024*1024)), &output, "sha256:abc123", 2*1024*1024)

	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatalf("Copy() error = %v", err)
	}
	got := output.String()
	if !strings.Contains(got, "下载完成 sha256:abc123") || !strings.Contains(got, "2.0 MiB") {
		t.Fatalf("progress output = %q, want final large download progress", got)
	}
}

func TestDownloadProgressLabelUsesBlobDigest(t *testing.T) {
	got := downloadProgressLabel("https://registry.example/v2/library/nginx/blobs/sha256:abcdef1234567890?x=1", "layer.tar.gz")
	if got != "sha256:abcdef123456" {
		t.Fatalf("downloadProgressLabel() = %q, want digest prefix", got)
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

func TestMaterializeLayerTarKeepsUncompressedLayer(t *testing.T) {
	dir := t.TempDir()
	blobPath := filepath.Join(dir, "layer.blob")
	tarPath := filepath.Join(dir, "layer.tar")
	content := []byte("plain tar content")
	if err := os.WriteFile(blobPath, content, 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := materializeLayerTar(blobPath, tarPath, "application/vnd.oci.image.layer.v1.tar"); err != nil {
		t.Fatalf("materializeLayerTar() error = %v", err)
	}
	got, err := os.ReadFile(tarPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("layer.tar = %q, want %q", got, content)
	}
	if _, err := os.Stat(blobPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blob file still exists or unexpected stat error: %v", err)
	}
}

func TestMaterializeLayerTarDecompressesGzipLayer(t *testing.T) {
	dir := t.TempDir()
	blobPath := filepath.Join(dir, "layer.blob")
	tarPath := filepath.Join(dir, "layer.tar")
	content := []byte("compressed tar content")
	file, err := os.Create(blobPath)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	gw := gzip.NewWriter(file)
	if _, err := gw.Write(content); err != nil {
		t.Fatalf("gzip Write() error = %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip Close() error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("file Close() error = %v", err)
	}

	if err := materializeLayerTar(blobPath, tarPath, "application/vnd.oci.image.layer.v1.tar+gzip"); err != nil {
		t.Fatalf("materializeLayerTar() error = %v", err)
	}
	got, err := os.ReadFile(tarPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("layer.tar = %q, want %q", got, content)
	}
	if _, err := os.Stat(blobPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blob file still exists or unexpected stat error: %v", err)
	}
}

func TestMaterializeLayerTarDecompressesZstdLayer(t *testing.T) {
	dir := t.TempDir()
	blobPath := filepath.Join(dir, "layer.blob")
	tarPath := filepath.Join(dir, "layer.tar")
	content := []byte("zstd compressed tar content")
	if err := writeZstdTestFile(blobPath, content); err != nil {
		t.Fatalf("writeZstdTestFile() error = %v", err)
	}

	if err := materializeLayerTar(blobPath, tarPath, "application/vnd.oci.image.layer.v1.tar+zstd"); err != nil {
		t.Fatalf("materializeLayerTar() error = %v", err)
	}
	got, err := os.ReadFile(tarPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("layer.tar = %q, want %q", got, content)
	}
	if _, err := os.Stat(blobPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blob file still exists or unexpected stat error: %v", err)
	}
}

func TestMaterializeLayerTarDetectsZstdHeader(t *testing.T) {
	dir := t.TempDir()
	blobPath := filepath.Join(dir, "layer.blob")
	tarPath := filepath.Join(dir, "layer.tar")
	content := []byte("zstd header compressed tar content")
	if err := writeZstdTestFile(blobPath, content); err != nil {
		t.Fatalf("writeZstdTestFile() error = %v", err)
	}

	if err := materializeLayerTar(blobPath, tarPath, "application/vnd.oci.image.layer.v1.tar"); err != nil {
		t.Fatalf("materializeLayerTar() error = %v", err)
	}
	got, err := os.ReadFile(tarPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("layer.tar = %q, want %q", got, content)
	}
	if _, err := os.Stat(blobPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blob file still exists or unexpected stat error: %v", err)
	}
}

func writeZstdTestFile(path string, content []byte) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	zw, err := zstd.NewWriter(file)
	if err != nil {
		_ = file.Close()
		return err
	}
	if _, err := zw.Write(content); err != nil {
		_ = zw.Close()
		_ = file.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
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
	clearProxyEnv(t)
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:7890")
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

func TestPullCommandExplicitProxyOverridesConfiguredDefaultProxy(t *testing.T) {
	clearProxyEnv(t)
	cmd := NewPullCommandWithDefaults(func() CommandDefaults {
		return CommandDefaults{Proxy: "http://127.0.0.1:7890"}
	})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--proxy", "127.0.0.1:7890", "busybox:latest"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want invalid explicit proxy error")
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

func TestPullCommandHasNoMirrorSubcommand(t *testing.T) {
	cmd := NewPullCommand()
	for _, sub := range cmd.Commands() {
		if sub.Name() == "mirror" {
			t.Fatal("pull should not expose mirror subcommand")
		}
	}
	if flag := cmd.Flags().Lookup("file"); flag == nil {
		t.Fatal("pull should expose --file for batch mode")
	}
	if flag := cmd.Flags().Lookup("concurrency"); flag == nil {
		t.Fatal("pull should expose --concurrency for batch mode")
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
	runner := newTestPullRunner()
	runner.loadPulledImage = func(ctx context.Context, path string, output io.Writer) error {
		loadedPath = path
		return nil
	}

	if err := runner.completePulledImage("busybox.tar", testBusyboxInfo(), PullOptions{Load: true}); err != nil {
		t.Fatalf("completePulledImage() error = %v", err)
	}
	if loadedPath != "busybox.tar" {
		t.Fatalf("loadedPath = %q, want busybox.tar", loadedPath)
	}
}

func TestCompletePulledImageReturnsLoadError(t *testing.T) {
	loadErr := errors.New("load failed")
	runner := newTestPullRunner()
	runner.loadPulledImage = func(ctx context.Context, path string, output io.Writer) error {
		return loadErr
	}

	err := runner.completePulledImage("busybox.tar", testBusyboxInfo(), PullOptions{Load: true})
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
		{
			name:   "http scheme is stripped from registry target",
			info:   testBusyboxInfo(),
			target: "http://registry.local:5000/mirror",
			want:   "registry.local:5000/mirror/busybox:latest",
		},
		{
			name:   "https scheme is stripped from tagged target",
			info:   testBusyboxInfo(),
			target: "https://registry.local/mirror/busybox:v3",
			want:   "registry.local/mirror/busybox:v3",
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/" {
			t.Fatalf("path = %q, want /v2/", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	targetRegistry := strings.TrimPrefix(server.URL, "http://")
	configPath := writePullDockerConfig(t, targetRegistry, "dev-user", "dev-pass")
	runner := newTestPullRunner()
	runner.httpClient = &http_utils.HTTPClient{Client: server.Client()}
	runner.loadPulledImage = func(ctx context.Context, path string, output io.Writer) error {
		calls = append(calls, "load:"+path)
		return nil
	}
	runner.tagPulledImage = func(ctx context.Context, source, target string) error {
		calls = append(calls, "tag:"+source+"->"+target)
		return nil
	}
	runner.pushPulledImage = func(ctx context.Context, target, registryAuth string, output io.Writer) error {
		authData, err := base64.URLEncoding.DecodeString(registryAuth)
		if err != nil {
			t.Fatalf("registryAuth is not Docker auth base64: %v", err)
		}
		var auth map[string]string
		if err := json.Unmarshal(authData, &auth); err != nil {
			t.Fatalf("registryAuth JSON error: %v", err)
		}
		if auth["serveraddress"] != targetRegistry || auth["username"] != "dev-user" || auth["password"] != "dev-pass" {
			t.Fatalf("registry auth = %#v, want target registry credentials", auth)
		}
		calls = append(calls, "push:"+target)
		return nil
	}

	err := runner.completePulledImage("busybox.tar", testBusyboxInfo(), PullOptions{To: targetRegistry, PlainHTTP: true, DockerConfig: configPath})
	if err != nil {
		t.Fatalf("completePulledImage() error = %v", err)
	}
	want := []string{
		"load:busybox.tar",
		"tag:library/busybox:latest->" + targetRegistry + "/library/busybox:latest",
		"push:" + targetRegistry + "/library/busybox:latest",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
}

func TestCompletePulledImageUsesHTTPToSchemeForTargetPreflight(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/" {
			t.Fatalf("path = %q, want /v2/", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	targetRegistry := strings.TrimPrefix(server.URL, "http://")
	runner := newTestPullRunner()
	runner.httpClient = &http_utils.HTTPClient{Client: server.Client()}
	runner.loadPulledImage = func(ctx context.Context, path string, output io.Writer) error { return nil }
	runner.tagPulledImage = func(ctx context.Context, source, target string) error {
		if strings.HasPrefix(target, "http://") {
			t.Fatalf("target passed to Docker tag contains scheme: %q", target)
		}
		return nil
	}
	runner.pushPulledImage = func(ctx context.Context, target, registryAuth string, output io.Writer) error { return nil }

	err := runner.completePulledImage("busybox.tar", testBusyboxInfo(), PullOptions{To: "http://" + targetRegistry})
	if err != nil {
		t.Fatalf("completePulledImage() error = %v", err)
	}
}

func TestCompletePulledImageReturnsPushError(t *testing.T) {
	pushErr := errors.New("push failed")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/" {
			t.Fatalf("path = %q, want /v2/", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	targetRegistry := strings.TrimPrefix(server.URL, "http://")
	runner := newTestPullRunner()
	runner.httpClient = &http_utils.HTTPClient{Client: server.Client()}
	runner.loadPulledImage = func(ctx context.Context, path string, output io.Writer) error { return nil }
	runner.tagPulledImage = func(ctx context.Context, source, target string) error { return nil }
	runner.pushPulledImage = func(ctx context.Context, target, registryAuth string, output io.Writer) error { return pushErr }

	err := runner.completePulledImage("busybox.tar", testBusyboxInfo(), PullOptions{To: targetRegistry, PlainHTTP: true})
	if err == nil {
		t.Fatal("completePulledImage() error = nil, want push error")
	}
	if !errors.Is(err, pushErr) {
		t.Fatalf("completePulledImage() error = %v, want wrapped %v", err, pushErr)
	}
}

func TestCompletePulledImagePreflightFailureSkipsDockerActions(t *testing.T) {
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	targetRegistry := strings.TrimPrefix(server.URL, "http://")
	runner := newTestPullRunner()
	runner.httpClient = &http_utils.HTTPClient{Client: server.Client()}
	runner.loadPulledImage = func(ctx context.Context, path string, output io.Writer) error {
		called = true
		return nil
	}
	runner.tagPulledImage = func(ctx context.Context, source, target string) error {
		called = true
		return nil
	}
	runner.pushPulledImage = func(ctx context.Context, target, registryAuth string, output io.Writer) error {
		called = true
		return nil
	}

	err := runner.completePulledImage("busybox.tar", testBusyboxInfo(), PullOptions{To: targetRegistry, PlainHTTP: true})
	if err == nil {
		t.Fatal("completePulledImage() error = nil, want preflight error")
	}
	if !strings.Contains(err.Error(), "推送前检查失败") {
		t.Fatalf("completePulledImage() error = %q, want registry check failure", err.Error())
	}
	if called {
		t.Fatal("Docker action was called after failed preflight")
	}
}

func TestCompletePulledImagePreflightAuthRequiredSkipsDockerActions(t *testing.T) {
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="private"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	targetRegistry := strings.TrimPrefix(server.URL, "http://")
	runner := newTestPullRunner()
	runner.httpClient = &http_utils.HTTPClient{Client: server.Client()}
	runner.loadPulledImage = func(ctx context.Context, path string, output io.Writer) error {
		called = true
		return nil
	}

	err := runner.completePulledImage("busybox.tar", testBusyboxInfo(), PullOptions{
		To:           targetRegistry,
		PlainHTTP:    true,
		DockerConfig: filepath.Join(t.TempDir(), "missing-config.json"),
	})
	if err == nil {
		t.Fatal("completePulledImage() error = nil, want auth-required error")
	}
	if !strings.Contains(err.Error(), "需要认证") || !strings.Contains(err.Error(), "docker login") {
		t.Fatalf("completePulledImage() error = %q, want clear auth guidance", err.Error())
	}
	if called {
		t.Fatal("Docker load was called after failed auth preflight")
	}
}

func TestCompletePulledImagePreflightUsesBasicCredentialFromDockerConfig(t *testing.T) {
	wantAuth := basicAuthHeader("demo", "secret")
	var authorizedPing bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/" {
			t.Fatalf("path = %q, want /v2/", r.URL.Path)
		}
		if r.Header.Get("Authorization") != wantAuth {
			w.Header().Set("WWW-Authenticate", `Basic realm="private"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		authorizedPing = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	var calls []string
	targetRegistry := strings.TrimPrefix(server.URL, "http://")
	runner := newTestPullRunner()
	runner.httpClient = &http_utils.HTTPClient{Client: server.Client()}
	runner.loadPulledImage = func(ctx context.Context, path string, output io.Writer) error {
		calls = append(calls, "load")
		return nil
	}
	runner.tagPulledImage = func(ctx context.Context, source, target string) error {
		calls = append(calls, "tag")
		return nil
	}
	runner.pushPulledImage = func(ctx context.Context, target, registryAuth string, output io.Writer) error {
		calls = append(calls, "push")
		return nil
	}

	configPath := writePullDockerConfig(t, targetRegistry, "demo", "secret")
	err := runner.completePulledImage("busybox.tar", testBusyboxInfo(), PullOptions{
		To:           targetRegistry,
		PlainHTTP:    true,
		DockerConfig: configPath,
	})
	if err != nil {
		t.Fatalf("completePulledImage() error = %v", err)
	}
	if !authorizedPing {
		t.Fatal("registry /v2/ was not retried with Basic auth")
	}
	if strings.Join(calls, ",") != "load,tag,push" {
		t.Fatalf("calls = %#v, want load/tag/push", calls)
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

func TestFetchWithRetryDoesNotRetryAfterRequestCancel(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()

	runner, err := NewPullRunner("", "linux", "amd64")
	if err != nil {
		t.Fatalf("NewPullRunner() error = %v", err)
	}
	runner.httpClient = &http_utils.HTTPClient{Client: server.Client()}

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runner.fetchWithRetry(ctx, server.URL, nil, nil)
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("test server was not called")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("fetchWithRetry() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("fetchWithRetry() did not return after cancellation")
	}
	if logBuf.Len() != 0 {
		t.Fatalf("fetchWithRetry() wrote retry log after cancellation: %q", logBuf.String())
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

func TestParseAuthChallenge(t *testing.T) {
	challenge := parseAuthChallenge(`Bearer realm="https://auth.example/token",service="registry.example",scope="repository:team/app:pull"`)
	if challenge.Scheme != "Bearer" {
		t.Fatalf("Scheme = %q, want Bearer", challenge.Scheme)
	}
	if challenge.Params["realm"] != "https://auth.example/token" ||
		challenge.Params["service"] != "registry.example" ||
		challenge.Params["scope"] != "repository:team/app:pull" {
		t.Fatalf("Params = %#v", challenge.Params)
	}
}

func TestFetchManifestAllowsAnonymousRegistry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/team/app/manifests/v1" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(testOCIManifestJSON()))
	}))
	defer server.Close()
	runner := newTestPullRunner()
	runner.httpClient = &http_utils.HTTPClient{Client: server.Client()}

	info := &ImageInfo{Registry: strings.TrimPrefix(server.URL, "http://"), Repository: "team", Image: "app", Tag: "v1"}
	manifest, auth, err := runner.fetchManifest(context.Background(), info, PullOptions{PlainHTTP: true})
	if err != nil {
		t.Fatalf("fetchManifest() error = %v", err)
	}
	if auth != nil {
		t.Fatalf("auth = %#v, want nil for anonymous registry", auth)
	}
	if manifest.Config.Digest == "" {
		t.Fatalf("manifest = %#v, want config digest", manifest)
	}
}

func TestFetchManifestUsesBearerChallenge(t *testing.T) {
	var tokenRequested bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/team/app/manifests/v1":
			if r.Header.Get("Authorization") != "Bearer test-token" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+serverURLFromRequest(r)+`/token",service="test-registry",scope="repository:team/app:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(testOCIManifestJSON()))
		case "/token":
			tokenRequested = true
			if r.URL.Query().Get("service") != "test-registry" || r.URL.Query().Get("scope") != "repository:team/app:pull" {
				t.Fatalf("token query = %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"token":"test-token"}`))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()
	runner := newTestPullRunner()
	runner.httpClient = &http_utils.HTTPClient{Client: server.Client()}

	info := &ImageInfo{Registry: strings.TrimPrefix(server.URL, "http://"), Repository: "team", Image: "app", Tag: "v1"}
	_, auth, err := runner.fetchManifest(context.Background(), info, PullOptions{PlainHTTP: true})
	if err != nil {
		t.Fatalf("fetchManifest() error = %v", err)
	}
	if !tokenRequested {
		t.Fatal("token endpoint was not requested")
	}
	if auth == nil || auth.Authorization != "Bearer test-token" {
		t.Fatalf("auth = %#v, want bearer token", auth)
	}
}

func TestFetchManifestUsesBasicCredentialFromDockerConfig(t *testing.T) {
	wantAuth := basicAuthHeader("demo", "secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != wantAuth {
			w.Header().Set("WWW-Authenticate", `Basic realm="private"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(testOCIManifestJSON()))
	}))
	defer server.Close()
	runner := newTestPullRunner()
	runner.httpClient = &http_utils.HTTPClient{Client: server.Client()}

	registryName := strings.TrimPrefix(server.URL, "http://")
	configPath := writePullDockerConfig(t, registryName, "demo", "secret")
	info := &ImageInfo{Registry: registryName, Repository: "team", Image: "app", Tag: "v1"}
	_, auth, err := runner.fetchManifest(context.Background(), info, PullOptions{PlainHTTP: true, DockerConfig: configPath})
	if err != nil {
		t.Fatalf("fetchManifest() error = %v", err)
	}
	if auth == nil || auth.Authorization != wantAuth {
		t.Fatalf("auth = %#v, want basic auth", auth)
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

func testOCIManifestJSON() string {
	return `{
		"schemaVersion": 2,
		"mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": {
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"size": 2
		},
		"layers": []
	}`
}

func serverURLFromRequest(r *http.Request) string {
	return "http://" + r.Host
}

func writePullDockerConfig(t *testing.T, registryName, username, password string) string {
	t.Helper()
	auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{"auths":{` + strconvQuote(registryName) + `:{"auth":` + strconvQuote(auth) + `}}}`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func strconvQuote(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func newTestPullRunner() *PullRunner {
	client, err := newPullHTTPClient("", defaultPullTimeout)
	if err != nil {
		panic(err)
	}
	return &PullRunner{
		platform:            targetPlatform{targetOS: "linux", targetArch: "amd64"},
		httpClient:          client,
		loadPulledImage:     loadImageTar,
		tagPulledImage:      tagImage,
		pushPulledImage:     pushImage,
		runCredentialHelper: defaultRunPullCredentialHelper,
	}
}
