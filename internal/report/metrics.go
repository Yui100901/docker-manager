package report

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	MetricScopeHealth  = "health"
	MetricScopeNetwork = "network"
	MetricScopeLogs    = "logs"
	MetricScopeVolumes = "volumes"
	MetricScopePrune   = "prune"
)

var metricScopeOrder = []string{
	MetricScopeHealth,
	MetricScopeNetwork,
	MetricScopeLogs,
	MetricScopeVolumes,
	MetricScopePrune,
}

var metricDefinitionsByScope = map[string][]MetricDefinition{
	MetricScopeHealth: {
		{Name: "total"},
		{Name: "running"},
		{Name: "stopped"},
		{Name: "restarting"},
		{Name: "unhealthy"},
		{Name: "restart_warnings"},
		{Name: "log_warnings"},
		{Name: "logs_unavailable"},
		{Name: "public_bindings"},
		{Name: "issues"},
		{Name: "warning_issues"},
		{Name: "error_issues"},
	},
	MetricScopeNetwork: {
		{Name: "networks"},
		{Name: "containers"},
		{Name: "ports"},
		{Name: "published_ports"},
		{Name: "public_bindings"},
		{Name: "risks"},
		{Name: "public_bind_risks"},
		{Name: "port_conflicts"},
		{Name: "wildcard_overlaps"},
		{Name: "warnings"},
	},
	MetricScopeLogs: {
		{Name: "scanned_containers"},
		{Name: "containers_matched"},
		{Name: "total_matches"},
		{Name: "errors"},
		{Name: "logs_unavailable"},
	},
	MetricScopeVolumes: {
		{Name: "total"},
		{Name: "unused"},
		{Name: "suspected_unused"},
		{Name: "used"},
		{Name: "unknown_size"},
		{Name: "reclaimable_size", Unit: "bytes"},
		{Name: "warnings"},
		{Name: "size_errors"},
	},
	MetricScopePrune: {
		{Name: "candidates"},
		{Name: "stopped_containers"},
		{Name: "dangling_images"},
		{Name: "unused_volumes"},
		{Name: "build_caches"},
		{Name: "estimated_bytes", Unit: "bytes"},
		{Name: "warnings"},
		{Name: "apply_failures"},
		{Name: "unknown_outcomes"},
	},
}

// ScopedThreshold is the validated representation of a threshold read from
// configuration. CLI thresholds intentionally use the unscoped metric form.
type ScopedThreshold struct {
	Scope   string
	Metric  string
	Maximum uint64
}

func (threshold ScopedThreshold) ScopedValue() string {
	return threshold.Scope + "." + threshold.UnscopedValue()
}

func (threshold ScopedThreshold) UnscopedValue() string {
	return threshold.Metric + "=" + strconv.FormatUint(threshold.Maximum, 10)
}

// MetricDefinitions returns a copy so callers cannot mutate the shared
// catalog used by configuration validation.
func MetricDefinitions(scope string) []MetricDefinition {
	return append([]MetricDefinition(nil), metricDefinitionsByScope[scope]...)
}

func ScopedMetricDefinitions() []MetricDefinition {
	var definitions []MetricDefinition
	for _, scope := range metricScopeOrder {
		for _, definition := range metricDefinitionsByScope[scope] {
			definition.Name = scope + "." + definition.Name
			definitions = append(definitions, definition)
		}
	}
	return definitions
}

// ParseScopedThresholds validates configuration thresholds. Configuration
// must identify the report scope; unscoped metric=max values are CLI-only.
func ParseScopedThresholds(values []string) ([]ScopedThreshold, error) {
	thresholds := make([]ScopedThreshold, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, raw := range values {
		name, maximumText, ok := strings.Cut(strings.TrimSpace(raw), "=")
		name = strings.TrimSpace(name)
		maximumText = strings.TrimSpace(maximumText)
		if !ok || name == "" || maximumText == "" || strings.Contains(maximumText, "=") {
			return nil, fmt.Errorf("thresholds entry %q must use scope.metric=maximum", raw)
		}

		scope, metric, scoped := strings.Cut(name, ".")
		scope = strings.TrimSpace(scope)
		metric = strings.TrimSpace(metric)
		if !scoped || scope == "" || metric == "" {
			return nil, fmt.Errorf("thresholds entry %q must use scope.metric=maximum; unscoped thresholds are CLI-only", raw)
		}
		definitions, knownScope := metricDefinitionsByScope[scope]
		if !knownScope {
			return nil, fmt.Errorf("thresholds entry %q uses unknown scope %q; available scopes: %s", raw, scope, strings.Join(metricScopeOrder, ", "))
		}
		if !containsMetric(definitions, metric) {
			return nil, fmt.Errorf("thresholds entry %q uses unknown metric %q for scope %q; available metrics: %s", raw, metric, scope, strings.Join(sortedMetricNames(definitions), ", "))
		}
		maximum, err := strconv.ParseUint(maximumText, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("thresholds entry %q must use an unsigned integer maximum", raw)
		}
		fullName := scope + "." + metric
		if _, duplicate := seen[fullName]; duplicate {
			return nil, fmt.Errorf("thresholds contains duplicate metric %q", fullName)
		}
		seen[fullName] = struct{}{}
		thresholds = append(thresholds, ScopedThreshold{Scope: scope, Metric: metric, Maximum: maximum})
	}
	return thresholds, nil
}

func containsMetric(definitions []MetricDefinition, name string) bool {
	for _, definition := range definitions {
		if definition.Name == name {
			return true
		}
	}
	return false
}

func sortedMetricNames(definitions []MetricDefinition) []string {
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Name)
	}
	sort.Strings(names)
	return names
}
