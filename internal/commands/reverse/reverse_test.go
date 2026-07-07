package reverse

import (
	"docker-manager/internal/docker"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
)

func TestReverseCommandNoLongerHasRerunFlag(t *testing.T) {
	cmd := NewReverseCommand()
	if flag := cmd.Flags().Lookup("rerun"); flag != nil {
		t.Fatal("reverse command still exposes --rerun")
	}
}

func TestReverseCommandHasNegativeBooleanAliases(t *testing.T) {
	cmd := NewReverseCommand()
	for _, name := range []string{"no-default-envs", "no-merge-ports"} {
		if flag := cmd.Flags().Lookup(name); flag == nil {
			t.Fatalf("reverse command missing --%s", name)
		}
	}
}

func TestRerunRequiresExplicitTarget(t *testing.T) {
	cmd := NewRerunCommand()
	cmd.SetArgs([]string{"--dry-run"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want explicit target error")
	}
	if !strings.Contains(err.Error(), "必须提供容器名称") {
		t.Fatalf("Execute() error = %q, want explicit target hint", err.Error())
	}
}

func TestRerunRequiresConfirm(t *testing.T) {
	cmd := NewRerunCommand()
	cmd.SetArgs([]string{"demo"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want confirmation error")
	}
	if !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("Execute() error = %q, want --confirm hint", err.Error())
	}
}

func TestRerunConfirmErrorMentionsRemoteDocker(t *testing.T) {
	t.Cleanup(func() { docker.Configure(docker.Options{}) })
	docker.Configure(docker.Options{Host: "tcp://docker.example:2375"})
	cmd := NewRerunCommand()
	cmd.SetArgs([]string{"demo"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want confirmation error")
	}
	if !strings.Contains(err.Error(), "tcp://docker.example:2375") {
		t.Fatalf("Execute() error = %q, want remote endpoint", err.Error())
	}
}

func TestReverseRunningCanCombineWithExplicitNames(t *testing.T) {
	cmd := NewReverseCommand()
	cmd.SetArgs([]string{"demo", "--running", "--reverse-type", "invalid"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want invalid type error")
	}
	if strings.Contains(err.Error(), "不能同时指定") {
		t.Fatalf("Execute() error = %q, did not expect --running conflict", err.Error())
	}
	if !strings.Contains(err.Error(), "无效的输出类型") {
		t.Fatalf("Execute() error = %q, want invalid type error", err.Error())
	}
}

func TestRunningContainerNamesFiltersAndSortsRunningContainers(t *testing.T) {
	got := runningContainerNames([]container.Summary{
		{ID: "id-c", Names: []string{"/worker"}, State: "running"},
		{ID: "id-b", Names: []string{"/stopped"}, State: "exited"},
		{ID: "id-a", State: "running"},
		{ID: "id-d", Names: []string{"/api"}, State: "running"},
	})
	want := []string{"api", "id-a", "worker"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("runningContainerNames() = %#v, want %#v", got, want)
	}
}

func TestMatchingContainerNamesSupportsWildcard(t *testing.T) {
	got := matchingContainerNames([]container.Summary{
		{ID: "id-api", Names: []string{"/api-2"}, Image: "demo/api:latest"},
		{ID: "id-db", Names: []string{"/db-1"}, Image: "demo/db:latest"},
		{ID: "id-api-worker", Names: []string{"/api-1"}, Image: "demo/api:latest"},
	}, "api-*")
	want := []string{"api-1", "api-2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("matchingContainerNames() = %#v, want %#v", got, want)
	}
}

func TestReverseContainerMatchesPatternByImage(t *testing.T) {
	c := container.Summary{ID: "id-api", Names: []string{"/api"}, Image: "registry.local/team/api:latest"}
	if !reverseContainerMatchesPattern(c, "*/team/api:*") {
		t.Fatal("reverseContainerMatchesPattern() = false, want true")
	}
}

func TestFilterReverseContainersDefaultsToAllSorted(t *testing.T) {
	got := filterReverseContainers([]container.Summary{
		{ID: "id-worker", Names: []string{"/worker"}, State: "running"},
		{ID: "id-api", Names: []string{"/api"}, State: "exited"},
	}, nil)
	if len(got) != 2 || reverseContainerDisplayName(got[0]) != "api" || reverseContainerDisplayName(got[1]) != "worker" {
		t.Fatalf("filterReverseContainers() = %#v, want api then worker", got)
	}
}

func TestFilterReverseContainersSupportsKeyedFilters(t *testing.T) {
	containers := []container.Summary{
		{
			ID:     "abcdef1234567890",
			Names:  []string{"/api-1"},
			Image:  "registry.local/team/api:latest",
			State:  "running",
			Status: "Up 2 minutes",
			Labels: map[string]string{"com.example.role": "api"},
		},
		{
			ID:     "fedcba1234567890",
			Names:  []string{"/worker-1"},
			Image:  "registry.local/team/worker:latest",
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
		{filter: "label:com.example.role=api", want: "api-1"},
	}
	for _, tt := range tests {
		t.Run(tt.filter, func(t *testing.T) {
			got := filterReverseContainers(containers, []string{tt.filter})
			if len(got) != 1 || reverseContainerDisplayName(got[0]) != tt.want {
				t.Fatalf("filterReverseContainers(%q) = %#v, want %s", tt.filter, got, tt.want)
			}
		})
	}
}

func TestReverseRunningFilterCanBeCombinedWithPatterns(t *testing.T) {
	containers := []container.Summary{
		{ID: "api-id", Names: []string{"/api-1"}, State: "running"},
		{ID: "api-old-id", Names: []string{"/api-old"}, State: "exited"},
		{ID: "db-id", Names: []string{"/db-1"}, State: "running"},
	}
	running := filterReverseRunningContainers(containers)
	got := reverseContainerNames(filterReverseContainers(running, []string{"api-*"}))
	want := []string{"api-1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("running filtered names = %#v, want %#v", got, want)
	}
}

func TestReverseTargetSelectionComment(t *testing.T) {
	if got := reverseTargetSelectionComment(3, false, nil); !strings.Contains(got, "默认解析全部本地容器 3 个") || !strings.HasPrefix(got, "#") {
		t.Fatalf("default comment = %q", got)
	}
	if got := reverseTargetSelectionComment(2, true, nil); !strings.Contains(got, "运行中容器 2 个") || !strings.HasPrefix(got, "#") {
		t.Fatalf("running comment = %q", got)
	}
	if got := reverseTargetSelectionComment(1, false, []string{"api"}); got != "" {
		t.Fatalf("filtered comment = %q, want empty", got)
	}
}
