package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"docker-manager/internal/sensitive"
)

func TestParsePolicyAndEvaluateThreshold(t *testing.T) {
	policy, err := ParsePolicy("warning", []string{"issues=1"}, []MetricDefinition{{Name: "issues"}})
	if err != nil {
		t.Fatal(err)
	}
	evaluation := policy.Evaluate(EvaluationInput{
		Command:  "health",
		Metrics:  map[string]uint64{"issues": 2},
		Findings: []Finding{{RuleID: "dm.health.public-port", Level: LevelWarning, Message: "public"}},
	})
	if evaluation.Status != "fail" || evaluation.Counts.Error != 1 || len(evaluation.Thresholds) != 1 || !evaluation.Thresholds[0].Breached {
		t.Fatalf("evaluation = %#v, want threshold failure", evaluation)
	}
	var gate *GateError
	if !errors.As(evaluation.GateError(), &gate) {
		t.Fatalf("GateError() = %v, want typed gate error", evaluation.GateError())
	}
}

func TestParsePolicyRejectsMalformedThresholds(t *testing.T) {
	definitions := []MetricDefinition{{Name: "issues"}}
	for _, value := range []string{"", "issues", "=1", "unknown=1", "issues=-1", "issues=18446744073709551616"} {
		if _, err := ParsePolicy("none", []string{value}, definitions); err == nil {
			t.Errorf("ParsePolicy(%q) = nil error, want rejection", value)
		}
	}
	if _, err := ParsePolicy("none", []string{"issues=1", "issues=2"}, definitions); err == nil {
		t.Fatal("duplicate threshold accepted")
	}
	if _, err := ParsePolicy("critical", nil, definitions); err == nil {
		t.Fatal("invalid fail-on accepted")
	}
}

func TestEvaluateResolvedFindingDoesNotFailNoteGate(t *testing.T) {
	policy, err := ParsePolicy("note", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	evaluation := policy.Evaluate(EvaluationInput{Findings: []Finding{{
		RuleID: "dm.prune.candidate.image", Level: LevelNote, Message: "done",
		Properties: map[string]interface{}{"resolved": true},
	}}})
	if evaluation.Status != "pass" {
		t.Fatalf("status = %s, want pass for resolved finding", evaluation.Status)
	}
}

func TestPrintEvaluatedSARIFIsStableAndRedacted(t *testing.T) {
	evaluation := (&Policy{failOn: LevelWarning}).Evaluate(EvaluationInput{
		Command:             "health",
		DockerEndpoint:      "tcp://user:password@example:2375",
		GeneratedAt:         "2026-08-27T00:00:00Z",
		ExecutionSuccessful: true,
		Findings: []Finding{
			{RuleID: "dm.health.public-port", Level: LevelWarning, Message: "Authorization: Bearer opaque", Resource: ResourceRef{Kind: "container", Name: "api", ID: "abc"}},
			{RuleID: "dm.health.public-port", Level: LevelWarning, Message: "second", Resource: ResourceRef{Kind: "container", Name: "web", ID: "def"}},
		},
		Metrics: map[string]uint64{"issues": 2},
	})
	var out bytes.Buffer
	if err := PrintEvaluatedWithProfile(&out, FormatSARIF, struct{}{}, evaluation, func(_ io.Writer) {}, sensitive.ProfileBasic); err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("invalid SARIF JSON: %v\n%s", err, out.String())
	}
	if got["version"] != "2.1.0" || got["$schema"] == nil {
		t.Fatalf("SARIF header = %#v", got)
	}
	if strings.Contains(out.String(), "password") || strings.Contains(out.String(), "opaque") {
		t.Fatalf("SARIF leaked sensitive data: %s", out.String())
	}
	if !strings.Contains(out.String(), "logicalLocations") || !strings.Contains(out.String(), "dm.health.public-port") {
		t.Fatalf("SARIF missing rule/location: %s", out.String())
	}
}
