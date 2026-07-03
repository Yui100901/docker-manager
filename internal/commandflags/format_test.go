package commandflags

import (
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
