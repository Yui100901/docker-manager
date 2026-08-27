package commandflags

import (
	"docker-manager/internal/completion"
	"docker-manager/internal/report"

	"github.com/spf13/cobra"
)

type FormatOptions struct {
	Format string
}

type AutomationOptions struct {
	FailOn     string
	Thresholds []string
}

// AddReportFormatFlag wires the shared report output flag at command-construction time.
// Keeping this helper outside internal/report lets the renderer package stay free of Cobra.
func AddReportFormatFlag(cmd *cobra.Command, format *string) {
	addReportFormatFlag(cmd, format, false)
}

func AddAutomationReportFlags(cmd *cobra.Command, format *string, options *AutomationOptions, metrics []string) {
	addReportFormatFlag(cmd, format, true)
	cmd.Flags().StringVar(&options.FailOn, "fail-on", report.LevelNone.String(), "报告门禁级别: none | note | warning | error")
	cmd.Flags().StringArrayVar(&options.Thresholds, "threshold", nil, "指标最大允许值，使用 metric=max，可重复指定")
	_ = cmd.RegisterFlagCompletionFunc("fail-on", completion.FixedValues(
		report.LevelNone.String(),
		report.LevelNote.String(),
		report.LevelWarning.String(),
		report.LevelError.String(),
	))
	_ = cmd.RegisterFlagCompletionFunc("threshold", thresholdCompletion(metrics))
}

func addReportFormatFlag(cmd *cobra.Command, format *string, sarif bool) {
	help := "业务报告输出格式: text | json | markdown | html"
	formats := []string{report.FormatText, report.FormatJSON, report.FormatMarkdown, "md", report.FormatHTML}
	if sarif {
		help += " | sarif"
		formats = append(formats, report.FormatSARIF)
	}
	cmd.Flags().StringVar(format, "format", report.FormatText, help)
	_ = cmd.RegisterFlagCompletionFunc("format", completion.FixedValues(formats...))
}
