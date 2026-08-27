package diagnostics

import (
	"context"
	"fmt"
	"io"
	"time"

	"docker-manager/internal/commandflags"
	"docker-manager/internal/completion"
	"docker-manager/internal/docker"
	rpt "docker-manager/internal/report"

	"github.com/spf13/cobra"
)

func NewNetworkCommand() *cobra.Command {
	opts := NetworkOptions{}
	cmd := &cobra.Command{
		Use:   "network [container-pattern...]",
		Short: "查看容器网络关系、端口映射和网络风险",
		RunE: func(cmd *cobra.Command, args []string) error {
			runOpts := opts
			runOpts.ContainerFilters = append(append([]string(nil), opts.ContainerFilters...), args...)
			policy, err := prepareReportAutomation(runOpts.Format, runOpts.AutomationOptions, networkMetricDefinitions)
			if err != nil {
				return err
			}
			report, err := runNetworkReport(cmd.Context(), runOpts)
			if err != nil {
				return fmt.Errorf("生成网络报告失败: %w", err)
			}
			evaluation := automationEvaluation(policy, networkAutomationData(report), true)
			if exposeAutomationEvaluation(runOpts.Format, policy) {
				report.Evaluation = evaluation
			}
			renderEvaluation := evaluation
			if !exposeAutomationEvaluation(runOpts.Format, policy) {
				renderEvaluation = nil
			}
			if err := rpt.PrintEvaluated(cmd.OutOrStdout(), runOpts.Format, report, renderEvaluation, func(w io.Writer) {
				printNetworkReport(w, report)
			}); err != nil {
				return err
			}
			return evaluation.GateError()
		},
		ValidArgsFunction: completion.LocalContainers,
	}
	commandflags.AddContainerFilterFlags(cmd, &opts.RunningOnly, &opts.ContainerFilters, "只查看正在运行的容器")
	commandflags.AddAutomationReportFlags(cmd, &opts.Format, &opts.AutomationOptions, automationMetricNames(networkMetricDefinitions))
	return cmd
}

func runNetworkReport(ctx context.Context, opts NetworkOptions) (NetworkReport, error) {
	if _, err := prepareReportAutomation(opts.Format, opts.AutomationOptions, networkMetricDefinitions); err != nil {
		return NetworkReport{}, err
	}
	svc, err := newNetworkDockerService()
	if err != nil {
		return NetworkReport{}, err
	}
	containers, err := svc.ListContainers(ctx, !opts.RunningOnly)
	if err != nil {
		return NetworkReport{}, err
	}
	hasContainerFilter := len(opts.ContainerFilters) > 0
	containers = filterContainerSummaries(containers, opts.ContainerFilters)
	inspectByID, inspectWarnings, err := inspectNetworkContainers(ctx, svc, containers)
	if err != nil {
		return NetworkReport{}, err
	}
	networks, err := svc.ListNetworks(ctx)
	if err != nil {
		return NetworkReport{}, err
	}
	if hasContainerFilter {
		networks = filterNetworksForContainersWithInspect(networks, containers, inspectByID)
	}
	inspectedNetworks, networkWarnings, err := inspectNetworks(ctx, svc, networks)
	if err != nil {
		return NetworkReport{}, err
	}
	report := buildNetworkReportDetailed(containers, inspectByID, inspectedNetworks)
	report.DockerEndpoint = docker.Endpoint()
	report.GeneratedAt = time.Now().Format(time.RFC3339)
	report.Target = buildContainerTargetSelection("查看", len(containers), opts.RunningOnly, opts.ContainerFilters)
	report.Warnings = append(report.Warnings, inspectWarnings...)
	report.Warnings = append(report.Warnings, networkWarnings...)
	return report, nil
}
