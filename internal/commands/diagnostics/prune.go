package diagnostics

import (
	"context"
	"fmt"
	"io"

	"docker-manager/internal/audit"
	"docker-manager/internal/commandflags"
	"docker-manager/internal/docker"
	rpt "docker-manager/internal/report"
	"docker-manager/internal/runcontrol"

	"github.com/spf13/cobra"
)

func NewPruneReportCommand() *cobra.Command {
	opts := PruneReportOptions{}
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "生成 Docker 可清理资源报告，可选执行清理",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			policy, err := prepareReportAutomation(opts.Format, opts.AutomationOptions, pruneMetricDefinitions)
			if err != nil {
				return err
			}
			var printedEvaluation *rpt.Evaluation
			printReport := func(report PruneReport) error {
				data := pruneAutomationData(report)
				evaluation := automationEvaluation(policy, data, !report.Applied || report.ApplyResult == nil || (report.ApplyResult != nil && len(report.ApplyResult.Failures) == 0 && len(report.ApplyResult.UnknownOutcomes) == 0))
				if exposeAutomationEvaluation(opts.Format, policy) {
					report.Evaluation = evaluation
				}
				printedEvaluation = evaluation
				renderEvaluation := evaluation
				if !exposeAutomationEvaluation(opts.Format, policy) {
					renderEvaluation = nil
				}
				return rpt.PrintEvaluated(cmd.OutOrStdout(), opts.Format, report, renderEvaluation, func(w io.Writer) {
					printPruneReport(w, report)
				})
			}
			if opts.Apply && docker.IsRemoteEndpoint() {
				fmt.Fprintf(cmd.ErrOrStderr(), "Target Docker: %s\n", docker.Endpoint())
			}
			report, err := runPruneReport(cmd.Context(), opts)
			if err != nil {
				if report.Applied && report.ApplyResult != nil {
					if printErr := printReport(report); printErr != nil {
						return fmt.Errorf("执行清理失败: %w；输出部分执行结果失败: %v", err, printErr)
					}
					return fmt.Errorf("执行清理失败: %w", err)
				}
				return fmt.Errorf("生成清理报告失败: %w", err)
			}
			if printErr := printReport(report); printErr != nil {
				return printErr
			}
			if printedEvaluation != nil {
				return printedEvaluation.GateError()
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&opts.Apply, "apply", false, "根据报告执行清理")
	cmd.Flags().BoolVar(&opts.Confirm, "confirm", false, "确认执行 --apply 清理操作")
	cmd.Flags().BoolVar(&opts.AllowNonAtomicDelete, "allow-non-atomic-delete", false, "显式接受 Docker 对 image/volume 不提供 compare-and-delete 的竞态边界")
	cmd.Flags().StringArrayVar(&opts.Only, "only", nil, "只处理指定资源类型，可重复指定: container | image | volume | build-cache")
	cmd.Flags().StringArrayVarP(&opts.Filters, "filter", "f", nil, "清理筛选条件，支持 label=key、label=key=value、label!=key、until=<duration|timestamp>，可重复指定")
	cmd.Flags().StringArrayVar(&opts.UntilValues, "until", nil, "仅清理该时间之前创建的资源，例如 24h、168h 或 RFC3339 时间；重复值必须一致")
	cmd.Flags().StringArrayVar(&opts.ProtectLabels, "protect-label", nil, "保护带有指定 label 的资源，例如 keep 或 env=prod，可重复指定")
	commandflags.AddAutomationReportFlags(cmd, &opts.Format, &opts.AutomationOptions, automationMetricNames(pruneMetricDefinitions))
	return cmd
}

func runPruneReport(ctx context.Context, opts PruneReportOptions) (PruneReport, error) {
	if _, err := prepareReportAutomation(opts.Format, opts.AutomationOptions, pruneMetricDefinitions); err != nil {
		return PruneReport{}, err
	}
	if err := ctx.Err(); err != nil {
		return PruneReport{}, err
	}
	if err := validatePruneReportFormat(opts.Format); err != nil {
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
	if err := checkPruneUsageItems(ctx, scope, usage); err != nil {
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
	report.NonAtomicDeleteAcknowledged = opts.AllowNonAtomicDelete
	if hasNonAtomicPruneCandidates(report) {
		report.Warnings = append(report.Warnings, "Docker API 不提供 image/volume compare-and-delete；apply 默认拒绝这些候选，只有明确接受竞态边界后才可执行")
	}
	if opts.Apply {
		if hasNonAtomicPruneCandidates(report) && !opts.AllowNonAtomicDelete {
			return report, fmt.Errorf("清理快照包含 image 或 volume 候选，但 Docker API 不支持 compare-and-delete；未执行任何删除，请复核 dry-run 后显式添加 --allow-non-atomic-delete")
		}
		if session := audit.FromContext(ctx); session != nil {
			candidates := pruneAuditCandidates(report)
			if _, err := session.AuthorizeMutation(ctx, audit.MutationRequest{
				Scope:        audit.MutationDockerPersistent,
				Confirmation: audit.Confirmation{Required: true, Provided: opts.Confirm, Mechanism: "--apply+--confirm"},
				Candidates:   candidates,
			}); err != nil {
				return report, fmt.Errorf("审计授权失败，未执行任何删除: %w", err)
			}
		}
		applyResult, err := applyPruneReport(ctx, svc, report)
		report.Applied = true
		report.ApplyResult = &applyResult
		if err != nil {
			return report, err
		}
	}
	return report, nil
}

// checkPruneUsageItems accounts for the snapshot entries that the report
// builder will inspect. A single reservation keeps the multi-kind check
// atomic, so a failed budget check cannot partially consume the allowance.
func checkPruneUsageItems(ctx context.Context, scope PruneScope, usage pruneDiskUsage) error {
	count := 0
	if scope.includes(pruneKindContainer) {
		count += len(usage.Containers)
	}
	if scope.includes(pruneKindImage) {
		count += len(usage.Images)
	}
	if scope.includes(pruneKindVolume) {
		count += len(usage.Volumes)
	}
	if scope.includesBuildCache() {
		count += len(usage.BuildCache)
	}
	return runcontrol.CheckItems(ctx, "prune", count)
}

func pruneAuditCandidates(report PruneReport) []audit.CandidateInput {
	result := make([]audit.CandidateInput, 0, len(report.StoppedContainers)+len(report.DanglingImages)+len(report.UnusedVolumes)+len(report.BuildCaches))
	for _, item := range report.StoppedContainers {
		result = append(result, audit.CandidateInput{Kind: "container", Action: "delete", Identifier: item.ID, Display: item.Name})
	}
	for _, item := range report.DanglingImages {
		result = append(result, audit.CandidateInput{Kind: "image", Action: "delete", Identifier: item.ID})
	}
	for _, item := range report.UnusedVolumes {
		result = append(result, audit.CandidateInput{Kind: "volume", Action: "delete", Identifier: item.Name, Display: item.Name})
	}
	for _, item := range report.BuildCaches {
		result = append(result, audit.CandidateInput{Kind: "build-cache", Action: "delete", Identifier: item.ID})
	}
	return result
}

func hasNonAtomicPruneCandidates(report PruneReport) bool {
	return len(report.DanglingImages) > 0 || len(report.UnusedVolumes) > 0
}

func validatePruneReportFormat(format string) error {
	return rpt.ValidateFormat(format, true)
}
