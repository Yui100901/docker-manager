package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"docker-manager/internal/appconfig"
	"docker-manager/internal/audit"
	"docker-manager/internal/commandflags"
	"docker-manager/internal/docker"
	rpt "docker-manager/internal/report"

	"github.com/spf13/cobra"
)

const (
	defaultPullOS   = "linux"
	defaultPullArch = "amd64"
)

type configShowReport struct {
	Path              string            `json:"path"`
	Exists            bool              `json:"exists"`
	Profile           string            `json:"profile,omitempty"`
	ProfileSource     string            `json:"profile_source"`
	AvailableProfiles []string          `json:"available_profiles,omitempty"`
	Effective         bool              `json:"effective"`
	Values            map[string]any    `json:"values"`
	Sources           map[string]string `json:"sources,omitempty"`
}

func newConfigCommand(cfg *appConfig, loaded *appconfig.Loaded) *cobra.Command {
	configCmd := &cobra.Command{
		Use:   "config",
		Short: "校验并查看配置",
	}
	configCmd.AddCommand(newConfigValidateCommand(loaded))
	configCmd.AddCommand(newConfigShowCommand(cfg, loaded))
	configCmd.AddCommand(newConfigProfilesCommand(loaded))
	return configCmd
}

func newConfigValidateCommand(loaded *appconfig.Loaded) *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "严格校验配置文件字段和值",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if loaded == nil || !loaded.Exists {
				fmt.Fprintf(cmd.OutOrStdout(), "配置有效：未找到隐式默认配置，使用内置默认值 (%s)\n", configLoadedPath(loaded))
				return nil
			}
			if loaded.Profile != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "配置有效: %s (profile=%s, source=%s)\n", loaded.Path, loaded.Profile, loaded.ProfileSource)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "配置有效: %s (未选择 profile)\n", loaded.Path)
			return nil
		},
	}
}

func newConfigProfilesCommand(loaded *appconfig.Loaded) *cobra.Command {
	return &cobra.Command{
		Use:   "profiles",
		Short: "列出配置文件中的命名环境",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			if loaded == nil || len(loaded.AvailableProfiles) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "未配置 profile")
				return
			}
			for _, name := range loaded.AvailableProfiles {
				marker := " "
				if name == loaded.Profile {
					marker = "*"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", marker, name)
			}
		},
	}
}

func newConfigShowCommand(cfg *appConfig, loaded *appconfig.Loaded) *cobra.Command {
	var (
		effective  bool
		showSource bool
		format     string
	)
	cmd := &cobra.Command{
		Use:   "show",
		Short: "显示配置值及其来源",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			report := buildConfigShowReport(cmd, cfg, loaded, effective, showSource)
			return rpt.Print(cmd.OutOrStdout(), format, report, func(w io.Writer) {
				printConfigShowReport(w, report)
			})
		},
	}
	cmd.Flags().BoolVar(&effective, "effective", true, "显示 flag、配置、环境变量和默认值合并后的最终配置")
	cmd.Flags().BoolVar(&showSource, "show-source", false, "显示每个配置值的来源")
	commandflags.AddReportFormatFlag(cmd, &format)
	return cmd
}

func buildConfigShowReport(cmd *cobra.Command, cfg *appConfig, loaded *appconfig.Loaded, effective, showSource bool) configShowReport {
	if cfg == nil {
		cfg = &appConfig{}
	}
	report := configShowReport{
		Path:              configLoadedPath(loaded),
		Exists:            loaded != nil && loaded.Exists,
		Profile:           loadedProfile(loaded),
		ProfileSource:     loadedProfileSource(loaded),
		AvailableProfiles: loadedAvailableProfiles(loaded),
		Effective:         effective,
		Values:            make(map[string]any),
	}
	if showSource {
		report.Sources = make(map[string]string)
	}
	set := func(name string, value any, source string) {
		report.Values[name] = value
		if showSource {
			report.Sources[name] = source
		}
	}

	configSource := func(field string) string {
		return loadedFieldSource(loaded, field)
	}
	rootFlags := cmd.Root().PersistentFlags()
	configPathSource := "default"
	if rootFlags.Changed("config") {
		configPathSource = "flag:--config"
	} else if strings.TrimSpace(os.Getenv(appconfig.EnvName)) != "" {
		configPathSource = "env:" + appconfig.EnvName
	}
	set("config_path", report.Path, configPathSource)
	set("selected_profile", report.Profile, report.ProfileSource)
	set("available_profiles", append([]string(nil), report.AvailableProfiles...), loadedProfilesSource(loaded))
	set("default_profile", cfg.DefaultProfile, configSource("default_profile"))

	if !effective {
		setRawConfigValues(set, *cfg, configSource)
		addRegistrySourceDetails(&report, loaded)
		return report
	}

	set("proxy", cfg.Proxy, configSource("proxy"))
	targetOS, targetOSSource := configStringOrDefault(cfg.TargetOS, defaultPullOS, configSource("os"))
	arch, archSource := configStringOrDefault(cfg.Arch, defaultPullArch, configSource("arch"))
	set("os", targetOS, targetOSSource)
	set("arch", arch, archSource)
	set("output_dir", cfg.OutputDir, configSource("output_dir"))
	set("ca_file", cfg.CAFile, configSource("ca_file"))
	set("ca_path", cfg.CAPath, configSource("ca_path"))
	set("registry_ca_file", cfg.RegistryCAFile, configSource("registry_ca_file"))
	set("registry_ca_path", cfg.RegistryCAPath, configSource("registry_ca_path"))
	set("ready_timeout", durationDisplay(cfg.ReadyTimeout, 30*time.Second), configSource("ready_timeout"))
	operationConcurrency := cfg.OperationConcurrency
	operationConcurrencySource := configSource("operation_concurrency")
	if operationConcurrencySource == "default" {
		operationConcurrency = defaultOperationConcurrency
	}
	set("operation_concurrency", operationConcurrency, operationConcurrencySource)
	set("operation_timeout", durationDisplayOptional(cfg.OperationTimeout), configSource("operation_timeout"))
	set("operation_rate_limit", cfg.OperationRateLimit, configSource("operation_rate_limit"))
	set("operation_max_items", cfg.OperationMaxItems, configSource("operation_max_items"))
	set("max_log_bytes", byteSizeDisplay(cfg.MaxLogBytes, 16<<20), configSource("max_log_bytes"))
	set("max_total_log_bytes", byteSizeDisplay(cfg.MaxTotalLogBytes, 256<<20), configSource("max_total_log_bytes"))
	set("fail_on", configStringDefault(cfg.FailOn, "none"), configSource("fail_on"))
	set("thresholds", append([]string(nil), cfg.Thresholds...), configSource("thresholds"))
	setEffectiveAuditValues(set, cmd, cfg, configSource)
	effectiveRedactProfile, redactProfileSource := configShowRedactProfile(cmd, cfg, loaded)
	set("redact_profile", effectiveRedactProfile, redactProfileSource)
	redactSecretsChanged := rootFlags.Changed("redact-secrets")
	set(
		"redact_secrets",
		flagOrConfigBool(redactSecretsChanged, cfg.RedactSecrets, "redact-secrets", configSource, cmd),
		outputFlagSource(redactSecretsChanged, "redact-secrets", configSource("redact_secrets")),
	)
	set("credential_helpers_disabled", cfg.CredentialHelpersDisabled, configSource("credential_helpers_disabled"))
	set("credential_helper_timeout", configuredCredentialHelperTimeout(cfg).String(), configSource("credential_helper_timeout"))
	set("registry_auth_realms", append([]string(nil), cfg.RegistryAuthRealms...), configSource("registry_auth_realms"))
	set("registries", cfg.Registries, configSource("registries"))

	verbose, verboseSource, quiet, quietSource := configShowOutputValues(cmd, cfg, configSource)
	set("verbose", verbose, verboseSource)
	set("quiet", quiet, quietSource)
	set("log_json", flagOrConfigBool(rootFlags.Changed("log-json"), cfg.JSON, "log-json", configSource, cmd), outputFlagSource(rootFlags.Changed("log-json"), "log-json", configSource("log_json")))

	dockerOptions := docker.EffectiveOptions()
	set("docker_host", docker.Endpoint(), dockerValueSource(rootFlags.Changed("docker-host"), "docker_host", "DOCKER_HOST", loaded))
	set("docker_tls_verify", boolPointerValue(dockerOptions.TLSVerify), dockerValueSource(rootFlags.Changed("docker-tls-verify"), "docker_tls_verify", "DOCKER_TLS_VERIFY", loaded))
	set("docker_cert_path", dockerOptions.CertPath, dockerValueSource(rootFlags.Changed("docker-cert-path"), "docker_cert_path", "DOCKER_CERT_PATH", loaded))
	set("docker_api_version", dockerOptions.APIVersion, dockerValueSource(rootFlags.Changed("docker-api-version"), "docker_api_version", "DOCKER_API_VERSION", loaded))
	set("docker_timeout", dockerOptions.Timeout.String(), dockerValueSource(rootFlags.Changed("docker-timeout"), "docker_timeout", "", loaded))
	addRegistrySourceDetails(&report, loaded)
	return report
}

func setRawConfigValues(set func(string, any, string), cfg appConfig, source func(string) string) {
	set("default_profile", cfg.DefaultProfile, source("default_profile"))
	set("proxy", cfg.Proxy, source("proxy"))
	set("os", cfg.TargetOS, source("os"))
	set("arch", cfg.Arch, source("arch"))
	set("output_dir", cfg.OutputDir, source("output_dir"))
	set("docker_host", cfg.DockerHost, source("docker_host"))
	set("docker_tls_verify", boolPointerValue(cfg.DockerTLSVerify), source("docker_tls_verify"))
	set("docker_cert_path", cfg.DockerCertPath, source("docker_cert_path"))
	set("docker_api_version", cfg.DockerAPIVersion, source("docker_api_version"))
	set("docker_timeout", cfg.DockerTimeout, source("docker_timeout"))
	set("ca_file", cfg.CAFile, source("ca_file"))
	set("ca_path", cfg.CAPath, source("ca_path"))
	set("registry_ca_file", cfg.RegistryCAFile, source("registry_ca_file"))
	set("registry_ca_path", cfg.RegistryCAPath, source("registry_ca_path"))
	set("ready_timeout", cfg.ReadyTimeout, source("ready_timeout"))
	set("operation_concurrency", cfg.OperationConcurrency, source("operation_concurrency"))
	set("operation_timeout", cfg.OperationTimeout, source("operation_timeout"))
	set("operation_rate_limit", cfg.OperationRateLimit, source("operation_rate_limit"))
	set("operation_max_items", cfg.OperationMaxItems, source("operation_max_items"))
	set("max_log_bytes", cfg.MaxLogBytes, source("max_log_bytes"))
	set("max_total_log_bytes", cfg.MaxTotalLogBytes, source("max_total_log_bytes"))
	set("fail_on", cfg.FailOn, source("fail_on"))
	set("thresholds", append([]string(nil), cfg.Thresholds...), source("thresholds"))
	set("audit_file", cfg.AuditFile, source("audit_file"))
	set("audit_actor", cfg.AuditActor, source("audit_actor"))
	set("audit_detail", cfg.AuditDetail, source("audit_detail"))
	set("audit_on_error", cfg.AuditOnError, source("audit_on_error"))
	set("audit_max_bytes", cfg.AuditMaxBytes, source("audit_max_bytes"))
	set("audit_max_files", cfg.AuditMaxFiles, source("audit_max_files"))
	set("audit_key_file", cfg.AuditKeyFile, source("audit_key_file"))
	redactProfileSource := source("redact_profile")
	if redactProfileSource == "default" {
		redactProfileSource = source("redact_secrets")
	}
	set("redact_profile", cfg.EffectiveRedactProfile(), redactProfileSource)
	set("redact_secrets", cfg.RedactSecrets, source("redact_secrets"))
	set("credential_helpers_disabled", cfg.CredentialHelpersDisabled, source("credential_helpers_disabled"))
	set("credential_helper_timeout", cfg.CredentialHelperTimeout, source("credential_helper_timeout"))
	set("registry_auth_realms", append([]string(nil), cfg.RegistryAuthRealms...), source("registry_auth_realms"))
	set("registries", cfg.Registries, source("registries"))
	set("verbose", cfg.Verbose, source("verbose"))
	set("quiet", cfg.Quiet, source("quiet"))
	set("log_json", cfg.JSON, source("log_json"))
}

func printConfigShowReport(w io.Writer, report configShowReport) {
	fmt.Fprintf(w, "配置文件: %s (exists=%v)\n", report.Path, report.Exists)
	if report.Profile == "" {
		fmt.Fprintf(w, "活动 profile: <none> [source=%s]\n", report.ProfileSource)
	} else {
		fmt.Fprintf(w, "活动 profile: %s [source=%s]\n", report.Profile, report.ProfileSource)
	}
	keys := make([]string, 0, len(report.Values))
	for key := range report.Values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if source := report.Sources[key]; source != "" {
			fmt.Fprintf(w, "%s: %v [source=%s]\n", key, report.Values[key], source)
			continue
		}
		fmt.Fprintf(w, "%s: %v\n", key, report.Values[key])
	}
	var sourceOnly []string
	for key := range report.Sources {
		if _, exists := report.Values[key]; !exists {
			sourceOnly = append(sourceOnly, key)
		}
	}
	sort.Strings(sourceOnly)
	for _, key := range sourceOnly {
		fmt.Fprintf(w, "%s [source=%s]\n", key, report.Sources[key])
	}
}

func addRegistrySourceDetails(report *configShowReport, loaded *appconfig.Loaded) {
	if report == nil || report.Sources == nil || loaded == nil {
		return
	}
	unique := map[string]struct{}{}
	for field, source := range loaded.FieldSources {
		if !strings.HasPrefix(field, "registries.") || source == "" {
			continue
		}
		report.Sources[field] = source
		unique[source] = struct{}{}
	}
	if len(unique) > 1 {
		report.Sources["registries"] = "mixed"
		return
	}
	for source := range unique {
		report.Sources["registries"] = source
	}
}

func loadedProfile(loaded *appconfig.Loaded) string {
	if loaded == nil {
		return ""
	}
	return loaded.Profile
}

func loadedProfileSource(loaded *appconfig.Loaded) string {
	if loaded == nil || loaded.ProfileSource == "" {
		return "default"
	}
	return loaded.ProfileSource
}

func loadedAvailableProfiles(loaded *appconfig.Loaded) []string {
	if loaded == nil {
		return nil
	}
	return append([]string(nil), loaded.AvailableProfiles...)
}

func loadedProfilesSource(loaded *appconfig.Loaded) string {
	if loaded != nil && loaded.Exists && (loaded.Fields["profiles"] || len(loaded.AvailableProfiles) > 0) {
		return "config:" + loaded.Path
	}
	return "default"
}

func loadedFieldSource(loaded *appconfig.Loaded, field string) string {
	if loaded == nil {
		return "default"
	}
	if source := loaded.FieldSources[field]; source != "" {
		return source
	}
	if loaded.Fields[field] {
		return "config:" + loaded.Path
	}
	return "default"
}

func configLoadedPath(loaded *appconfig.Loaded) string {
	if loaded == nil || strings.TrimSpace(loaded.Path) == "" {
		return appconfig.DefaultPath
	}
	return loaded.Path
}

func durationDisplay(value string, fallback time.Duration) string {
	parsed, err := appconfig.PositiveDuration("duration", value, fallback)
	if err != nil {
		return value
	}
	return parsed.String()
}

func durationDisplayOptional(value string) string {
	if strings.TrimSpace(value) == "" {
		return "disabled"
	}
	return durationDisplay(value, 0)
}

func byteSizeDisplay(value string, fallback uint64) uint64 {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	parsed, err := parseConfigByteSize(value)
	if err != nil {
		return 0
	}
	return parsed
}

func parseConfigByteSize(value string) (uint64, error) {
	value = strings.TrimSpace(value)
	multiplier := uint64(1)
	switch strings.ToUpper(value[len(value)-1:]) {
	case "K":
		multiplier, value = 1<<10, value[:len(value)-1]
	case "M":
		multiplier, value = 1<<20, value[:len(value)-1]
	case "G":
		multiplier, value = 1<<30, value[:len(value)-1]
	case "T":
		multiplier, value = 1<<40, value[:len(value)-1]
	}
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, err
	}
	return parsed * multiplier, nil
}

func setEffectiveAuditValues(set func(string, any, string), cmd *cobra.Command, cfg *appConfig, configSource func(string) string) {
	rootFlags := cmd.Root().PersistentFlags()
	stringValue := func(configured, configuredSource string, flagNames ...string) (string, string) {
		for _, name := range flagNames {
			flag := rootFlags.Lookup(name)
			if flag == nil || !flag.Changed {
				continue
			}
			value, err := rootFlags.GetString(name)
			if err == nil {
				return value, "flag:--" + name
			}
		}
		return configured, configuredSource
	}

	auditFile, auditFileSource := stringValue(cfg.AuditFile, configSource("audit_file"), "audit-file")
	set("audit_file", auditFile, auditFileSource)
	auditActor, auditActorSource := stringValue(cfg.AuditActor, configSource("audit_actor"), "audit-actor")
	set("audit_actor", auditActor, auditActorSource)
	auditKeyFile, auditKeyFileSource := stringValue(cfg.AuditKeyFile, configSource("audit_key_file"), "audit-key-file")
	set("audit_key_file", auditKeyFile, auditKeyFileSource)

	detail, detailSource := stringValue(cfg.AuditDetail, configSource("audit_detail"), "audit-detail", "audit-level")
	detail = strings.ToLower(strings.TrimSpace(detail))
	if detail == "" {
		detail = string(audit.DetailSafe)
	}
	set("audit_detail", detail, detailSource)

	policy, policySource := stringValue(cfg.AuditOnError, configSource("audit_on_error"), "audit-on-error", "audit-failure-policy")
	policy = strings.ToLower(strings.TrimSpace(policy))
	switch policy {
	case "":
		policy = string(audit.FailureDenyMutation)
	case "required":
		policy = "fail"
	}
	requiredChanged := rootFlags.Lookup("audit-required") != nil && rootFlags.Changed("audit-required")
	requiredFlag := false
	if rootFlags.Lookup("audit-required") != nil {
		requiredFlag, _ = rootFlags.GetBool("audit-required")
	}
	if requiredChanged && requiredFlag {
		policy = "fail"
		policySource = "flag:--audit-required"
	}
	set("audit_on_error", policy, policySource)
	requiredSource := policySource
	if requiredChanged && !requiredFlag && policy != "fail" {
		requiredSource = "flag:--audit-required"
	}
	set("audit_required", policy == "fail", requiredSource)

	maxBytes := cfg.AuditMaxBytes
	maxBytesSource := configSource("audit_max_bytes")
	if rootFlags.Lookup("audit-max-bytes") != nil && rootFlags.Changed("audit-max-bytes") {
		if value, err := rootFlags.GetInt64("audit-max-bytes"); err == nil {
			maxBytes = value
			maxBytesSource = "flag:--audit-max-bytes"
		}
	}
	set("audit_max_bytes", configInt64OrDefault(maxBytes, audit.DefaultAuditMaxBytes), maxBytesSource)

	maxFiles := cfg.AuditMaxFiles
	maxFilesSource := configSource("audit_max_files")
	if rootFlags.Lookup("audit-max-files") != nil && rootFlags.Changed("audit-max-files") {
		if value, err := rootFlags.GetInt("audit-max-files"); err == nil {
			maxFiles = value
			maxFilesSource = "flag:--audit-max-files"
		}
	}
	set("audit_max_files", configIntOrDefault(maxFiles, audit.DefaultAuditMaxFiles), maxFilesSource)

	if systemActor := rootFlags.Lookup("audit-system-actor"); systemActor != nil {
		source := "default"
		if systemActor.Changed {
			source = "flag:--audit-system-actor"
		}
		set("audit_system_actor", systemActor.Value.String(), source)
	}
}

func configStringDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func configIntOrDefault(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}

func configInt64OrDefault(value, fallback int64) int64 {
	if value == 0 {
		return fallback
	}
	return value
}

func configStringOrDefault(value, fallback, source string) (string, string) {
	if value == "" {
		return fallback, "default"
	}
	return value, source
}

func flagOrConfigBool(changed, configured bool, name string, _ func(string) string, cmd *cobra.Command) bool {
	if !changed {
		return configured
	}
	value, err := cmd.Root().PersistentFlags().GetBool(name)
	if err != nil {
		return configured
	}
	return value
}

func outputFlagSource(changed bool, flag, configuredSource string) string {
	if changed {
		return "flag:--" + flag
	}
	return configuredSource
}

func configShowOutputValues(cmd *cobra.Command, cfg *appConfig, configSource func(string) string) (bool, string, bool, string) {
	flags := cmd.Root().PersistentFlags()
	verboseChanged := flags.Changed("verbose")
	quietChanged := flags.Changed("quiet")
	verbose := flagOrConfigBool(verboseChanged, cfg.Verbose, "verbose", configSource, cmd)
	quiet := flagOrConfigBool(quietChanged, cfg.Quiet, "quiet", configSource, cmd)
	verboseSource := outputFlagSource(verboseChanged, "verbose", configSource("verbose"))
	quietSource := outputFlagSource(quietChanged, "quiet", configSource("quiet"))

	// Keep this precedence identical to applyOutputDefaults. A true explicit
	// output mode also becomes the source of the opposite mode's forced false.
	if verboseChanged && verbose {
		quiet = false
		quietSource = "flag:--verbose"
	}
	if quietChanged && quiet {
		verbose = false
		verboseSource = "flag:--quiet"
	}
	return verbose, verboseSource, quiet, quietSource
}

func dockerValueSource(flagChanged bool, field, env string, loaded *appconfig.Loaded) string {
	if flagChanged {
		return "flag:--" + strings.ReplaceAll(field, "_", "-")
	}
	if source := loadedFieldSource(loaded, field); source != "default" {
		return source
	}
	if value, exists := os.LookupEnv(env); exists && strings.TrimSpace(value) != "" {
		return "env:" + env
	}
	return "default"
}

func configShowRedactProfile(cmd *cobra.Command, cfg *appConfig, loaded *appconfig.Loaded) (string, string) {
	rootFlags := cmd.Root().PersistentFlags()
	rootProfile, _ := rootFlags.GetString("redact-profile")
	rootSecrets, _ := rootFlags.GetBool("redact-secrets")
	profile, err := resolveRootRedactProfile(cmd, cfg, rootProfile, rootSecrets)
	if err != nil {
		return cfg.EffectiveRedactProfile(), "invalid"
	}
	switch {
	case rootFlags.Changed("redact-profile"):
		return string(profile), "flag:--redact-profile"
	case rootFlags.Changed("redact-secrets"):
		return string(profile), "flag:--redact-secrets"
	case loaded != nil && (loaded.Fields["redact_profile"] || loaded.Fields["redact_secrets"]):
		source := loadedFieldSource(loaded, "redact_profile")
		if source == "default" {
			source = loadedFieldSource(loaded, "redact_secrets")
		}
		return string(profile), source
	default:
		return string(profile), "default"
	}
}

func boolPointerValue(value *bool) any {
	if value == nil {
		return nil
	}
	return *value
}
