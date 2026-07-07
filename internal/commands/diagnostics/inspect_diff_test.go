package diagnostics

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/strslice"
)

type fakeInspectDiffDockerService struct {
	inspects map[string]container.InspectResponse
	calls    []string
}

func (f *fakeInspectDiffDockerService) InspectContainer(ctx context.Context, name string) (container.InspectResponse, error) {
	f.calls = append(f.calls, name)
	return f.inspects[name], nil
}

func TestBuildInspectDiffReportComparesKeyFieldsAndShowsSecretsByDefault(t *testing.T) {
	left := inspectDiffFixture("nginx:1.25", []string{"MODE=prod", "PASSWORD=left"}, []string{"NET_ADMIN"})
	right := inspectDiffFixture("nginx:1.26", []string{"MODE=debug", "PASSWORD=right"}, []string{"SYS_TIME"})

	report := buildInspectDiffReport("left", "right", left, right, InspectDiffOptions{})
	if !hasInspectDiffChange(report, "config.image", `"nginx:1.25"`, `"nginx:1.26"`) {
		t.Fatalf("Changed = %#v, want config.image change", report.Changed)
	}
	envChange := findInspectDiffChange(report, "config.env")
	if envChange == nil {
		t.Fatalf("Changed = %#v, want config.env change", report.Changed)
	}
	if !strings.Contains(envChange.Left, "left") || !strings.Contains(envChange.Right, "right") {
		t.Fatalf("env diff = %#v, want visible secrets by default", envChange)
	}
	if !hasInspectDiffChange(report, "host.cap_add", `["NET_ADMIN"]`, `["SYS_TIME"]`) {
		t.Fatalf("Changed = %#v, want cap_add change", report.Changed)
	}
}

func TestBuildInspectDiffReportCanRedactSecrets(t *testing.T) {
	left := inspectDiffFixture("nginx:1.25", []string{"MODE=left", "PASSWORD=alpha-secret"}, nil)
	right := inspectDiffFixture("nginx:1.25", []string{"MODE=right", "PASSWORD=beta-secret"}, nil)

	report := buildInspectDiffReport("left", "right", left, right, InspectDiffOptions{RedactSecrets: true})
	envChange := findInspectDiffChange(report, "config.env")
	if envChange == nil {
		t.Fatal("config.env change not found")
	}
	if strings.Contains(envChange.Left, "alpha-secret") || strings.Contains(envChange.Right, "beta-secret") {
		t.Fatalf("env diff leaked secret: %#v", envChange)
	}
	if !strings.Contains(envChange.Left, "<redacted>") || !strings.Contains(envChange.Right, "<redacted>") {
		t.Fatalf("env diff = %#v, want redacted secret", envChange)
	}
}

func TestBuildInspectDiffReportRedactsSensitiveStringsOutsideEnvAndLabels(t *testing.T) {
	left := inspectDiffFixture("busybox:1", []string{"MODE=left"}, nil)
	left.Config.Cmd = strslice.StrSlice{"sh", "-c", "echo password=cmd-alpha"}
	left.Config.Entrypoint = strslice.StrSlice{"/bin/app", "--token=entry-alpha"}
	left.Config.Healthcheck = &container.HealthConfig{
		Test: []string{"CMD-SHELL", "curl -H 'Authorization: Bearer health-alpha' http://localhost/health"},
	}
	left.Config.Labels = map[string]string{
		"owner":     "team-a",
		"api_token": "label-alpha",
		"note":      "url=https://user:label-pass@example.com/path",
	}
	left.HostConfig.Binds = []string{"/host/password=bind-alpha:/data"}
	left.HostConfig.LogConfig = container.LogConfig{
		Type: "json-file",
		Config: map[string]string{
			"token":    "log-alpha",
			"max-size": "10m",
		},
	}

	right := inspectDiffFixture("busybox:1", []string{"MODE=right"}, nil)
	right.Config.Cmd = strslice.StrSlice{"sh", "-c", "echo password=cmd-beta"}
	right.Config.Entrypoint = strslice.StrSlice{"/bin/app", "--token=entry-beta"}
	right.Config.Healthcheck = &container.HealthConfig{
		Test: []string{"CMD-SHELL", "curl -H 'Authorization: Bearer health-beta' http://localhost/health"},
	}
	right.Config.Labels = map[string]string{
		"owner":     "team-b",
		"api_token": "label-beta",
		"note":      "url=https://user:label-pass-beta@example.com/path",
	}
	right.HostConfig.Binds = []string{"/host/password=bind-beta:/data"}
	right.HostConfig.LogConfig = container.LogConfig{
		Type: "json-file",
		Config: map[string]string{
			"token":    "log-beta",
			"max-size": "20m",
		},
	}

	report := buildInspectDiffReport("left", "right", left, right, InspectDiffOptions{RedactSecrets: true})
	joined := inspectDiffReportText(report)
	for _, leaked := range []string{
		"cmd-alpha", "cmd-beta",
		"entry-alpha", "entry-beta",
		"health-alpha", "health-beta",
		"label-alpha", "label-beta",
		"label-pass", "label-pass-beta",
		"bind-alpha", "bind-beta",
		"log-alpha", "log-beta",
	} {
		if strings.Contains(joined, leaked) {
			t.Fatalf("redacted inspect diff leaked %q:\n%s", leaked, joined)
		}
	}
	if strings.Count(joined, "<redacted>") < 6 {
		t.Fatalf("redacted inspect diff = %s, want multiple redacted markers", joined)
	}
}

func TestRunInspectDiffInspectsBothContainers(t *testing.T) {
	fake := &fakeInspectDiffDockerService{
		inspects: map[string]container.InspectResponse{
			"a": inspectDiffFixture("busybox:1", nil, nil),
			"b": inspectDiffFixture("busybox:2", nil, nil),
		},
	}
	restore := replaceInspectDiffServiceFactory(fake)
	defer restore()

	report, err := runInspectDiff(context.Background(), "a", "b", InspectDiffOptions{})
	if err != nil {
		t.Fatalf("runInspectDiff() error = %v", err)
	}
	if strings.Join(fake.calls, ",") != "a,b" {
		t.Fatalf("calls = %#v, want a,b", fake.calls)
	}
	if report.LeftName != "a" || report.RightName != "b" {
		t.Fatalf("report names = %s -> %s, want a -> b", report.LeftName, report.RightName)
	}
}

func TestPrintInspectDiffReportIncludesSummaryAndChangedFields(t *testing.T) {
	var out bytes.Buffer
	printInspectDiffReport(&out, InspectDiffReport{
		LeftName:  "a",
		RightName: "b",
		Changed: []InspectDiffEntry{{
			Path:  "config.image",
			Left:  `"busybox:1"`,
			Right: `"busybox:2"`,
		}},
	})

	got := out.String()
	for _, want := range []string{"容器 inspect 差异: a -> b", "摘要: 变更=1", "变更:", "config.image"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output = %q, want %q", got, want)
		}
	}
}

func inspectDiffFixture(image string, env []string, capAdd []string) container.InspectResponse {
	return container.InspectResponse{
		Name: "/demo",
		HostConfig: &container.HostConfig{
			CapAdd: capAdd,
		},
		Config: &container.Config{
			Image:  image,
			Env:    env,
			Labels: map[string]string{},
		},
	}
}

func inspectDiffReportText(report InspectDiffReport) string {
	var parts []string
	for _, entry := range report.Added {
		parts = append(parts, entry.Path, entry.Left, entry.Right)
	}
	for _, entry := range report.Removed {
		parts = append(parts, entry.Path, entry.Left, entry.Right)
	}
	for _, entry := range report.Changed {
		parts = append(parts, entry.Path, entry.Left, entry.Right)
	}
	return strings.Join(parts, "\n")
}

func hasInspectDiffChange(report InspectDiffReport, path, left, right string) bool {
	change := findInspectDiffChange(report, path)
	return change != nil && change.Left == left && change.Right == right
}

func findInspectDiffChange(report InspectDiffReport, path string) *InspectDiffEntry {
	for i := range report.Changed {
		if report.Changed[i].Path == path {
			return &report.Changed[i]
		}
	}
	return nil
}

func replaceInspectDiffServiceFactory(fake *fakeInspectDiffDockerService) func() {
	previous := newInspectDiffDockerService
	newInspectDiffDockerService = func() (inspectDiffDockerService, error) {
		return fake, nil
	}
	return func() {
		newInspectDiffDockerService = previous
	}
}
