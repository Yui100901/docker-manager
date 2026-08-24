package backup

import (
	"fmt"
	"io"
	"time"

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
	cmd.Flags().StringVar(&opts.SigningKey, "signing-key", "", "使用 PEM Ed25519 私钥签名 checksums.txt；仅用于 --bundle")
	cmd.Flags().StringVar(&opts.OutputDir, "output-dir", "", "备份输出目录；批量目标会在该目录下拆分子目录")
	cmd.Flags().BoolVar(&opts.Merge, "merge", false, "将多个容器合并为一个批量备份包，可整体 restore")
	return cmd
}

const defaultRestoreReadyTimeout = 30 * time.Second

type RestoreCommandDefaults struct {
	ReadyTimeout time.Duration
}

func NewRestoreCommand() *cobra.Command {
	return NewRestoreCommandWithDefaults(nil)
}

func NewRestoreCommandWithDefaults(defaults func() RestoreCommandDefaults) *cobra.Command {
	opts := RestoreOptions{ReadyTimeout: defaultRestoreReadyTimeout}
	var maxArchiveSize, maxExpandedSize, maxJSONSize string
	cmd := &cobra.Command{
		Use:   "restore <backup-dir-or-archive...>",
		Short: "从 backup 生成的目录、批量目录或 tar.gz 离线包恢复镜像、网络、volume 和容器",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runOpts := opts
			var err error
			if runOpts.MaxArchiveBytes, err = parseBackupByteSize("--max-archive-size", maxArchiveSize); err != nil {
				return err
			}
			if runOpts.MaxExpandedBytes, err = parseBackupByteSize("--max-expanded-size", maxExpandedSize); err != nil {
				return err
			}
			if runOpts.MaxJSONBytes, err = parseBackupByteSize("--max-json-size", maxJSONSize); err != nil {
				return err
			}
			if _, err := resolveRestoreLimits(runOpts); err != nil {
				return err
			}
			if !cmd.Flags().Changed("ready-timeout") && defaults != nil {
				if value := defaults().ReadyTimeout; value > 0 {
					runOpts.ReadyTimeout = value
				}
			}
			if runOpts.ReadyTimeout <= 0 {
				return fmt.Errorf("--ready-timeout 必须大于 0")
			}
			runOpts.Output = cmd.OutOrStdout()
			if runOpts.Name != "" && len(args) > 1 {
				return fmt.Errorf("--name 只支持恢复单个备份")
			}
			if runOpts.Confirm && runOpts.DryRun {
				return fmt.Errorf("--confirm 不能与 --dry-run 同时使用")
			}
			if runOpts.TrustedPublicKey != "" && runOpts.SkipChecksum {
				return fmt.Errorf("--trusted-public-key 不能与 --skip-checksum 同时使用")
			}
			if !runOpts.Confirm {
				if !runOpts.DryRun && runOpts.Format == rpt.FormatText {
					fmt.Fprintln(cmd.OutOrStdout(), "未提供 --confirm；默认只生成恢复计划，不会修改 Docker。")
				}
				runOpts.DryRun = true
			}
			if runOpts.DryRun && runOpts.Format != rpt.FormatText {
				for _, arg := range args {
					report, err := buildRestorePlanReport(cmd.Context(), arg, runOpts)
					if err != nil {
						return fmt.Errorf("生成恢复计划失败: %w", err)
					}
					if err := rpt.Print(cmd.OutOrStdout(), runOpts.Format, report, func(w io.Writer) {
						printRestorePlanReport(w, report)
					}); err != nil {
						return err
					}
				}
				return nil
			}
			if !runOpts.DryRun {
				var preparedBackups []*preparedRestoreBackup
				defer func() {
					for _, prepared := range preparedBackups {
						prepared.Close()
					}
				}()
				seenTargets := make(map[string]string)
				for _, arg := range args {
					prepared, err := prepareRestoreBackup(cmd.Context(), arg, runOpts)
					if err != nil {
						return fmt.Errorf("恢复预检失败: %w", err)
					}
					preparedBackups = append(preparedBackups, prepared)
					for _, target := range prepared.targets {
						if previous, exists := seenTargets[target]; exists {
							return fmt.Errorf("恢复预检失败: 目标容器 %s 同时来自 %s 和 %s", target, previous, arg)
						}
						seenTargets[target] = arg
					}
				}
				if err := validatePreparedRestoreSet(cmd.Context(), preparedBackups, runOpts); err != nil {
					return fmt.Errorf("恢复预检失败: %w", err)
				}
				printRestoreDockerTarget(cmd.OutOrStdout())
				for _, prepared := range preparedBackups {
					if err := executePreparedRestore(cmd.Context(), prepared, runOpts); err != nil {
						return fmt.Errorf("恢复失败: %w", err)
					}
					fmt.Fprintf(cmd.OutOrStdout(), "恢复完成: %s\n", prepared.source)
				}
				return nil
			}
			for _, arg := range args {
				if err := restoreBackup(cmd.Context(), arg, runOpts); err != nil {
					return fmt.Errorf("恢复失败: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "恢复 dry-run 完成: %s\n", arg)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.Name, "name", "", "恢复为新的容器名，默认使用备份中的容器名")
	cmd.Flags().BoolVar(&opts.Replace, "replace", false, "安全替换已存在容器；旧容器会临时保留以便失败回滚")
	cmd.Flags().BoolVar(&opts.NoStart, "no-start", false, "只创建容器，不启动")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "只预览恢复动作，不修改 Docker；配合 --format json/markdown/html 可输出结构化恢复计划")
	cmd.Flags().BoolVar(&opts.Confirm, "confirm", false, "显式确认执行恢复；未提供时默认只生成计划")
	cmd.Flags().BoolVar(&opts.AllowUnsafeHostConfig, "allow-dangerous-config", false, "允许恢复 privileged、host namespace、host bind、device、驱动选项等高风险配置")
	cmd.Flags().BoolVar(&opts.AllowUnsafeHostConfig, "allow-unsafe-host-config", false, "兼容别名: --allow-dangerous-config")
	_ = cmd.Flags().MarkHidden("allow-unsafe-host-config")
	cmd.Flags().StringVar(&opts.PassphraseFile, "passphrase-file", "", "解密加密备份包使用的口令文件")
	cmd.Flags().BoolVar(&opts.SkipChecksum, "skip-checksum", false, "跳过 checksums.txt 完整性校验")
	cmd.Flags().StringVar(&opts.TrustedPublicKey, "trusted-public-key", "", "使用 PEM Ed25519 公钥验证 checksums.txt.sig 和备份来源")
	cmd.Flags().DurationVar(&opts.ReadyTimeout, "ready-timeout", defaultRestoreReadyTimeout, "replace 候选容器等待 running/healthy 的最长时间")
	cmd.Flags().StringVar(&maxArchiveSize, "max-archive-size", "", "最大归档/密文输入大小，可用 K/M/G/T 后缀；默认 512G，只允许下调")
	cmd.Flags().StringVar(&maxExpandedSize, "max-expanded-size", "", "归档最大展开总大小，可用 K/M/G/T 后缀；默认 1T，只允许下调")
	cmd.Flags().StringVar(&maxJSONSize, "max-json-size", "", "manifest/inspect/network/volume JSON 累计最大大小，可用 K/M/G/T 后缀；默认 256M，只允许下调")
	cmd.Flags().IntVar(&opts.MaxParts, "max-parts", 0, "分卷恢复允许的最大连续分卷数；默认 999，只允许下调")
	commandflags.AddReportFormatFlag(cmd, &opts.Format)
	return cmd
}

func printRestoreDockerTarget(w io.Writer) {
	if docker.IsRemoteEndpoint() {
		fmt.Fprintf(w, "Target Docker: %s\n", docker.Endpoint())
	}
}
