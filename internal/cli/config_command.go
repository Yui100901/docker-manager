package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"docker-manager/internal/appconfig"
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
	Path      string            `json:"path"`
	Exists    bool              `json:"exists"`
	Effective bool              `json:"effective"`
	Values    map[string]any    `json:"values"`
	Sources   map[string]string `json:"sources,omitempty"`
}

func newConfigCommand(cfg *appConfig, loaded *appconfig.Loaded) *cobra.Command {
	configCmd := &cobra.Command{
		Use:   "config",
		Short: "校验并查看配置",
	}
	configCmd.AddCommand(newConfigValidateCommand(loaded))
	configCmd.AddCommand(newConfigShowCommand(cfg, loaded))
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
			fmt.Fprintf(cmd.OutOrStdout(), "配置有效: %s\n", loaded.Path)
			return nil
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
		Path:      configLoadedPath(loaded),
		Exists:    loaded != nil && loaded.Exists,
		Effective: effective,
		Values:    make(map[string]any),
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
		if loaded != nil && loaded.Fields[field] {
			return "config:" + loaded.Path
		}
		return "default"
	}
	rootFlags := cmd.Root().PersistentFlags()
	configPathSource := "default"
	if rootFlags.Changed("config") {
		configPathSource = "flag:--config"
	} else if strings.TrimSpace(os.Getenv(appconfig.EnvName)) != "" {
		configPathSource = "env:" + appconfig.EnvName
	}
	set("config_path", report.Path, configPathSource)

	if !effective {
		setRawConfigValues(set, *cfg, configSource)
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
	return report
}

func setRawConfigValues(set func(string, any, string), cfg appConfig, source func(string) string) {
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
	redactProfileSource := source("redact_profile")
	if redactProfileSource == "default" {
		redactProfileSource = source("redact_secrets")
	}
	set("redact_profile", cfg.EffectiveRedactProfile(), redactProfileSource)
	set("redact_secrets", cfg.RedactSecrets, source("redact_secrets"))
	set("credential_helpers_disabled", cfg.CredentialHelpersDisabled, source("credential_helpers_disabled"))
	set("credential_helper_timeout", cfg.CredentialHelperTimeout, source("credential_helper_timeout"))
	set("registry_auth_realms", append([]string(nil), cfg.RegistryAuthRealms...), source("registry_auth_realms"))
	set("verbose", cfg.Verbose, source("verbose"))
	set("quiet", cfg.Quiet, source("quiet"))
	set("log_json", cfg.JSON, source("log_json"))
}

func printConfigShowReport(w io.Writer, report configShowReport) {
	fmt.Fprintf(w, "配置文件: %s (exists=%v)\n", report.Path, report.Exists)
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
	if loaded != nil && loaded.Fields[field] {
		return "config:" + loaded.Path
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
		return string(profile), "config:" + loaded.Path
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
