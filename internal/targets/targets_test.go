package targets

import (
	"reflect"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
)

func TestRunningContainersPreservesMatchingOrder(t *testing.T) {
	containers := []container.Summary{
		{ID: "stopped", State: "exited"},
		{ID: "first", State: "running"},
		{ID: "paused", State: "paused"},
		{ID: "second", State: "running"},
	}

	got := RunningContainers(containers)
	want := []container.Summary{containers[1], containers[3]}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RunningContainers() = %#v, want %#v", got, want)
	}
}

func TestFilterContainersMatchesResourceFieldsAndSorts(t *testing.T) {
	containers := []container.Summary{
		{
			ID:     "sha256:bbbbbbbbbbbb9999",
			Names:  []string{"/worker"},
			Image:  "registry.example/team/worker:v2",
			State:  "exited",
			Status: "Exited (0)",
			Labels: map[string]string{"tier": "batch"},
		},
		{
			ID:     "sha256:aaaaaaaaaaaa1111",
			Names:  []string{"/api"},
			Image:  "registry.example/team/api:v1",
			State:  "running",
			Status: "Up 2 minutes",
			Labels: map[string]string{"tier": "frontend"},
		},
		{
			ID:    "cccccccccccc2222",
			Names: []string{"/db"},
			Image: "postgres:17",
			State: "running",
		},
	}

	tests := []struct {
		name    string
		filters []string
		want    []string
	}{
		{name: "name wildcard", filters: []string{"name:a*"}, want: []string{"api"}},
		{name: "short id", filters: []string{"id:bbbbbbbbbbbb"}, want: []string{"worker"}},
		{name: "image repository", filters: []string{"image:team/api"}, want: []string{"api"}},
		{name: "state", filters: []string{"state:running"}, want: []string{"api", "db"}},
		{name: "status", filters: []string{"status:Exited*"}, want: []string{"worker"}},
		{name: "label", filters: []string{"label:tier=frontend"}, want: []string{"api"}},
		{name: "filters are ORed", filters: []string{"name:worker", "name:db"}, want: []string{"db", "worker"}},
		{name: "no match", filters: []string{"name:missing"}, want: []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ContainerNames(FilterContainers(append([]container.Summary(nil), containers...), tt.filters))
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("FilterContainers(%v) names = %#v, want %#v", tt.filters, got, tt.want)
			}
		})
	}
}

func TestFilterContainersWithoutFiltersSortsInput(t *testing.T) {
	containers := []container.Summary{
		{ID: "fallback-b"},
		{ID: "fallback-a"},
		{ID: "ignored", Names: []string{"/zeta"}},
	}

	got := FilterContainers(containers, nil)
	want := []string{"fallback-a", "fallback-b", "zeta"}
	if names := ContainerNames(got); !reflect.DeepEqual(names, want) {
		t.Fatalf("ContainerNames(FilterContainers()) = %#v, want %#v", names, want)
	}
	if got[0].ID != "fallback-a" || containers[0].ID != "fallback-a" {
		t.Fatalf("FilterContainers() did not sort the original no-filter slice: %#v", got)
	}
}

func TestContainerNamesAndDisplayNameHandleEmptyAndSlashNames(t *testing.T) {
	containers := []container.Summary{
		{ID: "id-only"},
		{ID: "ignored", Names: []string{"/named", "/alias"}},
		{},
	}

	if got := ContainerDisplayName(containers[0]); got != "id-only" {
		t.Fatalf("ContainerDisplayName(ID fallback) = %q, want id-only", got)
	}
	if got := ContainerDisplayName(containers[1]); got != "named" {
		t.Fatalf("ContainerDisplayName(name) = %q, want named", got)
	}
	if got := ContainerNames(containers); !reflect.DeepEqual(got, []string{"id-only", "named"}) {
		t.Fatalf("ContainerNames() = %#v, want non-empty sorted names", got)
	}
}

func TestContainerMatchHelpers(t *testing.T) {
	c := container.Summary{
		ID:     "sha256:abcdef1234567890",
		Names:  []string{"/api"},
		Image:  "registry.example/team/api:v1",
		State:  "running",
		Labels: map[string]string{"owner": "platform"},
	}

	for _, filter := range []string{"api", "id:abcdef123456", "image:api", "label:owner=platform"} {
		if !ContainerMatchesFilter(c, filter) {
			t.Errorf("ContainerMatchesFilter(%q) = false, want true", filter)
		}
	}
	if ContainerMatchesFilter(c, "name:worker") {
		t.Fatal("ContainerMatchesFilter(name:worker) = true, want false")
	}
	if !ContainerMatchesFilters(c, nil) {
		t.Fatal("ContainerMatchesFilters(nil) = false, want match-all")
	}
	if !ContainerMatchesFilters(c, []string{"name:missing", "state:RUNNING"}) {
		t.Fatal("ContainerMatchesFilters(OR filters) = false, want true")
	}
	if ContainerMatchesFilters(c, []string{"  "}) {
		t.Fatal("ContainerMatchesFilters(blank) = true, want false")
	}
}

func TestBuildContainerSelectionModesAndCopiesFilters(t *testing.T) {
	tests := []struct {
		name       string
		running    bool
		filters    []string
		defaultAll bool
		message    string
	}{
		{name: "default all", defaultAll: true, message: "未指定容器筛选，默认备份全部本地容器 3 个"},
		{name: "running only", running: true, message: "仅备份运行中容器 3 个"},
		{name: "running filtered", running: true, filters: []string{"api-*", "db"}, message: "在运行中容器内按筛选条件 \"api-*, db\" 选中 3 个"},
		{name: "filtered", filters: []string{"api-*", "db"}, message: "按筛选条件 \"api-*, db\" 选中 3 个容器"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filters := append([]string(nil), tt.filters...)
			got := BuildContainerSelection("备份", 3, tt.running, filters)
			if got.Count != 3 || got.DefaultAll != tt.defaultAll || got.Running != tt.running {
				t.Fatalf("BuildContainerSelection() = %#v", got)
			}
			if got.Message != tt.message {
				t.Fatalf("Message = %q, want %q", got.Message, tt.message)
			}
			if strings.Join(got.Filters, ",") != strings.Join(tt.filters, ",") {
				t.Fatalf("Filters = %#v, want %#v", got.Filters, tt.filters)
			}
			if len(filters) > 0 {
				filters[0] = "changed"
				if got.Filters[0] == "changed" {
					t.Fatal("BuildContainerSelection() retained caller filter slice")
				}
			}
		})
	}
}
