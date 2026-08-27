package appconfig

import (
	"reflect"
	"strings"
	"testing"
)

func TestLoadAcceptsScopedReportThresholds(t *testing.T) {
	values := []string{
		"health.unhealthy=0",
		"network.public_bindings=0",
		"logs.total_matches=10",
		"volumes.reclaimable_size=1048576",
		"prune.estimated_bytes=2097152",
	}
	path := writeConfig(t, "thresholds:\n  - "+strings.Join(values, "\n  - ")+"\n")
	loaded, err := LoadWithOptions(path, LoadOptions{Required: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Config.Thresholds, values) {
		t.Fatalf("thresholds = %#v, want %#v", loaded.Config.Thresholds, values)
	}
}

func TestLoadRejectsInvalidScopedReportThresholdsEarly(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "unscoped", value: "unhealthy=0", want: "CLI-only"},
		{name: "unknown scope", value: "unknown.metric=0", want: "unknown scope"},
		{name: "unknown metric", value: "health.unknown_metric=0", want: "unknown metric"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadWithOptions(writeConfig(t, "thresholds:\n  - "+test.value+"\n"), LoadOptions{Required: true})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadWithOptions(%q) error = %v, want %q", test.value, err, test.want)
			}
		})
	}
}

func TestLoadRejectsInvalidThresholdInUnselectedProfile(t *testing.T) {
	path := writeConfig(t, "profiles:\n  dev:\n    thresholds:\n      - unhealthy=0\n")
	_, err := LoadWithOptions(path, LoadOptions{Required: true})
	if err == nil || !strings.Contains(err.Error(), "CLI-only") {
		t.Fatalf("LoadWithOptions() error = %v, want strict profile threshold validation", err)
	}
}
