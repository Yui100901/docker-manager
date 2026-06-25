package cli

import (
	"fmt"
	"io"
	"runtime"

	"github.com/spf13/cobra"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

type VersionInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	GoVersion string `json:"go_version"`
	GOOS      string `json:"goos"`
	GOARCH    string `json:"goarch"`
}

func newVersionCommand() *cobra.Command {
	opts := ReportFormatOptions{}
	cmd := &cobra.Command{
		Use:   "version",
		Short: "输出 dm 版本、commit 和构建信息",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			info := currentVersionInfo()
			return printReport(cmd.OutOrStdout(), opts.Format, info, func(w io.Writer) {
				printVersionInfo(w, info)
			})
		},
	}
	addReportFormatFlag(cmd, &opts.Format)
	return cmd
}

func currentVersionInfo() VersionInfo {
	return VersionInfo{
		Version:   version,
		Commit:    commit,
		BuildDate: buildDate,
		GoVersion: runtime.Version(),
		GOOS:      runtime.GOOS,
		GOARCH:    runtime.GOARCH,
	}
}

func printVersionInfo(w io.Writer, info VersionInfo) {
	fmt.Fprintf(w, "dm version\n")
	fmt.Fprintf(w, "  version: %s\n", info.Version)
	fmt.Fprintf(w, "  commit: %s\n", info.Commit)
	fmt.Fprintf(w, "  build_date: %s\n", info.BuildDate)
	fmt.Fprintf(w, "  go: %s\n", info.GoVersion)
	fmt.Fprintf(w, "  platform: %s/%s\n", info.GOOS, info.GOARCH)
}
