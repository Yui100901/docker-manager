package diagnostics

import (
	"context"
	"io"
	"runtime"
	"time"

	rpt "docker-manager/internal/report"

	"github.com/spf13/cobra"
)

func NewDoctorCommand() *cobra.Command {
	return NewDoctorCommandWithDefaults(nil)
}

func NewDoctorCommandWithDefaults(defaults func() DoctorDefaults) *cobra.Command {
	opts := DoctorOptions{
		ConfigPath:    ".dm.yaml",
		OutputDir:     ".",
		Timeout:       5 * time.Second,
		CheckE2E:      true,
		MinDiskFreeMB: 1024,
	}
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "检查 Docker、registry、代理、磁盘和测试前置条件",
		RunE: func(cmd *cobra.Command, args []string) error {
			if defaults != nil {
				cfg := defaults()
				if cfg.ConfigPath != "" && !cmd.Flags().Changed("dm-config") {
					opts.ConfigPath = cfg.ConfigPath
				}
				if cfg.OutputDir != "" && !cmd.Flags().Changed("output-dir") {
					opts.OutputDir = cfg.OutputDir
				}
			}
			report := runDoctor(cmd.Context(), opts)
			return rpt.Print(cmd.OutOrStdout(), opts.Format, report, func(w io.Writer) {
				printDoctorReport(w, report)
			})
		},
	}
	cmd.Flags().StringArrayVar(&opts.Registries, "registry", nil, "检查 registry 连通性和凭据，可重复指定")
	cmd.Flags().BoolVar(&opts.PlainHTTP, "plain-http", false, "使用 http:// 检查 registry /v2/")
	cmd.Flags().StringVar(&opts.DockerConfig, "docker-config", "", "Docker config.json 路径，默认使用 DOCKER_CONFIG/config.json 或 ~/.docker/config.json")
	cmd.Flags().StringVar(&opts.ConfigPath, "dm-config", opts.ConfigPath, "dm 配置文件路径")
	cmd.Flags().StringVar(&opts.OutputDir, "output-dir", opts.OutputDir, "检查磁盘空间的输出目录")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", opts.Timeout, "单项网络/Docker 检查超时时间")
	cmd.Flags().BoolVar(&opts.CheckE2E, "check-e2e", opts.CheckE2E, "检查 scripts/e2e.sh、Go 和 vendor 前置条件")
	cmd.Flags().Int64Var(&opts.MinDiskFreeMB, "min-disk-free-mb", opts.MinDiskFreeMB, "磁盘剩余空间告警阈值，单位 MB")
	rpt.AddFormatFlag(cmd, &opts.Format)
	return cmd
}

func runDoctor(ctx context.Context, opts DoctorOptions) DoctorReport {
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}
	if opts.ConfigPath == "" {
		opts.ConfigPath = ".dm.yaml"
	}
	if opts.OutputDir == "" {
		opts.OutputDir = "."
	}
	if opts.MinDiskFreeMB <= 0 {
		opts.MinDiskFreeMB = 1024
	}
	report := DoctorReport{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Platform:    runtime.GOOS + "/" + runtime.GOARCH,
	}
	report.Checks = append(report.Checks, checkDoctorDocker(ctx, opts.Timeout)...)
	cfg, configChecks := checkDoctorConfig(opts.ConfigPath)
	report.Checks = append(report.Checks, configChecks...)
	report.Checks = append(report.Checks, checkDoctorProxy(cfg)...)
	report.Checks = append(report.Checks, checkDoctorCA(cfg)...)
	report.Checks = append(report.Checks, checkDoctorDaemonConfig()...)
	report.Checks = append(report.Checks, checkDoctorDisk(opts.OutputDir, opts.MinDiskFreeMB))
	report.Checks = append(report.Checks, checkDoctorDockerConfig(ctx, opts)...)
	for _, registry := range opts.Registries {
		report.Checks = append(report.Checks, checkDoctorRegistry(ctx, registry, opts)...)
	}
	if len(opts.Registries) == 0 {
		report.Checks = append(report.Checks, DoctorCheck{
			Name:        "registry",
			Status:      "skipped",
			Message:     "未指定 --registry，跳过 registry 连通性检查",
			Recommended: "需要验证推送目标时执行 dm doctor --registry <registry>，内网 HTTP registry 可加 --plain-http",
		})
	}
	if opts.CheckE2E {
		report.Checks = append(report.Checks, checkDoctorToolchain()...)
	}
	report.OverallStatus = doctorOverallStatus(report.Checks)
	report.Recommendations = doctorRecommendations(report.Checks)
	return report
}
