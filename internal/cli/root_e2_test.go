package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"docker-manager/internal/audit"
	rpt "docker-manager/internal/report"
	"docker-manager/internal/runcontrol"

	"github.com/spf13/cobra"
)

func TestCommandExitCodeDistinguishesGateAndOperationalErrors(t *testing.T) {
	evaluation := &rpt.Evaluation{
		Status: "fail",
		FailOn: rpt.LevelError,
	}
	gate := &rpt.GateError{Evaluation: evaluation}
	operational := errors.New("daemon unavailable")
	emptyWrapper := emptyErrorGroup{}

	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "nil", want: 0},
		{name: "gate", err: gate, want: 2},
		{name: "wrapped gate", err: fmt.Errorf("command failed: %w", gate), want: 2},
		{name: "gate plus operational", err: errors.Join(gate, operational), want: 1},
		{name: "gate plus empty wrapper", err: errors.Join(gate, emptyWrapper), want: 1},
		{name: "gate plus cancellation", err: errors.Join(gate, context.Canceled), want: 130},
		{name: "operational", err: operational, want: 1},
		{name: "canceled", err: context.Canceled, want: 130},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := commandExitCode(test.err); got != test.want {
				t.Fatalf("commandExitCode(%v) = %d, want %d", test.err, got, test.want)
			}
		})
	}
}

func TestApplyReportAutomationDefaultsHonorsExplicitFlagsAndRepeatedExecution(t *testing.T) {
	cfg := &appConfig{FailOn: "warning", Thresholds: []string{
		"health.issues=3",
		"health.unhealthy=0",
		"network.public_bindings=0",
		"logs.total_matches=7",
		"volumes.reclaimable_size=1024",
		"prune.candidates=2",
	}}
	tests := []struct {
		name       string
		command    string
		grouped    bool
		thresholds []string
	}{
		{name: "top-level health", command: "health", thresholds: []string{"issues=3", "unhealthy=0"}},
		{name: "grouped health", command: "health", grouped: true, thresholds: []string{"issues=3", "unhealthy=0"}},
		{name: "network", command: "network", thresholds: []string{"public_bindings=0"}},
		{name: "logs", command: "logs", thresholds: []string{"total_matches=7"}},
		{name: "volumes", command: "volumes", thresholds: []string{"reclaimable_size=1024"}},
		{name: "prune", command: "prune", thresholds: []string{"candidates=2"}},
		{name: "report all", command: "all", grouped: true, thresholds: cfg.Thresholds},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var failOn string
			var thresholds []string
			cmd := &cobra.Command{Use: test.command}
			cmd.Flags().StringVar(&failOn, "fail-on", rpt.LevelNone.String(), "")
			cmd.Flags().StringArrayVar(&thresholds, "threshold", nil, "")
			if test.grouped {
				report := &cobra.Command{Use: "report"}
				report.AddCommand(cmd)
			}
			if err := applyReportAutomationDefaults(cmd, cfg); err != nil {
				t.Fatal(err)
			}
			if failOn != "warning" || !reflect.DeepEqual(thresholds, test.thresholds) {
				t.Fatalf("defaults = fail-on:%q thresholds:%#v, want warning/%#v", failOn, thresholds, test.thresholds)
			}
			if cmd.Flags().Changed("fail-on") || cmd.Flags().Changed("threshold") {
				t.Fatal("configuration defaults must not be reported as explicit flags")
			}
		})
	}

	var explicitFail string
	var explicitThresholds []string
	explicit := &cobra.Command{Use: "health"}
	explicit.Flags().StringVar(&explicitFail, "fail-on", rpt.LevelNone.String(), "")
	explicit.Flags().StringArrayVar(&explicitThresholds, "threshold", nil, "")
	if err := explicit.Flags().Set("fail-on", "error"); err != nil {
		t.Fatal(err)
	}
	if err := explicit.Flags().Set("threshold", "unhealthy=1"); err != nil {
		t.Fatal(err)
	}
	if err := applyReportAutomationDefaults(explicit, cfg); err != nil {
		t.Fatal(err)
	}
	if explicitFail != "error" || !reflect.DeepEqual(explicitThresholds, []string{"unhealthy=1"}) {
		t.Fatalf("explicit values = fail-on:%q thresholds:%#v", explicitFail, explicitThresholds)
	}

	var repeated []string
	reused := &cobra.Command{Use: "health"}
	reused.Flags().StringArrayVar(&repeated, "threshold", nil, "")
	if err := applyReportAutomationDefaults(reused, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Thresholds = []string{"network.public_bindings=0"}
	if err := applyReportAutomationDefaults(reused, cfg); err != nil {
		t.Fatal(err)
	}
	if len(repeated) != 0 {
		t.Fatalf("repeated defaults retained stale health thresholds: %#v", repeated)
	}
}

func TestRootInjectsConfiguredAutomationDefaultsBeforeLeafRun(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "dm.yaml")
	if err := os.WriteFile(configPath, []byte("fail_on: warning\nthresholds:\n  - health.issues=3\n  - health.unhealthy=0\n  - network.public_bindings=0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		command []string
		flags   []string
		want    string
	}{
		{name: "top-level health", command: []string{"health"}, want: "warning|issues=3,unhealthy=0"},
		{name: "grouped health", command: []string{"report", "health"}, want: "warning|issues=3,unhealthy=0"},
		{name: "report all", command: []string{"report", "all"}, want: "warning|health.issues=3,health.unhealthy=0,network.public_bindings=0"},
		{name: "explicit values replace config", command: []string{"health"}, flags: []string{"--fail-on=error", "--threshold=unhealthy=1"}, want: "error|unhealthy=1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := appConfig{}
			opts := outputOptions{}
			root, lifecycle := newRootCommandWithLifecycle(&cfg, &opts)
			target, _, err := root.Find(test.command)
			if err != nil {
				t.Fatal(err)
			}
			target.RunE = func(cmd *cobra.Command, _ []string) error {
				failOn, err := cmd.Flags().GetString("fail-on")
				if err != nil {
					return err
				}
				thresholds, err := cmd.Flags().GetStringArray("threshold")
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s|%s", failOn, strings.Join(thresholds, ","))
				return err
			}
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&bytes.Buffer{})
			args := []string{"--config", configPath}
			args = append(args, test.command...)
			args = append(args, test.flags...)
			root.SetArgs(args)
			err = root.Execute()
			if finishErr := lifecycle.finish(err); finishErr != nil {
				err = errors.Join(err, finishErr)
			}
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if got := out.String(); got != test.want {
				t.Fatalf("probe output = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRootInjectsConfiguredLogBudgetsAndHonorsExplicitFlags(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "dm.yaml")
	if err := os.WriteFile(configPath, []byte("max_log_bytes: 2M\nmax_total_log_bytes: 9M\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "configured defaults", want: "2M|9M"},
		{name: "explicit values", args: []string{"--max-log-bytes=3M", "--max-total-log-bytes=10M"}, want: "3M|10M"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var perContainer, total string
			cfg := appConfig{}
			opts := outputOptions{}
			root, lifecycle := newRootCommandWithLifecycle(&cfg, &opts)
			probe := &cobra.Command{
				Use: "log-budget-probe",
				RunE: func(cmd *cobra.Command, _ []string) error {
					_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s|%s", perContainer, total)
					return err
				},
			}
			probe.Flags().StringVar(&perContainer, "max-log-bytes", "16M", "")
			probe.Flags().StringVar(&total, "max-total-log-bytes", "256M", "")
			root.AddCommand(probe)
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&bytes.Buffer{})
			args := []string{"--config", configPath, "log-budget-probe"}
			args = append(args, test.args...)
			root.SetArgs(args)
			err := root.Execute()
			if finishErr := lifecycle.finish(err); finishErr != nil {
				err = errors.Join(err, finishErr)
			}
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if got := out.String(); got != test.want {
				t.Fatalf("probe output = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRuntimeFlagsAreScopedToE2Commands(t *testing.T) {
	cfg := appConfig{}
	opts := outputOptions{}
	root := newRootCommand(&cfg, &opts)
	runtimeFlags := []string{"concurrency", "operation-timeout", "rate-limit", "max-items"}
	exposed := [][]string{
		{"backup"}, {"restore"}, {"reverse"}, {"rerun"}, {"doctor"},
		{"tree"}, {"image", "tree"}, {"health"}, {"report", "health"},
		{"network"}, {"logs"}, {"diff"}, {"prune"}, {"volumes"},
		{"registry"}, {"report", "all"},
	}
	for _, path := range exposed {
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Fatalf("Find(%v) error = %v", path, err)
		}
		for _, name := range runtimeFlags {
			if cmd.Flags().Lookup(name) == nil {
				t.Errorf("%s missing --%s", strings.Join(path, " "), name)
			}
		}
	}

	hidden := []struct {
		path  []string
		flags []string
	}{
		{path: []string{"version"}, flags: runtimeFlags},
		{path: []string{"config", "show"}, flags: runtimeFlags},
		{path: []string{"config", "validate"}, flags: runtimeFlags},
		{path: []string{"completion"}, flags: runtimeFlags},
		{path: []string{"image", "save"}, flags: runtimeFlags},
		{path: []string{"image", "load"}, flags: runtimeFlags},
		{path: []string{"pull"}, flags: []string{"operation-timeout", "rate-limit", "max-items"}},
		{path: []string{"image", "pull"}, flags: []string{"operation-timeout", "rate-limit", "max-items"}},
	}
	for _, test := range hidden {
		cmd, _, err := root.Find(test.path)
		if err != nil {
			t.Fatalf("Find(%v) error = %v", test.path, err)
		}
		for _, name := range test.flags {
			if cmd.Flags().Lookup(name) != nil {
				t.Errorf("%s unexpectedly exposes --%s", strings.Join(test.path, " "), name)
			}
		}
	}
}

func TestConfiguredRuntimeControllerOnlyAppliesToSupportedCommands(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "dm.yaml")
	if err := os.WriteFile(configPath, []byte("operation_concurrency: 2\noperation_timeout: 1m\noperation_rate_limit: 3\noperation_max_items: 4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		path       []string
		args       []string
		controlled bool
	}{
		{name: "pull config only", path: []string{"pull"}, controlled: true},
		{name: "health flags and config", path: []string{"health"}, args: []string{"--concurrency=5"}, controlled: true},
		{name: "version excluded", path: []string{"version"}},
		{name: "config excluded", path: []string{"config", "show"}, args: []string{"--format=json"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := appConfig{}
			opts := outputOptions{}
			root, lifecycle := newRootCommandWithLifecycle(&cfg, &opts)
			target, _, err := root.Find(test.path)
			if err != nil {
				t.Fatal(err)
			}
			target.RunE = func(cmd *cobra.Command, _ []string) error {
				controller, ok := runcontrol.FromContext(cmd.Context())
				if ok != test.controlled {
					return fmt.Errorf("controller present = %t, want %t", ok, test.controlled)
				}
				if !ok {
					return nil
				}
				limits := controller.Limits()
				wantConcurrency := 2
				if target.Name() == "health" {
					wantConcurrency = 5
				}
				if limits.Concurrency != wantConcurrency || limits.Timeout.String() != "1m0s" || limits.Rate != 3 || limits.MaxItems != 4 {
					return fmt.Errorf("runtime limits = %#v", limits)
				}
				return nil
			}
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			args := append([]string{"--config", configPath}, test.path...)
			args = append(args, test.args...)
			root.SetArgs(args)
			err = root.Execute()
			if finishErr := lifecycle.finish(err); finishErr != nil {
				err = errors.Join(err, finishErr)
			}
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
		})
	}
}

func TestRuntimeControllerDistinguishesDefaultAndExplicitZeroConcurrency(t *testing.T) {
	tests := []struct {
		name           string
		config         string
		wantPresent    bool
		wantConcurrent int
	}{
		{name: "omitted uses shared default", wantPresent: true, wantConcurrent: defaultOperationConcurrency},
		{name: "explicit zero disables shared limit", config: "operation_concurrency: 0\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "dm.yaml")
			if err := os.WriteFile(configPath, []byte(test.config), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := appConfig{}
			opts := outputOptions{}
			root, lifecycle := newRootCommandWithLifecycle(&cfg, &opts)
			target, _, err := root.Find([]string{"pull"})
			if err != nil {
				t.Fatal(err)
			}
			target.RunE = func(cmd *cobra.Command, _ []string) error {
				controller, ok := runcontrol.FromContext(cmd.Context())
				if ok != test.wantPresent {
					return fmt.Errorf("controller present = %t, want %t", ok, test.wantPresent)
				}
				if ok && controller.Limits().Concurrency != test.wantConcurrent {
					return fmt.Errorf("concurrency = %d, want %d", controller.Limits().Concurrency, test.wantConcurrent)
				}
				return nil
			}
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.SetArgs([]string{"--config", configPath, "pull"})
			err = root.Execute()
			if finishErr := lifecycle.finish(err); finishErr != nil {
				err = errors.Join(err, finishErr)
			}
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
		})
	}
}

func TestRootLifecycleSupportsRepeatedExecution(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "events.jsonl")
	configPath := filepath.Join(dir, "dm.yaml")
	config := fmt.Sprintf("audit_file: %q\noperation_concurrency: 1\n", filepath.ToSlash(auditPath))
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := appConfig{}
	opts := outputOptions{}
	root, lifecycle := newRootCommandWithLifecycle(&cfg, &opts)
	target, _, err := root.Find([]string{"health"})
	if err != nil {
		t.Fatal(err)
	}
	var runIDs []string
	target.RunE = func(cmd *cobra.Command, _ []string) error {
		if err := cmd.Context().Err(); err != nil {
			return fmt.Errorf("execution inherited canceled context: %w", err)
		}
		if _, ok := runcontrol.FromContext(cmd.Context()); !ok {
			return errors.New("runtime controller is missing")
		}
		session := audit.FromContext(cmd.Context())
		if session == nil {
			return errors.New("audit session is missing")
		}
		runIDs = append(runIDs, session.RunID())
		return nil
	}
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--config", configPath, "health"})
	for execution := 0; execution < 2; execution++ {
		err = root.Execute()
		if finishErr := lifecycle.finish(err); finishErr != nil {
			err = errors.Join(err, finishErr)
		}
		if err != nil {
			t.Fatalf("execution %d error = %v", execution+1, err)
		}
	}
	if len(runIDs) != 2 || runIDs[0] == runIDs[1] {
		t.Fatalf("run IDs = %#v, want two distinct runs", runIDs)
	}
	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 4 {
		t.Fatalf("audit event count = %d, want two start/finish pairs", len(lines))
	}
}

func TestRootLifecycleResetsDestructiveFlagsAcrossLeaves(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "dm.yaml")
	if err := os.WriteFile(configPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := appConfig{}
	opts := outputOptions{}
	root, lifecycle := newRootCommandWithLifecycle(&cfg, &opts)
	prune, _, err := root.Find([]string{"prune"})
	if err != nil {
		t.Fatal(err)
	}
	network, _, err := root.Find([]string{"network"})
	if err != nil {
		t.Fatal(err)
	}
	prune.RunE = func(cmd *cobra.Command, _ []string) error {
		apply, _ := cmd.Flags().GetBool("apply")
		confirm, _ := cmd.Flags().GetBool("confirm")
		only, _ := cmd.Flags().GetStringArray("only")
		if !apply || !confirm || !reflect.DeepEqual(only, []string{"image"}) {
			return fmt.Errorf("first prune flags = apply:%t confirm:%t only:%#v", apply, confirm, only)
		}
		return nil
	}
	network.RunE = func(_ *cobra.Command, _ []string) error {
		apply, _ := prune.Flags().GetBool("apply")
		confirm, _ := prune.Flags().GetBool("confirm")
		only, _ := prune.Flags().GetStringArray("only")
		if apply || confirm || len(only) != 0 {
			return fmt.Errorf("prune flags leaked into next leaf = apply:%t confirm:%t only:%#v", apply, confirm, only)
		}
		for _, name := range []string{"apply", "confirm", "only"} {
			if prune.Flags().Changed(name) {
				return fmt.Errorf("prune --%s remained changed", name)
			}
		}
		return nil
	}
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	execute := func(args ...string) {
		t.Helper()
		root.SetArgs(args)
		executeErr := root.Execute()
		if finishErr := lifecycle.finish(executeErr); finishErr != nil {
			executeErr = errors.Join(executeErr, finishErr)
		}
		if executeErr != nil {
			t.Fatalf("Execute(%q) error = %v", args, executeErr)
		}
	}
	execute("--config", configPath, "prune", "--apply", "--confirm", "--only", "image")
	execute("--config", configPath, "network")
}

func TestRootLifecycleResetsConfigDerivedFlagsToDefaults(t *testing.T) {
	dir := t.TempDir()
	configuredPath := filepath.Join(dir, "configured.yaml")
	configured := "fail_on: warning\nthresholds:\n  - health.issues=3\nmax_log_bytes: 1M\nmax_total_log_bytes: 2M\n"
	if err := os.WriteFile(configuredPath, []byte(configured), 0o600); err != nil {
		t.Fatal(err)
	}
	emptyPath := filepath.Join(dir, "empty.yaml")
	if err := os.WriteFile(emptyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := appConfig{}
	opts := outputOptions{}
	root, lifecycle := newRootCommandWithLifecycle(&cfg, &opts)
	health, _, err := root.Find([]string{"health"})
	if err != nil {
		t.Fatal(err)
	}
	type observedFlags struct {
		failOn      string
		thresholds  []string
		maxLogBytes string
		maxTotal    string
		changed     bool
	}
	var observations []observedFlags
	health.RunE = func(cmd *cobra.Command, _ []string) error {
		failOn, _ := cmd.Flags().GetString("fail-on")
		thresholds, _ := cmd.Flags().GetStringArray("threshold")
		maxLogBytes, _ := cmd.Flags().GetString("max-log-bytes")
		maxTotal, _ := cmd.Flags().GetString("max-total-log-bytes")
		observations = append(observations, observedFlags{
			failOn:      failOn,
			thresholds:  thresholds,
			maxLogBytes: maxLogBytes,
			maxTotal:    maxTotal,
			changed: cmd.Flags().Changed("fail-on") || cmd.Flags().Changed("threshold") ||
				cmd.Flags().Changed("max-log-bytes") || cmd.Flags().Changed("max-total-log-bytes"),
		})
		return nil
	}
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	for _, path := range []string{configuredPath, emptyPath} {
		root.SetArgs([]string{"--config", path, "health"})
		executeErr := root.Execute()
		if finishErr := lifecycle.finish(executeErr); finishErr != nil {
			executeErr = errors.Join(executeErr, finishErr)
		}
		if executeErr != nil {
			t.Fatalf("Execute(%q) error = %v", path, executeErr)
		}
	}
	want := []observedFlags{
		{failOn: "warning", thresholds: []string{"issues=3"}, maxLogBytes: "1M", maxTotal: "2M"},
		{failOn: "none", thresholds: []string{}, maxLogBytes: "16M", maxTotal: "256M"},
	}
	if !reflect.DeepEqual(observations, want) {
		t.Fatalf("observed flags = %#v, want %#v", observations, want)
	}
}

func TestRootLifecycleResetsSliceParserState(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "dm.yaml")
	if err := os.WriteFile(configPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := appConfig{}
	opts := outputOptions{}
	root, lifecycle := newRootCommandWithLifecycle(&cfg, &opts)
	health, _, err := root.Find([]string{"health"})
	if err != nil {
		t.Fatal(err)
	}
	var observations [][]string
	health.RunE = func(cmd *cobra.Command, _ []string) error {
		keywords, getErr := cmd.Flags().GetStringArray("keyword")
		if getErr != nil {
			return getErr
		}
		observations = append(observations, append([]string(nil), keywords...))
		return nil
	}
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	for _, args := range [][]string{
		{"--config", configPath, "health", "--keyword", "alpha", "--keyword", "beta"},
		{"--config", configPath, "health", "--keyword", "gamma"},
	} {
		root.SetArgs(args)
		executeErr := root.Execute()
		if finishErr := lifecycle.finish(executeErr); finishErr != nil {
			executeErr = errors.Join(executeErr, finishErr)
		}
		if executeErr != nil {
			t.Fatalf("Execute(%q) error = %v", args, executeErr)
		}
	}
	want := [][]string{{"alpha", "beta"}, {"gamma"}}
	if !reflect.DeepEqual(observations, want) {
		t.Fatalf("keywords = %#v, want %#v", observations, want)
	}
}

func TestRootLifecycleResetsCobraHelpFlag(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "dm.yaml")
	if err := os.WriteFile(configPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := appConfig{}
	opts := outputOptions{}
	root, lifecycle := newRootCommandWithLifecycle(&cfg, &opts)
	health, _, err := root.Find([]string{"health"})
	if err != nil {
		t.Fatal(err)
	}
	runs := 0
	health.RunE = func(_ *cobra.Command, _ []string) error {
		runs++
		return nil
	}
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	for _, args := range [][]string{
		{"--config", configPath, "health", "--help"},
		{"--config", configPath, "health"},
	} {
		root.SetArgs(args)
		executeErr := root.Execute()
		if finishErr := lifecycle.finish(executeErr); finishErr != nil {
			executeErr = errors.Join(executeErr, finishErr)
		}
		if executeErr != nil {
			t.Fatalf("Execute(%q) error = %v", args, executeErr)
		}
	}
	if runs != 1 {
		t.Fatalf("health RunE calls = %d, want 1 after help execution", runs)
	}
}

func TestUnknownCommandWithHelpFailsWithoutPoisoningReusedRoot(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "dm.yaml")
	if err := os.WriteFile(configPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := appConfig{}
	opts := outputOptions{}
	root, lifecycle := newRootCommandWithLifecycle(&cfg, &opts)
	health, _, err := root.Find([]string{"health"})
	if err != nil {
		t.Fatal(err)
	}
	runs := 0
	health.RunE = func(_ *cobra.Command, _ []string) error {
		runs++
		return nil
	}
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})

	for _, args := range [][]string{
		{"--config", configPath, "logs-scan", "--help"},
		{"--config", configPath, "--help", "logs-scan"},
	} {
		root.SetArgs(args)
		unknownErr := root.Execute()
		if unknownErr == nil {
			t.Fatalf("Execute(%q) error = nil", args)
		}
		if !strings.Contains(unknownErr.Error(), `unknown command "logs-scan"`) {
			t.Fatalf("Execute(%q) error = %v", args, unknownErr)
		}
		if code := commandExitCode(unknownErr); code == 0 {
			t.Fatalf("Execute(%q) exit code = %d, want non-zero", args, code)
		}
		if finishErr := lifecycle.finish(unknownErr); finishErr != nil {
			t.Fatalf("finish unknown command %q: %v", args, finishErr)
		}
	}

	for _, args := range [][]string{
		{"--config", configPath, "health", "--help"},
		{"--config", configPath, "health"},
	} {
		root.SetArgs(args)
		executeErr := root.Execute()
		if finishErr := lifecycle.finish(executeErr); finishErr != nil {
			executeErr = errors.Join(executeErr, finishErr)
		}
		if executeErr != nil {
			t.Fatalf("Execute(%q) error = %v", args, executeErr)
		}
	}
	if runs != 1 {
		t.Fatalf("health RunE calls = %d, want 1 after normal help", runs)
	}
}

func TestExplicitAuditRecordsFailuresBeforePersistentPreRun(t *testing.T) {
	dir := t.TempDir()
	malformedConfig := filepath.Join(dir, "malformed.yaml")
	if err := os.WriteFile(malformedConfig, []byte("unknown_e2_field: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		args      func(string) []string
		operation string
	}{
		{
			name: "malformed config",
			args: func(auditPath string) []string {
				return []string{"--audit-file", auditPath, "--audit-required", "--config", malformedConfig, "version"}
			},
			operation: "version",
		},
		{
			name: "missing arguments",
			args: func(auditPath string) []string {
				return []string{"--audit-file", auditPath, "--audit-required", "backup"}
			},
			operation: "backup",
		},
		{
			name: "unknown flag",
			args: func(auditPath string) []string {
				return []string{"--audit-file", auditPath, "--audit-required", "version", "--unknown-e2-flag"}
			},
			operation: "version",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			auditPath := filepath.Join(t.TempDir(), "events.jsonl")
			cfg := appConfig{}
			opts := outputOptions{}
			root, lifecycle := newRootCommandWithLifecycle(&cfg, &opts)
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.SetArgs(test.args(auditPath))
			executeErr := root.Execute()
			if executeErr == nil {
				t.Fatal("Execute() error = nil, want failure")
			}
			if finishErr := lifecycle.finish(executeErr); finishErr != nil {
				t.Fatalf("finish() error = %v", finishErr)
			}

			data, err := os.ReadFile(auditPath)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) != 2 {
				t.Fatalf("audit event count = %d, want start and finish", len(lines))
			}
			var start, finish audit.Event
			if err := json.Unmarshal([]byte(lines[0]), &start); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(lines[1]), &finish); err != nil {
				t.Fatal(err)
			}
			if start.Type != audit.EventCommandStart || finish.Type != audit.EventCommandFinish {
				t.Fatalf("audit event types = %q/%q", start.Type, finish.Type)
			}
			if start.Operation != test.operation || finish.Operation != test.operation || finish.Outcome != audit.OutcomeFailed {
				t.Fatalf("audit lifecycle = start:%#v finish:%#v", start, finish)
			}
		})
	}
}

func TestConfiguredAuditRecordsRuntimePreparationFailures(t *testing.T) {
	tests := []struct {
		name        string
		profile     string
		config      func(auditPath, unusedPath string) string
		wantProfile string
	}{
		{
			name: "base audit config",
			config: func(auditPath, _ string) string {
				return fmt.Sprintf("audit_file: %q\naudit_on_error: fail\n", filepath.ToSlash(auditPath))
			},
		},
		{
			name:    "profile audit config",
			profile: "production",
			config: func(auditPath, unusedPath string) string {
				return fmt.Sprintf("audit_file: %q\naudit_on_error: fail\nprofiles:\n  production:\n    audit_file: %q\n", filepath.ToSlash(unusedPath), filepath.ToSlash(auditPath))
			},
			wantProfile: "production",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			auditPath := filepath.Join(dir, "events.jsonl")
			unusedPath := filepath.Join(dir, "unused-base.jsonl")
			configPath := filepath.Join(dir, "dm.yaml")
			if err := os.WriteFile(configPath, []byte(test.config(auditPath, unusedPath)), 0o600); err != nil {
				t.Fatal(err)
			}

			cfg := appConfig{}
			root, lifecycle := newRootCommandWithLifecycle(&cfg, &outputOptions{})
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			args := []string{"--config", configPath}
			if test.profile != "" {
				args = append(args, "--profile", test.profile)
			}
			args = append(args, "health", "--concurrency=65")
			root.SetArgs(args)
			executeErr := root.Execute()
			if executeErr == nil {
				t.Fatal("Execute() error = nil, want runtime validation failure")
			}
			if finishErr := lifecycle.finish(executeErr); finishErr != nil {
				t.Fatalf("finish() error = %v", finishErr)
			}

			events := readAuditLifecycleEvents(t, auditPath)
			if len(events) != 2 {
				t.Fatalf("audit event count = %d, want 2", len(events))
			}
			if events[0].Type != audit.EventCommandStart || events[1].Type != audit.EventCommandFinish {
				t.Fatalf("audit event types = %q/%q, want start/finish", events[0].Type, events[1].Type)
			}
			if events[0].Operation != "health" || events[0].Profile != test.wantProfile || events[1].Outcome != audit.OutcomeFailed {
				t.Fatalf("audit lifecycle = start:%#v finish:%#v", events[0], events[1])
			}
			if _, err := os.Stat(unusedPath); !os.IsNotExist(err) {
				t.Fatalf("unused base audit path stat error = %v, want not exist", err)
			}
		})
	}
}

func TestAuditFallbackDoesNotReusePreviousExecutionConfig(t *testing.T) {
	dir := t.TempDir()
	configuredAuditPath := filepath.Join(dir, "configured.jsonl")
	explicitAuditPath := filepath.Join(dir, "explicit.jsonl")
	validConfigPath := filepath.Join(dir, "valid.yaml")
	validConfig := fmt.Sprintf("audit_file: %q\naudit_on_error: fail\n", filepath.ToSlash(configuredAuditPath))
	if err := os.WriteFile(validConfigPath, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	malformedConfigPath := filepath.Join(dir, "malformed.yaml")
	if err := os.WriteFile(malformedConfigPath, []byte("unknown_e2_field: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := appConfig{}
	root, lifecycle := newRootCommandWithLifecycle(&cfg, &outputOptions{})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	execute := func(args []string, wantErr bool) {
		t.Helper()
		root.SetArgs(args)
		executeErr := root.Execute()
		if wantErr != (executeErr != nil) {
			t.Fatalf("Execute(%q) error = %v, wantErr=%v", args, executeErr, wantErr)
		}
		if finishErr := lifecycle.finish(executeErr); finishErr != nil {
			t.Fatalf("finish(%q) error = %v", args, finishErr)
		}
	}

	execute([]string{"--config", validConfigPath, "version"}, false)
	if events := readAuditLifecycleEvents(t, configuredAuditPath); len(events) != 2 {
		t.Fatalf("configured audit event count = %d, want 2", len(events))
	}
	execute([]string{"--config", malformedConfigPath, "version"}, true)
	if events := readAuditLifecycleEvents(t, configuredAuditPath); len(events) != 2 {
		t.Fatalf("stale configured audit event count = %d, want 2", len(events))
	}
	execute([]string{"--audit-file", explicitAuditPath, "--audit-required", "--config", malformedConfigPath, "version"}, true)
	events := readAuditLifecycleEvents(t, explicitAuditPath)
	if len(events) != 2 || events[0].Type != audit.EventCommandStart || events[1].Type != audit.EventCommandFinish || events[1].Outcome != audit.OutcomeFailed {
		t.Fatalf("explicit fallback audit lifecycle = %#v", events)
	}
}

func readAuditLifecycleEvents(t *testing.T, path string) []audit.Event {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	events := make([]audit.Event, len(lines))
	for index, line := range lines {
		if err := json.Unmarshal([]byte(line), &events[index]); err != nil {
			t.Fatalf("decode audit event %d: %v", index, err)
		}
	}
	return events
}

func TestRootLifecycleRecordsEffectiveProfileAndConfiguredActor(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "events.jsonl")
	configPath := filepath.Join(dir, "dm.yaml")
	config := fmt.Sprintf("default_profile: production\naudit_file: %q\naudit_actor: configured-operator\nprofiles:\n  production: {}\n", filepath.ToSlash(auditPath))
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := appConfig{}
	opts := outputOptions{}
	root, lifecycle := newRootCommandWithLifecycle(&cfg, &opts)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--config", configPath, "version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if err := lifecycle.finish(nil); err != nil {
		t.Fatalf("finish() error = %v", err)
	}
	if err := lifecycle.finish(nil); err != nil {
		t.Fatalf("second finish() error = %v", err)
	}

	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("audit event count = %d, want start and finish", len(lines))
	}
	var start audit.Event
	if err := json.Unmarshal([]byte(lines[0]), &start); err != nil {
		t.Fatal(err)
	}
	if start.Profile != "production" || start.Operator.AssertedActor != "configured-operator" {
		t.Fatalf("audit metadata = profile:%q actor:%q", start.Profile, start.Operator.AssertedActor)
	}
}

func TestAuditStartHonorsCommandCancellation(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "events.jsonl")
	cfg := appConfig{AuditFile: auditPath, AuditOnError: "fail"}
	cmd := &cobra.Command{Use: "probe"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd.SetContext(ctx)
	lifecycle := &rootLifecycle{}

	err := initializeAuditSession(cmd, &cfg, "", &auditConfigOptions{}, lifecycle)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("initializeAuditSession() error = %v, want context.Canceled", err)
	}
	if lifecycle.session != nil || lifecycle.sink != nil {
		t.Fatalf("failed start retained lifecycle resources: %#v", lifecycle)
	}
}

type emptyErrorGroup struct{}

func (emptyErrorGroup) Error() string   { return "empty error group" }
func (emptyErrorGroup) Unwrap() []error { return nil }
