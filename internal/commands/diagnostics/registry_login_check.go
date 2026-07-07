package diagnostics

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"docker-manager/internal/commandflags"
	"docker-manager/internal/docker"
	"docker-manager/internal/registryauth"
	rpt "docker-manager/internal/report"

	"github.com/docker/docker/api/types/registry"
	mobyclient "github.com/moby/moby/client"
	"github.com/spf13/cobra"
)

type registryLoginDockerService interface {
	RegistryLogin(ctx context.Context, auth registry.AuthConfig) (registry.AuthenticateOKBody, error)
}

var newRegistryLoginDockerService = func() (registryLoginDockerService, error) {
	cli, err := docker.NewMobyClient()
	if err != nil {
		return nil, err
	}
	return &dockerRegistryLoginService{cli: cli}, nil
}

var registryCheckHTTPClient httpDoer = http.DefaultClient
var runDockerCredentialHelper registryauth.HelperRunner = defaultRunDockerCredentialHelper

type dockerRegistryLoginService struct {
	cli *mobyclient.Client
}

type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

type RegistryLoginCheckOptions struct {
	DockerConfig  string
	PlainHTTP     bool
	Timeout       time.Duration
	FailOnError   bool
	FailOnWarning bool
	commandflags.FormatOptions
}

type RegistryLoginCheckReport struct {
	Registry        string           `json:"registry"`
	DockerConfig    string           `json:"docker_config"`
	ConfigFound     bool             `json:"config_found"`
	Credential      CredentialReport `json:"credential"`
	RegistryPing    CheckResult      `json:"registry_ping"`
	DockerLogin     CheckResult      `json:"docker_login"`
	Recommendations []string         `json:"recommendations,omitempty"`
}

type CredentialReport struct {
	Found    bool   `json:"found"`
	Source   string `json:"source,omitempty"`
	Helper   string `json:"helper,omitempty"`
	Username string `json:"username,omitempty"`
	Message  string `json:"message,omitempty"`
}

type CheckResult struct {
	Status     string `json:"status"`
	Message    string `json:"message,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type dockerConfigFile = registryauth.Config
type dockerAuthEntry = registryauth.AuthEntry
type registryCredential = registryauth.Credential

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
	cmd.Flags().StringVar(&opts.DockerConfig, "docker-config", "", "Docker config.json 路径，默认使用 DOCKER_CONFIG/config.json 或 ~/.docker/config.json")
	cmd.Flags().BoolVar(&opts.PlainHTTP, "plain-http", false, "使用 http:// 访问 registry /v2/，用于未启用 TLS 的内网 registry")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", opts.Timeout, "registry 连通性检查超时时间")
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

func defaultDockerConfigPath() string {
	return registryauth.DefaultConfigPath()
}

func readDockerConfig(path string) (dockerConfigFile, bool, error) {
	return registryauth.ReadConfig(path)
}

func resolveRegistryCredential(ctx context.Context, cfg dockerConfigFile, registryName string) registryCredential {
	return registryauth.ResolveCredential(ctx, cfg, registryName, runDockerCredentialHelper)
}

func findCredentialHelper(cfg dockerConfigFile, keys []string) (string, string) {
	return registryauth.FindCredentialHelper(cfg, keys)
}

func registryConfigKeys(registryName string) []string {
	return registryauth.ConfigKeys(registryName)
}

func credentialFromAuthEntry(entry dockerAuthEntry) registryCredential {
	return registryauth.CredentialFromAuthEntry(entry)
}

func defaultRunDockerCredentialHelper(ctx context.Context, helper, server string) (registryCredential, error) {
	return registryauth.DefaultRunCredentialHelper(ctx, helper, server)
}

func pingRegistryV2(ctx context.Context, registryName string, plainHTTP bool, cred registryCredential) CheckResult {
	scheme := "https"
	if plainHTTP {
		scheme = "http"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s://%s/v2/", scheme, registryName), nil)
	if err != nil {
		return CheckResult{Status: "failed", Message: err.Error()}
	}
	if cred.Username != "" && cred.Password != "" {
		req.SetBasicAuth(cred.Username, cred.Password)
	}
	resp, err := registryCheckHTTPClient.Do(req)
	if err != nil {
		return CheckResult{Status: "failed", Message: err.Error()}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	switch resp.StatusCode {
	case http.StatusOK:
		return CheckResult{Status: "ok", HTTPStatus: resp.StatusCode, Message: "registry /v2/ 可访问"}
	case http.StatusUnauthorized:
		if cred.Found {
			return CheckResult{Status: "failed", HTTPStatus: resp.StatusCode, Message: "registry 需要认证，但已配置凭据未被 /v2/ 接受"}
		}
		return CheckResult{Status: "warning", HTTPStatus: resp.StatusCode, Message: "registry 可访问但需要认证"}
	case http.StatusForbidden:
		return CheckResult{Status: "failed", HTTPStatus: resp.StatusCode, Message: "registry 拒绝访问"}
	default:
		if resp.StatusCode >= 200 && resp.StatusCode < 400 {
			return CheckResult{Status: "ok", HTTPStatus: resp.StatusCode, Message: resp.Status}
		}
		return CheckResult{Status: "failed", HTTPStatus: resp.StatusCode, Message: resp.Status}
	}
}

func dockerRegistryLogin(ctx context.Context, registryName string, cred registryCredential) CheckResult {
	if !cred.Found {
		return CheckResult{Status: "skipped", Message: "没有可用于 Docker RegistryLogin 的凭据"}
	}
	svc, err := newRegistryLoginDockerService()
	if err != nil {
		return CheckResult{Status: "failed", Message: err.Error()}
	}
	auth := registry.AuthConfig{
		Username:      cred.Username,
		Password:      cred.Password,
		IdentityToken: cred.IdentityToken,
		ServerAddress: registryName,
	}
	resp, err := svc.RegistryLogin(ctx, auth)
	if err != nil {
		return CheckResult{Status: "failed", Message: err.Error()}
	}
	if resp.Status != "" {
		return CheckResult{Status: "ok", Message: resp.Status}
	}
	return CheckResult{Status: "ok", Message: "Docker registry 登录已接受"}
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
		tips = append(tips, "Artifactory/JCR Router 8082: /v2/ 可访问不代表 Docker push blob 链路可用；若 Docker push 报 tls: unrecognized name 或 HTTP 端口跳 HTTPS，优先验证 Tomcat 8081、TLS 证书、反向代理和 external URL 配置")
	}
	return uniqueStrings(tips)
}

func isArtifactoryRouterCandidate(registryName string) bool {
	lower := strings.ToLower(strings.TrimSpace(registryName))
	return strings.HasSuffix(lower, ":8082") ||
		(strings.Contains(lower, "router") &&
			(strings.Contains(lower, "artifactory") || strings.Contains(lower, "jfrog")))
}

func printRegistryLoginCheckReport(w io.Writer, report RegistryLoginCheckReport) {
	fmt.Fprintln(w, "Docker registry 登录检查")
	fmt.Fprintf(w, "Registry: %s\n", report.Registry)
	fmt.Fprintf(w, "Docker config: %s 已找到=%v\n", report.DockerConfig, report.ConfigFound)
	fmt.Fprintf(w, "凭据: 已找到=%v 来源=%s", report.Credential.Found, valueOr(report.Credential.Source, "无"))
	if report.Credential.Helper != "" {
		fmt.Fprintf(w, " helper=%s", report.Credential.Helper)
	}
	if report.Credential.Username != "" {
		fmt.Fprintf(w, " 用户=%s", report.Credential.Username)
	}
	if report.Credential.Message != "" {
		fmt.Fprintf(w, " 信息=%s", report.Credential.Message)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Registry 连通性: %s", checkStatusText(report.RegistryPing.Status))
	if report.RegistryPing.HTTPStatus != 0 {
		fmt.Fprintf(w, " http=%d", report.RegistryPing.HTTPStatus)
	}
	if report.RegistryPing.Message != "" {
		fmt.Fprintf(w, " 信息=%s", report.RegistryPing.Message)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Docker 登录: %s", checkStatusText(report.DockerLogin.Status))
	if report.DockerLogin.Message != "" {
		fmt.Fprintf(w, " 信息=%s", report.DockerLogin.Message)
	}
	fmt.Fprintln(w)
	if len(report.Recommendations) > 0 {
		fmt.Fprintln(w, "\n建议:")
		for _, tip := range report.Recommendations {
			fmt.Fprintf(w, "  - %s\n", tip)
		}
	}
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func checkStatusText(status string) string {
	switch status {
	case "ok":
		return "通过"
	case "warning":
		return "警告"
	case "failed":
		return "失败"
	case "skipped":
		return "跳过"
	default:
		return status
	}
}

func uniqueStrings(values []string) []string {
	return registryauth.UniqueStrings(values)
}

func (s *dockerRegistryLoginService) RegistryLogin(ctx context.Context, auth registry.AuthConfig) (registry.AuthenticateOKBody, error) {
	result, err := s.cli.RegistryLogin(ctx, mobyclient.RegistryLoginOptions{
		Username:      auth.Username,
		Password:      auth.Password,
		ServerAddress: auth.ServerAddress,
		IdentityToken: auth.IdentityToken,
		RegistryToken: auth.RegistryToken,
	})
	if err != nil {
		return registry.AuthenticateOKBody{}, err
	}
	return docker.ConvertDockerType[registry.AuthenticateOKBody](result.Auth)
}
