package diagnostics

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"docker-manager/internal/commandflags"
	"docker-manager/internal/registryauth"
	rpt "docker-manager/internal/report"

	"github.com/spf13/cobra"
)

func NewRegistryReportCommand() *cobra.Command {
	opts := RegistryLoginCheckOptions{Timeout: 5 * time.Second, FailOnError: true}
	cmd := &cobra.Command{
		Use:   "registry <registry>",
		Short: "检查 Docker registry 登录配置、凭据和连通性",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	configPath := opts.DockerConfig
	if configPath == "" {
		configPath = defaultDockerConfigPath()
	}
	cfg, configFound, configErr := readDockerConfig(configPath)
	cred := registryCredential{ServerAddress: normalized}
	if configErr != nil {
		cred.Message = configErr.Error()
	} else {
		cred = resolveRegistryCredential(ctx, cfg, normalized)
	}

	report := RegistryLoginCheckReport{
		Registry:     normalized,
		DockerConfig: configPath,
		ConfigFound:  configFound,
		Credential:   buildCredentialReport(cred, configErr),
		RegistryPing: pingRegistryV2(ctx, normalized, opts.PlainHTTP, cred),
		DockerLogin:  dockerRegistryLogin(ctx, normalized, cred),
	}
	report.Recommendations = registryLoginRecommendations(report)
	return report, nil
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
	value = strings.TrimPrefix(value, "https://")
	value = strings.TrimPrefix(value, "http://")
	value = strings.TrimSuffix(value, "/")
	if value == "" {
		return "", fmt.Errorf("registry 不能为空")
	}
	if strings.Contains(value, "/") {
		return "", fmt.Errorf("registry 只接受主机名和可选端口，例如 registry.local:5000")
	}
	return value, nil
}

func buildCredentialReport(cred registryCredential, configErr error) CredentialReport {
	report := CredentialReport{
		Found:    cred.Found,
		Source:   cred.Source,
		Helper:   cred.Helper,
		Username: cred.Username,
		Message:  cred.Message,
	}
	if configErr != nil {
		report.Message = configErr.Error()
	}
	return report
}

func registryLoginRecommendations(report RegistryLoginCheckReport) []string {
	var tips []string
	if !report.ConfigFound {
		tips = append(tips, "未找到 Docker config.json，可先执行 docker login <registry>")
	}
	if !report.Credential.Found {
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
