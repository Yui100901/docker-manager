package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"
)

type registryLoginDockerService interface {
	RegistryLogin(ctx context.Context, auth registry.AuthConfig) (registry.AuthenticateOKBody, error)
}

var newRegistryLoginDockerService = func() (registryLoginDockerService, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &dockerRegistryLoginService{cli: cli}, nil
}

var registryCheckHTTPClient httpDoer = http.DefaultClient
var runDockerCredentialHelper = defaultRunDockerCredentialHelper

type dockerRegistryLoginService struct {
	cli *client.Client
}

type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

type RegistryLoginCheckOptions struct {
	DockerConfig string
	PlainHTTP    bool
	Timeout      time.Duration
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

type dockerConfigFile struct {
	Auths       map[string]dockerAuthEntry `json:"auths"`
	CredsStore  string                     `json:"credsStore"`
	CredHelpers map[string]string          `json:"credHelpers"`
}

type dockerAuthEntry struct {
	Auth          string `json:"auth"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
}

type registryCredential struct {
	Found         bool
	Source        string
	Helper        string
	Username      string
	Password      string
	IdentityToken string
	ServerAddress string
	Message       string
}

type dockerCredentialHelperResponse struct {
	ServerURL string `json:"ServerURL"`
	Username  string `json:"Username"`
	Secret    string `json:"Secret"`
}

func newRegistryLoginCheckCommand() *cobra.Command {
	opts := RegistryLoginCheckOptions{Timeout: 5 * time.Second}
	cmd := &cobra.Command{
		Use:   "registry-login-check <registry>",
		Short: "检查 Docker registry 登录配置、凭据和连通性",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			report, err := runRegistryLoginCheck(cmd.Context(), args[0], opts)
			if err != nil {
				return fmt.Errorf("registry login check failed: %w", err)
			}
			printRegistryLoginCheckReport(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.DockerConfig, "docker-config", "", "Docker config.json 路径，默认使用 DOCKER_CONFIG/config.json 或 ~/.docker/config.json")
	cmd.Flags().BoolVar(&opts.PlainHTTP, "plain-http", false, "使用 http:// 访问 registry /v2/，用于未启用 TLS 的内网 registry")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", opts.Timeout, "registry 连通性检查超时时间")
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
	if dir := strings.TrimSpace(os.Getenv("DOCKER_CONFIG")); dir != "" {
		return filepath.Join(dir, "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".docker", "config.json")
	}
	return filepath.Join(home, ".docker", "config.json")
}

func readDockerConfig(path string) (dockerConfigFile, bool, error) {
	var cfg dockerConfigFile
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, false, nil
		}
		return cfg, false, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, true, err
	}
	return cfg, true, nil
}

func resolveRegistryCredential(ctx context.Context, cfg dockerConfigFile, registryName string) registryCredential {
	keys := registryConfigKeys(registryName)
	if helper, server := findCredentialHelper(cfg, keys); helper != "" {
		cred, err := runDockerCredentialHelper(ctx, helper, server)
		if err != nil {
			return registryCredential{
				Source:        "credential-helper",
				Helper:        helper,
				ServerAddress: server,
				Message:       err.Error(),
			}
		}
		cred.Found = true
		cred.Source = "credential-helper"
		cred.Helper = helper
		if cred.ServerAddress == "" {
			cred.ServerAddress = server
		}
		return cred
	}
	for _, key := range keys {
		entry, ok := cfg.Auths[key]
		if !ok {
			continue
		}
		cred := credentialFromAuthEntry(entry)
		cred.Found = cred.Username != "" || cred.Password != "" || cred.IdentityToken != ""
		cred.Source = "auths"
		cred.ServerAddress = key
		if !cred.Found {
			cred.Message = "auths entry exists but contains no usable credential"
		}
		return cred
	}
	return registryCredential{Message: "no matching auths, credHelpers or credsStore entry"}
}

func findCredentialHelper(cfg dockerConfigFile, keys []string) (string, string) {
	for _, key := range keys {
		if helper := strings.TrimSpace(cfg.CredHelpers[key]); helper != "" {
			return helper, key
		}
	}
	if helper := strings.TrimSpace(cfg.CredsStore); helper != "" {
		return helper, keys[0]
	}
	return "", ""
}

func registryConfigKeys(registryName string) []string {
	keys := []string{
		registryName,
		"https://" + registryName,
		"http://" + registryName,
		"https://" + registryName + "/v1/",
	}
	if registryName == "docker.io" || registryName == "registry-1.docker.io" || registryName == "index.docker.io" {
		keys = append(keys, "https://index.docker.io/v1/", "index.docker.io", "docker.io", "registry-1.docker.io")
	}
	return uniqueStrings(keys)
}

func credentialFromAuthEntry(entry dockerAuthEntry) registryCredential {
	cred := registryCredential{
		Username:      entry.Username,
		Password:      entry.Password,
		IdentityToken: entry.IdentityToken,
	}
	if cred.Username == "" && cred.Password == "" && entry.Auth != "" {
		decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
		if err == nil {
			username, password, ok := strings.Cut(string(decoded), ":")
			if ok {
				cred.Username = username
				cred.Password = password
			}
		}
	}
	return cred
}

func defaultRunDockerCredentialHelper(ctx context.Context, helper, server string) (registryCredential, error) {
	cmd := exec.CommandContext(ctx, "docker-credential-"+helper, "get")
	cmd.Stdin = strings.NewReader(server)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return registryCredential{}, fmt.Errorf("docker-credential-%s get failed: %s", helper, msg)
	}
	var resp dockerCredentialHelperResponse
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return registryCredential{}, err
	}
	return registryCredential{
		Username:      resp.Username,
		Password:      resp.Secret,
		ServerAddress: resp.ServerURL,
	}, nil
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
		return CheckResult{Status: "ok", HTTPStatus: resp.StatusCode, Message: "registry /v2/ reachable"}
	case http.StatusUnauthorized:
		if cred.Found {
			return CheckResult{Status: "failed", HTTPStatus: resp.StatusCode, Message: "registry requires auth and configured credential was not accepted by /v2/"}
		}
		return CheckResult{Status: "warning", HTTPStatus: resp.StatusCode, Message: "registry reachable but requires authentication"}
	case http.StatusForbidden:
		return CheckResult{Status: "failed", HTTPStatus: resp.StatusCode, Message: "registry denied access"}
	default:
		if resp.StatusCode >= 200 && resp.StatusCode < 400 {
			return CheckResult{Status: "ok", HTTPStatus: resp.StatusCode, Message: resp.Status}
		}
		return CheckResult{Status: "failed", HTTPStatus: resp.StatusCode, Message: resp.Status}
	}
}

func dockerRegistryLogin(ctx context.Context, registryName string, cred registryCredential) CheckResult {
	if !cred.Found {
		return CheckResult{Status: "skipped", Message: "no credential available for Docker RegistryLogin"}
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
	return CheckResult{Status: "ok", Message: "Docker registry login accepted"}
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
	return uniqueStrings(tips)
}

func printRegistryLoginCheckReport(w io.Writer, report RegistryLoginCheckReport) {
	fmt.Fprintln(w, "Docker registry login check")
	fmt.Fprintf(w, "Registry: %s\n", report.Registry)
	fmt.Fprintf(w, "Docker config: %s found=%v\n", report.DockerConfig, report.ConfigFound)
	fmt.Fprintf(w, "Credential: found=%v source=%s", report.Credential.Found, valueOr(report.Credential.Source, "none"))
	if report.Credential.Helper != "" {
		fmt.Fprintf(w, " helper=%s", report.Credential.Helper)
	}
	if report.Credential.Username != "" {
		fmt.Fprintf(w, " username=%s", report.Credential.Username)
	}
	if report.Credential.Message != "" {
		fmt.Fprintf(w, " message=%s", report.Credential.Message)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Registry ping: %s", report.RegistryPing.Status)
	if report.RegistryPing.HTTPStatus != 0 {
		fmt.Fprintf(w, " http=%d", report.RegistryPing.HTTPStatus)
	}
	if report.RegistryPing.Message != "" {
		fmt.Fprintf(w, " message=%s", report.RegistryPing.Message)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Docker login: %s", report.DockerLogin.Status)
	if report.DockerLogin.Message != "" {
		fmt.Fprintf(w, " message=%s", report.DockerLogin.Message)
	}
	fmt.Fprintln(w)
	if len(report.Recommendations) > 0 {
		fmt.Fprintln(w, "\nRecommendations:")
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

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func (s *dockerRegistryLoginService) RegistryLogin(ctx context.Context, auth registry.AuthConfig) (registry.AuthenticateOKBody, error) {
	return s.cli.RegistryLogin(ctx, auth)
}
