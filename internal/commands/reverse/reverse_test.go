package reverse

import (
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
)

func TestReverseRerunRequiresConfirm(t *testing.T) {
	cmd := NewReverseCommand()
	cmd.SetArgs([]string{"demo", "--rerun"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want confirmation error")
	}
	if !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("Execute() error = %q, want --confirm hint", err.Error())
	}
}

func TestReverseRerunDryRunDoesNotRequireConfirm(t *testing.T) {
	cmd := NewReverseCommand()
	cmd.SetArgs([]string{"demo", "--rerun", "--dry-run", "--reverse-type", "invalid"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want invalid type error after confirm gate")
	}
	if strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("Execute() error = %q, did not expect --confirm gate for dry-run", err.Error())
	}
	if !strings.Contains(err.Error(), "无效的输出类型") {
		t.Fatalf("Execute() error = %q, want invalid type error", err.Error())
	}
}

func TestReverseRerunConfirmPassesConfirmGate(t *testing.T) {
	cmd := NewReverseCommand()
	cmd.SetArgs([]string{"demo", "--rerun", "--confirm", "--reverse-type", "invalid"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want invalid type error after confirm gate")
	}
	if strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("Execute() error = %q, did not expect --confirm gate", err.Error())
	}
	if !strings.Contains(err.Error(), "无效的输出类型") {
		t.Fatalf("Execute() error = %q, want invalid type error", err.Error())
	}
}

func TestReverseRunningCannotCombineWithExplicitNames(t *testing.T) {
	cmd := NewReverseCommand()
	cmd.SetArgs([]string{"demo", "--running"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want --running conflict")
	}
	if !strings.Contains(err.Error(), "--running") {
		t.Fatalf("Execute() error = %q, want --running conflict", err.Error())
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
