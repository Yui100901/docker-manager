package appconfig

import (
	"math"
	"strings"
	"testing"
)

func TestConfigValidateRejectsNonFiniteOperationRate(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if err := (Config{OperationRateLimit: value}).Validate(); err == nil || !strings.Contains(err.Error(), "operation_rate_limit") {
			t.Fatalf("Config.Validate(rate=%v) error = %v, want operation_rate_limit rejection", value, err)
		}
	}
}

func TestLoadRejectsNonFiniteOperationRate(t *testing.T) {
	for _, value := range []string{".nan", ".inf", "-.inf"} {
		_, err := LoadWithOptions(writeConfig(t, "operation_rate_limit: "+value+"\n"), LoadOptions{Required: true})
		if err == nil || !strings.Contains(err.Error(), "operation_rate_limit") {
			t.Fatalf("LoadWithOptions(rate=%s) error = %v, want operation_rate_limit rejection", value, err)
		}
	}
}

func TestConfigValidateOperationRuntimeBounds(t *testing.T) {
	if err := (Config{
		OperationConcurrency: 64,
		OperationTimeout:     "24h",
		OperationRateLimit:   1000,
		OperationMaxItems:    100000,
	}).Validate(); err != nil {
		t.Fatalf("Config.Validate(maximum runtime values) error = %v", err)
	}
	for _, test := range []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "negative concurrency", cfg: Config{OperationConcurrency: -1}, want: "operation_concurrency"},
		{name: "large concurrency", cfg: Config{OperationConcurrency: 65}, want: "operation_concurrency"},
		{name: "zero timeout", cfg: Config{OperationTimeout: "0s"}, want: "greater than zero"},
		{name: "large timeout", cfg: Config{OperationTimeout: "24h1ns"}, want: "operation_timeout"},
		{name: "negative rate", cfg: Config{OperationRateLimit: -1}, want: "operation_rate_limit"},
		{name: "large rate", cfg: Config{OperationRateLimit: 1000.1}, want: "operation_rate_limit"},
		{name: "negative items", cfg: Config{OperationMaxItems: -1}, want: "operation_max_items"},
		{name: "large items", cfg: Config{OperationMaxItems: 100001}, want: "operation_max_items"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.cfg.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Config.Validate() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadProfileMergesExplicitZeroRuntimeValues(t *testing.T) {
	path := writeConfig(t, `
operation_concurrency: 8
operation_rate_limit: 12.5
operation_max_items: 100
profiles:
  constrained:
    operation_concurrency: 0
    operation_rate_limit: 0
    operation_max_items: 0
`)
	loaded, err := LoadWithOptions(path, LoadOptions{Required: true, Profile: "constrained", ProfileExplicit: true})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Config.OperationConcurrency != 0 || loaded.Config.OperationRateLimit != 0 || loaded.Config.OperationMaxItems != 0 {
		t.Fatalf("effective runtime values = %#v, want explicit profile zero values", loaded.Config)
	}
	for _, field := range []string{"operation_concurrency", "operation_rate_limit", "operation_max_items"} {
		if !loaded.Fields[field] {
			t.Fatalf("Fields[%q] = false, want profile presence", field)
		}
		if got := loaded.FieldSources[field]; !strings.Contains(got, "profile:constrained@") {
			t.Fatalf("FieldSources[%q] = %q, want constrained profile", field, got)
		}
	}
}
