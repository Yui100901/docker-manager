package diagnostics

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"time"

	"docker-manager/internal/commandflags"
	"docker-manager/internal/parallel"
	"docker-manager/internal/registryauth"
	rpt "docker-manager/internal/report"
	"docker-manager/internal/runcontrol"

	"github.com/spf13/cobra"
)

func NewDoctorCommand() *cobra.Command {
	return NewDoctorCommandWithDefaults(nil)
}

func NewDoctorCommandWithDefaults(defaults func() DoctorDefaults) *cobra.Command {
	opts := DoctorOptions{
		ConfigPath:              ".dm.yaml",
		OutputDir:               ".",
		Timeout:                 5 * time.Second,
		CheckE2E:                true,
		MinDiskFreeMB:           1024,
		CredentialHelperTimeout: registryauth.DefaultCredentialHelperTimeout,
	}
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "检查 Docker、registry、代理、磁盘和测试前置条件",
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.MinDiskFreeMB < 0 {
				return fmt.Errorf("--min-disk-free-mb 不能为负数")
			}
			if defaults != nil {
				cfg := defaults()
				if cfg.ConfigPath != "" {
					opts.ConfigPath = cfg.ConfigPath
				}
				opts.LoadedConfig = cfg.LoadedConfig
				opts.ConfigLoadError = cfg.ConfigLoadError
				if cfg.OutputDir != "" && !cmd.Flags().Changed("output-dir") {
					opts.OutputDir = cfg.OutputDir
				}
				if cfg.DisableCredentialHelpers && !cmd.Flags().Changed("disable-credential-helpers") {
					opts.DisableCredentialHelpers = true
				}
				if cfg.CredentialHelperTimeout > 0 && !cmd.Flags().Changed("credential-helper-timeout") {
					opts.CredentialHelperTimeout = cfg.CredentialHelperTimeout
				}
				opts.ResolveRegistryPolicy = cfg.ResolveRegistryPolicy
			}
			opts.plainHTTPExplicit = cmd.Flags().Changed("plain-http")
			opts.proxyExplicit = cmd.Flags().Changed("proxy")
			opts.noProxyExplicit = cmd.Flags().Changed("no-proxy")
			opts.registryCAFileExplicit = cmd.Flags().Changed("registry-ca-file")
			opts.registryCAPathExplicit = cmd.Flags().Changed("registry-ca-path")
			opts.timeoutExplicit = cmd.Flags().Changed("timeout")
			report, err := runDoctor(cmd.Context(), opts)
			if err != nil {
				return err
			}
			return rpt.Print(cmd.OutOrStdout(), opts.Format, report, func(w io.Writer) {
				printDoctorReport(w, report)
			})
		},
	}
	cmd.Flags().StringArrayVar(&opts.Registries, "registry", nil, "检查 registry 连通性和凭据，可重复指定")
	commandflags.AddPlainHTTPFlag(cmd, &opts.PlainHTTP)
	cmd.Flags().StringVar(&opts.Proxy, "proxy", "", "registry /v2/ 检查使用的代理；未指定时读取精确 registry 策略或标准代理环境变量")
	cmd.Flags().BoolVar(&opts.NoProxy, "no-proxy", false, "registry /v2/ 检查强制直连，不使用配置或环境代理")
	cmd.Flags().StringVar(&opts.RegistryCAFile, "registry-ca-file", "", "registry /v2/ HTTPS 检查额外信任的 PEM CA 文件")
	cmd.Flags().StringVar(&opts.RegistryCAPath, "registry-ca-path", "", "registry /v2/ HTTPS 检查额外信任的 PEM CA 目录")
	commandflags.AddDockerConfigFlag(cmd, &opts.DockerConfig)
	commandflags.AddCredentialHelperFlags(cmd, &opts.DisableCredentialHelpers, &opts.CredentialHelperTimeout, opts.CredentialHelperTimeout)
	cmd.Flags().StringVar(&opts.OutputDir, "output-dir", opts.OutputDir, "检查磁盘空间的输出目录")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", opts.Timeout, "单项网络/Docker 检查超时时间")
	cmd.Flags().BoolVar(&opts.CheckE2E, "check-e2e", opts.CheckE2E, "检查 scripts/e2e.sh、Go 和 vendor 前置条件")
	cmd.Flags().Int64Var(&opts.MinDiskFreeMB, "min-disk-free-mb", opts.MinDiskFreeMB, "磁盘剩余空间告警阈值，单位 MB")
	commandflags.AddReportFormatFlag(cmd, &opts.Format)
	return cmd
}

func runDoctor(ctx context.Context, opts DoctorOptions) (DoctorReport, error) {
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
	cfg, configChecks := resolveDoctorConfig(opts.ConfigPath, opts.LoadedConfig, opts.ConfigLoadError)
	groups := []doctorCheckGroup{
		{index: 0, check: func() []DoctorCheck { return checkDoctorDocker(ctx, opts.Timeout) }},
		{index: 1, check: func() []DoctorCheck { return configChecks }},
		{index: 2, check: func() []DoctorCheck { return checkDoctorProxy(cfg) }},
		{index: 3, check: func() []DoctorCheck { return checkDoctorCA(cfg) }},
		{index: 4, check: checkDoctorDaemonConfig},
		{index: 5, check: func() []DoctorCheck { return []DoctorCheck{checkDoctorDisk(opts.OutputDir, opts.MinDiskFreeMB)} }},
		{index: 6, check: func() []DoctorCheck { return checkDoctorDockerConfig(ctx, opts) }},
	}
	nextIndex := 7
	for _, registry := range opts.Registries {
		registry := registry
		groups = append(groups, doctorCheckGroup{
			index: nextIndex,
			check: func() []DoctorCheck { return checkDoctorRegistry(ctx, registry, opts) },
		})
		nextIndex++
	}
	if len(opts.Registries) == 0 {
		groups = append(groups, doctorCheckGroup{
			index: nextIndex,
			check: func() []DoctorCheck {
				return []DoctorCheck{{
					Name:        "registry",
					Status:      "skipped",
					Message:     "未指定 --registry，跳过 registry 连通性检查",
					Recommended: "需要验证推送目标时执行 dm doctor --registry <registry>，内网 HTTP registry 可加 --plain-http",
				}}
			},
		})
		nextIndex++
	}
	if opts.CheckE2E {
		groups = append(groups, doctorCheckGroup{
			index: nextIndex,
			check: checkDoctorToolchain,
		})
		nextIndex++
	}
	if err := runcontrol.CheckItems(ctx, "doctor-check", len(groups)); err != nil {
		return report, err
	}
	for _, checks := range runDoctorCheckGroups(ctx, nextIndex, groups) {
		report.Checks = append(report.Checks, checks...)
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	report.OverallStatus = doctorOverallStatus(report.Checks)
	report.Recommendations = doctorRecommendations(report.Checks)
	return report, nil
}

type doctorCheckGroup struct {
	index int
	check func() []DoctorCheck
}

func runDoctorCheckGroups(ctx context.Context, total int, groups []doctorCheckGroup) [][]DoctorCheck {
	results := make([][]DoctorCheck, total)
	parallel.ForEachIndex(ctx, len(groups), diagnosticsInspectConcurrency, func(ctx context.Context, i int) {
		group := groups[i]
		results[group.index] = group.check()
	})
	return results
}
