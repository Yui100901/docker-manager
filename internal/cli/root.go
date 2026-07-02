package cli

import (
	"context"
	"docker-manager/internal/commands/backup"
	"docker-manager/internal/commands/diagnostics"
	"docker-manager/internal/commands/images"
	"docker-manager/internal/commands/pull"
	"docker-manager/internal/commands/reverse"
	"docker-manager/internal/completion"
	dockerapi "docker-manager/internal/docker"
	"docker-manager/internal/version"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

func Run() int {
	cfg := appConfig{}
	opts := outputOptions{}
	rootCmd := newRootCommand(&cfg, &opts)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	rootCmd.SetContext(ctx)
	preseedJSONErrorMode(&opts, os.Args[1:])
	if err := rootCmd.Execute(); err != nil {
		writeCommandError(rootCmd.ErrOrStderr(), err, opts)
		if isCommandCanceled(err) {
			return 130
		}
		return 1
	}
	return 0
}

func preseedJSONErrorMode(opts *outputOptions, args []string) {
	for _, arg := range args {
		var value string
		switch {
		case arg == "--log-json":
			opts.JSON = true
		case strings.HasPrefix(arg, "--log-json="):
			value = strings.TrimPrefix(arg, "--log-json=")
		default:
			continue
		}
		if value == "" {
			continue
		}
		parsed, err := strconv.ParseBool(value)
		if err == nil {
			opts.JSON = parsed
		}
	}
}

func newRootCommand(cfg *appConfig, opts *outputOptions) *cobra.Command {
	configPath := defaultConfigPath
	effectiveConfigPath := configPath
	var dockerHost string
	var dockerTLSVerify bool
	var dockerCertPath string
	var dockerAPIVersion string
	rootCmd := &cobra.Command{
		Use:           "dm <command>",
		Short:         "Docker 运维辅助工具",
		SilenceErrors: true,
		SilenceUsage:  true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			effectiveConfigPath = resolveConfigPath(configPath, cmd.Root().PersistentFlags().Changed("config"))
			loaded, err := loadAppConfig(effectiveConfigPath)
			if err != nil {
				if isDoctorCommand(cmd) {
					configureLogging(*opts)
					return nil
				}
				return err
			}
			*cfg = loaded
			applyOutputDefaults(cmd, cfg, opts)
			applyDockerDefaults(cmd, cfg, dockerHost, dockerTLSVerify, dockerCertPath, dockerAPIVersion)
			configureLogging(*opts)
			return nil
		},
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd.Help()
		},
	}

	opts.Verbose = cfg.Verbose
	opts.Quiet = cfg.Quiet
	opts.JSON = cfg.JSON
	rootCmd.PersistentFlags().StringVar(&configPath, "config", defaultConfigPath, "配置文件路径")
	rootCmd.PersistentFlags().BoolVar(&opts.Verbose, "verbose", opts.Verbose, "输出详细日志")
	rootCmd.PersistentFlags().BoolVar(&opts.Quiet, "quiet", opts.Quiet, "隐藏信息日志")
	rootCmd.PersistentFlags().BoolVar(&opts.JSON, "log-json", opts.JSON, "以 JSON 输出日志和错误，不影响业务报告格式")
	rootCmd.PersistentFlags().StringVar(&dockerHost, "docker-host", "", "Docker daemon 地址，默认读取 DOCKER_HOST 或本地 Docker")
	rootCmd.PersistentFlags().BoolVar(&dockerTLSVerify, "docker-tls-verify", false, "启用 Docker TCP TLS 证书校验，默认读取 DOCKER_TLS_VERIFY")
	rootCmd.PersistentFlags().StringVar(&dockerCertPath, "docker-cert-path", "", "Docker TLS 证书目录，默认读取 DOCKER_CERT_PATH")
	rootCmd.PersistentFlags().StringVar(&dockerAPIVersion, "docker-api-version", "", "Docker API 版本，默认读取 DOCKER_API_VERSION 或自动协商")

	rootCmd.AddCommand(backup.NewBackupCommand())
	rootCmd.AddCommand(backup.NewRestoreCommand())
	rootCmd.AddCommand(newImageCommand(cfg))
	rootCmd.AddCommand(diagnostics.NewReportCommand())
	rootCmd.AddCommand(newImageShortcutCommands(cfg)...)
	rootCmd.AddCommand(newReportShortcutCommands()...)
	rootCmd.AddCommand(diagnostics.NewDoctorCommandWithDefaults(func() diagnostics.DoctorDefaults {
		return diagnostics.DoctorDefaults{ConfigPath: effectiveConfigPath, OutputDir: cfg.OutputDir}
	}))
	rootCmd.AddCommand(completion.NewCommand())
	rootCmd.AddCommand(version.NewCommand())
	rootCmd.AddCommand(reverse.NewReverseCommand())
	rootCmd.AddCommand(reverse.NewRerunCommand())
	return rootCmd
}

func newImageShortcutCommands(cfg *appConfig) []*cobra.Command {
	return []*cobra.Command{
		pull.NewPullCommandWithDefaults(func() pull.CommandDefaults {
			return pull.CommandDefaults{
				Proxy:     cfg.Proxy,
				TargetOS:  cfg.TargetOS,
				Arch:      cfg.Arch,
				OutputDir: cfg.OutputDir,
			}
		}),
		images.NewLoadCommand(),
		images.NewSaveCommandWithDefaults(func() string { return cfg.OutputDir }),
		diagnostics.NewImageTreeCommand(),
	}
}

func newReportShortcutCommands() []*cobra.Command {
	return []*cobra.Command{
		diagnostics.NewHealthCommand(),
		diagnostics.NewNetworkCommand(),
		diagnostics.NewLogsScanCommand(),
		diagnostics.NewInspectDiffCommand(),
		diagnostics.NewPruneReportCommand(),
		diagnostics.NewVolumesReportCommand(),
		diagnostics.NewRegistryReportCommand(),
	}
}

func newImageCommand(cfg *appConfig) *cobra.Command {
	imageCmd := diagnostics.NewImageCommand()
	imageCmd.AddCommand(images.NewLoadCommand())
	imageCmd.AddCommand(images.NewSaveCommandWithDefaults(func() string { return cfg.OutputDir }))
	imageCmd.AddCommand(pull.NewPullCommandWithDefaults(func() pull.CommandDefaults {
		return pull.CommandDefaults{
			Proxy:     cfg.Proxy,
			TargetOS:  cfg.TargetOS,
			Arch:      cfg.Arch,
			OutputDir: cfg.OutputDir,
		}
	}))
	return imageCmd
}

func isDoctorCommand(cmd *cobra.Command) bool {
	for current := cmd; current != nil; current = current.Parent() {
		if current.Name() == "doctor" {
			return true
		}
	}
	return false
}

func applyOutputDefaults(cmd *cobra.Command, cfg *appConfig, opts *outputOptions) {
	flags := cmd.Root().PersistentFlags()
	if !flags.Changed("verbose") {
		opts.Verbose = cfg.Verbose
	}
	if !flags.Changed("quiet") {
		opts.Quiet = cfg.Quiet
	}
	if !flags.Changed("log-json") {
		opts.JSON = cfg.JSON
	}
	if flags.Changed("verbose") && opts.Verbose {
		opts.Quiet = false
	}
	if flags.Changed("quiet") && opts.Quiet {
		opts.Verbose = false
	}
}

func applyDockerDefaults(cmd *cobra.Command, cfg *appConfig, host string, tlsVerify bool, certPath string, apiVersion string) {
	flags := cmd.Root().PersistentFlags()
	opts := dockerapi.Options{
		Host:       cfg.DockerHost,
		TLSVerify:  cfg.DockerTLSVerify,
		CertPath:   cfg.DockerCertPath,
		APIVersion: cfg.DockerAPIVersion,
	}
	if flags.Changed("docker-host") {
		opts.Host = host
	}
	if flags.Changed("docker-tls-verify") {
		value := tlsVerify
		opts.TLSVerify = &value
	}
	if flags.Changed("docker-cert-path") {
		opts.CertPath = certPath
	}
	if flags.Changed("docker-api-version") {
		opts.APIVersion = apiVersion
	}
	dockerapi.Configure(opts)
}
