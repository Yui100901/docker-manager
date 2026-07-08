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
	"sync"
	"time"

	"docker-manager/internal/commandflags"
	"docker-manager/internal/commands/pull"
	"docker-manager/internal/parallel"
	"docker-manager/internal/registryauth"
	rpt "docker-manager/internal/report"

	"github.com/distribution/reference"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const registrySyncDefaultTagPageSize = 1000

const (
	registrySyncStatusPlanned = "planned"
	registrySyncStatusSuccess = "success"
	registrySyncStatusSkipped = "skipped"
	registrySyncStatusFailed  = "failed"
)

var registrySyncHTTPClient httpDoer = http.DefaultClient

type registrySyncPullRunner interface {
	PullImage(imageName string, opts pull.PullOptions) error
	TargetManifestExists(ctx context.Context, imageName, target string, opts pull.PullOptions) (bool, error)
}

var newRegistrySyncPullRunner = func(proxy, targetOS, arch string, timeout time.Duration) (registrySyncPullRunner, error) {
	return pull.NewPullRunnerWithTimeout(proxy, targetOS, arch, timeout)
}

type RegistrySyncOptions struct {
	Config         string
	DockerConfig   string
	PlainHTTP      bool
	Timeout        time.Duration
	DryRun         bool
	Apply          bool
	FailOnError    bool
	Proxy          string
	TargetOS       string
	Arch           string
	OutputDir      string
	Concurrency    int
	Retries        int
	SkipExisting   bool
	ProgressOutput io.Writer
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
	Include    []string `json:"include,omitempty" yaml:"include"`
	Exclude    []string `json:"exclude,omitempty" yaml:"exclude"`
	Sort       string   `json:"sort,omitempty" yaml:"sort"`
	Limit      int      `json:"limit,omitempty" yaml:"limit"`
	KeepLatest int      `json:"keep_latest,omitempty" yaml:"keep_latest"`
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
	Succeeded  int `json:"succeeded"`
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
	Source         string                `json:"source"`
	Target         string                `json:"target"`
	TargetInput    string                `json:"target_input,omitempty"`
	Tag            string                `json:"tag"`
	Platform       string                `json:"platform,omitempty"`
	Status         string                `json:"status"`
	Reason         string                `json:"reason,omitempty"`
	Attempts       int                   `json:"attempts,omitempty"`
	AttemptDetails []RegistrySyncAttempt `json:"attempt_details,omitempty"`
}

type RegistrySyncAttempt struct {
	Attempt int    `json:"attempt"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
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
	return newRegistrySyncCommand("registry-sync", false)
}

func newRegistrySyncCommand(use string, allowApply bool) *cobra.Command {
	opts := RegistrySyncOptions{
		Timeout:     30 * time.Second,
		DryRun:      true,
		FailOnError: true,
		TargetOS:    "linux",
		Arch:        "amd64",
		OutputDir:   ".",
		Concurrency: 1,
		Retries:     0,
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: "按配置生成 registry 镜像同步计划",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !allowApply {
				opts.DryRun = true
			} else if opts.Apply {
				opts.DryRun = false
			}
			opts.ProgressOutput = cmd.OutOrStdout()
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
	cmd.Flags().BoolVar(&opts.FailOnError, "fail-on-error", opts.FailOnError, "同步计划存在失败项时返回非零退出码")
	if allowApply {
		cmd.Flags().BoolVar(&opts.DryRun, "dry-run", opts.DryRun, "只生成同步计划，不执行拉取或推送")
		cmd.Flags().BoolVar(&opts.Apply, "apply", false, "执行同步；未指定时仅生成 dry-run 计划")
		cmd.Flags().StringVar(&opts.Proxy, "proxy", "", "强制指定 HTTP 代理；为空时使用环境变量代理")
		cmd.Flags().StringVar(&opts.TargetOS, "os", opts.TargetOS, "执行同步时选择的目标操作系统")
		cmd.Flags().StringVarP(&opts.Arch, "arch", "a", opts.Arch, "执行同步时选择的目标架构")
		cmd.Flags().StringVar(&opts.OutputDir, "output-dir", opts.OutputDir, "同步执行时保存中间镜像 tar 的目录")
		cmd.Flags().IntVar(&opts.Concurrency, "concurrency", opts.Concurrency, "同步执行并发数")
		cmd.Flags().IntVar(&opts.Retries, "retries", opts.Retries, "同步执行失败后的重试次数")
		cmd.Flags().BoolVar(&opts.SkipExisting, "skip-existing", false, "目标 registry 已存在同名 manifest 时跳过")
	}
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
	}
	recalculateRegistrySyncSummary(&report)
	if !opts.DryRun {
		if err := executeRegistrySyncPlan(ctx, &report, opts); err != nil {
			return report, err
		}
		recalculateRegistrySyncSummary(&report)
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
	result.TagsListed = len(tags)

	var items []RegistrySyncItem
	for _, decision := range selectRegistrySyncTags(tags, mirror.Tags) {
		if err := ctx.Err(); err != nil {
			result.Status = "failed"
			result.Message = err.Error()
			return result, items
		}
		tag := decision.Tag
		if !decision.Selected {
			items = append(items, RegistrySyncItem{
				Source: source.Display + ":" + tag,
				Tag:    tag,
				Status: "skipped",
				Reason: decision.Reason,
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
			targetInput := registrySyncTargetInputRef(target, targetRepo, tag)
			platforms := mirror.Platforms
			if len(platforms) == 0 {
				platforms = []string{""}
			}
			if !opts.DryRun && len(platforms) > 1 {
				items = append(items, RegistrySyncItem{
					Source:      source.Display + ":" + tag,
					Target:      targetRepo + ":" + tag,
					TargetInput: targetInput,
					Tag:         tag,
					Status:      registrySyncStatusFailed,
					Reason:      "执行阶段暂不支持将多个 platform 合并推送为 manifest list，请先保留单个平台",
				})
				continue
			}
			for _, platform := range platforms {
				items = append(items, RegistrySyncItem{
					Source:      source.Display + ":" + tag,
					Target:      targetRepo + ":" + tag,
					TargetInput: targetInput,
					Tag:         tag,
					Platform:    platform,
					Status:      registrySyncStatusPlanned,
					Reason:      registrySyncPlannedReason(opts),
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

func registrySyncTargetInputRef(input, normalizedRepo, tag string) string {
	value := strings.TrimRight(strings.TrimSpace(input), "/")
	if strings.Contains(value, "://") {
		return value + ":" + tag
	}
	return normalizedRepo + ":" + tag
}

func registrySyncPlannedReason(opts RegistrySyncOptions) string {
	if opts.DryRun {
		return "dry-run"
	}
	return "pending"
}

func executeRegistrySyncPlan(ctx context.Context, report *RegistrySyncReport, opts RegistrySyncOptions) error {
	if opts.Concurrency <= 0 {
		return fmt.Errorf("--concurrency 必须大于 0")
	}
	if opts.Retries < 0 {
		return fmt.Errorf("--retries 不能小于 0")
	}
	if opts.OutputDir == "" {
		opts.OutputDir = "."
	}
	var planned []int
	for i, item := range report.Items {
		if item.Status == registrySyncStatusPlanned {
			planned = append(planned, i)
		}
	}
	if len(planned) == 0 {
		return ctx.Err()
	}
	progressOutput := opts.ProgressOutput
	if progressOutput == nil {
		progressOutput = io.Discard
	}

	var mu sync.Mutex
	parallel.ForEachIndex(ctx, len(planned), opts.Concurrency, func(ctx context.Context, index int) {
		itemIndex := planned[index]
		result := executeRegistrySyncItem(ctx, report.Items[itemIndex], opts, progressOutput)
		mu.Lock()
		report.Items[itemIndex] = result
		mu.Unlock()
	})
	return ctx.Err()
}

func executeRegistrySyncItem(ctx context.Context, item RegistrySyncItem, opts RegistrySyncOptions, progressOutput io.Writer) RegistrySyncItem {
	result := item
	if err := ctx.Err(); err != nil {
		result.Status = registrySyncStatusFailed
		result.Reason = err.Error()
		return result
	}
	targetOS, arch, err := registrySyncExecutionPlatform(item.Platform, opts)
	if err != nil {
		result.Status = registrySyncStatusFailed
		result.Reason = err.Error()
		return result
	}
	runner, err := newRegistrySyncPullRunner(opts.Proxy, targetOS, arch, opts.Timeout)
	if err != nil {
		result.Status = registrySyncStatusFailed
		result.Reason = err.Error()
		return result
	}
	to := registrySyncItemTargetInput(item)
	pullOpts := pull.PullOptions{
		Context:        ctx,
		OutputDir:      opts.OutputDir,
		To:             to,
		DockerConfig:   opts.DockerConfig,
		PlainHTTP:      opts.PlainHTTP,
		ProgressOutput: progressOutput,
	}
	if opts.SkipExisting {
		found, err := runner.TargetManifestExists(ctx, item.Source, item.Target, pullOpts)
		if err != nil {
			result.Status = registrySyncStatusFailed
			result.Reason = "检查目标 manifest 失败: " + err.Error()
			return result
		}
		if found {
			result.Status = registrySyncStatusSkipped
			result.Reason = "target exists"
			return result
		}
	}

	maxAttempts := opts.Retries + 1
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		result.Attempts = attempt
		if err := ctx.Err(); err != nil {
			result.Status = registrySyncStatusFailed
			result.Reason = err.Error()
			result.AttemptDetails = append(result.AttemptDetails, RegistrySyncAttempt{
				Attempt: attempt,
				Status:  registrySyncStatusFailed,
				Message: err.Error(),
			})
			return result
		}
		if err := runner.PullImage(item.Source, pullOpts); err != nil {
			lastErr = err
			result.AttemptDetails = append(result.AttemptDetails, RegistrySyncAttempt{
				Attempt: attempt,
				Status:  registrySyncStatusFailed,
				Message: err.Error(),
			})
			continue
		}
		result.Status = registrySyncStatusSuccess
		result.Reason = "synced"
		result.AttemptDetails = append(result.AttemptDetails, RegistrySyncAttempt{
			Attempt: attempt,
			Status:  registrySyncStatusSuccess,
		})
		return result
	}
	result.Status = registrySyncStatusFailed
	if lastErr != nil {
		result.Reason = lastErr.Error()
	}
	return result
}

func registrySyncExecutionPlatform(platform string, opts RegistrySyncOptions) (string, string, error) {
	if strings.TrimSpace(platform) == "" {
		targetOS := strings.TrimSpace(opts.TargetOS)
		arch := strings.TrimSpace(opts.Arch)
		if targetOS == "" {
			targetOS = "linux"
		}
		if arch == "" {
			arch = "amd64"
		}
		return targetOS, arch, nil
	}
	parts := strings.Split(platform, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", fmt.Errorf("platform %q 格式无效，请使用 os/arch，例如 linux/amd64", platform)
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), nil
}

func registrySyncItemTargetInput(item RegistrySyncItem) string {
	if strings.TrimSpace(item.TargetInput) != "" {
		return item.TargetInput
	}
	return item.Target
}

func recalculateRegistrySyncSummary(report *RegistrySyncReport) {
	summary := RegistrySyncSummary{Mirrors: len(report.Mirrors)}
	for _, mirror := range report.Mirrors {
		summary.Targets += len(mirror.Targets)
		summary.TagsListed += mirror.TagsListed
		if mirror.Status == registrySyncStatusFailed && mirror.TagsListed == 0 {
			summary.Failed++
		}
	}
	for _, item := range report.Items {
		switch item.Status {
		case registrySyncStatusPlanned:
			summary.Planned++
		case registrySyncStatusSuccess:
			summary.Succeeded++
		case registrySyncStatusSkipped:
			summary.Skipped++
		case registrySyncStatusFailed:
			summary.Failed++
		}
	}
	report.Summary = summary
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
	if _, ok := named.(reference.Tagged); ok {
		return "", fmt.Errorf("target 应为 repository，不应包含 tag: %s", input)
	}
	if _, ok := named.(reference.Digested); ok {
		return "", fmt.Errorf("target 应为 repository，不应包含 digest: %s", input)
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

type registrySyncTagDecision struct {
	Tag      string
	Selected bool
	Reason   string
}

func selectRegistrySyncTags(tags []string, rules RegistrySyncTagRules) []registrySyncTagDecision {
	tags = registryauth.UniqueStrings(tags)
	sortRegistrySyncTags(tags, registrySyncTagSortMode(rules))

	var selected []registrySyncTagDecision
	var skipped []registrySyncTagDecision
	for _, tag := range tags {
		switch {
		case len(rules.Include) > 0 && !registrySyncAnyPatternMatch(rules.Include, tag):
			skipped = append(skipped, registrySyncTagDecision{Tag: tag, Reason: "include rule"})
		case registrySyncAnyPatternMatch(rules.Exclude, tag):
			skipped = append(skipped, registrySyncTagDecision{Tag: tag, Reason: "exclude rule"})
		default:
			selected = append(selected, registrySyncTagDecision{Tag: tag, Selected: true, Reason: "tag rule"})
		}
	}

	limit, reason := registrySyncTagLimit(rules)
	if limit > 0 && len(selected) > limit {
		limited := append([]registrySyncTagDecision(nil), selected[:limit]...)
		for _, decision := range selected[limit:] {
			decision.Selected = false
			decision.Reason = reason
			skipped = append(skipped, decision)
		}
		selected = limited
	}
	return append(selected, skipped...)
}

func registrySyncTagLimit(rules RegistrySyncTagRules) (int, string) {
	if rules.KeepLatest > 0 {
		return rules.KeepLatest, "keep_latest"
	}
	if rules.Limit > 0 {
		return rules.Limit, "limit"
	}
	return 0, ""
}

func registrySyncTagSortMode(rules RegistrySyncTagRules) string {
	mode := strings.ToLower(strings.TrimSpace(rules.Sort))
	if mode == "" && rules.KeepLatest > 0 {
		return "semver-desc"
	}
	if mode == "" {
		return "name-asc"
	}
	switch mode {
	case "name", "name-asc", "name-desc", "semver", "semver-asc", "semver-desc":
		if mode == "name" {
			return "name-asc"
		}
		if mode == "semver" {
			return "semver-desc"
		}
		return mode
	default:
		return "name-asc"
	}
}

func sortRegistrySyncTags(tags []string, mode string) {
	sort.SliceStable(tags, func(i, j int) bool {
		a, b := tags[i], tags[j]
		switch mode {
		case "name-desc":
			return a > b
		case "semver-asc":
			if cmp := compareRegistrySyncSemverTag(a, b); cmp != 0 {
				return cmp < 0
			}
			return a < b
		case "semver-desc":
			if cmp := compareRegistrySyncSemverTag(a, b); cmp != 0 {
				return cmp > 0
			}
			return a > b
		default:
			return a < b
		}
	})
}

func compareRegistrySyncSemverTag(a, b string) int {
	va, oka := parseRegistrySyncSemver(a)
	vb, okb := parseRegistrySyncSemver(b)
	switch {
	case oka && !okb:
		return 1
	case !oka && okb:
		return -1
	case !oka && !okb:
		return strings.Compare(a, b)
	}
	maxLen := len(va)
	if len(vb) > maxLen {
		maxLen = len(vb)
	}
	for i := 0; i < maxLen; i++ {
		var ai, bi int
		if i < len(va) {
			ai = va[i]
		}
		if i < len(vb) {
			bi = vb[i]
		}
		if ai < bi {
			return -1
		}
		if ai > bi {
			return 1
		}
	}
	return 0
}

func parseRegistrySyncSemver(tag string) ([]int, bool) {
	value := strings.TrimPrefix(strings.TrimSpace(tag), "v")
	if value == "" {
		return nil, false
	}
	if before, _, ok := strings.Cut(value, "-"); ok {
		value = before
	}
	parts := strings.Split(value, ".")
	if len(parts) == 0 {
		return nil, false
	}
	version := make([]int, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			return nil, false
		}
		n := 0
		for _, r := range part {
			if r < '0' || r > '9' {
				return nil, false
			}
			n = n*10 + int(r-'0')
		}
		version = append(version, n)
	}
	return version, true
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
	fmt.Fprintf(w, "摘要: mirrors=%d targets=%d tags=%d planned=%d success=%d skipped=%d failed=%d\n\n",
		report.Summary.Mirrors,
		report.Summary.Targets,
		report.Summary.TagsListed,
		report.Summary.Planned,
		report.Summary.Succeeded,
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
		if item.Attempts > 0 {
			fmt.Fprintf(w, " attempts=%d", item.Attempts)
		}
		if item.Reason != "" {
			fmt.Fprintf(w, " 原因=%s", item.Reason)
		}
		fmt.Fprintln(w)
	}
}
