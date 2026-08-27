package report

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

const (
	LevelNone    Level = "none"
	LevelNote    Level = "note"
	LevelWarning Level = "warning"
	LevelError   Level = "error"
)

type Level string

func (level Level) String() string {
	return string(level)
}

type ResourceRef struct {
	Kind string `json:"kind,omitempty"`
	Name string `json:"name,omitempty"`
	ID   string `json:"id,omitempty"`
}

type Finding struct {
	RuleID           string                 `json:"rule_id"`
	Level            Level                  `json:"level"`
	Message          string                 `json:"message"`
	Resource         ResourceRef            `json:"resource,omitempty"`
	RelatedResources []ResourceRef          `json:"related_resources,omitempty"`
	Properties       map[string]interface{} `json:"properties,omitempty"`
	Identity         string                 `json:"-"`
}

type FindingCounts struct {
	Note    int `json:"note"`
	Warning int `json:"warning"`
	Error   int `json:"error"`
}

type Threshold struct {
	Metric   string `json:"metric"`
	Maximum  uint64 `json:"maximum"`
	Actual   uint64 `json:"actual"`
	Unit     string `json:"unit,omitempty"`
	Breached bool   `json:"breached"`
}

type Evaluation struct {
	Status     string            `json:"status"`
	FailOn     Level             `json:"fail_on"`
	Counts     FindingCounts     `json:"counts"`
	Metrics    map[string]uint64 `json:"metrics"`
	Thresholds []Threshold       `json:"thresholds,omitempty"`
	Findings   []Finding         `json:"findings,omitempty"`

	Command             string `json:"-"`
	DockerEndpoint      string `json:"-"`
	GeneratedAt         string `json:"-"`
	ExecutionSuccessful bool   `json:"-"`
}

type MetricDefinition struct {
	Name string
	Unit string
}

type thresholdLimit struct {
	metric  string
	maximum uint64
	unit    string
}

type Policy struct {
	failOn     Level
	thresholds []thresholdLimit
}

type EvaluationInput struct {
	Command             string
	DockerEndpoint      string
	GeneratedAt         string
	ExecutionSuccessful bool
	Findings            []Finding
	Metrics             map[string]uint64
}

type GateError struct {
	Evaluation *Evaluation
}

func (err *GateError) Error() string {
	if err == nil || err.Evaluation == nil {
		return "report policy gate failed"
	}
	evaluation := err.Evaluation
	return fmt.Sprintf(
		"report policy gate failed: fail-on=%s note=%d warning=%d error=%d",
		evaluation.FailOn,
		evaluation.Counts.Note,
		evaluation.Counts.Warning,
		evaluation.Counts.Error,
	)
}

// ExitCode is intentionally separate from the CLI package so callers that
// embed a report command can preserve the gate-specific status without
// importing Cobra or process-level concerns.
func (err *GateError) ExitCode() int { return 2 }

func ParsePolicy(failOn string, values []string, definitions []MetricDefinition) (Policy, error) {
	level, err := NormalizeLevel(failOn)
	if err != nil {
		return Policy{}, fmt.Errorf("--fail-on: %w", err)
	}
	allowed := make(map[string]MetricDefinition, len(definitions))
	for _, definition := range definitions {
		name := strings.TrimSpace(definition.Name)
		if name == "" {
			continue
		}
		definition.Name = name
		allowed[name] = definition
	}
	seen := make(map[string]bool, len(values))
	limits := make([]thresholdLimit, 0, len(values))
	for _, raw := range values {
		metric, maximumText, ok := strings.Cut(strings.TrimSpace(raw), "=")
		metric = strings.TrimSpace(metric)
		maximumText = strings.TrimSpace(maximumText)
		if !ok || metric == "" || maximumText == "" {
			return Policy{}, fmt.Errorf("--threshold %q 必须使用 metric=max 格式", raw)
		}
		definition, ok := allowed[metric]
		if !ok {
			return Policy{}, fmt.Errorf("--threshold 使用了不支持的指标 %q，可用指标: %s", metric, strings.Join(metricNames(definitions), ", "))
		}
		if seen[metric] {
			return Policy{}, fmt.Errorf("--threshold 指标 %q 重复", metric)
		}
		maximum, err := strconv.ParseUint(maximumText, 10, 64)
		if err != nil {
			return Policy{}, fmt.Errorf("--threshold %q 的最大值必须是非负整数: %w", raw, err)
		}
		seen[metric] = true
		limits = append(limits, thresholdLimit{metric: metric, maximum: maximum, unit: definition.Unit})
	}
	sort.Slice(limits, func(i, j int) bool { return limits[i].metric < limits[j].metric })
	return Policy{failOn: level, thresholds: limits}, nil
}

func NormalizeLevel(value string) (Level, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return LevelNone, nil
	}
	switch Level(value) {
	case LevelNone, LevelNote, LevelWarning, LevelError:
		return Level(value), nil
	case "warn":
		return LevelWarning, nil
	default:
		return "", fmt.Errorf("不支持的级别 %q，请使用 none、note、warning 或 error", value)
	}
}

func (policy Policy) Active() bool {
	return policy.failOn != LevelNone || len(policy.thresholds) > 0
}

func (policy Policy) Evaluate(input EvaluationInput) *Evaluation {
	metrics := make(map[string]uint64, len(input.Metrics))
	for name, value := range input.Metrics {
		metrics[name] = value
	}
	findings := append([]Finding(nil), input.Findings...)
	thresholds := make([]Threshold, 0, len(policy.thresholds))
	thresholdFailed := false
	for _, limit := range policy.thresholds {
		actual := metrics[limit.metric]
		breached := actual > limit.maximum
		thresholds = append(thresholds, Threshold{
			Metric:   limit.metric,
			Maximum:  limit.maximum,
			Actual:   actual,
			Unit:     limit.unit,
			Breached: breached,
		})
		if !breached {
			continue
		}
		thresholdFailed = true
		findings = append(findings, Finding{
			RuleID:   "dm." + input.Command + ".threshold." + limit.metric,
			Level:    LevelError,
			Message:  fmt.Sprintf("指标 %s=%d 超过最大允许值 %d", limit.metric, actual, limit.maximum),
			Resource: ResourceRef{Kind: "report", Name: input.Command},
			Properties: map[string]interface{}{
				"actual":   actual,
				"maximum":  limit.maximum,
				"operator": "<=",
				"unit":     limit.unit,
			},
			Identity: limit.metric,
		})
	}
	normalizeAndSortFindings(findings)
	counts := countFindings(findings)
	failed := thresholdFailed || findingsReachLevel(findings, policy.failOn)
	status := "pass"
	if failed {
		status = "fail"
	}
	return &Evaluation{
		Status:              status,
		FailOn:              policy.failOn,
		Counts:              counts,
		Metrics:             metrics,
		Thresholds:          thresholds,
		Findings:            findings,
		Command:             input.Command,
		DockerEndpoint:      input.DockerEndpoint,
		GeneratedAt:         input.GeneratedAt,
		ExecutionSuccessful: input.ExecutionSuccessful,
	}
}

func (evaluation *Evaluation) GateError() error {
	if evaluation == nil || evaluation.Status != "fail" {
		return nil
	}
	return &GateError{Evaluation: evaluation}
}

func PrintEvaluationText(w io.Writer, evaluation *Evaluation) {
	if evaluation == nil {
		return
	}
	fmt.Fprintf(w, "\n自动化门禁: %s fail-on=%s note=%d warning=%d error=%d\n",
		strings.ToUpper(evaluation.Status),
		evaluation.FailOn,
		evaluation.Counts.Note,
		evaluation.Counts.Warning,
		evaluation.Counts.Error,
	)
	for _, threshold := range evaluation.Thresholds {
		fmt.Fprintf(w, "  threshold %s: actual=%d maximum=%d breached=%v\n", threshold.Metric, threshold.Actual, threshold.Maximum, threshold.Breached)
	}
}

func metricNames(definitions []MetricDefinition) []string {
	names := make([]string, 0, len(definitions))
	seen := map[string]bool{}
	for _, definition := range definitions {
		name := strings.TrimSpace(definition.Name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func normalizeAndSortFindings(findings []Finding) {
	for i := range findings {
		if _, err := NormalizeLevel(string(findings[i].Level)); err != nil || findings[i].Level == LevelNone || findings[i].Level == "" {
			findings[i].Level = LevelWarning
		}
		findings[i].RuleID = strings.TrimSpace(findings[i].RuleID)
		if findings[i].RuleID == "" {
			findings[i].RuleID = "dm.report.finding"
		}
	}
	sort.SliceStable(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.RuleID != b.RuleID {
			return a.RuleID < b.RuleID
		}
		if a.Resource.Kind != b.Resource.Kind {
			return a.Resource.Kind < b.Resource.Kind
		}
		if a.Resource.Name != b.Resource.Name {
			return a.Resource.Name < b.Resource.Name
		}
		if a.Resource.ID != b.Resource.ID {
			return a.Resource.ID < b.Resource.ID
		}
		if a.Identity != b.Identity {
			return a.Identity < b.Identity
		}
		return a.Message < b.Message
	})
}

func countFindings(findings []Finding) FindingCounts {
	var counts FindingCounts
	for _, finding := range findings {
		switch finding.Level {
		case LevelNote:
			counts.Note++
		case LevelError:
			counts.Error++
		default:
			counts.Warning++
		}
	}
	return counts
}

func findingsReachLevel(findings []Finding, failOn Level) bool {
	threshold := levelRank(failOn)
	if threshold == 0 {
		return false
	}
	for _, finding := range findings {
		if resolved, ok := finding.Properties["resolved"].(bool); ok && resolved {
			continue
		}
		if levelRank(finding.Level) >= threshold {
			return true
		}
	}
	return false
}

func levelRank(level Level) int {
	switch level {
	case LevelNote:
		return 1
	case LevelWarning:
		return 2
	case LevelError:
		return 3
	default:
		return 0
	}
}
