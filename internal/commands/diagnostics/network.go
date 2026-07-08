package diagnostics

import (
	"context"
	"fmt"
	"io"

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
			report, err := runNetworkReport(cmd.Context(), runOpts)
			if err != nil {
				return fmt.Errorf("生成网络报告失败: %w", err)
			}
			return rpt.Print(cmd.OutOrStdout(), runOpts.Format, report, func(w io.Writer) {
				printNetworkReport(w, report)
			})
		},
		ValidArgsFunction: completion.LocalContainers,
	}
	commandflags.AddContainerFilterFlags(cmd, &opts.RunningOnly, &opts.ContainerFilters, "只查看正在运行的容器")
	commandflags.AddReportFormatFlag(cmd, &opts.Format)
	return cmd
}

func runNetworkReport(ctx context.Context, opts NetworkOptions) (NetworkReport, error) {
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
	report.Target = buildContainerTargetSelection("查看", len(containers), opts.RunningOnly, opts.ContainerFilters)
	report.Warnings = append(report.Warnings, inspectWarnings...)
	report.Warnings = append(report.Warnings, networkWarnings...)
	return report, nil
}
