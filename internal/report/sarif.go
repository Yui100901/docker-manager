package report

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const sarifSchema = "https://json.schemastore.org/sarif-2.1.0.json"

type sarifLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool        sarifTool         `json:"tool"`
	Invocations []sarifInvocation `json:"invocations,omitempty"`
	Results     []sarifResult     `json:"results,omitempty"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name     string                     `json:"name"`
	FullName string                     `json:"fullName,omitempty"`
	Rules    []sarifReportingDescriptor `json:"rules,omitempty"`
}

type sarifReportingDescriptor struct {
	ID                   string                    `json:"id"`
	Name                 string                    `json:"name,omitempty"`
	ShortDescription     sarifMessage              `json:"shortDescription"`
	DefaultConfiguration sarifDefaultConfiguration `json:"defaultConfiguration"`
}

type sarifDefaultConfiguration struct {
	Level Level `json:"level"`
}

type sarifInvocation struct {
	ExecutionSuccessful bool                   `json:"executionSuccessful"`
	Properties          map[string]interface{} `json:"properties,omitempty"`
}

type sarifResult struct {
	RuleID              string                 `json:"ruleId"`
	Level               Level                  `json:"level"`
	Message             sarifMessage           `json:"message"`
	Locations           []sarifLocation        `json:"locations,omitempty"`
	PartialFingerprints map[string]string      `json:"partialFingerprints,omitempty"`
	Properties          map[string]interface{} `json:"properties,omitempty"`
}

type sarifMessage struct {
	Text string `json:"text"`
}

type sarifLocation struct {
	LogicalLocations []sarifLogicalLocation `json:"logicalLocations,omitempty"`
}

type sarifLogicalLocation struct {
	Name               string `json:"name,omitempty"`
	FullyQualifiedName string `json:"fullyQualifiedName,omitempty"`
	Kind               string `json:"kind,omitempty"`
}

func buildSARIF(evaluation *Evaluation) sarifLog {
	if evaluation == nil {
		evaluation = &Evaluation{Status: "pass", FailOn: LevelNone, ExecutionSuccessful: true}
	}
	rulesByID := map[string]sarifReportingDescriptor{}
	results := make([]sarifResult, 0, len(evaluation.Findings))
	for _, finding := range evaluation.Findings {
		if existing, ok := rulesByID[finding.RuleID]; !ok {
			rulesByID[finding.RuleID] = sarifReportingDescriptor{
				ID:                   finding.RuleID,
				Name:                 sarifRuleName(finding.RuleID),
				ShortDescription:     sarifMessage{Text: finding.RuleID},
				DefaultConfiguration: sarifDefaultConfiguration{Level: finding.Level},
			}
		} else if levelRank(finding.Level) > levelRank(existing.DefaultConfiguration.Level) {
			existing.DefaultConfiguration.Level = finding.Level
			rulesByID[finding.RuleID] = existing
		}
		result := sarifResult{
			RuleID:              finding.RuleID,
			Level:               finding.Level,
			Message:             sarifMessage{Text: finding.Message},
			PartialFingerprints: map[string]string{"dockerManagerFinding/v1": findingFingerprint(evaluation, finding)},
			Properties:          finding.Properties,
		}
		resources := append([]ResourceRef{finding.Resource}, finding.RelatedResources...)
		for _, resource := range resources {
			if resource.Kind == "" && resource.Name == "" && resource.ID == "" {
				continue
			}
			name := resource.Name
			if name == "" {
				name = resource.ID
			}
			fullyQualified := strings.Trim(strings.Join([]string{resource.Kind, name}, "."), ".")
			result.Locations = append(result.Locations, sarifLocation{LogicalLocations: []sarifLogicalLocation{{
				Name:               name,
				FullyQualifiedName: fullyQualified,
				Kind:               resource.Kind,
			}}})
		}
		results = append(results, result)
	}
	ruleIDs := make([]string, 0, len(rulesByID))
	for ruleID := range rulesByID {
		ruleIDs = append(ruleIDs, ruleID)
	}
	sortStrings(ruleIDs)
	rules := make([]sarifReportingDescriptor, 0, len(ruleIDs))
	for _, ruleID := range ruleIDs {
		rules = append(rules, rulesByID[ruleID])
	}
	properties := map[string]interface{}{
		"command":        evaluation.Command,
		"dockerEndpoint": evaluation.DockerEndpoint,
		"generatedAt":    evaluation.GeneratedAt,
		"gateStatus":     evaluation.Status,
		"failOn":         evaluation.FailOn,
		"metrics":        evaluation.Metrics,
		"thresholds":     evaluation.Thresholds,
	}
	return sarifLog{
		Schema:  sarifSchema,
		Version: "2.1.0",
		Runs: []sarifRun{{
			Tool: sarifTool{Driver: sarifDriver{
				Name:     "docker-manager",
				FullName: "Docker Manager",
				Rules:    rules,
			}},
			Invocations: []sarifInvocation{{ExecutionSuccessful: evaluation.ExecutionSuccessful, Properties: properties}},
			Results:     results,
		}},
	}
}

func findingFingerprint(evaluation *Evaluation, finding Finding) string {
	identity := finding.Identity
	if identity == "" {
		identity = strings.Join([]string{
			finding.Resource.Kind,
			finding.Resource.ID,
			finding.Resource.Name,
		}, "\x00")
		if identity == "\x00\x00" {
			identity = "default"
		}
	}
	value := strings.Join([]string{
		evaluation.Command,
		evaluation.DockerEndpoint,
		finding.RuleID,
		finding.Resource.Kind,
		finding.Resource.ID,
		finding.Resource.Name,
		identity,
	}, "\x00")
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func sarifRuleName(ruleID string) string {
	parts := strings.FieldsFunc(ruleID, func(r rune) bool {
		return r == '.' || r == '-' || r == '_'
	})
	if len(parts) == 0 {
		return "finding"
	}
	return strings.Join(parts, "_")
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
