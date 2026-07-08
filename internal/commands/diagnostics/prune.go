package diagnostics

import (
	"context"
	"fmt"
	"io"

	"docker-manager/internal/commandflags"
	"docker-manager/internal/docker"
	rpt "docker-manager/internal/report"

	"github.com/spf13/cobra"
)

func NewPruneReportCommand() *cobra.Command {
	opts := PruneReportOptions{}
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "生成 Docker 可清理资源报告，可选执行清理",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.Apply && docker.IsRemoteEndpoint() {
				fmt.Fprintf(cmd.OutOrStdout(), "Target Docker: %s\n", docker.Endpoint())
			}
			report, err := runPruneReport(cmd.Context(), opts)
			if err != nil {
				return fmt.Errorf("生成清理报告失败: %w", err)
			}
			return rpt.Print(cmd.OutOrStdout(), opts.Format, report, func(w io.Writer) {
				printPruneReport(w, report)
			})
		},
	}
	cmd.Flags().BoolVar(&opts.Apply, "apply", false, "根据报告执行清理")
	cmd.Flags().BoolVar(&opts.Confirm, "confirm", false, "确认执行 --apply 清理操作")
	commandflags.AddPruneScopeFlags(cmd, &opts.Only, &opts.Filters, &opts.Until, &opts.ProtectLabels)
	commandflags.AddReportFormatFlag(cmd, &opts.Format)
	return cmd
}

func runPruneReport(ctx context.Context, opts PruneReportOptions) (PruneReport, error) {
	if err := ctx.Err(); err != nil {
		return PruneReport{}, err
	}
	scope, err := buildPruneScope(opts)
	if err != nil {
		return PruneReport{}, err
	}
	// Keep destructive cleanup behind an explicit confirmation even when the
	// report scope is narrow; dry-run/report output remains the default path.
	if opts.Apply && !opts.Confirm {
		message := "report prune --apply 会删除 Docker 资源"
		if docker.IsRemoteEndpoint() {
			message += "；目标 Docker: " + docker.Endpoint()
		}
		return PruneReport{}, fmt.Errorf("%s；如确认执行，请添加 --confirm", message)
	}
	svc, err := newPruneDockerService()
	if err != nil {
		return PruneReport{}, err
	}
	usage, err := svc.DiskUsage(ctx, pruneDiskUsageOptions(scope))
	if err != nil {
		return PruneReport{}, err
	}
	if err := ctx.Err(); err != nil {
		return PruneReport{}, err
	}

	var volumeRefs map[string][]VolumeContainerRef
	var volumeWarnings []string
	if scope.includes(pruneKindVolume) && len(usage.Volumes) > 0 {
		volumeRefs, volumeWarnings, err = inspectPruneVolumeRefs(ctx, svc)
		if err != nil {
			return PruneReport{}, err
		}
	}

	report, err := buildPruneReportWithVolumeRefs(ctx, usage, scope, volumeRefs, volumeWarnings)
	if err != nil {
		return report, err
	}
	if opts.Apply {
		if err := ensurePruneVolumeCandidatesStillUnreferenced(ctx, svc, report.UnusedVolumes); err != nil {
			return report, err
		}
		applyResult, err := applyPruneReport(ctx, svc, scope)
		if err != nil {
			return report, err
		}
		report.Applied = true
		report.ApplyResult = &applyResult
	}
	return report, nil
}
