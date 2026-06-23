package pull

import (
	"strings"
	"testing"
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
