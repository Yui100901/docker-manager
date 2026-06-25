package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
)

type fakeHealthDockerService struct {
	containers []container.Summary
	inspects   map[string]container.InspectResponse
	logs       map[string]string
	allFlag    bool
}

func (f *fakeHealthDockerService) ListContainers(ctx context.Context, all bool) ([]container.Summary, error) {
	f.allFlag = all
	return f.containers, nil
}

func (f *fakeHealthDockerService) InspectContainer(ctx context.Context, id string) (container.InspectResponse, error) {
	if inspect, ok := f.inspects[id]; ok {
		return inspect, nil
	}
	return container.InspectResponse{}, nil
}

func (f *fakeHealthDockerService) ContainerLogs(ctx context.Context, id string, options container.LogsOptions) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(f.logs[id])), nil
}

func TestBuildHealthReportDetectsContainerIssues(t *testing.T) {
	fake := &fakeHealthDockerService{
		containers: []container.Summary{
			{
				ID:     "api-container",
				Names:  []string{"/api"},
				Image:  "demo/api:latest",
				State:  "running",
				Status: "Up",
				Ports: []container.Port{
					{IP: "0.0.0.0", PublicPort: 8080, PrivatePort: 80, Type: "tcp"},
				},
			},
			{
				ID:     "worker-container",
				Names:  []string{"/worker"},
				Image:  "demo/worker:latest",
				State:  "exited",
				Status: "Exited",
			},
		},
		inspects: map[string]container.InspectResponse{
			"api-container": {
				ContainerJSONBase: &container.ContainerJSONBase{
					ID:           "api-container",
					Name:         "/api",
					RestartCount: 5,
					State: &container.State{
						Status: "running",
						Health: &container.Health{
							Status:        "unhealthy",
							FailingStreak: 2,
						},
					},
				},
				Config: &container.Config{Image: "demo/api:latest", Tty: true},
			},
			"worker-container": {
				ContainerJSONBase: &container.ContainerJSONBase{
					ID:   "worker-container",
					Name: "/worker",
					State: &container.State{
						Status:    "exited",
						ExitCode:  137,
						OOMKilled: true,
					},
				},
				Config: &container.Config{Image: "demo/worker:latest", Tty: true},
			},
		},
		logs: map[string]string{
			"api-container":    "ok\npanic: failed to connect\n",
			"worker-container": "killed by oom\n",
		},
	}

	report := buildHealthReport(context.Background(), fake, fake.containers, HealthOptions{
		LogTail:          100,
		RestartThreshold: 3,
		Keywords:         []string{"panic", "oom"},
	})

	if report.Summary.Total != 2 || report.Summary.Running != 1 || report.Summary.Stopped != 1 {
		t.Fatalf("Summary = %#v, want total=2 running=1 stopped=1", report.Summary)
	}
	for _, riskType := range []string{"unhealthy", "restart-count", "public-port", "oom-killed", "non-zero-exit", "log-keyword"} {
		if !hasHealthIssue(report, riskType) {
			t.Fatalf("Issues = %#v, want %s", report.Issues, riskType)
		}
	}
	if report.Summary.PublicBindings != 1 {
		t.Fatalf("PublicBindings = %d, want 1", report.Summary.PublicBindings)
	}
}

func TestRunHealthReportRunningOnlyPassesContainerListFlag(t *testing.T) {
	fake := &fakeHealthDockerService{}
	restore := replaceHealthServiceFactory(fake)
	defer restore()

	if _, err := runHealthReport(context.Background(), HealthOptions{RunningOnly: true, NoLogs: true}); err != nil {
		t.Fatalf("runHealthReport() error = %v", err)
	}
	if fake.allFlag {
		t.Fatal("ListContainers all = true, want false for running-only")
	}
}

func TestBuildHealthReportRedactsLogSecretsWhenRequested(t *testing.T) {
	fake := &fakeHealthDockerService{
		containers: []container.Summary{{
			ID:    "api",
			Names: []string{"/api"},
			State: "running",
		}},
		inspects: map[string]container.InspectResponse{
			"api": {
				ContainerJSONBase: &container.ContainerJSONBase{
					Name:  "/api",
					State: &container.State{Status: "running"},
				},
				Config: &container.Config{Image: "demo/api", Tty: true},
			},
		},
		logs: map[string]string{
			"api": "ERROR token=secret-token\n",
		},
	}

	report := buildHealthReport(context.Background(), fake, fake.containers, HealthOptions{
		LogTail:          100,
		RestartThreshold: 3,
		Keywords:         []string{"error"},
		RedactSecrets:    true,
	})

	if len(report.Containers) != 1 || len(report.Containers[0].LogMatches) != 1 {
		t.Fatalf("Containers = %#v, want one log match", report.Containers)
	}
	line := report.Containers[0].LogMatches[0].Line
	if strings.Contains(line, "secret-token") || !strings.Contains(line, "<redacted>") {
		t.Fatalf("Log line = %q, want redacted secret", line)
	}
}

func TestFindLogMatchesDeduplicatesKeywords(t *testing.T) {
	got := findLogMatches("INFO ok\nERROR panic happened\n", normalizeKeywords([]string{"panic", "error", "ERROR"}))
	if len(got) != 1 {
		t.Fatalf("findLogMatches() = %#v, want 1 match", got)
	}
	if strings.Join(got[0].Keywords, ",") != "error,panic" {
		t.Fatalf("Keywords = %#v, want error,panic", got[0].Keywords)
	}
}

func TestPrintHealthReportIncludesSummaryIssuesAndContainers(t *testing.T) {
	var out bytes.Buffer
	printHealthReport(&out, HealthReport{
		GeneratedAt: "2026-06-24T12:00:00Z",
		Summary: HealthSummary{
			Total:          1,
			Running:        1,
			PublicBindings: 1,
		},
		Issues: []HealthIssue{{Severity: "warn", Container: "api", Type: "public-port", Message: "public port binding"}},
		Containers: []HealthContainer{{
			Name:         "api",
			Image:        "demo/api",
			State:        "running",
			RestartCount: 1,
			PublicPorts:  []string{"0.0.0.0:8080->80/tcp"},
		}},
	})

	got := out.String()
	for _, want := range []string{"Docker health report", "Containers: total=1", "Issues:", "public-port", "public_port=0.0.0.0:8080->80/tcp"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output = %q, want %q", got, want)
		}
	}
}

func hasHealthIssue(report HealthReport, issueType string) bool {
	for _, issue := range report.Issues {
		if issue.Type == issueType {
			return true
		}
	}
	return false
}

func replaceHealthServiceFactory(fake *fakeHealthDockerService) func() {
	previous := newHealthDockerService
	newHealthDockerService = func() (healthDockerService, error) {
		return fake, nil
	}
	return func() {
		newHealthDockerService = previous
	}
}
