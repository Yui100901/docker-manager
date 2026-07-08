package diagnostics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"docker-manager/internal/commandflags"
	"docker-manager/internal/registryauth"
	rpt "docker-manager/internal/report"

	"github.com/distribution/reference"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const registrySyncDefaultTagPageSize = 1000

var registrySyncHTTPClient httpDoer = http.DefaultClient

type RegistrySyncOptions struct {
	Config       string
	DockerConfig string
	PlainHTTP    bool
	Timeout      time.Duration
	DryRun       bool
	FailOnError  bool
	commandflags.FormatOptions
}

type RegistrySyncConfig struct {
	Mirrors []RegistrySyncMirror `json:"mirrors" yaml:"mirrors"`
}

type RegistrySyncMirror struct {
	Source    string               `json:"source" yaml:"source"`
	Targets   []string             `json:"targets" yaml:"targets"`
	Tags      RegistrySyncTagRules `json:"tags" yaml:"tags"`
	Platforms []string             `json:"platforms,omitempty" yaml:"platforms"`
}

type RegistrySyncTagRules struct {
	Include []string `json:"include,omitempty" yaml:"include"`
	Exclude []string `json:"exclude,omitempty" yaml:"exclude"`
}

type RegistrySyncReport struct {
	GeneratedAt string                     `json:"generated_at"`
	Config      string                     `json:"config"`
	DryRun      bool                       `json:"dry_run"`
	Summary     RegistrySyncSummary        `json:"summary"`
	Mirrors     []RegistrySyncMirrorResult `json:"mirrors"`
	Items       []RegistrySyncItem         `json:"items"`
	Warnings    []string                   `json:"warnings,omitempty"`
}

type RegistrySyncSummary struct {
	Mirrors    int `json:"mirrors"`
	Targets    int `json:"targets"`
	TagsListed int `json:"tags_listed"`
	Planned    int `json:"planned"`
	Skipped    int `json:"skipped"`
	Failed     int `json:"failed"`
}

type RegistrySyncMirrorResult struct {
	Source     string   `json:"source"`
	Registry   string   `json:"registry,omitempty"`
	Repository string   `json:"repository,omitempty"`
	Targets    []string `json:"targets,omitempty"`
	TagsListed int      `json:"tags_listed"`
	Status     string   `json:"status"`
	Message    string   `json:"message,omitempty"`
}

type RegistrySyncItem struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	Tag      string `json:"tag"`
	Platform string `json:"platform,omitempty"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
}

type registrySyncImageRef struct {
	Registry     string
	AuthRegistry string
	Repository   string
	Display      string
}

type registrySyncAuth struct {
	Authorization string
}

func NewRegistrySyncReportCommand() *cobra.Command {
	return newRegistrySyncCommand("registry-sync")
}

func newRegistrySyncCommand(use string) *cobra.Command {
	opts := RegistrySyncOptions{
		Timeout:     30 * time.Second,
		DryRun:      true,
		FailOnError: true,
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: "按配置生成 registry 镜像同步计划",
		RunE: func(cmd *cobra.Command, args []string) error {
			report, err := runRegistrySync(cmd.Context(), opts)
			if err != nil {
				return err
			}
			if printErr := rpt.Print(cmd.OutOrStdout(), opts.Format, report, func(w io.Writer) {
				printRegistrySyncReport(w, report)
			}); printErr != nil {
				return printErr
			}
			return registrySyncExitError(report, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Config, "file", "", "registry 同步策略 YAML 文件路径")
	cmd.Flags().StringVar(&opts.DockerConfig, "docker-config", "", "Docker config.json 路径，默认使用 DOCKER_CONFIG/config.json 或 ~/.docker/config.json")
	cmd.Flags().BoolVar(&opts.PlainHTTP, "plain-http", false, "使用 http:// 访问 registry，适用于未启用 TLS 的内网 registry")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", opts.Timeout, "registry tag 读取总超时时间")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", opts.DryRun, "只生成同步计划，不执行拉取或推送")
	cmd.Flags().BoolVar(&opts.FailOnError, "fail-on-error", opts.FailOnError, "同步计划存在失败项时返回非零退出码")
	commandflags.AddReportFormatFlag(cmd, &opts.Format)
	return cmd
}

func runRegistrySync(ctx context.Context, opts RegistrySyncOptions) (RegistrySyncReport, error) {
	report := RegistrySyncReport{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Config:      opts.Config,
		DryRun:      opts.DryRun,
	}
	if strings.TrimSpace(opts.Config) == "" {
		return report, fmt.Errorf("请通过 --file 指定 registry 同步策略文件")
	}
	if !opts.DryRun {
		report.Warnings = append(report.Warnings, "真实同步执行尚未实现，请使用 --dry-run 生成计划")
		return report, fmt.Errorf("registry sync execute is not implemented")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	cfg, err := readRegistrySyncConfig(opts.Config)
	if err != nil {
		return report, err
	}
	report.Summary.Mirrors = len(cfg.Mirrors)
	for _, mirror := range cfg.Mirrors {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		result, items := buildRegistrySyncMirrorPlan(ctx, mirror, opts)
		report.Mirrors = append(report.Mirrors, result)
		report.Items = append(report.Items, items...)
		report.Summary.Targets += len(mirror.Targets)
		report.Summary.TagsListed += result.TagsListed
		for _, item := range items {
			switch item.Status {
			case "planned":
				report.Summary.Planned++
			case "skipped":
				report.Summary.Skipped++
			case "failed":
				report.Summary.Failed++
			}
		}
		if result.Status == "failed" && len(items) == 0 {
			report.Summary.Failed++
		}
	}
	return report, nil
}

func readRegistrySyncConfig(path string) (RegistrySyncConfig, error) {
	var cfg RegistrySyncConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func buildRegistrySyncMirrorPlan(ctx context.Context, mirror RegistrySyncMirror, opts RegistrySyncOptions) (RegistrySyncMirrorResult, []RegistrySyncItem) {
	result := RegistrySyncMirrorResult{
		Source:  mirror.Source,
		Targets: append([]string(nil), mirror.Targets...),
		Status:  "planned",
	}
	source, err := parseRegistrySyncImageRef(mirror.Source)
	if err != nil {
		result.Status = "failed"
		result.Message = err.Error()
		return result, nil
	}
	result.Registry = source.Registry
	result.Repository = source.Repository
	if len(mirror.Targets) == 0 {
		result.Status = "failed"
		result.Message = "未配置 targets"
		return result, nil
	}

	tags, err := listRegistrySyncTags(ctx, source, opts)
	if err != nil {
		result.Status = "failed"
		result.Message = err.Error()
		return result, nil
	}
	sort.Strings(tags)
	result.TagsListed = len(tags)

	var items []RegistrySyncItem
	for _, tag := range tags {
		if err := ctx.Err(); err != nil {
			result.Status = "failed"
			result.Message = err.Error()
			return result, items
		}
		if !registrySyncTagSelected(tag, mirror.Tags) {
			items = append(items, RegistrySyncItem{
				Source: source.Display + ":" + tag,
				Tag:    tag,
				Status: "skipped",
				Reason: "tag rule",
			})
			continue
		}
		for _, target := range mirror.Targets {
			targetRepo, err := registrySyncRepositoryRef(target)
			if err != nil {
				items = append(items, RegistrySyncItem{
					Source: source.Display + ":" + tag,
					Target: target,
					Tag:    tag,
					Status: "failed",
					Reason: err.Error(),
				})
				continue
			}
			platforms := mirror.Platforms
			if len(platforms) == 0 {
				platforms = []string{""}
			}
			for _, platform := range platforms {
				items = append(items, RegistrySyncItem{
					Source:   source.Display + ":" + tag,
					Target:   targetRepo + ":" + tag,
					Tag:      tag,
					Platform: platform,
					Status:   "planned",
					Reason:   "dry-run",
				})
			}
		}
	}
	if len(items) == 0 {
		result.Status = "warning"
		result.Message = "tag 规则未匹配任何同步项"
	}
	return result, items
}

func parseRegistrySyncImageRef(input string) (registrySyncImageRef, error) {
	value := stripRegistrySyncScheme(strings.TrimSpace(input))
	if value == "" {
		return registrySyncImageRef{}, fmt.Errorf("source 不能为空")
	}
	named, err := reference.ParseNormalizedNamed(value)
	if err != nil {
		return registrySyncImageRef{}, err
	}
	domain := reference.Domain(named)
	repository := reference.Path(named)
	if repository == "" {
		return registrySyncImageRef{}, fmt.Errorf("source 缺少 repository")
	}
	registryHost := domain
	if registryHost == "docker.io" {
		registryHost = "registry-1.docker.io"
	}
	return registrySyncImageRef{
		Registry:     registryHost,
		AuthRegistry: domain,
		Repository:   repository,
		Display:      domain + "/" + repository,
	}, nil
}

func registrySyncRepositoryRef(input string) (string, error) {
	value := stripRegistrySyncScheme(strings.TrimSpace(input))
	if value == "" {
		return "", fmt.Errorf("target 不能为空")
	}
	named, err := reference.ParseNormalizedNamed(value)
	if err != nil {
		return "", err
	}
	return reference.Domain(named) + "/" + reference.Path(named), nil
}

func stripRegistrySyncScheme(input string) string {
	if strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://") {
		parsed, err := url.Parse(input)
		if err == nil && parsed.Host != "" {
			return strings.TrimPrefix(parsed.Host+parsed.EscapedPath(), "/")
		}
	}
	return strings.TrimSuffix(input, "/")
}

func registrySyncTagSelected(tag string, rules RegistrySyncTagRules) bool {
	included := len(rules.Include) == 0 || registrySyncAnyPatternMatch(rules.Include, tag)
	if !included {
		return false
	}
	return !registrySyncAnyPatternMatch(rules.Exclude, tag)
}

func registrySyncAnyPatternMatch(patterns []string, value string) bool {
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if pattern == value {
			return true
		}
		if ok, err := path.Match(pattern, value); err == nil && ok {
			return true
		}
	}
	return false
}

func listRegistrySyncTags(ctx context.Context, source registrySyncImageRef, opts RegistrySyncOptions) ([]string, error) {
	scheme := "https"
	if opts.PlainHTTP {
		scheme = "http"
	}
	nextURL := fmt.Sprintf("%s://%s/v2/%s/tags/list?n=%d", scheme, source.Registry, source.Repository, registrySyncDefaultTagPageSize)
	var tags []string
	var auth *registrySyncAuth
	for nextURL != "" {
		body, header, nextAuth, err := fetchRegistrySyncBytes(ctx, nextURL, source, opts, auth)
		if err != nil {
			return nil, err
		}
		auth = nextAuth
		var resp struct {
			Name string   `json:"name"`
			Tags []string `json:"tags"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, err
		}
		tags = append(tags, resp.Tags...)
		nextURL = registrySyncNextLinkURL(header.Get("Link"), nextURL)
	}
	return registryauth.UniqueStrings(tags), nil
}

func fetchRegistrySyncBytes(ctx context.Context, rawURL string, source registrySyncImageRef, opts RegistrySyncOptions, auth *registrySyncAuth) ([]byte, http.Header, *registrySyncAuth, error) {
	body, header, status, err := registrySyncGET(ctx, rawURL, auth)
	if err == nil {
		return body, header, auth, nil
	}
	if status != http.StatusUnauthorized {
		return nil, header, auth, err
	}
	nextAuth, authErr := resolveRegistrySyncAuth(ctx, header.Get("WWW-Authenticate"), source, opts)
	if authErr != nil {
		return nil, header, auth, authErr
	}
	body, header, _, err = registrySyncGET(ctx, rawURL, nextAuth)
	if err != nil {
		return nil, header, nextAuth, err
	}
	return body, header, nextAuth, nil
}

func registrySyncGET(ctx context.Context, rawURL string, auth *registrySyncAuth) ([]byte, http.Header, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, 0, err
	}
	if auth != nil && auth.Authorization != "" {
		req.Header.Set("Authorization", auth.Authorization)
	}
	resp, err := registrySyncHTTPClient.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, resp.Header.Clone(), resp.StatusCode, readErr
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, resp.Header.Clone(), resp.StatusCode, fmt.Errorf("HTTP %s", resp.Status)
	}
	return body, resp.Header.Clone(), resp.StatusCode, nil
}

func resolveRegistrySyncAuth(ctx context.Context, header string, source registrySyncImageRef, opts RegistrySyncOptions) (*registrySyncAuth, error) {
	challenge := parseRegistrySyncAuthChallenge(header)
	cred, _ := loadRegistrySyncCredential(ctx, source, opts.DockerConfig)
	switch strings.ToLower(challenge.Scheme) {
	case "bearer":
		token, err := fetchRegistrySyncBearerToken(ctx, challenge, source, cred)
		if err != nil {
			return nil, err
		}
		return &registrySyncAuth{Authorization: "Bearer " + token}, nil
	case "basic":
		if cred.Username == "" && cred.Password == "" {
			return nil, fmt.Errorf("registry %s 需要 Basic 认证，但未找到 Docker 凭据", source.AuthRegistry)
		}
		return &registrySyncAuth{Authorization: registryauth.BasicAuthHeader(cred.Username, cred.Password)}, nil
	default:
		if cred.IdentityToken != "" {
			return &registrySyncAuth{Authorization: "Bearer " + cred.IdentityToken}, nil
		}
		if cred.Username != "" || cred.Password != "" {
			return &registrySyncAuth{Authorization: registryauth.BasicAuthHeader(cred.Username, cred.Password)}, nil
		}
		if strings.TrimSpace(header) == "" {
			return nil, fmt.Errorf("registry %s 返回 401 但没有 WWW-Authenticate challenge", source.AuthRegistry)
		}
		return nil, fmt.Errorf("不支持的 registry 认证方式 %q", challenge.Scheme)
	}
}

func loadRegistrySyncCredential(ctx context.Context, source registrySyncImageRef, configPath string) (registryauth.Credential, error) {
	if configPath == "" {
		configPath = registryauth.DefaultConfigPath()
	}
	cfg, _, err := registryauth.ReadConfig(configPath)
	if err != nil {
		return registryauth.Credential{}, err
	}
	return registryauth.ResolveCredential(ctx, cfg, source.AuthRegistry, runDockerCredentialHelper), nil
}

func fetchRegistrySyncBearerToken(ctx context.Context, challenge registrySyncAuthChallenge, source registrySyncImageRef, cred registryauth.Credential) (string, error) {
	realm := challenge.Params["realm"]
	if realm == "" {
		return "", fmt.Errorf("Bearer challenge 缺少 realm")
	}
	parsed, err := url.Parse(realm)
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	if service := challenge.Params["service"]; service != "" {
		query.Set("service", service)
	}
	scope := challenge.Params["scope"]
	if scope == "" {
		scope = "repository:" + source.Repository + ":pull"
	}
	query.Set("scope", scope)
	parsed.RawQuery = query.Encode()
	auth := &registrySyncAuth{}
	if cred.IdentityToken != "" {
		auth.Authorization = "Bearer " + cred.IdentityToken
	} else if cred.Username != "" || cred.Password != "" {
		auth.Authorization = registryauth.BasicAuthHeader(cred.Username, cred.Password)
	}
	body, _, _, err := registrySyncGET(ctx, parsed.String(), auth)
	if err != nil {
		return "", err
	}
	var token struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &token); err != nil {
		return "", err
	}
	if token.Token != "" {
		return token.Token, nil
	}
	if token.AccessToken != "" {
		return token.AccessToken, nil
	}
	return "", fmt.Errorf("认证响应不包含 token")
}

type registrySyncAuthChallenge struct {
	Scheme string
	Params map[string]string
}

func parseRegistrySyncAuthChallenge(header string) registrySyncAuthChallenge {
	header = strings.TrimSpace(header)
	if header == "" {
		return registrySyncAuthChallenge{Params: map[string]string{}}
	}
	scheme, rest, _ := strings.Cut(header, " ")
	return registrySyncAuthChallenge{
		Scheme: strings.TrimSpace(scheme),
		Params: parseRegistrySyncChallengeParams(rest),
	}
}

func parseRegistrySyncChallengeParams(input string) map[string]string {
	params := map[string]string{}
	for len(input) > 0 {
		input = strings.TrimLeft(input, " ,")
		if input == "" {
			break
		}
		key, rest, ok := strings.Cut(input, "=")
		if !ok {
			break
		}
		key = strings.TrimSpace(key)
		rest = strings.TrimLeft(rest, " ")
		var value string
		if strings.HasPrefix(rest, "\"") {
			value, rest = readRegistrySyncQuotedValue(rest[1:])
		} else {
			value, rest, _ = strings.Cut(rest, ",")
		}
		if key != "" {
			params[strings.ToLower(key)] = value
		}
		input = rest
	}
	return params
}

func readRegistrySyncQuotedValue(input string) (string, string) {
	var sb strings.Builder
	escaped := false
	for i, r := range input {
		if escaped {
			sb.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r == '"' {
			return sb.String(), input[i+1:]
		}
		sb.WriteRune(r)
	}
	return sb.String(), ""
}

func registrySyncNextLinkURL(header string, current string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	for _, part := range strings.Split(header, ",") {
		link, params, ok := strings.Cut(strings.TrimSpace(part), ";")
		if !ok || !strings.Contains(params, `rel="next"`) {
			continue
		}
		link = strings.Trim(link, "<> ")
		if link == "" {
			continue
		}
		parsed, err := url.Parse(link)
		if err != nil {
			return ""
		}
		if parsed.IsAbs() {
			return parsed.String()
		}
		base, err := url.Parse(current)
		if err != nil {
			return ""
		}
		return base.ResolveReference(parsed).String()
	}
	return ""
}

func registrySyncExitError(report RegistrySyncReport, opts RegistrySyncOptions) error {
	if opts.FailOnError && report.Summary.Failed > 0 {
		return fmt.Errorf("registry sync plan has failed items: %d", report.Summary.Failed)
	}
	return nil
}

func printRegistrySyncReport(w io.Writer, report RegistrySyncReport) {
	fmt.Fprintf(w, "Registry 同步计划 (%s)\n", report.GeneratedAt)
	fmt.Fprintf(w, "配置: %s dry-run=%v\n", report.Config, report.DryRun)
	fmt.Fprintf(w, "摘要: mirrors=%d targets=%d tags=%d planned=%d skipped=%d failed=%d\n\n",
		report.Summary.Mirrors,
		report.Summary.Targets,
		report.Summary.TagsListed,
		report.Summary.Planned,
		report.Summary.Skipped,
		report.Summary.Failed,
	)
	if len(report.Warnings) > 0 {
		fmt.Fprintln(w, "警告:")
		for _, warning := range report.Warnings {
			fmt.Fprintf(w, "  - %s\n", warning)
		}
		fmt.Fprintln(w)
	}
	for _, mirror := range report.Mirrors {
		fmt.Fprintf(w, "源: %s registry=%s repo=%s tags=%d 状态=%s",
			mirror.Source,
			mirror.Registry,
			mirror.Repository,
			mirror.TagsListed,
			mirror.Status,
		)
		if mirror.Message != "" {
			fmt.Fprintf(w, " 信息=%s", mirror.Message)
		}
		fmt.Fprintln(w)
	}
	if len(report.Items) == 0 {
		fmt.Fprintln(w, "\n未生成同步项")
		return
	}
	fmt.Fprintln(w, "\n同步项:")
	for _, item := range report.Items {
		if item.Status == "skipped" {
			fmt.Fprintf(w, "  - [%s] %s tag=%s 原因=%s\n", item.Status, item.Source, item.Tag, item.Reason)
			continue
		}
		fmt.Fprintf(w, "  - [%s] %s -> %s", item.Status, item.Source, item.Target)
		if item.Platform != "" {
			fmt.Fprintf(w, " platform=%s", item.Platform)
		}
		if item.Reason != "" {
			fmt.Fprintf(w, " 原因=%s", item.Reason)
		}
		fmt.Fprintln(w)
	}
}
