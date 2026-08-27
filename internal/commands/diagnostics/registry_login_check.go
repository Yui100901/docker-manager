package diagnostics

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"docker-manager/internal/appconfig"
	"docker-manager/internal/commandflags"
	"docker-manager/internal/registryauth"
	rpt "docker-manager/internal/report"

	"github.com/spf13/cobra"
)

func NewRegistryReportCommand() *cobra.Command {
	return NewRegistryReportCommandWithDefaults(nil)
}

func NewRegistryReportCommandWithDefaults(defaults func() RegistryLoginCheckDefaults) *cobra.Command {
	opts := RegistryLoginCheckOptions{Timeout: 5 * time.Second, CredentialHelperTimeout: registryauth.DefaultCredentialHelperTimeout, FailOnError: true}
	cmd := &cobra.Command{
		Use:   "registry <registry>",
		Short: "检查 Docker registry 登录配置、凭据和连通性",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if defaults != nil {
				cfg := defaults()
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
			report, err := runRegistryLoginCheck(cmd.Context(), args[0], opts)
			if err != nil {
				return fmt.Errorf("检查 registry 登录失败: %w", err)
			}
			if err := rpt.Print(cmd.OutOrStdout(), opts.Format, report, func(w io.Writer) {
				printRegistryLoginCheckReport(w, report)
			}); err != nil {
				return err
			}
			return registryLoginCheckExitError(report, opts)
		},
	}
	commandflags.AddRegistryClientFlags(cmd, &opts.DockerConfig, &opts.PlainHTTP, &opts.Timeout, opts.Timeout)
	cmd.Flags().StringVar(&opts.Proxy, "proxy", "", "registry /v2/ 检查使用的代理；未指定时读取精确 registry 策略或标准代理环境变量")
	cmd.Flags().BoolVar(&opts.NoProxy, "no-proxy", false, "registry /v2/ 检查强制直连，不使用配置或环境代理")
	cmd.Flags().StringVar(&opts.RegistryCAFile, "registry-ca-file", "", "registry /v2/ HTTPS 检查额外信任的 PEM CA 文件")
	cmd.Flags().StringVar(&opts.RegistryCAPath, "registry-ca-path", "", "registry /v2/ HTTPS 检查额外信任的 PEM CA 目录")
	commandflags.AddCredentialHelperFlags(cmd, &opts.DisableCredentialHelpers, &opts.CredentialHelperTimeout, opts.CredentialHelperTimeout)
	cmd.Flags().BoolVar(&opts.FailOnError, "fail-on-error", opts.FailOnError, "registry 检查出现 failed 状态时返回非零退出码")
	cmd.Flags().BoolVar(&opts.FailOnWarning, "fail-on-warning", false, "registry 检查出现 warning 状态时也返回非零退出码")
	commandflags.AddReportFormatFlag(cmd, &opts.Format)
	return cmd
}

func runRegistryLoginCheck(ctx context.Context, registryName string, opts RegistryLoginCheckOptions) (RegistryLoginCheckReport, error) {
	normalized, err := normalizeRegistryName(registryName)
	if err != nil {
		return RegistryLoginCheckReport{}, err
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}
	opts, credentialAllowed, err := applyRegistryPolicy(normalized, opts)
	if err != nil {
		return RegistryLoginCheckReport{}, err
	}
	if opts.CredentialHelperTimeout <= 0 {
		opts.CredentialHelperTimeout = registryauth.DefaultCredentialHelperTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	httpClient, closeHTTPClient, err := newRegistryCheckHTTPClient(opts)
	if err != nil {
		return RegistryLoginCheckReport{}, fmt.Errorf("配置 registry /v2/ HTTP client 失败: %w", err)
	}
	defer closeHTTPClient()

	configPath := opts.DockerConfig
	if configPath == "" {
		configPath = defaultDockerConfigPath()
	}
	cred := registryCredential{ServerAddress: normalized}
	var (
		configFound bool
		configErr   error
	)
	credentialReport := credentialPolicyBlockedReport()
	if !credentialAllowed {
		configFound = regularFileExists(configPath)
	} else {
		cfg, found, err := readDockerConfig(configPath)
		configFound = found
		configErr = err
		if configErr != nil {
			cred.Message = configErr.Error()
		} else {
			cred = resolveRegistryCredentialWithOptions(ctx, cfg, normalized, opts)
		}
		credentialReport = buildCredentialReport(cred, configErr)
	}

	report := RegistryLoginCheckReport{
		Registry:     normalized,
		DockerConfig: configPath,
		ConfigFound:  configFound,
		Credential:   credentialReport,
		RegistryPing: pingRegistryV2WithClient(ctx, httpClient, normalized, opts.PlainHTTP, cred),
		DockerLogin:  dockerRegistryLogin(ctx, normalized, cred, credentialAllowed),
	}
	report.Recommendations = registryLoginRecommendations(report)
	return report, nil
}

func regularFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func registryLoginCheckExitError(report RegistryLoginCheckReport, opts RegistryLoginCheckOptions) error {
	if opts.FailOnError && registryReportHasStatus(report, "failed") {
		return fmt.Errorf("registry check failed: %s", report.Registry)
	}
	if opts.FailOnWarning && registryReportHasStatus(report, "warning") {
		return fmt.Errorf("registry check has warnings: %s", report.Registry)
	}
	return nil
}

func registryReportHasStatus(report RegistryLoginCheckReport, status string) bool {
	return report.RegistryPing.Status == status || report.DockerLogin.Status == status
}

func normalizeRegistryName(input string) (string, error) {
	value := strings.TrimSpace(input)
	normalized, err := appconfig.NormalizeRegistryEndpoint(value)
	if err != nil {
		return "", fmt.Errorf("registry 只接受规范化的 host[:port]，例如 registry.local:5000: %w", err)
	}
	return normalized, nil
}

func buildCredentialReport(cred registryCredential, configErr error) CredentialReport {
	report := CredentialReport{
		Found:        cred.Found,
		Source:       cred.Source,
		Helper:       cred.Helper,
		HelperSource: cred.HelperSource,
		HelperPath:   cred.HelperPath,
		Username:     cred.Username,
		Message:      cred.Message,
	}
	if configErr != nil {
		report.Message = configErr.Error()
	}
	return report
}

func registryLoginRecommendations(report RegistryLoginCheckReport) []string {
	var tips []string
	if !report.ConfigFound && report.Credential.Source != "registry-policy" {
		tips = append(tips, "未找到 Docker config.json，可先执行 docker login <registry>")
	}
	if !report.Credential.Found && report.Credential.Source != "registry-policy" {
		tips = append(tips, "未找到可用凭据，push 前请执行 docker login "+report.Registry)
	}
	if report.RegistryPing.Status == "failed" {
		tips = append(tips, "检查 registry 地址、网络、TLS 证书；内网 HTTP registry 可尝试 --plain-http")
	}
	if report.DockerLogin.Status == "failed" {
		tips = append(tips, "Docker 登录验证失败，建议重新 docker login "+report.Registry)
	}
	if isArtifactoryRouterCandidate(report.Registry) && report.RegistryPing.Status != "failed" {
		tips = append(tips, "Artifactory/JCR Router 8082: /v2/ 可访问不代表 Docker push blob 链路可用；若 Docker push 报 tls: unrecognized name 或 HTTP 端口走 HTTPS，优先验证 Tomcat 8081、TLS 证书、反向代理和 external URL 配置")
	}
	return uniqueStrings(tips)
}

func isArtifactoryRouterCandidate(registryName string) bool {
	lower := strings.ToLower(strings.TrimSpace(registryName))
	return strings.HasSuffix(lower, ":8082") ||
		(strings.Contains(lower, "router") &&
			(strings.Contains(lower, "artifactory") || strings.Contains(lower, "jfrog")))
}

func uniqueStrings(values []string) []string {
	return registryauth.UniqueStrings(values)
}
