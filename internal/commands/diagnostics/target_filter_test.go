package diagnostics

import (
	"bytes"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
)

func TestFilterContainerSummariesSupportsKeyedFilters(t *testing.T) {
	containers := []container.Summary{
		{
			ID:     "abcdef1234567890",
			Names:  []string{"/api-1"},
			Image:  "registry.local/team/api:v1",
			State:  "running",
			Status: "Up 2 minutes",
			Labels: map[string]string{"com.example.role": "api"},
		},
		{
			ID:     "fedcba1234567890",
			Names:  []string{"/worker-1"},
			Image:  "registry.local/team/worker:v2",
			State:  "exited",
			Status: "Exited (0)",
			Labels: map[string]string{"com.example.role": "worker"},
		},
	}

	tests := []struct {
		filter string
		want   string
	}{
		{filter: "name:api-*", want: "api-1"},
		{filter: "id=abcdef123456", want: "api-1"},
		{filter: "image:worker", want: "worker-1"},
		{filter: "state:running", want: "api-1"},
		{filter: "status:Exited*", want: "worker-1"},
		{filter: "label:com.example.role=worker", want: "worker-1"},
	}
	for _, tt := range tests {
		t.Run(tt.filter, func(t *testing.T) {
			got := filterContainerSummaries(containers, []string{tt.filter})
			if len(got) != 1 || firstContainerName(got[0].Names) != tt.want {
				t.Fatalf("filterContainerSummaries(%q) = %#v, want %s", tt.filter, got, tt.want)
			}
		})
	}
}

func TestBuildContainerTargetSelectionMessages(t *testing.T) {
	tests := []struct {
		name       string
		action     string
		count      int
		running    bool
		filters    []string
		defaultAll bool
		want       string
	}{
		{name: "default all", action: "检查", count: 3, want: "默认检查全部本地容器 3 个", defaultAll: true},
		{name: "running", action: "扫描", count: 2, running: true, want: "仅扫描运行中容器 2 个"},
		{name: "filters", action: "查看", count: 1, filters: []string{"image:nginx"}, want: "选中 1 个容器"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildContainerTargetSelection(tt.action, tt.count, tt.running, tt.filters)
			if got.DefaultAll != tt.defaultAll {
				t.Fatalf("DefaultAll = %v, want %v", got.DefaultAll, tt.defaultAll)
			}
			if !strings.Contains(got.Message, tt.want) {
				t.Fatalf("Message = %q, want contains %q", got.Message, tt.want)
			}
		})
	}
}

func TestPrintTargetSelection(t *testing.T) {
	var out bytes.Buffer
	printTargetSelection(&out, TargetSelection{Message: "默认检查全部本地容器 2 个"})
	if !strings.Contains(out.String(), "目标: 默认检查全部本地容器 2 个") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestFilterVolumesByPatternsSupportsKeyedFilters(t *testing.T) {
	volumes := []*volume.Volume{
		{Name: "app_data", Driver: "local", Mountpoint: "/var/lib/docker/volumes/app_data/_data", Labels: map[string]string{"app": "demo"}},
		{Name: "cache", Driver: "nfs", Mountpoint: "/mnt/cache", Labels: map[string]string{"app": "cache"}},
	}

	tests := []struct {
		filter string
		want   string
	}{
		{filter: "name:app_*", want: "app_data"},
		{filter: "driver:nfs", want: "cache"},
		{filter: "mountpoint:*/app_data/*", want: "app_data"},
		{filter: "label:app=cache", want: "cache"},
	}
	for _, tt := range tests {
		t.Run(tt.filter, func(t *testing.T) {
			got := filterVolumesByPatterns(volumes, []string{tt.filter})
			if len(got) != 1 || got[0].Name != tt.want {
				t.Fatalf("filterVolumesByPatterns(%q) = %#v, want %s", tt.filter, got, tt.want)
			}
		})
	}
}
