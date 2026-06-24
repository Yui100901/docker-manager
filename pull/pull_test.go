package pull

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
