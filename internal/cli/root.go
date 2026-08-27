package cli

import (
	"context"
	"docker-manager/internal/appconfig"
	"docker-manager/internal/commands/backup"
	"docker-manager/internal/commands/diagnostics"
	"docker-manager/internal/commands/images"
	"docker-manager/internal/commands/pull"
	"docker-manager/internal/commands/reverse"
	"docker-manager/internal/completion"
	dockerapi "docker-manager/internal/docker"
	"docker-manager/internal/dockerconfig"
	"docker-manager/internal/registryauth"
	"docker-manager/internal/sensitive"
	"docker-manager/internal/version"
	"errors"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func Run() int {
	sensitive.SetDefaultProfile(sensitive.ProfileNone)
	args := os.Args[1:]
	preseededProfile, profilePreseeded := preseedRedactionProfileForError(args)
	cfg := appConfig{}
	opts := outputOptions{}
	rootCmd := newRootCommand(&cfg, &opts)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	rootCmd.SetContext(ctx)
	preseedJSONErrorMode(&opts, args)
	executedCmd, err := rootCmd.ExecuteC()
	if err != nil {
		applyParsedRedactProfileForError(executedCmd)
		if profilePreseeded {
			sensitive.SetDefaultProfile(preseededProfile)
		}
		writeCommandError(rootCmd.ErrOrStderr(), err, opts)
		if isCommandCanceled(err) {
			return 130
		}
		return 1
	}
	return 0
}

func preseedRedactionProfileForError(args []string) (sensitive.Profile, bool) {
	var (
		profile        sensitive.Profile
		profileChanged bool
		legacyValue    bool
		legacyChanged  bool
	)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		switch {
		case arg == "--redact-profile":
			if i+1 >= len(args) || args[i+1] == "--" {
				continue
			}
			i++
			if parsed, err := sensitive.NormalizeProfile(args[i], false); err == nil {
				profile = parsed
				profileChanged = true
			}
		case strings.HasPrefix(arg, "--redact-profile="):
			value := strings.TrimPrefix(arg, "--redact-profile=")
			if parsed, err := sensitive.NormalizeProfile(value, false); err == nil {
				profile = parsed
				profileChanged = true
			}
		case arg == "--redact-secrets":
			legacyValue = true
			legacyChanged = true
		case strings.HasPrefix(arg, "--redact-secrets="):
			value := strings.TrimPrefix(arg, "--redact-secrets=")
			if parsed, err := strconv.ParseBool(value); err == nil {
				legacyValue = parsed
				legacyChanged = true
			}
		}
	}
	if profileChanged {
		sensitive.SetDefaultProfile(profile)
		return profile, true
	}
	if legacyChanged {
		profile, _ = sensitive.NormalizeProfile("", legacyValue)
		sensitive.SetDefaultProfile(profile)
		return profile, true
	}
	return sensitive.ProfileNone, false
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
	loadedConfig := appconfig.Loaded{Path: configPath, Fields: map[string]bool{}}
	var configLoadError error
	configResolved := false
	var profileName string
	var dockerHost string
	var dockerTLSVerify bool
	var dockerCertPath string
	var dockerAPIVersion string
	dockerTimeout := dockerapi.DefaultRequestTimeout
	var redactSecrets bool
	var redactProfile string
	outputsWrapped := false
	wrapSensitiveOutputs := func(cmd *cobra.Command) {
		if outputsWrapped {
			return
		}
		cmd.SetOut(sensitive.NewDynamicWriter(cmd.OutOrStdout()))
		cmd.SetErr(sensitive.NewDynamicWriter(cmd.ErrOrStderr()))
		outputsWrapped = true
	}
	rootCmd := &cobra.Command{
		Use:           "dm <command>",
		Short:         "Docker 运维辅助工具",
		SilenceErrors: true,
		SilenceUsage:  true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			flagProfile, err := resolveRootRedactProfile(cmd, &appConfig{}, redactProfile, redactSecrets)
			if err != nil {
				return err
			}
			sensitive.SetDefaultProfile(flagProfile)
			if flagProfile != sensitive.ProfileNone {
				wrapSensitiveOutputs(cmd)
			}
			configFlagChanged := cmd.Root().PersistentFlags().Changed("config")
			profileFlagChanged := cmd.Root().PersistentFlags().Changed("profile")
			effectiveConfigPath = resolveConfigPath(configPath, configFlagChanged)
			configLoadError = nil
			configResolved = false
			loaded, err := appconfig.LoadWithOptions(effectiveConfigPath, appconfig.LoadOptions{
				Required:        appconfig.IsExplicitPath(configFlagChanged) || profileSelectionRequiresConfig(profileName, profileFlagChanged),
				Profile:         profileName,
				ProfileExplicit: profileFlagChanged,
			})
			if err != nil {
				if isDoctorCommand(cmd) && !errors.Is(err, os.ErrNotExist) {
					configLoadError = err
					*cfg = appConfig{}
					loadedConfig = appconfig.Loaded{Path: effectiveConfigPath, Fields: map[string]bool{}}
					profile, profileErr := resolveRootRedactProfile(cmd, cfg, redactProfile, redactSecrets)
					if profileErr != nil {
						return profileErr
					}
					sensitive.SetDefaultProfile(profile)
					if profile != sensitive.ProfileNone {
						wrapSensitiveOutputs(cmd)
					}
					applyOutputDefaults(cmd, cfg, opts)
					if err := applyDockerDefaults(cmd, cfg); err != nil {
						return err
					}
					configureLogging(*opts)
					return nil
				}
				return err
			}
			loadedConfig = loaded
			configResolved = true
			*cfg = loaded.Config
			profile, err := resolveRootRedactProfile(cmd, cfg, redactProfile, redactSecrets)
			if err != nil {
				return err
			}
			sensitive.SetDefaultProfile(profile)
			if profile != sensitive.ProfileNone {
				wrapSensitiveOutputs(cmd)
			}
			applyOutputDefaults(cmd, cfg, opts)
			if err := applyDockerDefaults(cmd, cfg); err != nil {
				return err
			}
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
	rootCmd.PersistentFlags().StringVar(&profileName, "profile", "", "选择命名环境，优先于 DM_PROFILE 和 default_profile")
	_ = rootCmd.RegisterFlagCompletionFunc("profile", completion.ConfigProfiles)
	rootCmd.PersistentFlags().BoolVar(&opts.Verbose, "verbose", opts.Verbose, "输出详细日志")
	rootCmd.PersistentFlags().BoolVar(&opts.Quiet, "quiet", opts.Quiet, "隐藏信息日志")
	rootCmd.PersistentFlags().BoolVar(&opts.JSON, "log-json", opts.JSON, "以 JSON 输出日志和错误，不影响业务报告格式")
	rootCmd.PersistentFlags().BoolVar(&redactSecrets, "redact-secrets", false, "显式脱敏疑似凭据和敏感值；默认管理员输出不脱敏")
	rootCmd.PersistentFlags().StringVar(&redactProfile, "redact-profile", "", "全局脱敏策略: none | basic | strict；默认读取配置且缺省为 none")
	rootCmd.PersistentFlags().StringVar(&dockerHost, "docker-host", "", "Docker daemon 地址，默认读取 DOCKER_HOST 或本地 Docker")
	rootCmd.PersistentFlags().BoolVar(&dockerTLSVerify, "docker-tls-verify", false, "校验 Docker TCP 服务端证书，要求有效证书目录；默认读取 DOCKER_TLS_VERIFY")
	rootCmd.PersistentFlags().StringVar(&dockerCertPath, "docker-cert-path", "", "Docker TLS/mTLS 证书目录，含 ca.pem/cert.pem/key.pem；默认读取 DOCKER_CERT_PATH")
	rootCmd.PersistentFlags().StringVar(&dockerAPIVersion, "docker-api-version", "", "Docker API 版本，默认读取 DOCKER_API_VERSION 或自动协商")
	rootCmd.PersistentFlags().DurationVar(&dockerTimeout, "docker-timeout", dockerapi.DefaultRequestTimeout, "Docker daemon 连接、TLS 握手和响应头超时时间")

	commandSet := newRootCommandSet(cfg)
	rootCmd.AddCommand(backup.NewBackupCommand())
	rootCmd.AddCommand(backup.NewRestoreCommandWithDefaults(func() backup.RestoreCommandDefaults {
		readyTimeout, _ := appconfig.PositiveDuration("ready_timeout", cfg.ReadyTimeout, 30*time.Second)
		return backup.RestoreCommandDefaults{ReadyTimeout: readyTimeout}
	}))
	rootCmd.AddCommand(commandSet.newImageGroup())
	rootCmd.AddCommand(commandSet.newReportGroup())
	rootCmd.AddCommand(commandSet.newImageShortcuts()...)
	rootCmd.AddCommand(commandSet.newReportShortcuts()...)
	rootCmd.AddCommand(diagnostics.NewDoctorCommandWithDefaults(func() diagnostics.DoctorDefaults {
		var resolvedConfig *appconfig.Loaded
		if configResolved {
			resolved := loadedConfig
			resolvedConfig = &resolved
		}
		return diagnostics.DoctorDefaults{
			ConfigPath:               effectiveConfigPath,
			LoadedConfig:             resolvedConfig,
			ConfigLoadError:          configLoadError,
			OutputDir:                cfg.OutputDir,
			DisableCredentialHelpers: cfg.CredentialHelpersDisabled,
			CredentialHelperTimeout:  configuredCredentialHelperTimeout(cfg),
			ResolveRegistryPolicy:    registryPolicyResolver(cfg),
		}
	}))
	rootCmd.AddCommand(completion.NewCommand())
	rootCmd.AddCommand(version.NewCommand())
	rootCmd.AddCommand(reverse.NewReverseCommand())
	rootCmd.AddCommand(reverse.NewRerunCommandWithDefaults(func() reverse.RerunCommandDefaults {
		readyTimeout, _ := appconfig.PositiveDuration("ready_timeout", cfg.ReadyTimeout, 30*time.Second)
		return reverse.RerunCommandDefaults{ReadyTimeout: readyTimeout}
	}))
	rootCmd.AddCommand(newConfigCommand(cfg, &loadedConfig))
	return rootCmd
}

func profileSelectionRequiresConfig(profile string, explicit bool) bool {
	if explicit {
		return strings.TrimSpace(profile) != ""
	}
	return strings.TrimSpace(os.Getenv(appconfig.ProfileEnvName)) != ""
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
				Proxy:                    cfg.Proxy,
				TargetOS:                 cfg.TargetOS,
				Arch:                     cfg.Arch,
				OutputDir:                cfg.OutputDir,
				DisableCredentialHelpers: cfg.CredentialHelpersDisabled,
				CredentialHelperTimeout:  configuredCredentialHelperTimeout(cfg),
				AuthRealmAllowlist:       append([]string(nil), cfg.RegistryAuthRealms...),
				RegistryPolicies:         cloneAppRegistryPolicies(cfg.Registries),
			}
		})
	}
	registryCommand := func() *cobra.Command {
		return diagnostics.NewRegistryReportCommandWithDefaults(func() diagnostics.RegistryLoginCheckDefaults {
			return diagnostics.RegistryLoginCheckDefaults{
				DisableCredentialHelpers: cfg.CredentialHelpersDisabled,
				CredentialHelperTimeout:  configuredCredentialHelperTimeout(cfg),
				ResolveRegistryPolicy:    registryPolicyResolver(cfg),
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
			{name: "registry", new: registryCommand},
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

func resolveRootRedactProfile(cmd *cobra.Command, cfg *appConfig, rootProfile string, rootSecrets bool) (sensitive.Profile, error) {
	profileValue := cfg.RedactProfile
	legacyValue := cfg.RedactSecrets
	rootFlags := cmd.Root().PersistentFlags()
	rootProfileChanged := rootFlags.Changed("redact-profile")
	if rootProfileChanged {
		profileValue = rootProfile
	}
	if rootFlags.Changed("redact-secrets") {
		legacyValue = rootSecrets
		if !rootProfileChanged {
			profileValue = ""
		}
	}

	localFlags := cmd.LocalNonPersistentFlags()
	localProfileChanged := localFlags.Changed("redact-profile")
	if localProfileChanged {
		value, err := localFlags.GetString("redact-profile")
		if err != nil {
			return "", err
		}
		profileValue = value
	}
	if localFlags.Changed("redact-secrets") {
		value, err := localFlags.GetBool("redact-secrets")
		if err != nil {
			return "", err
		}
		legacyValue = value
		if !localProfileChanged {
			profileValue = ""
		}
	}
	return sensitive.NormalizeProfile(profileValue, legacyValue)
}

func applyParsedRedactProfileForError(cmd *cobra.Command) {
	if cmd == nil {
		return
	}
	rootFlags := cmd.Root().PersistentFlags()
	localFlags := cmd.LocalNonPersistentFlags()
	if !rootFlags.Changed("redact-profile") && !rootFlags.Changed("redact-secrets") &&
		!localFlags.Changed("redact-profile") && !localFlags.Changed("redact-secrets") {
		return
	}
	rootProfile, profileErr := rootFlags.GetString("redact-profile")
	rootSecrets, secretsErr := rootFlags.GetBool("redact-secrets")
	if profileErr != nil || secretsErr != nil {
		return
	}
	profile, err := resolveRootRedactProfile(cmd, &appConfig{}, rootProfile, rootSecrets)
	if err == nil {
		sensitive.SetDefaultProfile(profile)
	}
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

func applyDockerDefaults(cmd *cobra.Command, cfg *appConfig) error {
	opts, err := dockerconfig.OptionsFromRootFlags(*cfg, cmd)
	if err != nil {
		return err
	}
	dockerapi.Configure(opts)
	return nil
}

func configuredCredentialHelperTimeout(cfg *appConfig) time.Duration {
	if cfg == nil {
		return registryauth.DefaultCredentialHelperTimeout
	}
	timeout, _ := appconfig.PositiveDuration("credential_helper_timeout", cfg.CredentialHelperTimeout, registryauth.DefaultCredentialHelperTimeout)
	return timeout
}

func registryPolicyResolver(cfg *appConfig) diagnostics.RegistryPolicyResolver {
	return func(registry string) (appconfig.RegistryPolicy, bool) {
		if cfg == nil {
			return appconfig.RegistryPolicy{}, false
		}
		return appconfig.ResolveRegistryPolicy(cfg.Registries, registry)
	}
}

func cloneAppRegistryPolicies(policies map[string]appconfig.RegistryPolicy) map[string]appconfig.RegistryPolicy {
	if policies == nil {
		return nil
	}
	result := make(map[string]appconfig.RegistryPolicy, len(policies))
	for registry, policy := range policies {
		if policy.CredentialScope != nil {
			policy.CredentialScope = append([]string{}, policy.CredentialScope...)
		}
		if policy.AuthRealms != nil {
			policy.AuthRealms = append([]string{}, policy.AuthRealms...)
		}
		result[registry] = policy
	}
	return result
}
