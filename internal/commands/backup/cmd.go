package backup

import (
	"fmt"
	"io"

	"docker-manager/internal/commandflags"
	"docker-manager/internal/completion"
	"docker-manager/internal/docker"
	rpt "docker-manager/internal/report"

	"github.com/spf13/cobra"
)

func NewBackupCommand() *cobra.Command {
	opts := BackupOptions{IncludeImage: true}
	var noImage bool
	cmd := &cobra.Command{
		Use:   "backup <container-filter...>",
		Short: "批量备份容器 inspect、镜像、compose、volume 和 network 元数据",
		Long:  "批量备份容器 inspect、镜像、compose、volume 和 network 元数据。\n\n使用 --output-dir 指定备份输出目录。",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runOpts := opts
			if noImage {
				runOpts.IncludeImage = false
			}
			runOpts.OutputDir = opts.OutputDir
			runOpts.Output = cmd.OutOrStdout()
			result, err := backupContainers(cmd.Context(), args, runOpts)
			if err != nil {
				return fmt.Errorf("备份容器失败: %w", err)
			}
			for _, path := range result.Paths {
				if runOpts.DryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "备份 dry-run 完成: %s\n", path)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "备份已创建: %s\n", path)
				}
			}
			return nil
		},
		ValidArgsFunction: completion.LocalContainers,
	}
	cmd.Flags().BoolVar(&noImage, "no-image", false, "不导出容器镜像 tar")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "只预览备份动作，不写入文件")
	cmd.Flags().BoolVar(&opts.Bundle, "bundle", false, "生成离线迁移包 tar.gz，并附带 README、restore 脚本和 checksums")
	cmd.Flags().StringVar(&opts.BundleOutput, "bundle-output", "", "离线迁移包输出路径，默认 <backup-dir>.tar.gz")
	cmd.Flags().BoolVar(&opts.Encrypt, "encrypt", false, "加密离线迁移包；需要 --passphrase-file")
	cmd.Flags().StringVar(&opts.PassphraseFile, "passphrase-file", "", "加密或解密备份包使用的口令文件")
	cmd.Flags().StringVar(&opts.SplitSize, "split-size", "", "按指定大小分卷输出离线迁移包，例如 512M、2G")
	cmd.Flags().StringVar(&opts.OutputDir, "output-dir", "", "备份输出目录；批量目标会在该目录下拆分子目录")
	cmd.Flags().BoolVar(&opts.Merge, "merge", false, "将多个容器合并为一个批量备份包，可整体 restore")
	return cmd
}

func NewRestoreCommand() *cobra.Command {
	opts := RestoreOptions{}
	cmd := &cobra.Command{
		Use:   "restore <backup-dir-or-archive...>",
		Short: "从 backup 生成的目录、批量目录或 tar.gz 离线包恢复镜像、网络、volume 和容器",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Output = cmd.OutOrStdout()
			if opts.Name != "" && len(args) > 1 {
				return fmt.Errorf("--name 只支持恢复单个备份")
			}
			if opts.DryRun && opts.Format != rpt.FormatText {
				for _, arg := range args {
					report, err := buildRestorePlanReport(cmd.Context(), arg, opts)
					if err != nil {
						return fmt.Errorf("生成恢复计划失败: %w", err)
					}
					if err := rpt.Print(cmd.OutOrStdout(), opts.Format, report, func(w io.Writer) {
						printRestorePlanReport(w, report)
					}); err != nil {
						return err
					}
				}
				return nil
			}
			if !opts.DryRun {
				printRestoreDockerTarget(cmd.OutOrStdout())
			}
			for _, arg := range args {
				if err := restoreBackup(cmd.Context(), arg, opts); err != nil {
					return fmt.Errorf("恢复失败: %w", err)
				}
				if opts.DryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "恢复 dry-run 完成: %s\n", arg)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "恢复完成: %s\n", arg)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.Name, "name", "", "恢复为新的容器名，默认使用备份中的容器名")
	cmd.Flags().BoolVar(&opts.Replace, "replace", false, "如果目标容器已存在则先删除")
	cmd.Flags().BoolVar(&opts.NoStart, "no-start", false, "只创建容器，不启动")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "只预览恢复动作，不修改 Docker；配合 --format json/markdown/html 可输出结构化恢复计划")
	cmd.Flags().StringVar(&opts.PassphraseFile, "passphrase-file", "", "解密加密备份包使用的口令文件")
	cmd.Flags().BoolVar(&opts.SkipChecksum, "skip-checksum", false, "跳过 checksums.txt 完整性校验")
	commandflags.AddReportFormatFlag(cmd, &opts.Format)
	return cmd
}

func printRestoreDockerTarget(w io.Writer) {
	if docker.IsRemoteEndpoint() {
		fmt.Fprintf(w, "Target Docker: %s\n", docker.Endpoint())
	}
}
