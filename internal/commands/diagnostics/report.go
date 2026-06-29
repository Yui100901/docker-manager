package diagnostics

import "github.com/spf13/cobra"

func NewReportCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Docker 诊断报告工具",
	}

	cmd.AddCommand(
		NewHealthCommand(),
		NewNetworkCommand(),
		NewLogsScanCommand(),
		NewInspectDiffCommand(),
		NewPruneReportCommand(),
		NewVolumesReportCommand(),
		NewRegistryReportCommand(),
	)
	return cmd
}
