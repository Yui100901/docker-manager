package diagnostics

import (
	"bytes"
	"context"
	"docker-manager/internal/docker"
	"io"
	"net/netip"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	mobyclient "github.com/moby/moby/client"
)

type fakeHealthDockerService struct {
	containers []container.Summary
	inspects   map[string]container.InspectResponse
	logs       map[string]string
	allFlag    bool
	logOptions []mobyclient.ContainerLogsOptions
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

func (f *fakeHealthDockerService) ContainerLogs(ctx context.Context, id string, options mobyclient.ContainerLogsOptions) (io.ReadCloser, error) {
	f.logOptions = append(f.logOptions, options)
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
				Ports: []container.PortSummary{
					{IP: netip.MustParseAddr("0.0.0.0"), PublicPort: 8080, PrivatePort: 80, Type: "tcp"},
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
				Config: &container.Config{Image: "demo/api:latest", Tty: true},
			},
			"worker-container": {
				ID:   "worker-container",
				Name: "/worker",
				State: &container.State{
					Status:    "exited",
					ExitCode:  137,
					OOMKilled: true,
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

func TestHealthCommandRemovesRunningOnlyCompatibilityFlag(t *testing.T) {
	cmd := NewHealthCommand()
	if flag := cmd.Flags().Lookup("running-only"); flag != nil {
		t.Fatal("running-only compatibility flag should be removed")
	}
	if flag := cmd.Flags().Lookup("running"); flag == nil {
		t.Fatal("running flag should remain available")
	}
}

func TestRunHealthReportFiltersContainersByWildcard(t *testing.T) {
	fake := &fakeHealthDockerService{
		containers: []container.Summary{
			{ID: "api-id", Names: []string{"/api-1"}, Image: "demo/api:latest", State: "running"},
			{ID: "db-id", Names: []string{"/db-1"}, Image: "demo/db:latest", State: "running"},
		},
		inspects: map[string]container.InspectResponse{
			"api-id": {Name: "/api-1", State: &container.State{Status: "running"}},
		},
		logs: map[string]string{"api-id": "ok\n"},
	}
	restore := replaceHealthServiceFactory(fake)
	defer restore()

	report, err := runHealthReport(context.Background(), HealthOptions{
		ContainerFilters: []string{"api-*"},
		NoLogs:           true,
	})
	if err != nil {
		t.Fatalf("runHealthReport() error = %v", err)
	}
	if len(report.Containers) != 1 || report.Containers[0].Name != "api-1" {
		t.Fatalf("Containers = %#v, want api-1", report.Containers)
	}
}

func TestBuildHealthReportIncludesResourceDependenciesFromInspect(t *testing.T) {
	fake := &fakeHealthDockerService{
		containers: []container.Summary{{
			ID:    "api-id",
			Names: []string{"/api"},
			Image: "summary/api",
			State: "running",
		}},
		inspects: map[string]container.InspectResponse{
			"api-id": {
				ID:           "api-id",
				Name:         "/api",
				Image:        "sha256:image-id",
				RestartCount: 1,
				HostConfig: &container.HostConfig{
					NetworkMode: "app_net",
					RestartPolicy: container.RestartPolicy{
						Name:              "on-failure",
						MaximumRetryCount: 5,
					},
					LogConfig: container.LogConfig{
						Type:   "json-file",
						Config: map[string]string{"max-size": "10m"},
					},
				},
				State: &container.State{Status: container.StateRunning},
				Config: &container.Config{
					Image:        "demo/api:latest",
					ExposedPorts: network.PortSet{network.MustParsePort("443/tcp"): struct{}{}},
				},
				NetworkSettings: &container.NetworkSettings{
					Ports: network.PortMap{
						network.MustParsePort("80/tcp"): []network.PortBinding{{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: "8080"}},
					},
					Networks: map[string]*network.EndpointSettings{
						"app_net": {
							NetworkID:  "network-id",
							EndpointID: "endpoint-id",
							IPAddress:  netip.MustParseAddr("172.20.0.2"),
							Gateway:    netip.MustParseAddr("172.20.0.1"),
							Aliases:    []string{"api"},
						},
					},
				},
				Mounts: []container.MountPoint{
					{Type: mount.TypeVolume, Name: "api_data", Destination: "/data", RW: true},
					{Type: mount.TypeBind, Source: "/host/config", Destination: "/config", Mode: "ro", RW: false},
				},
			},
		},
		logs: map[string]string{"api-id": "ok\n"},
	}

	report := buildHealthReport(context.Background(), fake, fake.containers, HealthOptions{
		NoLogs:           true,
		RestartThreshold: 3,
	})

	if len(report.Containers) != 1 {
		t.Fatalf("Containers = %#v, want one container", report.Containers)
	}
	item := report.Containers[0]
	if item.ImageID != "image-id" || item.RestartPolicy != "on-failure:5" || item.LogDriver != "json-file" || item.LogOptions["max-size"] != "10m" || item.NetworkMode != "app_net" {
		t.Fatalf("container = %#v, want image/log/restart/network context", item)
	}
	if len(item.Networks) != 1 || item.Networks[0].Name != "app_net" || item.Networks[0].IPAddress != "172.20.0.2" || item.Networks[0].Gateway != "172.20.0.1" {
		t.Fatalf("Networks = %#v, want app_net inspect context", item.Networks)
	}
	if len(item.Mounts) != 2 || item.Mounts[0].Destination != "/config" || item.Mounts[1].Name != "api_data" {
		t.Fatalf("Mounts = %#v, want sorted bind and volume mounts", item.Mounts)
	}
	if len(item.PublicPorts) != 1 || item.PublicPorts[0] != "0.0.0.0:8080->80/tcp" {
		t.Fatalf("PublicPorts = %#v, want published public port", item.PublicPorts)
	}
	if len(item.ExposedPorts) != 1 || item.ExposedPorts[0] != "443/tcp" {
		t.Fatalf("ExposedPorts = %#v, want exposed-only 443/tcp", item.ExposedPorts)
	}
	if report.Summary.PublicBindings != 1 || !hasHealthIssue(report, "public-port") {
		t.Fatalf("Summary=%#v Issues=%#v, want public port issue", report.Summary, report.Issues)
	}
}

func TestBuildHealthReportReportsUnsupportedLogDriver(t *testing.T) {
	fake := &fakeHealthDockerService{
		containers: []container.Summary{{
			ID:    "api-id",
			Names: []string{"/api"},
			Image: "demo/api",
			State: "running",
		}},
		inspects: map[string]container.InspectResponse{
			"api-id": {
				Name: "/api",
				HostConfig: &container.HostConfig{
					LogConfig: container.LogConfig{Type: "awslogs"},
				},
				State:  &container.State{Status: container.StateRunning},
				Config: &container.Config{Image: "demo/api"},
			},
		},
		logs: map[string]string{"api-id": "ERROR should not be read\n"},
	}

	report := buildHealthReport(context.Background(), fake, fake.containers, HealthOptions{
		LogTail:          100,
		RestartThreshold: 3,
		Keywords:         []string{"error"},
	})

	if len(fake.logOptions) != 0 {
		t.Fatalf("ContainerLogs called %#v, want skipped for awslogs", fake.logOptions)
	}
	if report.Summary.LogsUnavailable != 1 || !hasHealthIssue(report, "logs-unavailable") {
		t.Fatalf("Summary=%#v Issues=%#v, want logs-unavailable", report.Summary, report.Issues)
	}
	if len(report.Containers) != 1 || report.Containers[0].LogDriver != "awslogs" || report.Containers[0].LogReadability != "unsupported" {
		t.Fatalf("Containers = %#v, want unsupported awslogs", report.Containers)
	}
	if !strings.Contains(report.Containers[0].LogReadabilityMessage, "awslogs") {
		t.Fatalf("LogReadabilityMessage = %q, want awslogs reason", report.Containers[0].LogReadabilityMessage)
	}
}

func TestRunHealthReportIncludesDockerEndpoint(t *testing.T) {
	t.Cleanup(func() { docker.Configure(docker.Options{}) })
	docker.Configure(docker.Options{Host: "tcp://docker.example:2375"})
	fake := &fakeHealthDockerService{
		containers: []container.Summary{{ID: "api-id", Names: []string{"/api"}, State: "running"}},
		inspects: map[string]container.InspectResponse{
			"api-id": {Name: "/api", State: &container.State{Status: "running"}},
		},
	}
	restore := replaceHealthServiceFactory(fake)
	defer restore()

	report, err := runHealthReport(context.Background(), HealthOptions{NoLogs: true})
	if err != nil {
		t.Fatalf("runHealthReport() error = %v", err)
	}
	if report.DockerEndpoint != "tcp://docker.example:2375" {
		t.Fatalf("DockerEndpoint = %q, want remote endpoint", report.DockerEndpoint)
	}
	var out bytes.Buffer
	printHealthReport(&out, report)
	if !strings.Contains(out.String(), "来源 Docker: tcp://docker.example:2375") {
		t.Fatalf("health text output = %q, want docker endpoint", out.String())
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
				Name:   "/api",
				State:  &container.State{Status: "running"},
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
	for _, want := range []string{"Docker 体检报告", "容器: 总数=1", "问题:", "public-port", "公网端口=0.0.0.0:8080->80/tcp"} {
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
