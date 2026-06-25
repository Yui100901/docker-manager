package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
)

type fakeLogsScanDockerService struct {
	containers []container.Summary
	inspects   map[string]container.InspectResponse
	logs       map[string]string
	allFlag    bool
	logOptions []container.LogsOptions
}

func (f *fakeLogsScanDockerService) ListContainers(ctx context.Context, all bool) ([]container.Summary, error) {
	f.allFlag = all
	return f.containers, nil
}

func (f *fakeLogsScanDockerService) InspectContainer(ctx context.Context, id string) (container.InspectResponse, error) {
	if inspect, ok := f.inspects[id]; ok {
		return inspect, nil
	}
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{Name: "/" + id},
		Config:            &container.Config{Image: "busybox", Tty: true},
	}, nil
}

func (f *fakeLogsScanDockerService) ContainerLogs(ctx context.Context, id string, options container.LogsOptions) (io.ReadCloser, error) {
	f.logOptions = append(f.logOptions, options)
	return io.NopCloser(strings.NewReader(f.logs[id])), nil
}

func TestFindLogScanMatchesIncludesContext(t *testing.T) {
	matches := findLogScanMatches("one\ntwo before\npanic happened\nafter\nlast\n", []string{"panic"}, 1)
	if len(matches) != 1 {
		t.Fatalf("matches = %#v, want 1", matches)
	}
	if matches[0].LineNumber != 3 || matches[0].Line != "panic happened" {
		t.Fatalf("match = %#v, want line 3", matches[0])
	}
	if strings.Join(matches[0].Before, ",") != "two before" {
		t.Fatalf("Before = %#v, want two before", matches[0].Before)
	}
	if strings.Join(matches[0].After, ",") != "after" {
		t.Fatalf("After = %#v, want after", matches[0].After)
	}
}

func TestBuildLogsScanReportScansExplicitContainers(t *testing.T) {
	fake := &fakeLogsScanDockerService{
		inspects: map[string]container.InspectResponse{
			"api": {
				ContainerJSONBase: &container.ContainerJSONBase{
					Name:  "/api",
					ID:    "api-id",
					State: &container.State{Status: "running"},
				},
				Config: &container.Config{Image: "demo/api", Tty: true},
			},
		},
		logs: map[string]string{
			"api": "ok\nERROR failed\n",
		},
	}

	report := buildLogsScanReport(context.Background(), fake, []container.Summary{{ID: "api", Names: []string{"/api"}}}, LogsScanOptions{
		Tail:     100,
		Context:  0,
		Keywords: []string{"error"},
	})

	if report.Summary.ScannedContainers != 1 || report.Summary.TotalMatches != 1 || report.Summary.ContainersMatched != 1 {
		t.Fatalf("Summary = %#v, want one match", report.Summary)
	}
	if len(report.Containers) != 1 || report.Containers[0].Name != "api" || len(report.Containers[0].Matches) != 1 {
		t.Fatalf("Containers = %#v, want api with one match", report.Containers)
	}
}

func TestBuildLogsScanReportRedactsSecretsWhenRequested(t *testing.T) {
	fake := &fakeLogsScanDockerService{
		logs: map[string]string{
			"api": "before token=abc123\nERROR password=super-secret\nnext Authorization: Bearer token-value\n",
		},
	}

	report := buildLogsScanReport(context.Background(), fake, []container.Summary{{ID: "api", Names: []string{"/api"}}}, LogsScanOptions{
		Tail:          100,
		Context:       1,
		Keywords:      []string{"error"},
		RedactSecrets: true,
	})

	if len(report.Containers) != 1 || len(report.Containers[0].Matches) != 1 {
		t.Fatalf("Containers = %#v, want one match", report.Containers)
	}
	match := report.Containers[0].Matches[0]
	joined := strings.Join(append(append([]string{}, match.Before...), append([]string{match.Line}, match.After...)...), "\n")
	for _, leaked := range []string{"abc123", "super-secret", "token-value"} {
		if strings.Contains(joined, leaked) {
			t.Fatalf("redacted log output leaked %q: %q", leaked, joined)
		}
	}
	if !strings.Contains(joined, "<redacted>") {
		t.Fatalf("redacted log output = %q, want <redacted>", joined)
	}
}

func TestLogsScanTargetsRunningOnlyFiltersContainers(t *testing.T) {
	fake := &fakeLogsScanDockerService{
		containers: []container.Summary{
			{ID: "api", Names: []string{"/api"}, State: "running"},
			{ID: "old", Names: []string{"/old"}, State: "exited"},
		},
	}
	targets, err := logsScanTargets(context.Background(), fake, LogsScanOptions{RunningOnly: true})
	if err != nil {
		t.Fatalf("logsScanTargets() error = %v", err)
	}
	if fake.allFlag {
		t.Fatal("ListContainers all = true, want false for running-only")
	}
	if len(targets) != 1 || firstContainerName(targets[0].Names) != "api" {
		t.Fatalf("targets = %#v, want api", targets)
	}
}

func TestReadContainerLogTextPassesTailAndSince(t *testing.T) {
	fake := &fakeLogsScanDockerService{
		logs: map[string]string{"api": "error\n"},
	}
	_, err := readContainerLogText(context.Background(), fake, "api", container.InspectResponse{
		Config: &container.Config{Tty: true},
	}, LogsScanOptions{Tail: 10, Since: "1h"})
	if err != nil {
		t.Fatalf("readContainerLogText() error = %v", err)
	}
	if len(fake.logOptions) != 1 || fake.logOptions[0].Tail != "10" || fake.logOptions[0].Since == "" {
		t.Fatalf("logOptions = %#v, want tail=10 and since set", fake.logOptions)
	}
}

func TestLogsScanTargetsFiltersByWildcard(t *testing.T) {
	fake := &fakeLogsScanDockerService{
		containers: []container.Summary{
			{ID: "api-id", Names: []string{"/api-1"}, Image: "demo/api:latest", State: "running"},
			{ID: "db-id", Names: []string{"/db-1"}, Image: "demo/db:latest", State: "exited"},
		},
	}
	targets, err := logsScanTargets(context.Background(), fake, LogsScanOptions{Filters: []string{"api-*"}})
	if err != nil {
		t.Fatalf("logsScanTargets() error = %v", err)
	}
	if !fake.allFlag {
		t.Fatal("ListContainers all = false, want true when filtering explicit targets")
	}
	if len(targets) != 1 || firstContainerName(targets[0].Names) != "api-1" {
		t.Fatalf("targets = %#v, want api-1", targets)
	}
}

func TestValidateLogsScanArgsRejectsInvalidCombinations(t *testing.T) {
	tests := []struct {
		name string
		opts LogsScanOptions
	}{
		{name: "missing target", opts: LogsScanOptions{Tail: 1}},
		{name: "all and running", opts: LogsScanOptions{All: true, RunningOnly: true, Tail: 1}},
		{name: "bad context", opts: LogsScanOptions{Filters: []string{"api"}, Tail: 1, Context: -1}},
		{name: "bad tail", opts: LogsScanOptions{Filters: []string{"api"}, Tail: 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateLogsScanArgs(tt.opts); err == nil {
				t.Fatal("validateLogsScanArgs() error = nil, want error")
			}
		})
	}
}

func TestValidateLogsScanArgsAllowsFiltersWithRunningOnly(t *testing.T) {
	if err := validateLogsScanArgs(LogsScanOptions{Filters: []string{"api*"}, RunningOnly: true, Tail: 1}); err != nil {
		t.Fatalf("validateLogsScanArgs() error = %v, want nil", err)
	}
}

func TestPrintLogsScanReportIncludesMatches(t *testing.T) {
	var out bytes.Buffer
	printLogsScanReport(&out, LogsScanReport{
		GeneratedAt: "2026-06-24T12:00:00Z",
		Keywords:    []string{"error"},
		Summary:     LogsScanSummary{ScannedContainers: 1, ContainersMatched: 1, TotalMatches: 1},
		Containers: []LogsScanContainer{{
			Name:  "api",
			Image: "demo/api",
			State: "running",
			Matches: []LogScanMatch{{
				LineNumber: 2,
				Line:       "ERROR failed",
				Keywords:   []string{"error"},
				Before:     []string{"before"},
				After:      []string{"after"},
			}},
		}},
	})
	got := out.String()
	for _, want := range []string{"Docker 日志扫描", "摘要: 已扫描=1", "第 2 行 [error] ERROR failed", "| before", "| after"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output = %q, want %q", got, want)
		}
	}
}
