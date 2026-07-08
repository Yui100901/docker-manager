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

	commandSet := newRootCommandSet(cfg)
	rootCmd.AddCommand(backup.NewBackupCommand())
	rootCmd.AddCommand(backup.NewRestoreCommand())
	rootCmd.AddCommand(commandSet.newImageGroup())
	rootCmd.AddCommand(commandSet.newReportGroup())
	rootCmd.AddCommand(commandSet.newImageShortcuts()...)
	rootCmd.AddCommand(commandSet.newReportShortcuts()...)
	rootCmd.AddCommand(diagnostics.NewDoctorCommandWithDefaults(func() diagnostics.DoctorDefaults {
		return diagnostics.DoctorDefaults{ConfigPath: effectiveConfigPath, OutputDir: cfg.OutputDir}
	}))
	rootCmd.AddCommand(completion.NewCommand())
	rootCmd.AddCommand(version.NewCommand())
	rootCmd.AddCommand(reverse.NewReverseCommand())
	rootCmd.AddCommand(reverse.NewRerunCommand())
	return rootCmd
}

type commandFactory struct {
	name string
	new  func() *cobra.Command
}

type rootCommandSet struct {
	image  []commandFactory
	report []commandFactory
}

func newRootCommandSet(cfg *appConfig) rootCommandSet {
	pullCommand := func() *cobra.Command {
		return pull.NewPullCommandWithDefaults(func() pull.CommandDefaults {
			return pull.CommandDefaults{
				Proxy:     cfg.Proxy,
				TargetOS:  cfg.TargetOS,
				Arch:      cfg.Arch,
				OutputDir: cfg.OutputDir,
			}
		})
	}
	saveCommand := func() *cobra.Command {
		return images.NewSaveCommandWithDefaults(func() string { return cfg.OutputDir })
	}
	return rootCommandSet{
		image: []commandFactory{
			{name: "pull", new: pullCommand},
			{name: "save", new: saveCommand},
			{name: "load", new: images.NewLoadCommand},
			{name: "tree", new: diagnostics.NewImageTreeCommand},
		},
		report: []commandFactory{
			{name: "health", new: diagnostics.NewHealthCommand},
			{name: "network", new: diagnostics.NewNetworkCommand},
			{name: "logs", new: diagnostics.NewLogsScanCommand},
			{name: "diff", new: diagnostics.NewInspectDiffCommand},
			{name: "prune", new: diagnostics.NewPruneReportCommand},
			{name: "volumes", new: diagnostics.NewVolumesReportCommand},
			{name: "registry", new: diagnostics.NewRegistryReportCommand},
		},
	}
}

func (set rootCommandSet) newImageShortcuts() []*cobra.Command {
	return newCommandsFromFactories(set.image)
}

func (set rootCommandSet) newReportShortcuts() []*cobra.Command {
	return newCommandsFromFactories(set.report)
}

func (set rootCommandSet) newImageGroup() *cobra.Command {
	imageCmd := diagnostics.NewImageCommand()
	imageCmd.AddCommand(newCommandsFromFactories(set.image)...)
	return imageCmd
}

func (set rootCommandSet) newReportGroup() *cobra.Command {
	reportCmd := diagnostics.NewReportCommand()
	reportCmd.AddCommand(newCommandsFromFactories(set.report)...)
	return reportCmd
}

func newCommandsFromFactories(factories []commandFactory) []*cobra.Command {
	commands := make([]*cobra.Command, 0, len(factories))
	for _, factory := range factories {
		commands = append(commands, factory.new())
	}
	return commands
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
