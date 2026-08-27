package report

import (
	"reflect"
	"strings"
	"testing"
)

func TestMetricCatalogBuildsScopedDefinitionsWithoutSharingMutableSlices(t *testing.T) {
	health := MetricDefinitions(MetricScopeHealth)
	if len(health) != 12 || health[0].Name != "total" {
		t.Fatalf("health definitions = %#v", health)
	}
	health[0].Name = "mutated"
	if got := MetricDefinitions(MetricScopeHealth)[0].Name; got != "total" {
		t.Fatalf("shared metric catalog was mutated: %q", got)
	}

	definitions := ScopedMetricDefinitions()
	got := make(map[string]string, len(definitions))
	for _, definition := range definitions {
		got[definition.Name] = definition.Unit
	}
	for name, unit := range map[string]string{
		"health.unhealthy":         "",
		"network.public_bindings":  "",
		"logs.total_matches":       "",
		"volumes.reclaimable_size": "bytes",
		"prune.estimated_bytes":    "bytes",
	} {
		if got[name] != unit {
			t.Errorf("scoped definition %q unit = %q, want %q", name, got[name], unit)
		}
	}
}

func TestParseScopedThresholds(t *testing.T) {
	values := []string{
		"health.unhealthy=0",
		"network.public_bindings=1",
		"logs.total_matches=02",
		"volumes.reclaimable_size=4096",
		"prune.estimated_bytes=8192",
	}
	got, err := ParseScopedThresholds(values)
	if err != nil {
		t.Fatal(err)
	}
	want := []ScopedThreshold{
		{Scope: MetricScopeHealth, Metric: "unhealthy", Maximum: 0},
		{Scope: MetricScopeNetwork, Metric: "public_bindings", Maximum: 1},
		{Scope: MetricScopeLogs, Metric: "total_matches", Maximum: 2},
		{Scope: MetricScopeVolumes, Metric: "reclaimable_size", Maximum: 4096},
		{Scope: MetricScopePrune, Metric: "estimated_bytes", Maximum: 8192},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseScopedThresholds() = %#v, want %#v", got, want)
	}
	if got[0].ScopedValue() != "health.unhealthy=0" || got[0].UnscopedValue() != "unhealthy=0" {
		t.Fatalf("threshold routing values = %q / %q", got[0].ScopedValue(), got[0].UnscopedValue())
	}
}

func TestParseScopedThresholdsRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   string
	}{
		{name: "unscoped", values: []string{"unhealthy=0"}, want: "CLI-only"},
		{name: "unknown scope", values: []string{"unknown.metric=0"}, want: "unknown scope"},
		{name: "unknown metric", values: []string{"health.unknown_metric=0"}, want: "unknown metric"},
		{name: "malformed", values: []string{"health.unhealthy"}, want: "scope.metric=maximum"},
		{name: "negative", values: []string{"health.unhealthy=-1"}, want: "unsigned integer"},
		{name: "overflow", values: []string{"health.unhealthy=18446744073709551616"}, want: "unsigned integer"},
		{name: "duplicate", values: []string{"health.unhealthy=0", " health.unhealthy = 1 "}, want: "duplicate"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseScopedThresholds(test.values)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ParseScopedThresholds(%#v) error = %v, want %q", test.values, err, test.want)
			}
		})
	}
}
