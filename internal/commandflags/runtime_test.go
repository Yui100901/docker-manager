package commandflags

import (
	"testing"
	"time"

	"docker-manager/internal/runcontrol"

	"github.com/spf13/cobra"
)

func TestRuntimeFlagsAndDefaults(t *testing.T) {
	limits := runcontrol.Limits{Concurrency: 8, MaxItems: 10_000}
	cmd := &cobra.Command{Use: "test"}
	AddRuntimeFlags(cmd, &limits)
	cmd.SetArgs([]string{"--concurrency", "3", "--operation-timeout", "2m", "--rate-limit", "4.5"})
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		resolved, err := ResolveRuntimeLimits(cmd, limits, func() runcontrol.Limits {
			return runcontrol.Limits{Concurrency: 12, Timeout: time.Hour, Rate: 20, MaxItems: 500}
		})
		if err != nil {
			return err
		}
		if resolved.Concurrency != 3 || resolved.Timeout != 2*time.Minute || resolved.Rate != 4.5 || resolved.MaxItems != 500 {
			t.Fatalf("resolved limits = %#v", resolved)
		}
		return nil
	}
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestResolveRuntimeLimitsValidatesExplicitValues(t *testing.T) {
	limits := runcontrol.Limits{}
	cmd := &cobra.Command{Use: "test"}
	AddRuntimeFlags(cmd, &limits)
	cmd.SetArgs([]string{"--concurrency", "65"})
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		_, err := ResolveRuntimeLimits(cmd, limits, nil)
		return err
	}
	if err := cmd.Execute(); err == nil {
		t.Fatal("Execute() error = nil, want concurrency rejection")
	}
}
