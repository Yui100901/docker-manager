package commandflags

import (
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestAddReportFormatFlagDescribesBusinessReportOutput(t *testing.T) {
	var format string
	cmd := &cobra.Command{Use: "demo"}
	AddReportFormatFlag(cmd, &format)
	flag := cmd.Flags().Lookup("format")
	if flag == nil {
		t.Fatal("format flag missing")
	}
	if !strings.Contains(flag.Usage, "业务报告输出格式") {
		t.Fatalf("format usage = %q, want business report wording", flag.Usage)
	}
}

func TestAddAutomationReportFlagsIncludesSARIFAndThresholdCompletion(t *testing.T) {
	var format string
	var options AutomationOptions
	cmd := &cobra.Command{Use: "demo"}
	AddAutomationReportFlags(cmd, &format, &options, []string{"issues", "reclaimable_size"})
	for _, name := range []string{"format", "fail-on", "threshold"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("missing flag %q", name)
		}
	}
	if got := cmd.Flags().Lookup("format").Usage; !strings.Contains(got, "sarif") {
		t.Fatalf("format usage = %q, want sarif", got)
	}
	completion, ok := cmd.GetFlagCompletionFunc("threshold")
	if !ok {
		t.Fatal("threshold completion missing")
	}
	values, directive := completion(cmd, nil, "iss")
	if directive != cobra.ShellCompDirectiveNoFileComp || !reflect.DeepEqual(values, []string{"issues="}) {
		t.Fatalf("threshold completion = %#v/%v", values, directive)
	}
}
