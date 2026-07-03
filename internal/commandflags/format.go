package commandflags

import (
	"docker-manager/internal/completion"
	"docker-manager/internal/report"

	"github.com/spf13/cobra"
)

type FormatOptions struct {
	Format string
}

// AddReportFormatFlag wires the shared report output flag at command-construction time.
// Keeping this helper outside internal/report lets the renderer package stay free of Cobra.
func AddReportFormatFlag(cmd *cobra.Command, format *string) {
	cmd.Flags().StringVar(format, "format", report.FormatText, "业务报告输出格式: text | json | markdown | html")
	_ = cmd.RegisterFlagCompletionFunc("format", completion.FixedValues(report.FormatText, report.FormatJSON, report.FormatMarkdown, "md", report.FormatHTML))
}
