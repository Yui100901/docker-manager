package diagnostics

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"docker-manager/internal/commandflags"
	rpt "docker-manager/internal/report"
)

type automationData struct {
	command        string
	dockerEndpoint string
	generatedAt    string
	findings       []rpt.Finding
	metrics        map[string]uint64
}

var (
	healthMetricDefinitions  = rpt.MetricDefinitions(rpt.MetricScopeHealth)
	networkMetricDefinitions = rpt.MetricDefinitions(rpt.MetricScopeNetwork)
	logsMetricDefinitions    = rpt.MetricDefinitions(rpt.MetricScopeLogs)
	volumeMetricDefinitions  = rpt.MetricDefinitions(rpt.MetricScopeVolumes)
	pruneMetricDefinitions   = rpt.MetricDefinitions(rpt.MetricScopePrune)
)

func prepareReportAutomation(format string, options commandflags.AutomationOptions, definitions []rpt.MetricDefinition) (rpt.Policy, error) {
	if err := rpt.ValidateFormat(format, true); err != nil {
		return rpt.Policy{}, err
	}
	return rpt.ParsePolicy(options.FailOn, options.Thresholds, definitions)
}

func automationMetricNames(definitions []rpt.MetricDefinition) []string {
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Name)
	}
	sort.Strings(names)
	return names
}

func zeroMetricMap(definitions []rpt.MetricDefinition) map[string]uint64 {
	metrics := make(map[string]uint64, len(definitions))
	for _, definition := range definitions {
		metrics[definition.Name] = 0
	}
	return metrics
}

func automationEvaluation(policy rpt.Policy, data automationData, executionSuccessful bool) *rpt.Evaluation {
	return policy.Evaluate(rpt.EvaluationInput{
		Command:             data.command,
		DockerEndpoint:      data.dockerEndpoint,
		GeneratedAt:         data.generatedAt,
		ExecutionSuccessful: executionSuccessful,
		Findings:            data.findings,
		Metrics:             data.metrics,
	})
}

func exposeAutomationEvaluation(format string, policy rpt.Policy) bool {
	return format == rpt.FormatSARIF || policy.Active()
}

func healthAutomationData(report HealthReport) automationData {
	metrics := zeroMetricMap(healthMetricDefinitions)
	metrics["total"] = nonNegativeInt(report.Summary.Total)
	metrics["running"] = nonNegativeInt(report.Summary.Running)
	metrics["stopped"] = nonNegativeInt(report.Summary.Stopped)
	metrics["restarting"] = nonNegativeInt(report.Summary.Restarting)
	metrics["unhealthy"] = nonNegativeInt(report.Summary.Unhealthy)
	metrics["restart_warnings"] = nonNegativeInt(report.Summary.RestartWarnings)
	metrics["log_warnings"] = nonNegativeInt(report.Summary.LogWarnings)
	metrics["logs_unavailable"] = nonNegativeInt(report.Summary.LogsUnavailable)
	metrics["public_bindings"] = nonNegativeInt(report.Summary.PublicBindings)
	metrics["issues"] = uint64(len(report.Issues))
	containerIDs := make(map[string]string, len(report.Containers))
	for _, item := range report.Containers {
		containerIDs[item.Name] = item.ID
	}
	findings := make([]rpt.Finding, 0, len(report.Issues))
	for _, issue := range report.Issues {
		level := diagnosticLevel(issue.Severity)
		if level == rpt.LevelError {
			metrics["error_issues"]++
		} else {
			metrics["warning_issues"]++
		}
		findings = append(findings, rpt.Finding{
			RuleID:   "dm.health." + ruleComponent(issue.Type),
			Level:    level,
			Message:  issue.Message,
			Resource: rpt.ResourceRef{Kind: "container", Name: issue.Container, ID: containerIDs[issue.Container]},
			Identity: issue.Type,
		})
	}
	return automationData{
		command:        "health",
		dockerEndpoint: report.DockerEndpoint,
		generatedAt:    report.GeneratedAt,
		findings:       findings,
		metrics:        metrics,
	}
}

func logsAutomationData(report LogsScanReport) automationData {
	metrics := zeroMetricMap(logsMetricDefinitions)
	metrics["scanned_containers"] = nonNegativeInt(report.Summary.ScannedContainers)
	metrics["containers_matched"] = nonNegativeInt(report.Summary.ContainersMatched)
	metrics["total_matches"] = nonNegativeInt(report.Summary.TotalMatches)
	metrics["errors"] = nonNegativeInt(report.Summary.Errors)
	metrics["logs_unavailable"] = nonNegativeInt(report.Summary.LogsUnavailable)
	var findings []rpt.Finding
	for _, item := range report.Containers {
		resource := rpt.ResourceRef{Kind: "container", Name: item.Name, ID: item.ID}
		if item.Error != "" {
			kind := item.ErrorType
			if kind == "" {
				kind = "scan-failed"
			}
			level := rpt.LevelError
			if kind == "logs-unavailable" {
				level = rpt.LevelWarning
			}
			findings = append(findings, rpt.Finding{
				RuleID:   "dm.logs." + ruleComponent(kind),
				Level:    level,
				Message:  item.Error,
				Resource: resource,
				Identity: kind,
			})
		}
		for _, match := range item.Matches {
			findings = append(findings, rpt.Finding{
				RuleID:   "dm.logs.keyword-match",
				Level:    rpt.LevelWarning,
				Message:  match.Line,
				Resource: resource,
				Properties: map[string]interface{}{
					"line_number": match.LineNumber,
					"keywords":    append([]string(nil), match.Keywords...),
				},
				Identity: strconv.Itoa(match.LineNumber) + ":" + strings.Join(match.Keywords, ","),
			})
		}
	}
	return automationData{
		command:        "logs",
		dockerEndpoint: report.DockerEndpoint,
		generatedAt:    report.GeneratedAt,
		findings:       findings,
		metrics:        metrics,
	}
}

func networkAutomationData(report NetworkReport) automationData {
	metrics := zeroMetricMap(networkMetricDefinitions)
	metrics["networks"] = uint64(len(report.Networks))
	metrics["containers"] = uint64(len(report.Containers))
	metrics["ports"] = uint64(len(report.Ports))
	metrics["risks"] = uint64(len(report.Risks))
	metrics["warnings"] = uint64(len(report.Warnings))
	for _, port := range report.Ports {
		if port.Published {
			metrics["published_ports"]++
		}
	}
	var findings []rpt.Finding
	for index, risk := range report.Risks {
		level := rpt.LevelWarning
		switch risk.Type {
		case "public-bind":
			metrics["public_bindings"]++
			metrics["public_bind_risks"]++
		case "port-conflict":
			level = rpt.LevelError
			metrics["port_conflicts"]++
		case "wildcard-overlap":
			level = rpt.LevelError
			metrics["wildcard_overlaps"]++
		}
		resources := make([]rpt.ResourceRef, 0, len(risk.Containers))
		for _, name := range risk.Containers {
			resources = append(resources, rpt.ResourceRef{Kind: "container", Name: name})
		}
		var primary rpt.ResourceRef
		if len(resources) > 0 {
			primary = resources[0]
			resources = resources[1:]
		}
		findings = append(findings, rpt.Finding{
			RuleID:           "dm.network." + ruleComponent(risk.Type),
			Level:            level,
			Message:          risk.Message,
			Resource:         primary,
			RelatedResources: resources,
			Identity:         fmt.Sprintf("%s:%d", risk.Type, index),
		})
	}
	for index, warning := range report.Warnings {
		findings = append(findings, rpt.Finding{
			RuleID:   "dm.network.collection-warning",
			Level:    rpt.LevelWarning,
			Message:  warning,
			Resource: rpt.ResourceRef{Kind: "docker-endpoint", Name: report.DockerEndpoint},
			Identity: strconv.Itoa(index),
		})
	}
	return automationData{command: "network", dockerEndpoint: report.DockerEndpoint, generatedAt: report.GeneratedAt, findings: findings, metrics: metrics}
}

func volumeAutomationData(report VolumeReport) automationData {
	metrics := zeroMetricMap(volumeMetricDefinitions)
	metrics["total"] = nonNegativeInt(report.Summary.Total)
	metrics["unused"] = nonNegativeInt(report.Summary.Unused)
	metrics["suspected_unused"] = nonNegativeInt(report.Summary.SuspectedUnused)
	metrics["used"] = nonNegativeInt(report.Summary.Used)
	metrics["unknown_size"] = nonNegativeInt(report.Summary.UnknownSize)
	metrics["reclaimable_size"] = nonNegativeInt64(report.Summary.ReclaimableSize)
	metrics["warnings"] = uint64(len(report.Warnings))
	var findings []rpt.Finding
	for _, item := range report.Volumes {
		resource := rpt.ResourceRef{Kind: "volume", Name: item.Name}
		switch item.Status {
		case "unused", "suspected-unused":
			findings = append(findings, rpt.Finding{
				RuleID:   "dm.volumes." + ruleComponent(item.Status),
				Level:    rpt.LevelNote,
				Message:  fmt.Sprintf("volume %s 状态为 %s", item.Name, item.Status),
				Resource: resource,
				Identity: item.Status,
			})
		}
		if item.Size < 0 {
			findings = append(findings, rpt.Finding{
				RuleID:   "dm.volumes.unknown-size",
				Level:    rpt.LevelNote,
				Message:  fmt.Sprintf("volume %s 大小未知", item.Name),
				Resource: resource,
				Identity: "unknown-size",
			})
		}
		if item.SizeError != "" {
			metrics["size_errors"]++
			findings = append(findings, rpt.Finding{
				RuleID:   "dm.volumes.size-probe-failed",
				Level:    rpt.LevelWarning,
				Message:  item.SizeError,
				Resource: resource,
				Identity: "size-probe-failed",
			})
		}
	}
	for index, warning := range report.Warnings {
		if strings.Contains(warning, "大小探测失败") {
			continue
		}
		findings = append(findings, rpt.Finding{
			RuleID:   "dm.volumes.collection-warning",
			Level:    rpt.LevelWarning,
			Message:  warning,
			Resource: rpt.ResourceRef{Kind: "docker-endpoint", Name: report.DockerEndpoint},
			Identity: strconv.Itoa(index),
		})
	}
	return automationData{command: "volumes", dockerEndpoint: report.DockerEndpoint, generatedAt: report.GeneratedAt, findings: findings, metrics: metrics}
}

func pruneAutomationData(report PruneReport) automationData {
	metrics := zeroMetricMap(pruneMetricDefinitions)
	metrics["estimated_bytes"] = report.EstimatedBytes
	metrics["warnings"] = uint64(len(report.Warnings))
	var findings []rpt.Finding
	addCandidate := func(kind, id, name string, _ int64) {
		metrics["candidates"]++
		metrics[pruneCandidateMetric(kind)]++
		properties := map[string]interface{}{}
		if pruneCandidateResolved(report, kind, id) {
			properties["resolved"] = true
			properties["applied"] = true
		}
		findings = append(findings, rpt.Finding{
			RuleID:     "dm.prune.candidate." + ruleComponent(kind),
			Level:      rpt.LevelNote,
			Message:    fmt.Sprintf("发现可清理的 %s 候选 %s", kind, valueOr(name, id)),
			Resource:   rpt.ResourceRef{Kind: kind, Name: name, ID: id},
			Properties: properties,
			Identity:   id,
		})
	}
	for _, item := range report.StoppedContainers {
		addCandidate(pruneKindContainer, item.ID, item.Name, item.Size)
	}
	for _, item := range report.DanglingImages {
		addCandidate(pruneKindImage, item.ID, "", item.Size)
	}
	for _, item := range report.UnusedVolumes {
		addCandidate(pruneKindVolume, item.Name, item.Name, item.Size)
	}
	for _, item := range report.BuildCaches {
		addCandidate(pruneKindBuildCache, item.ID, "", item.Size)
	}
	for index, warning := range report.Warnings {
		findings = append(findings, rpt.Finding{
			RuleID:   "dm.prune.collection-warning",
			Level:    rpt.LevelWarning,
			Message:  warning,
			Resource: rpt.ResourceRef{Kind: "docker-endpoint", Name: report.DockerEndpoint},
			Identity: strconv.Itoa(index),
		})
	}
	if result := report.ApplyResult; result != nil {
		metrics["apply_failures"] = uint64(len(result.Failures))
		metrics["unknown_outcomes"] = uint64(len(result.UnknownOutcomes))
		for _, failure := range result.Failures {
			findings = append(findings, rpt.Finding{
				RuleID:   "dm.prune.apply-failed",
				Level:    rpt.LevelError,
				Message:  failure.Error,
				Resource: rpt.ResourceRef{Kind: failure.Kind, ID: failure.ID, Name: resourceName(failure.Kind, failure.ID)},
				Identity: failure.Kind + ":" + failure.ID,
			})
		}
		for _, outcome := range result.UnknownOutcomes {
			findings = append(findings, rpt.Finding{
				RuleID:   "dm.prune.apply-unknown",
				Level:    rpt.LevelError,
				Message:  outcome.Reason,
				Resource: rpt.ResourceRef{Kind: outcome.Kind, ID: outcome.ID, Name: resourceName(outcome.Kind, outcome.ID)},
				Identity: outcome.Kind + ":" + outcome.ID,
			})
		}
	}
	return automationData{
		command:        "prune",
		dockerEndpoint: report.DockerEndpoint,
		generatedAt:    report.GeneratedAt,
		findings:       findings,
		metrics:        metrics,
	}
}

func reportAllAutomationData(report ReportAllReport) automationData {
	data := automationData{
		command:        "report-all",
		dockerEndpoint: report.DockerEndpoint,
		generatedAt:    report.GeneratedAt,
		metrics:        map[string]uint64{},
	}
	merge := func(prefix string, child automationData) {
		data.findings = append(data.findings, child.findings...)
		for name, value := range child.metrics {
			data.metrics[prefix+"."+name] = value
		}
	}
	if report.Health != nil {
		merge(reportAllKindHealth, healthAutomationData(*report.Health))
	}
	if report.Network != nil {
		merge(reportAllKindNetwork, networkAutomationData(*report.Network))
	}
	if report.Logs != nil {
		merge(reportAllKindLogs, logsAutomationData(*report.Logs))
	}
	if report.Volumes != nil {
		merge(reportAllKindVolumes, volumeAutomationData(*report.Volumes))
	}
	if report.Prune != nil {
		merge(reportAllKindPrune, pruneAutomationData(*report.Prune))
	}
	for _, section := range report.Sections {
		if section.Status != "failed" {
			continue
		}
		data.findings = append(data.findings, rpt.Finding{
			RuleID:   "dm.report.section-failed",
			Level:    rpt.LevelError,
			Message:  section.Error,
			Resource: rpt.ResourceRef{Kind: "report-section", Name: section.Name},
			Identity: section.Name,
		})
	}
	return data
}

func reportAllMetricDefinitions() []rpt.MetricDefinition {
	return rpt.ScopedMetricDefinitions()
}

func diagnosticLevel(value string) rpt.Level {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "error", "failed":
		return rpt.LevelError
	case "note", "info":
		return rpt.LevelNote
	default:
		return rpt.LevelWarning
	}
}

func ruleComponent(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var result strings.Builder
	lastDash := false
	for _, char := range value {
		allowed := char >= 'a' && char <= 'z' || char >= '0' && char <= '9'
		if allowed {
			result.WriteRune(char)
			lastDash = false
			continue
		}
		if !lastDash && result.Len() > 0 {
			result.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(result.String(), "-")
}

func nonNegativeInt(value int) uint64 {
	if value <= 0 {
		return 0
	}
	return uint64(value)
}

func nonNegativeInt64(value int64) uint64 {
	if value <= 0 {
		return 0
	}
	return uint64(value)
}

func pruneCandidateMetric(kind string) string {
	switch kind {
	case pruneKindContainer:
		return "stopped_containers"
	case pruneKindImage:
		return "dangling_images"
	case pruneKindVolume:
		return "unused_volumes"
	default:
		return "build_caches"
	}
}

func pruneCandidateResolved(report PruneReport, kind, id string) bool {
	if !report.Applied || report.ApplyResult == nil {
		return false
	}
	var deleted []string
	switch kind {
	case pruneKindContainer:
		deleted = report.ApplyResult.ContainersDeleted
	case pruneKindImage:
		deleted = report.ApplyResult.ImagesDeleted
	case pruneKindVolume:
		deleted = report.ApplyResult.VolumesDeleted
	case pruneKindBuildCache:
		deleted = report.ApplyResult.BuildCachesDeleted
	}
	for _, item := range deleted {
		if item == id {
			return true
		}
	}
	return false
}

func resourceName(kind, id string) string {
	if kind == pruneKindVolume {
		return id
	}
	return ""
}
