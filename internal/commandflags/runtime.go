package commandflags

import (
	"docker-manager/internal/runcontrol"

	"github.com/spf13/cobra"
)

type RuntimeDefaults func() runcontrol.Limits

func AddRuntimeFlags(cmd *cobra.Command, limits *runcontrol.Limits) {
	cmd.Flags().IntVar(&limits.Concurrency, "concurrency", limits.Concurrency, "只读资源任务并发数，0 表示关闭额外并发限制")
	cmd.Flags().DurationVar(&limits.Timeout, "operation-timeout", limits.Timeout, "整条命令的最长运行时间，0 表示不增加外层超时")
	cmd.Flags().Float64Var(&limits.Rate, "rate-limit", limits.Rate, "每秒启动的只读资源任务数，0 表示不限速")
	cmd.Flags().IntVar(&limits.MaxItems, "max-items", limits.MaxItems, "整条命令累计处理的资源项上限，0 表示不增加上限")
}

// ResolveRuntimeLimits applies complete command defaults to fields that were
// not explicitly set on the command line, then validates the result.
func ResolveRuntimeLimits(cmd *cobra.Command, current runcontrol.Limits, defaults RuntimeDefaults) (runcontrol.Limits, error) {
	resolved := current
	if defaults != nil {
		configured := defaults()
		flags := cmd.Flags()
		if !flags.Changed("concurrency") {
			resolved.Concurrency = configured.Concurrency
		}
		if !flags.Changed("operation-timeout") {
			resolved.Timeout = configured.Timeout
		}
		if !flags.Changed("rate-limit") {
			resolved.Rate = configured.Rate
		}
		if !flags.Changed("max-items") {
			resolved.MaxItems = configured.MaxItems
		}
	}
	return resolved, resolved.Validate()
}
