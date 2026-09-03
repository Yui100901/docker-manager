package cli

import (
	"context"
	"docker-manager/internal/appconfig"
	"docker-manager/internal/audit"
	"docker-manager/internal/commandflags"
	"docker-manager/internal/commands/backup"
	"docker-manager/internal/commands/diagnostics"
	"docker-manager/internal/commands/images"
	"docker-manager/internal/commands/pull"
	"docker-manager/internal/commands/reverse"
	"docker-manager/internal/completion"
	dockerapi "docker-manager/internal/docker"
	"docker-manager/internal/dockerconfig"
	"docker-manager/internal/registryauth"
	rpt "docker-manager/internal/report"
	"docker-manager/internal/runcontrol"
	"docker-manager/internal/sensitive"
	"docker-manager/internal/version"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func Run() int {
	sensitive.SetDefaultProfile(sensitive.ProfileNone)
	args := os.Args[1:]
	preseededProfile, profilePreseeded := preseedRedactionProfileForError(args)
	cfg := appConfig{}
	opts := outputOptions{}
	rootCmd, lifecycle := newRootCommandWithLifecycle(&cfg, &opts)
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
	}
	// lifecycle.finish resets bound flag variables so the root can be reused.
	// Preserve this execution's output mode for any command or audit-close error.
	errorOpts := opts
	if lifecycle != nil {
		if finishErr := lifecycle.finish(err); finishErr != nil {
			err = errors.Join(err, finishErr)
		}
	}
	if err != nil {
		writeCommandError(rootCmd.ErrOrStderr(), err, errorOpts)
		return commandExitCode(err)
	}
	return 0
}

// commandExitCode keeps process-level status mapping in one place. A policy
// gate is distinct from an operational/rendering failure, but an error that
// combines both must retain the operational failure status.
func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	if isCommandCanceled(err) {
		return 130
	}
	var gateErr *rpt.GateError
	if errors.As(err, &gateErr) && !hasOperationalError(err, gateErr) {
		return gateErr.ExitCode()
	}
	return 1
}

// rootLifecycle owns per-execution resources that must outlive Cobra's RunE,
// notably the audit session and the shared runtime controller cancellation.
type rootLifecycle struct {
	session              *audit.Session
	sink                 io.Closer
	runtimeCancel        context.CancelFunc
	failureCommand       *cobra.Command
	startFailure         func(*cobra.Command) error
	auditAttempted       bool
	auditFallbackConfig  *appConfig
	auditFallbackProfile string
	flagState            *commandFlagState
}

func (l *rootLifecycle) finish(commandErr error) (finishErr error) {
	if l == nil {
		return nil
	}
	if l.flagState != nil {
		defer func() {
			finishErr = errors.Join(finishErr, l.flagState.reset())
		}()
	}
	var fallbackErr error
	if commandErr != nil && l.session == nil && !l.auditAttempted && l.failureCommand != nil && l.startFailure != nil {
		fallbackErr = l.startFailure(l.failureCommand)
	}
	runtimeCancel := l.runtimeCancel
	session := l.session
	sink := l.sink
	l.runtimeCancel = nil
	l.session = nil
	l.sink = nil
	l.failureCommand = nil
	l.auditAttempted = false
	l.auditFallbackConfig = nil
	l.auditFallbackProfile = ""

	if runtimeCancel != nil {
		runtimeCancel()
	}
	if session == nil {
		if sink != nil {
			return errors.Join(fallbackErr, sink.Close())
		}
		return fallbackErr
	}
	outcome := audit.OutcomeSuccess
	if errors.Is(commandErr, context.Canceled) {
		outcome = audit.OutcomeCanceled
	} else if commandErr != nil {
		outcome = audit.OutcomeFailed
	}
	auditFinishErr := session.Finish(context.Background(), audit.FinishResult{Outcome: outcome, Err: commandErr})
	if sink != nil {
		auditFinishErr = errors.Join(auditFinishErr, sink.Close())
	}
	return errors.Join(fallbackErr, auditFinishErr)
}

type commandFlagState struct {
	root  *cobra.Command
	flags []commandFlagSnapshot
}

type commandFlagSnapshot struct {
	flag         *pflag.Flag
	defaultValue string
	slice        *repeatableSliceValue
	defaultSlice []string
}

// repeatableSliceValue preserves pflag's fresh-FlagSet behavior after a root
// command is executed more than once. pflag slice values keep an internal
// changed bit that Flag.Changed does not reset, so the first value of the next
// parse must explicitly replace the restored default.
type repeatableSliceValue struct {
	pflag.Value
	slice   pflag.SliceValue
	changed bool
}

func (v *repeatableSliceValue) Set(value string) error {
	if !v.changed {
		if err := v.slice.Replace(nil); err != nil {
			return err
		}
	}
	if err := v.Value.Set(value); err != nil {
		return err
	}
	v.changed = true
	return nil
}

func (v *repeatableSliceValue) Append(value string) error {
	return v.slice.Append(value)
}

func (v *repeatableSliceValue) Replace(values []string) error {
	return v.slice.Replace(values)
}

func (v *repeatableSliceValue) GetSlice() []string {
	return v.slice.GetSlice()
}

func captureCommandFlagState(root *cobra.Command) *commandFlagState {
	state := &commandFlagState{root: root}
	if root == nil {
		return state
	}
	root.InitDefaultHelpCmd()
	// Find runs before Cobra lazily creates the selected command's help flag.
	// Seed the root flag so --help is treated as a no-value flag while finding
	// an unknown top-level command, regardless of argument order.
	root.InitDefaultHelpFlag()
	seen := make(map[*pflag.Flag]struct{})
	captureFlagSet := func(flags *pflag.FlagSet) {
		if flags == nil {
			return
		}
		flags.VisitAll(func(flag *pflag.Flag) {
			if _, ok := seen[flag]; ok {
				return
			}
			seen[flag] = struct{}{}
			snapshot := commandFlagSnapshot{flag: flag, defaultValue: flag.Value.String()}
			if slice, ok := flag.Value.(pflag.SliceValue); ok {
				values := append([]string(nil), slice.GetSlice()...)
				wrapped := &repeatableSliceValue{Value: flag.Value, slice: slice}
				flag.Value = wrapped
				snapshot.slice = wrapped
				snapshot.defaultSlice = values
			}
			state.flags = append(state.flags, snapshot)
		})
	}
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		captureFlagSet(cmd.Flags())
		captureFlagSet(cmd.PersistentFlags())
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(root)
	return state
}

func (s *commandFlagState) reset() error {
	if s == nil {
		return nil
	}
	var resetErr error
	for i := range s.flags {
		snapshot := &s.flags[i]
		if snapshot.flag == nil {
			continue
		}
		if snapshot.slice != nil {
			if err := snapshot.slice.Replace(append([]string(nil), snapshot.defaultSlice...)); err != nil {
				resetErr = errors.Join(resetErr, fmt.Errorf("reset --%s: %w", snapshot.flag.Name, err))
			}
			snapshot.slice.changed = false
		} else if err := snapshot.flag.Value.Set(snapshot.defaultValue); err != nil {
			resetErr = errors.Join(resetErr, fmt.Errorf("reset --%s: %w", snapshot.flag.Name, err))
		}
		snapshot.flag.Changed = false
	}
	// Cobra still adds help/version flags lazily to selected leaf commands.
	// Reset those flags in addition to the construction-time snapshot.
	var resetGeneratedFlags func(*cobra.Command)
	resetGeneratedFlags = func(cmd *cobra.Command) {
		for _, name := range []string{"help", "version"} {
			flag := cmd.LocalNonPersistentFlags().Lookup(name)
			if flag == nil {
				continue
			}
			if err := flag.Value.Set("false"); err != nil {
				resetErr = errors.Join(resetErr, fmt.Errorf("reset --%s: %w", name, err))
			}
			flag.Changed = false
		}
		for _, child := range cmd.Commands() {
			resetGeneratedFlags(child)
		}
	}
	if s.root != nil {
		resetGeneratedFlags(s.root)
	}
	return resetErr
}

func hasOperationalError(err error, gate *rpt.GateError) bool {
	if err == nil {
		return false
	}
	if gate == nil {
		return true
	}
	if _, direct := err.(*rpt.GateError); direct {
		return false
	}
	if grouped, ok := err.(interface{ Unwrap() []error }); ok {
		items := grouped.Unwrap()
		if len(items) == 0 {
			return true
		}
		seenChild := false
		for _, item := range items {
			if item == nil {
				continue
			}
			seenChild = true
			if hasOperationalError(item, gate) {
				return true
			}
		}
		// A non-gate wrapper with no meaningful children is itself an
		// operational error; only a transparent wrapper around a gate is
		// eligible for the gate-only exit status.
		return !seenChild
	}
	if unwrapped := errors.Unwrap(err); unwrapped != nil {
		return hasOperationalError(unwrapped, gate)
	}
	return true
}

type unavailableAuditSink struct{ err error }

func (s unavailableAuditSink) Append(context.Context, audit.Event) error {
	if s.err != nil {
		return s.err
	}
	return errors.New("audit sink unavailable")
}

func (s unavailableAuditSink) Close() error { return nil }

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
	cmd, _ := newRootCommandWithLifecycle(cfg, opts)
	return cmd
}

func newRootCommandWithLifecycle(cfg *appConfig, opts *outputOptions) (*cobra.Command, *rootLifecycle) {
	configPath := defaultConfigPath
	effectiveConfigPath := configPath
	loadedConfig := appconfig.Loaded{Path: configPath, Fields: map[string]bool{}}
	var configLoadError error
	configResolved := false
	var profileName string
	var activeProfile string
	var dockerHost string
	var dockerTLSVerify bool
	var dockerCertPath string
	var dockerAPIVersion string
	dockerTimeout := dockerapi.DefaultRequestTimeout
	var redactSecrets bool
	var redactProfile string
	var auditFile string
	var auditActor string
	var auditDetail string
	var auditOnError string
	var auditRequired bool
	var auditKeyFile string
	var auditMaxBytes int64
	var auditMaxFiles int
	outputsWrapped := false
	lifecycle := &rootLifecycle{}
	runtimeLimits := make(map[*cobra.Command]*runcontrol.Limits)
	wrapSensitiveOutputs := func(cmd *cobra.Command) {
		if outputsWrapped {
			return
		}
		cmd.SetOut(sensitive.NewDynamicWriter(cmd.OutOrStdout()))
		cmd.SetErr(sensitive.NewDynamicWriter(cmd.ErrOrStderr()))
		outputsWrapped = true
	}
	prepareExecution := func(cmd *cobra.Command) error {
		if err := initializeAuditSession(cmd, cfg, activeProfile, &auditConfigOptions{
			File:            auditFile,
			Actor:           auditActor,
			Detail:          auditDetail,
			OnError:         auditOnError,
			Required:        auditRequired,
			KeyFile:         auditKeyFile,
			MaxBytes:        auditMaxBytes,
			MaxFiles:        auditMaxFiles,
			FileChanged:     cmd.Root().PersistentFlags().Changed("audit-file"),
			ActorChanged:    cmd.Root().PersistentFlags().Changed("audit-actor"),
			DetailChanged:   cmd.Root().PersistentFlags().Changed("audit-detail"),
			OnErrorChanged:  cmd.Root().PersistentFlags().Changed("audit-on-error"),
			RequiredChanged: cmd.Root().PersistentFlags().Changed("audit-required"),
			KeyFileChanged:  cmd.Root().PersistentFlags().Changed("audit-key-file"),
			MaxBytesChanged: cmd.Root().PersistentFlags().Changed("audit-max-bytes"),
			MaxFilesChanged: cmd.Root().PersistentFlags().Changed("audit-max-files"),
		}, lifecycle); err != nil {
			return err
		}
		if err := applyReportAutomationDefaults(cmd, cfg); err != nil {
			return err
		}
		if err := applyLogBudgetDefaults(cmd, cfg); err != nil {
			return err
		}
		if err := applyRuntimeController(cmd, cfg, loadedConfig.Fields, runtimeLimits, lifecycle); err != nil {
			return err
		}
		return nil
	}
	rootCmd := &cobra.Command{
		Use:           "dm <command>",
		Short:         "Docker 运维辅助工具",
		SilenceErrors: true,
		SilenceUsage:  true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			lifecycle.failureCommand = cmd
			lifecycle.auditFallbackConfig = nil
			lifecycle.auditFallbackProfile = ""
			resetCommandExecutionContext(cmd)
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
			activeProfile = ""
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
					activeProfile, _ = appconfig.ResolveProfile(profileName, profileFlagChanged, "")
					return prepareExecution(cmd)
				}
				return err
			}
			loadedConfig = loaded
			configResolved = true
			activeProfile = loaded.Profile
			*cfg = loaded.Config
			fallbackConfig := loaded.Config
			lifecycle.auditFallbackConfig = &fallbackConfig
			lifecycle.auditFallbackProfile = loaded.Profile
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
			return prepareExecution(cmd)
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
	rootCmd.PersistentFlags().StringVar(&auditFile, "audit-file", "", "结构化审计 JSONL 文件；为空表示关闭")
	rootCmd.PersistentFlags().StringVar(&auditActor, "audit-actor", "", "审计事件中的声明操作人标识（不会替代系统操作人）")
	rootCmd.PersistentFlags().StringVar(&auditDetail, "audit-detail", "", "审计详情级别: safe | full")
	rootCmd.PersistentFlags().StringVar(&auditOnError, "audit-on-error", "", "审计写入失败策略: warn | deny-mutation | fail")
	rootCmd.PersistentFlags().BoolVar(&auditRequired, "audit-required", false, "审计写入失败时拒绝命令（等价于 --audit-on-error=fail）")
	rootCmd.PersistentFlags().StringVar(&auditKeyFile, "audit-key-file", "", "审计标识 HMAC key 文件路径")
	rootCmd.PersistentFlags().Int64Var(&auditMaxBytes, "audit-max-bytes", 0, "单个审计文件最大字节数，0 使用默认值")
	rootCmd.PersistentFlags().IntVar(&auditMaxFiles, "audit-max-files", 0, "审计轮转文件数量，0 使用默认值")

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
	registerRuntimeFlags(rootCmd, runtimeLimits)
	registerExecutionTracking(rootCmd, lifecycle)
	lifecycle.startFailure = func(cmd *cobra.Command) error {
		if cmd == nil {
			return nil
		}
		fallbackConfig := &appConfig{}
		fallbackProfile := strings.TrimSpace(profileName)
		if lifecycle.auditFallbackConfig != nil {
			fallbackConfig = lifecycle.auditFallbackConfig
			fallbackProfile = lifecycle.auditFallbackProfile
		}
		resetCommandExecutionContext(cmd)
		return initializeAuditSession(cmd, fallbackConfig, fallbackProfile, &auditConfigOptions{
			File:            auditFile,
			Actor:           auditActor,
			Detail:          auditDetail,
			OnError:         auditOnError,
			Required:        auditRequired,
			KeyFile:         auditKeyFile,
			MaxBytes:        auditMaxBytes,
			MaxFiles:        auditMaxFiles,
			FileChanged:     rootCmd.PersistentFlags().Changed("audit-file"),
			ActorChanged:    rootCmd.PersistentFlags().Changed("audit-actor"),
			DetailChanged:   rootCmd.PersistentFlags().Changed("audit-detail"),
			OnErrorChanged:  rootCmd.PersistentFlags().Changed("audit-on-error"),
			RequiredChanged: rootCmd.PersistentFlags().Changed("audit-required"),
			KeyFileChanged:  rootCmd.PersistentFlags().Changed("audit-key-file"),
			MaxBytesChanged: rootCmd.PersistentFlags().Changed("audit-max-bytes"),
			MaxFilesChanged: rootCmd.PersistentFlags().Changed("audit-max-files"),
		}, lifecycle)
	}
	lifecycle.flagState = captureCommandFlagState(rootCmd)
	return rootCmd, lifecycle
}

func profileSelectionRequiresConfig(profile string, explicit bool) bool {
	if explicit {
		return strings.TrimSpace(profile) != ""
	}
	return strings.TrimSpace(os.Getenv(appconfig.ProfileEnvName)) != ""
}

type auditConfigOptions struct {
	File            string
	Actor           string
	Detail          string
	OnError         string
	Required        bool
	KeyFile         string
	MaxBytes        int64
	MaxFiles        int
	FileChanged     bool
	ActorChanged    bool
	DetailChanged   bool
	OnErrorChanged  bool
	RequiredChanged bool
	KeyFileChanged  bool
	MaxBytesChanged bool
	MaxFilesChanged bool
}

func initializeAuditSession(cmd *cobra.Command, cfg *appConfig, profile string, flags *auditConfigOptions, lifecycle *rootLifecycle) error {
	if cmd == nil || lifecycle == nil {
		return nil
	}
	if flags == nil {
		flags = &auditConfigOptions{}
	}
	if lifecycle.session != nil {
		return nil
	}
	if cfg == nil {
		cfg = &appConfig{}
	}
	maxBytes := cfg.AuditMaxBytes
	if flags.MaxBytesChanged {
		maxBytes = flags.MaxBytes
	}
	maxFiles := cfg.AuditMaxFiles
	if flags.MaxFilesChanged {
		maxFiles = flags.MaxFiles
	}
	if err := appconfig.ValidateAuditRotation(maxBytes, maxFiles); err != nil {
		lifecycle.auditAttempted = true
		return err
	}
	file := cfg.AuditFile
	if flags.FileChanged {
		file = flags.File
	}
	if strings.TrimSpace(file) == "" {
		return nil
	}
	lifecycle.auditAttempted = true
	detailValue := cfg.AuditDetail
	if flags.DetailChanged {
		detailValue = flags.Detail
	}
	detail := audit.Detail(strings.ToLower(strings.TrimSpace(detailValue)))
	if detail == "" {
		detail = audit.DetailSafe
	}
	if detail != audit.DetailSafe && detail != audit.DetailFull {
		return fmt.Errorf("audit_detail must be safe or full")
	}
	policyValue := cfg.AuditOnError
	if flags.OnErrorChanged {
		policyValue = flags.OnError
	}
	if flags.RequiredChanged && flags.Required {
		policyValue = "fail"
	}
	policy := audit.FailurePolicy(strings.ToLower(strings.TrimSpace(policyValue)))
	switch policy {
	case "":
		policy = audit.FailureDenyMutation
	case "fail", "required":
		policy = audit.FailureRequired
	case audit.FailureWarn, audit.FailureDenyMutation:
	default:
		return fmt.Errorf("audit_on_error must be warn, deny-mutation, or fail")
	}
	keyFile := cfg.AuditKeyFile
	if flags.KeyFileChanged {
		keyFile = flags.KeyFile
	}
	actor := cfg.AuditActor
	if flags.ActorChanged {
		actor = flags.Actor
	}
	fileSink, openErr := audit.OpenFileSink(audit.FileOptions{
		Path: file, KeyPath: keyFile, MaxBytes: maxBytes, MaxFiles: maxFiles,
	})
	var sink audit.Sink
	if openErr != nil {
		sink = unavailableAuditSink{err: openErr}
		if policy == audit.FailureRequired {
			return fmt.Errorf("open audit sink: %w", openErr)
		}
	} else {
		sink = fileSink
		lifecycle.sink = fileSink
	}
	operation := auditOperationName(cmd)
	session, err := audit.NewSession(audit.SessionOptions{
		Sink: sink, Detail: detail, FailurePolicy: policy,
		Operation: operation, Command: cmd.CommandPath(), Profile: profile,
		Endpoint: dockerapi.Endpoint(), Operator: audit.CurrentOperator(actor),
		Warning: func(warn error) {
			if warn != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Audit warning: %s\n", sensitive.RedactText(warn.Error(), sensitive.DefaultProfile()))
			}
		},
	})
	if err != nil {
		if lifecycle.sink != nil {
			_ = lifecycle.sink.Close()
			lifecycle.sink = nil
		}
		return err
	}
	lifecycle.session = session
	if err := session.Start(cmd.Context()); err != nil {
		if lifecycle.sink != nil {
			_ = lifecycle.sink.Close()
			lifecycle.sink = nil
		}
		lifecycle.session = nil
		return err
	}
	cmd.SetContext(audit.WithSession(cmd.Context(), session))
	return nil
}

func auditOperationName(cmd *cobra.Command) string {
	if cmd == nil {
		return "command"
	}
	parts := strings.Fields(cmd.CommandPath())
	if len(parts) > 0 && parts[0] == "dm" {
		parts = parts[1:]
	}
	if len(parts) == 0 {
		return "command"
	}
	for i := range parts {
		parts[i] = strings.ToLower(strings.TrimSpace(parts[i]))
		parts[i] = strings.NewReplacer("-", "_", "/", "_").Replace(parts[i])
	}
	return strings.Join(parts, ".")
}

func registerRuntimeFlags(root *cobra.Command, runtimeLimitsForCommand map[*cobra.Command]*runcontrol.Limits) {
	if root == nil {
		return
	}
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		switch runtimeControlForCommand(cmd) {
		case runtimeControlFlags:
			limits := &runcontrol.Limits{}
			commandflags.AddRuntimeFlags(cmd, limits)
			runtimeLimitsForCommand[cmd] = limits
		case runtimeControlConfigOnly:
			// Pull keeps its established --concurrency, --timeout, and
			// --total-timeout meanings while sharing configured E2 limits.
			runtimeLimitsForCommand[cmd] = nil
		}
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(root)
}

func applyRuntimeController(cmd *cobra.Command, cfg *appConfig, configuredFields map[string]bool, limitsByCommand map[*cobra.Command]*runcontrol.Limits, lifecycle *rootLifecycle) error {
	if cmd == nil || lifecycle == nil {
		return nil
	}
	configured, controlled := limitsByCommand[cmd]
	if !controlled {
		return nil
	}
	limits := runcontrol.Limits{Concurrency: defaultOperationConcurrency}
	if cfg != nil {
		if configuredFields["operation_concurrency"] {
			limits.Concurrency = cfg.OperationConcurrency
		}
		limits.Rate = cfg.OperationRateLimit
		limits.MaxItems = cfg.OperationMaxItems
		if cfg.OperationTimeout != "" {
			parsed, err := appconfig.PositiveDuration("operation_timeout", cfg.OperationTimeout, 0)
			if err != nil {
				return err
			}
			limits.Timeout = parsed
		}
	}
	if configured != nil {
		flags := cmd.Flags()
		if flags.Changed("concurrency") {
			limits.Concurrency = configured.Concurrency
		}
		if flags.Changed("operation-timeout") {
			limits.Timeout = configured.Timeout
		}
		if flags.Changed("rate-limit") {
			limits.Rate = configured.Rate
		}
		if flags.Changed("max-items") {
			limits.MaxItems = configured.MaxItems
		}
	}
	if limits == (runcontrol.Limits{}) {
		return nil
	}
	controller, err := runcontrol.New(limits)
	if err != nil {
		return err
	}
	ctx, cancel := controller.Context(cmd.Context())
	cmd.SetContext(ctx)
	lifecycle.runtimeCancel = cancel
	return nil
}

const defaultOperationConcurrency = 8

type runtimeControlMode uint8

const (
	runtimeControlNone runtimeControlMode = iota
	runtimeControlFlags
	runtimeControlConfigOnly
)

func runtimeControlForCommand(cmd *cobra.Command) runtimeControlMode {
	if cmd == nil || cmd.RunE == nil {
		return runtimeControlNone
	}
	switch cmd.Name() {
	case "pull":
		return runtimeControlConfigOnly
	case "backup", "restore", "reverse", "rerun", "doctor", "tree",
		"health", "network", "logs", "diff", "prune", "volumes", "registry", "all":
		return runtimeControlFlags
	default:
		return runtimeControlNone
	}
}

func resetCommandExecutionContext(cmd *cobra.Command) {
	if cmd == nil {
		return
	}
	root := cmd.Root()
	if root == nil || root == cmd {
		return
	}
	ctx := root.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	cmd.SetContext(ctx)
}

func registerExecutionTracking(root *cobra.Command, lifecycle *rootLifecycle) {
	if root == nil || lifecycle == nil {
		return
	}
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		lifecycle.failureCommand = cmd
		return err
	})
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		if cmd.Runnable() && cmd.Args != nil {
			validateArgs := cmd.Args
			cmd.Args = func(current *cobra.Command, args []string) error {
				lifecycle.failureCommand = current
				return validateArgs(current, args)
			}
		}
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(root)
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

// applyReportAutomationDefaults maps the global configuration policy onto a
// report leaf's local flags. Configured thresholds are namespaced; individual
// reports receive only their scope with the prefix removed, while report all
// retains the full metric name.
func applyReportAutomationDefaults(cmd *cobra.Command, cfg *appConfig) error {
	if cmd == nil || cfg == nil {
		return nil
	}
	flags := cmd.Flags()
	if failOn := flags.Lookup("fail-on"); failOn != nil && !flags.Changed("fail-on") && strings.TrimSpace(cfg.FailOn) != "" {
		if err := failOn.Value.Set(cfg.FailOn); err != nil {
			return fmt.Errorf("apply fail_on: %w", err)
		}
	}
	threshold := flags.Lookup("threshold")
	if threshold == nil || flags.Changed("threshold") {
		return nil
	}
	values, err := configuredThresholdsForCommand(cmd, cfg.Thresholds)
	if err != nil {
		return fmt.Errorf("apply thresholds: %w", err)
	}
	// pflag's StringArray value exposes Replace, which avoids retaining the
	// previous config-derived value across repeated command execution.
	if replacer, ok := threshold.Value.(interface{ Replace([]string) error }); ok {
		if err := replacer.Replace(values); err != nil {
			return fmt.Errorf("apply thresholds: %w", err)
		}
		return nil
	}
	for _, value := range values {
		if err := threshold.Value.Set(value); err != nil {
			return fmt.Errorf("apply threshold %q: %w", value, err)
		}
	}
	return nil
}

func configuredThresholdsForCommand(cmd *cobra.Command, values []string) ([]string, error) {
	scope, namespaced := reportThresholdScope(cmd)
	if scope == "" && !namespaced {
		return nil, nil
	}
	thresholds, err := rpt.ParseScopedThresholds(values)
	if err != nil {
		return nil, err
	}
	routed := make([]string, 0, len(thresholds))
	for _, threshold := range thresholds {
		if namespaced {
			routed = append(routed, threshold.ScopedValue())
			continue
		}
		if threshold.Scope == scope {
			routed = append(routed, threshold.UnscopedValue())
		}
	}
	return routed, nil
}

func reportThresholdScope(cmd *cobra.Command) (scope string, namespaced bool) {
	if cmd == nil {
		return "", false
	}
	switch cmd.Name() {
	case rpt.MetricScopeHealth, rpt.MetricScopeNetwork, rpt.MetricScopeLogs, rpt.MetricScopeVolumes, rpt.MetricScopePrune:
		return cmd.Name(), false
	case "all":
		return "", true
	default:
		return "", false
	}
}

func applyLogBudgetDefaults(cmd *cobra.Command, cfg *appConfig) error {
	if cmd == nil || cfg == nil {
		return nil
	}
	flags := cmd.Flags()
	for _, setting := range []struct {
		flag  string
		field string
		value string
	}{
		{flag: "max-log-bytes", field: "max_log_bytes", value: cfg.MaxLogBytes},
		{flag: "max-total-log-bytes", field: "max_total_log_bytes", value: cfg.MaxTotalLogBytes},
	} {
		flag := flags.Lookup(setting.flag)
		if flag == nil || flags.Changed(setting.flag) || strings.TrimSpace(setting.value) == "" {
			continue
		}
		if err := flag.Value.Set(setting.value); err != nil {
			return fmt.Errorf("apply %s: %w", setting.field, err)
		}
	}
	return nil
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
