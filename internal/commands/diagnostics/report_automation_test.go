package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	rpt "docker-manager/internal/report"

	"github.com/moby/moby/api/types/container"
	"github.com/spf13/cobra"
)

func TestHealthAutomationDataUsesStableIssueRulesAndMetrics(t *testing.T) {
	data := healthAutomationData(HealthReport{
		GeneratedAt:    "2026-08-27T00:00:00Z",
		DockerEndpoint: "unix:///var/run/docker.sock",
		Summary:        HealthSummary{Total: 2, Running: 1, Unhealthy: 1, PublicBindings: 1},
		Containers:     []HealthContainer{{Name: "api", ID: "abc"}},
		Issues: []HealthIssue{
			{Severity: "error", Container: "api", Type: "unhealthy", Message: "bad"},
			{Severity: "warn", Container: "api", Type: "public-port", Message: "public"},
		},
	})
	if data.metrics["unhealthy"] != 1 || data.metrics["issues"] != 2 || data.metrics["error_issues"] != 1 || data.metrics["warning_issues"] != 1 {
		t.Fatalf("metrics = %#v", data.metrics)
	}
	if len(data.findings) != 2 || data.findings[0].RuleID != "dm.health.unhealthy" || data.findings[1].RuleID != "dm.health.public-port" {
		t.Fatalf("findings = %#v, want sorted stable rules", data.findings)
	}
	if data.findings[1].Resource.ID != "abc" {
		t.Fatalf("finding resource = %#v", data.findings[1].Resource)
	}
}

func TestLogsNetworkVolumeAndPruneAutomationDataExposeTypedRules(t *testing.T) {
	logs := logsAutomationData(LogsScanReport{
		DockerEndpoint: "docker",
		Summary:        LogsScanSummary{ScannedContainers: 1, ContainersMatched: 1, TotalMatches: 1, Errors: 1, LogsUnavailable: 1},
		Containers:     []LogsScanContainer{{Name: "api", ID: "abc", ErrorType: "logs-unavailable", Error: "driver unavailable", Matches: []LogScanMatch{{LineNumber: 4, Line: "ERROR", Keywords: []string{"error"}}}}},
	})
	if logs.metrics["total_matches"] != 1 || len(logs.findings) != 2 {
		t.Fatalf("logs = %#v", logs)
	}
	if logs.findings[0].RuleID != "dm.logs.logs-unavailable" || logs.findings[1].RuleID != "dm.logs.keyword-match" {
		t.Fatalf("logs rules = %#v", logs.findings)
	}

	network := networkAutomationData(NetworkReport{
		DockerEndpoint: "docker",
		GeneratedAt:    "2026-08-27T00:00:00Z",
		Networks:       []NetworkRef{{Name: "net"}},
		Containers:     []NetworkContainerRef{{Name: "api"}},
		Ports:          []PortMappingRef{{Container: "api", Published: true}},
		Risks:          []NetworkRisk{{Type: "public-bind", Containers: []string{"api"}}, {Type: "port-conflict", Containers: []string{"api", "web"}}},
		Warnings:       []string{"inspect fallback"},
	})
	if network.metrics["published_ports"] != 1 || network.metrics["public_bind_risks"] != 1 || network.metrics["port_conflicts"] != 1 || network.metrics["warnings"] != 1 {
		t.Fatalf("network metrics = %#v", network.metrics)
	}
	if network.generatedAt != "2026-08-27T00:00:00Z" {
		t.Fatalf("network generatedAt = %q", network.generatedAt)
	}

	volume := volumeAutomationData(VolumeReport{
		DockerEndpoint: "docker",
		GeneratedAt:    "2026-08-27T00:00:00Z",
		Summary:        VolumeSummary{Total: 2, Unused: 1, UnknownSize: 1, ReclaimableSize: 1024},
		Volumes:        []VolumeRef{{Name: "cache", Status: "unused", Size: -1, SizeError: "probe failed"}},
		Warnings:       []string{"volume cache 大小探测失败: probe failed"},
	})
	if volume.metrics["reclaimable_size"] != 1024 || volume.metrics["size_errors"] != 1 || len(volume.findings) < 2 {
		t.Fatalf("volume = %#v metrics=%#v", volume.findings, volume.metrics)
	}
	if volume.generatedAt != "2026-08-27T00:00:00Z" {
		t.Fatalf("volume generatedAt = %q", volume.generatedAt)
	}

	prune := pruneAutomationData(PruneReport{
		DockerEndpoint:    "docker",
		EstimatedBytes:    2048,
		StoppedContainers: []PruneContainerRef{{ID: "c1", Name: "old"}},
		DanglingImages:    []PruneImageRef{{ID: "i1", Size: 10}},
		UnusedVolumes:     []PruneVolumeRef{{Name: "v1", Size: 20}},
		BuildCaches:       []PruneBuildCacheRef{{ID: "b1", Size: 30}},
	})
	if prune.metrics["candidates"] != 4 || prune.metrics["estimated_bytes"] != 2048 || len(prune.findings) != 4 {
		t.Fatalf("prune = %#v metrics=%#v", prune.findings, prune.metrics)
	}
}

func TestReportAllAutomationUsesNamespacedMetrics(t *testing.T) {
	data := reportAllAutomationData(ReportAllReport{
		DockerEndpoint: "docker",
		Health:         &HealthReport{Summary: HealthSummary{Unhealthy: 2}},
		Network:        &NetworkReport{Risks: []NetworkRisk{{Type: "public-bind"}}},
		Sections:       []ReportAllSection{{Name: "health", Status: "ok"}},
	})
	if data.metrics["health.unhealthy"] != 2 || data.metrics["network.risks"] != 1 {
		t.Fatalf("namespaced metrics = %#v", data.metrics)
	}
	definitions := reportAllMetricDefinitions()
	policy, err := rpt.ParsePolicy("none", []string{"health.unhealthy=1"}, definitions)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation := automationEvaluation(policy, data, true); evaluation.Status != "fail" {
		t.Fatalf("evaluation status = %s, want fail", evaluation.Status)
	}
}

func TestAutomationFlagsRejectInvalidThresholdBeforeDocker(t *testing.T) {
	called := 0
	original := newHealthDockerService
	newHealthDockerService = func() (healthDockerService, error) {
		called++
		return nil, errors.New("should not be called")
	}
	t.Cleanup(func() { newHealthDockerService = original })
	cmd := NewHealthCommand()
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--threshold=unknown=1"})
	if err := cmd.Execute(); err == nil || called != 0 {
		t.Fatalf("Execute() error=%v dockerCalls=%d, want validation before Docker", err, called)
	}
}

func TestHealthCommandReturnsTypedGateErrorAndJSONEvaluation(t *testing.T) {
	original := newHealthDockerService
	newHealthDockerService = func() (healthDockerService, error) {
		return &fakeHealthDockerService{
			containers: []container.Summary{{ID: "api", Names: []string{"/api"}, State: "running"}},
			inspects:   map[string]container.InspectResponse{"api": {ID: "api", Name: "/api", State: &container.State{Status: "running", Health: &container.Health{Status: "unhealthy"}}}},
		}, nil
	}
	t.Cleanup(func() { newHealthDockerService = original })
	var stdout bytes.Buffer
	cmd := NewHealthCommand()
	cmd.SilenceUsage = true
	cmd.SetOut(&stdout)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--format=json", "--fail-on=error"})
	err := cmd.ExecuteContext(context.Background())
	var gate *rpt.GateError
	if !errors.As(err, &gate) {
		t.Fatalf("Execute() error=%v, want GateError", err)
	}
	var decoded HealthReport
	if jsonErr := json.Unmarshal(stdout.Bytes(), &decoded); jsonErr != nil {
		t.Fatalf("JSON output invalid: %v\n%s", jsonErr, stdout.String())
	}
	if decoded.Evaluation == nil || decoded.Evaluation.Status != "fail" {
		t.Fatalf("evaluation = %#v", decoded.Evaluation)
	}
	if strings.Contains(stdout.String(), "自动化门禁") {
		t.Fatal("JSON output contains text gate trailer")
	}
}

func TestAutomationFlagPresenceForAllCommands(t *testing.T) {
	commands := []*cobra.Command{
		NewHealthCommand(), NewLogsScanCommand(), NewNetworkCommand(), NewVolumesReportCommand(), NewPruneReportCommand(), NewReportAllCommand(),
	}
	for _, cmd := range commands {
		for _, flag := range []string{"format", "fail-on", "threshold"} {
			if cmd.Flags().Lookup(flag) == nil {
				t.Errorf("%s missing --%s", cmd.Name(), flag)
			}
		}
	}
}
