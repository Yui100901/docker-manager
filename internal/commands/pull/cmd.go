package pull

import (
	"context"
	rpt "docker-manager/internal/report"
	"errors"
	"fmt"
	"github.com/spf13/cobra"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
)

func NewPullCommand() *cobra.Command {
	return NewPullCommandWithDefaults(nil)
}

func NewPullCommandWithDefaults(defaults func() CommandDefaults) *cobra.Command {
	var imageNameList []string
	var targetOS string
	var arch string
	var proxy string
	var output string
	var outputDir string
	var load bool
	var to string
	var dockerConfig string
	var plainHTTP bool
	var verboseHTTP bool
	timeout := defaultPullTimeout
	batchOpts := PullBatchOptions{
		OutputDir:   ".",
		Concurrency: 1,
		Retries:     1,
	}
	cmd := &cobra.Command{
		Use:   "pull <images...>",
		Short: "无需 Docker 客户端下载 Docker 镜像",
		Long: `无需 Docker 客户端下载 Docker 镜像，从官方镜像源拉取。
默认使用 HTTP_PROXY/HTTPS_PROXY 环境变量代理；未设置则直连。可通过 --proxy 强制指定代理。
默认拉取 linux/amd64 镜像。
支持直接传多个镜像或通过 --file 读取镜像列表；批量模式可使用 --concurrency、--retries、--resume、--skip-existing 和 --report。`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()

			applyCommandDefaults(cmd, defaults, &proxy, &targetOS, &arch, &outputDir)
			if !cmd.Flags().Changed("output-dir") {
				batchOpts.OutputDir = outputDir
			}
			configureHTTPLogging(verboseHTTP)
			imageNameList = args
			if shouldRunPullBatch(cmd, imageNameList, batchOpts) {
				if timeout <= 0 {
					return fmt.Errorf("--timeout 必须大于 0")
				}
				runner, err := NewPullRunnerWithTimeout(proxy, targetOS, arch, timeout)
				if err != nil {
					return fmt.Errorf("配置代理失败: %w", err)
				}
				if output != "" {
					return fmt.Errorf("--output 只能在拉取单个镜像时使用，请改用 --output-dir")
				}
				batchOpts.Images = append([]string(nil), imageNameList...)
				batchOpts.To = to
				batchOpts.OutputDir = outputDir
				batchOpts.Load = load
				batchOpts.DockerConfig = dockerConfig
				batchOpts.PlainHTTP = plainHTTP
				batchOpts.ProgressOutput = cmd.OutOrStdout()
				report, err := runPullBatch(ctx, runner, batchOpts)
				if errors.Is(err, context.Canceled) {
					return err
				}
				if report.GeneratedAt != "" {
					printErr := rpt.Print(cmd.OutOrStdout(), batchOpts.Format, report, func(w io.Writer) {
						printPullBatchReport(w, report)
					})
					if printErr != nil {
						return printErr
					}
					if batchOpts.ReportFile != "" {
						if writeErr := writePullBatchReport(batchOpts.ReportFile, report); writeErr != nil {
							return writeErr
						}
					}
				}
				return err
			}
			if len(imageNameList) == 0 {
				return fmt.Errorf("pull 需要至少一个镜像，可通过位置参数或 --file 指定")
			}
			if timeout <= 0 {
				return fmt.Errorf("--timeout 必须大于 0")
			}
			runner, err := NewPullRunnerWithTimeout(proxy, targetOS, arch, timeout)
			if err != nil {
				return fmt.Errorf("配置代理失败: %w", err)
			}
			opts := PullOptions{
				Context:        ctx,
				Output:         output,
				OutputDir:      outputDir,
				Load:           load,
				To:             to,
				DockerConfig:   dockerConfig,
				PlainHTTP:      plainHTTP,
				ProgressOutput: cmd.OutOrStdout(),
			}
			var pullErrs []error
			success := 0
			total := len(imageNameList)
			log.Printf("Pull images: total=%d os=%s arch=%s output=%s outputDir=%s to=%s plainHTTP=%v", total, targetOS, arch, output, outputDir, to, plainHTTP)
			for i, imageName := range imageNameList {
				log.Printf("Pull image [%d/%d]: %s", i+1, total, imageName)
				if err := runner.getImage(imageName, opts); err != nil {
					if errors.Is(err, context.Canceled) {
						return err
					}
					log.Printf("%s 拉取失败: %v", imageName, err)
					pullErrs = append(pullErrs, fmt.Errorf("%s: %w", imageName, err))
					continue
				}
				success++
			}
			log.Printf("Pull summary: total=%d success=%d failed=%d", total, success, len(pullErrs))
			return errors.Join(pullErrs...)
		},
	}
	cmd.Flags().StringVarP(&targetOS, "os", "", "linux", "目标操作系统")
	cmd.Flags().StringVarP(&arch, "arch", "a", "amd64", "目标架构")
	cmd.Flags().StringVar(&proxy, "proxy", "", "强制指定 HTTP 代理，例如 http://127.0.0.1:7890；为空时使用环境变量代理")
	cmd.Flags().DurationVar(&timeout, "timeout", defaultPullTimeout, "连接、TLS 握手和响应头超时时间，例如 30s、2m、5m")
	cmd.Flags().StringVarP(&output, "output", "o", "", "输出 tar 文件路径，仅支持单个镜像")
	cmd.Flags().StringVar(&outputDir, "output-dir", ".", "输出 tar 文件目录")
	cmd.Flags().BoolVar(&load, "load", false, "拉取并打包完成后自动导入 Docker")
	cmd.Flags().BoolVar(&verboseHTTP, "verbose-http", false, "输出底层 HTTP 请求调试日志")
	cmd.Flags().StringVar(&to, "to", "", "pull 后导入 Docker、tag 并 push 到目标 registry/repository；可用 http:// 或 https:// 指定目标协议")
	cmd.Flags().StringVar(&dockerConfig, "docker-config", "", "Docker config.json 路径，默认使用 DOCKER_CONFIG/config.json 或 ~/.docker/config.json")
	cmd.Flags().BoolVar(&plainHTTP, "plain-http", false, "使用 http:// 拉取 registry，适用于未启用 TLS 的内网 registry")
	cmd.Flags().StringVarP(&batchOpts.File, "file", "f", "", "镜像列表文件，空行和 # 注释会被忽略")
	cmd.Flags().IntVar(&batchOpts.Concurrency, "concurrency", batchOpts.Concurrency, "批量模式并发数量")
	cmd.Flags().IntVar(&batchOpts.Retries, "retries", batchOpts.Retries, "批量模式单个镜像失败后的重试次数")
	cmd.Flags().BoolVar(&batchOpts.SkipExisting, "skip-existing", false, "批量推送时目标 registry 已存在同名 manifest 则跳过，需要配合 --to")
	cmd.Flags().BoolVar(&batchOpts.Resume, "resume", false, "批量模式读取状态文件并跳过已经成功的镜像")
	cmd.Flags().StringVar(&batchOpts.StateFile, "state-file", "", "批量模式状态文件路径，默认写入 <output-dir>/pull-state.json")
	cmd.Flags().StringVar(&batchOpts.ReportFile, "report", "", "批量模式额外写入 JSON 汇总报告文件")
	rpt.AddFormatFlag(cmd, &batchOpts.Format)
	_ = cmd.RegisterFlagCompletionFunc("os", completePullValues("linux", "windows"))
	_ = cmd.RegisterFlagCompletionFunc("arch", completePullValues("amd64", "arm64", "arm", "386", "ppc64le", "s390x"))
	return cmd
}

func shouldRunPullBatch(cmd *cobra.Command, images []string, opts PullBatchOptions) bool {
	flags := cmd.Flags()
	return len(images) > 1 ||
		opts.File != "" ||
		opts.Resume ||
		opts.SkipExisting ||
		opts.ReportFile != "" ||
		opts.StateFile != "" ||
		flags.Changed("concurrency") ||
		flags.Changed("retries") ||
		flags.Changed("format")
}

func completePullValues(values ...string) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		var suggestions []string
		for _, value := range values {
			if strings.HasPrefix(value, toComplete) {
				suggestions = append(suggestions, value)
			}
		}
		return suggestions, cobra.ShellCompDirectiveNoFileComp
	}
}

func applyCommandDefaults(cmd *cobra.Command, defaults func() CommandDefaults, proxy, targetOS, arch, outputDir *string) {
	if defaults == nil {
		return
	}
	cfg := defaults()
	flags := cmd.Flags()
	if cfg.Proxy != "" && !flags.Changed("proxy") {
		*proxy = cfg.Proxy
	}
	if cfg.TargetOS != "" && !flags.Changed("os") {
		*targetOS = cfg.TargetOS
	}
	if cfg.Arch != "" && !flags.Changed("arch") {
		*arch = cfg.Arch
	}
	if cfg.OutputDir != "" && !flags.Changed("output-dir") {
		*outputDir = cfg.OutputDir
	}
}
