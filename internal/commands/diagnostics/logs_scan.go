package diagnostics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"docker-manager/internal/commandflags"
	"docker-manager/internal/completion"
	"docker-manager/internal/docker"
	"docker-manager/internal/parallel"
	rpt "docker-manager/internal/report"

	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
	"github.com/spf13/cobra"
)

type logsScanDockerService interface {
	ListContainers(ctx context.Context, all bool) ([]container.Summary, error)
	InspectContainer(ctx context.Context, id string) (container.InspectResponse, error)
	ContainerLogs(ctx context.Context, id string, options mobyclient.ContainerLogsOptions) (io.ReadCloser, error)
}

var newLogsScanDockerService = func() (logsScanDockerService, error) {
	cli, err := docker.NewMobyClient()
	if err != nil {
		return nil, err
	}
	return &dockerLogsScanService{cli: cli}, nil
}

type dockerLogsScanService struct {
	cli *mobyclient.Client
}

type LogsScanOptions struct {
	RunningOnly   bool
	Tail          int
	Context       int
	Since         string
	Keywords      []string
	Filters       []string
	RedactSecrets bool
	RedactProfile string
	commandflags.FormatOptions
}

type LogsScanReport struct {
	GeneratedAt    string              `json:"generated_at"`
	DockerEndpoint string              `json:"docker_endpoint"`
	Target         TargetSelection     `json:"target"`
	Keywords       []string            `json:"keywords"`
	Containers     []LogsScanContainer `json:"containers"`
	Summary        LogsScanSummary     `json:"summary"`
}

type LogsScanSummary struct {
	ScannedContainers int `json:"scanned_containers"`
	ContainersMatched int `json:"containers_matched"`
	TotalMatches      int `json:"total_matches"`
	Errors            int `json:"errors"`
	LogsUnavailable   int `json:"logs_unavailable"`
}

type LogsScanContainer struct {
	ID                    string         `json:"id"`
	Name                  string         `json:"name"`
	Image                 string         `json:"image,omitempty"`
	State                 string         `json:"state,omitempty"`
	LogDriver             string         `json:"log_driver,omitempty"`
	LogReadability        string         `json:"log_readability,omitempty"`
	LogReadabilityMessage string         `json:"log_readability_message,omitempty"`
	Error                 string         `json:"error,omitempty"`
	Matches               []LogScanMatch `json:"matches,omitempty"`
}

type LogScanMatch struct {
	LineNumber int      `json:"line_number"`
	Line       string   `json:"line"`
	Keywords   []string `json:"keywords"`
	Before     []string `json:"before,omitempty"`
	After      []string `json:"after,omitempty"`
}

func NewLogsScanCommand() *cobra.Command {
	opts := defaultLogsScanOptions()
	cmd := &cobra.Command{
		Use:   "logs [container-pattern...]",
		Short: "扫描容器最近日志中的错误关键词",
		RunE: func(cmd *cobra.Command, args []string) error {
			runOpts := opts
			runOpts.Filters = append(append([]string(nil), opts.Filters...), args...)
			if err := validateLogsScanArgs(runOpts); err != nil {
				return err
			}
			if _, err := normalizeRedactProfile(runOpts.RedactProfile, runOpts.RedactSecrets); err != nil {
				return err
			}
			report, err := runLogsScan(cmd.Context(), runOpts)
			if err != nil {
				return fmt.Errorf("扫描日志失败: %w", err)
			}
			return rpt.Print(cmd.OutOrStdout(), runOpts.Format, report, func(w io.Writer) {
				printLogsScanReport(w, report)
			})
		},
		ValidArgsFunction: completion.LocalContainers,
	}
	commandflags.AddRunningFlag(cmd, &opts.RunningOnly, "只扫描正在运行的容器")
	cmd.Flags().IntVar(&opts.Tail, "tail", opts.Tail, "每个容器扫描最近日志行数，-1 表示全部")
	cmd.Flags().IntVar(&opts.Context, "context", opts.Context, "命中日志前后各输出多少行上下文")
	cmd.Flags().StringVar(&opts.Since, "since", "", "只扫描该时间之后的日志，例如 30m、2h 或 RFC3339 时间")
	cmd.Flags().StringArrayVar(&opts.Keywords, "keyword", opts.Keywords, "日志扫描关键词，可重复指定")
	commandflags.AddContainerFilterFlag(cmd, &opts.Filters, "")
	commandflags.AddRedactFlags(cmd, &opts.RedactSecrets, &opts.RedactProfile, "脱敏日志命中行和上下文中的疑似敏感信息，便于分享输出")
	commandflags.AddReportFormatFlag(cmd, &opts.Format)
	return cmd
}

func validateLogsScanArgs(opts LogsScanOptions) error {
	if opts.Context < 0 {
		return fmt.Errorf("--context 不能小于 0")
	}
	if opts.Tail == 0 || opts.Tail < -1 {
		return fmt.Errorf("--tail 必须为正数，或使用 -1 表示全部")
	}
	return nil
}

func runLogsScan(ctx context.Context, opts LogsScanOptions) (LogsScanReport, error) {
	if _, err := normalizeRedactProfile(opts.RedactProfile, opts.RedactSecrets); err != nil {
		return LogsScanReport{}, err
	}
	svc, err := newLogsScanDockerService()
	if err != nil {
		return LogsScanReport{}, err
	}
	targets, err := logsScanTargets(ctx, svc, opts)
	if err != nil {
		return LogsScanReport{}, err
	}
	report, err := buildLogsScanReport(ctx, svc, targets, opts)
	if err != nil {
		return report, err
	}
	report.Target = buildContainerTargetSelection("扫描", len(targets), opts.RunningOnly, opts.Filters)
	return report, nil
}

func logsScanTargets(ctx context.Context, svc logsScanDockerService, opts LogsScanOptions) ([]container.Summary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	listAll := !opts.RunningOnly
	containers, err := svc.ListContainers(ctx, listAll)
	if err != nil {
		return nil, err
	}
	if opts.RunningOnly {
		var running []container.Summary
		for _, c := range containers {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if c.State == "running" {
				running = append(running, c)
			}
		}
		containers = running
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	containers = filterContainerSummaries(containers, opts.Filters)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sort.Slice(containers, func(i, j int) bool {
		return firstContainerName(containers[i].Names) < firstContainerName(containers[j].Names)
	})
	return containers, nil
}

type logsScanBuildResult struct {
	item    LogsScanContainer
	summary LogsScanSummary
	err     error
}

func buildLogsScanReport(ctx context.Context, svc logsScanDockerService, targets []container.Summary, opts LogsScanOptions) (LogsScanReport, error) {
	keywords := normalizeKeywords(opts.Keywords)
	report := LogsScanReport{
		GeneratedAt:    time.Now().Format(time.RFC3339),
		DockerEndpoint: docker.Endpoint(),
		Keywords:       keywords,
	}
	results := make([]logsScanBuildResult, len(targets))
	if err := parallel.ForEachIndexErr(ctx, len(targets), diagnosticsInspectConcurrency, func(ctx context.Context, i int) error {
		result := buildLogsScanContainerResult(ctx, svc, targets[i], opts, keywords)
		results[i] = result
		return result.err
	}); err != nil {
		return report, err
	}
	for _, result := range results {
		if result.item.Name == "" && result.item.ID == "" {
			continue
		}
		report.Summary.ScannedContainers += result.summary.ScannedContainers
		report.Summary.ContainersMatched += result.summary.ContainersMatched
		report.Summary.TotalMatches += result.summary.TotalMatches
		report.Summary.Errors += result.summary.Errors
		report.Summary.LogsUnavailable += result.summary.LogsUnavailable
		report.Containers = append(report.Containers, result.item)
	}
	sortLogsScanReport(&report)
	return report, nil
}

func buildLogsScanContainerResult(ctx context.Context, svc logsScanDockerService, target container.Summary, opts LogsScanOptions, keywords []string) logsScanBuildResult {
	item := LogsScanContainer{
		ID:    shortID(target.ID),
		Name:  firstContainerName(target.Names),
		Image: target.Image,
		State: string(target.State),
	}
	ref := target.ID
	if ref == "" {
		ref = item.Name
	}
	if item.Name == "" {
		item.Name = ref
	}
	result := logsScanBuildResult{item: item}
	result.summary.ScannedContainers++

	inspect, err := svc.InspectContainer(ctx, ref)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			result.err = ctxErr
			return result
		}
		if errors.Is(err, context.Canceled) {
			result.err = err
			return result
		}
		result.item.Error = fmt.Sprintf("inspect ??: %v", err)
		result.summary.Errors++
		return result
	}
	if err := ctx.Err(); err != nil {
		result.err = err
		return result
	}
	applyLogsScanInspect(&result.item, inspect)
	availability := containerLogDriverAvailability(inspect)
	applyLogsScanAvailability(&result.item, availability)
	if !availability.Readable {
		result.item.Error = availability.Reason
		result.summary.Errors++
		result.summary.LogsUnavailable++
		return result
	}

	text, err := readContainerLogText(ctx, svc, ref, inspect, opts)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			result.err = ctxErr
			return result
		}
		if errors.Is(err, context.Canceled) {
			result.err = err
			return result
		}
		result.item.Error = fmt.Sprintf("??????: %v", err)
		result.summary.Errors++
		return result
	}
	result.item.Matches, err = findLogScanMatchesWithContext(ctx, text, keywords, opts.Context)
	if err != nil {
		result.err = err
		return result
	}
	redactProfile, err := normalizeRedactProfile(opts.RedactProfile, opts.RedactSecrets)
	if err != nil {
		result.err = err
		return result
	}
	if redactProfile != "none" {
		redactLogScanMatches(result.item.Matches, redactProfile)
	}
	if len(result.item.Matches) > 0 {
		result.summary.ContainersMatched++
		result.summary.TotalMatches += len(result.item.Matches)
	}
	return result
}

func applyLogsScanInspect(item *LogsScanContainer, inspect container.InspectResponse) {
	if item.ID == "" {
		item.ID = shortID(inspect.ID)
	}
	if name := normalizeContainerName(inspect.Name); name != "" {
		item.Name = name
	}
	if inspect.Config != nil && item.Image == "" {
		item.Image = inspect.Config.Image
	}
	if inspect.State != nil && inspect.State.Status != "" {
		item.State = string(inspect.State.Status)
	}
}

func applyLogsScanAvailability(item *LogsScanContainer, availability logDriverAvailability) {
	item.LogDriver = availability.Driver
	item.LogReadability = availability.Status
	item.LogReadabilityMessage = availability.Reason
}

func readContainerLogText(ctx context.Context, svc logsScanDockerService, id string, inspect container.InspectResponse, opts LogsScanOptions) (string, error) {
	if availability := containerLogDriverAvailability(inspect); !availability.Readable {
		return "", fmt.Errorf("%s", availability.Reason)
	}
	tailValue := strconv.Itoa(opts.Tail)
	if opts.Tail < 0 {
		tailValue = "all"
	}
	options := mobyclient.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       tailValue,
	}
	if opts.Since != "" {
		options.Since = normalizeLogsSince(opts.Since)
	}
	reader, err := svc.ContainerLogs(ctx, id, options)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	return readDockerLogsWithContext(ctx, reader, inspect.Config != nil && inspect.Config.Tty)
}

func normalizeLogsSince(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if d, err := time.ParseDuration(value); err == nil {
		return strconv.FormatInt(time.Now().Add(-d).Unix(), 10)
	}
	return value
}

func findLogScanMatches(text string, keywords []string, contextLines int) []LogScanMatch {
	matches, _ := findLogScanMatchesWithContext(context.Background(), text, keywords, contextLines)
	return matches
}

func findLogScanMatchesWithContext(ctx context.Context, text string, keywords []string, contextLines int) ([]LogScanMatch, error) {
	lines := splitLogLines(text)
	var matches []LogScanMatch
	for i, line := range lines {
		if err := ctx.Err(); err != nil {
			return matches, err
		}
		lower := strings.ToLower(line)
		var found []string
		for _, keyword := range keywords {
			if strings.Contains(lower, keyword) {
				found = append(found, keyword)
			}
		}
		if len(found) == 0 {
			continue
		}
		match := LogScanMatch{
			LineNumber: i + 1,
			Line:       line,
			Keywords:   found,
			Before:     surroundingLines(lines, i-contextLines, i),
			After:      surroundingLines(lines, i+1, i+1+contextLines),
		}
		matches = append(matches, match)
	}
	return matches, nil
}

func redactLogScanMatches(matches []LogScanMatch, profile sensitiveProfile) {
	for i := range matches {
		matches[i].Line = redactSensitiveTextWithProfile(matches[i].Line, profile)
		matches[i].Before = redactStringSliceWithProfile(matches[i].Before, profile)
		matches[i].After = redactStringSliceWithProfile(matches[i].After, profile)
	}
}

func splitLogLines(text string) []string {
	var lines []string
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimRight(rawLine, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

func surroundingLines(lines []string, start, end int) []string {
	if start < 0 {
		start = 0
	}
	if end > len(lines) {
		end = len(lines)
	}
	if start >= end {
		return nil
	}
	return append([]string(nil), lines[start:end]...)
}

func printLogsScanReport(w io.Writer, report LogsScanReport) {
	fmt.Fprintf(w, "Docker 日志扫描 (%s)\n", report.GeneratedAt)
	printDockerEndpoint(w, report.DockerEndpoint)
	printTargetSelection(w, report.Target)
	fmt.Fprintf(w, "关键词: %s\n", strings.Join(report.Keywords, ", "))
	fmt.Fprintf(w, "摘要: 已扫描=%d 命中容器=%d 命中行=%d 错误=%d\n\n", report.Summary.ScannedContainers, report.Summary.ContainersMatched, report.Summary.TotalMatches, report.Summary.Errors)

	for _, c := range report.Containers {
		status := fmt.Sprintf("命中=%d", len(c.Matches))
		if c.Error != "" {
			status += " 错误=" + c.Error
		}
		fmt.Fprintf(w, "%s 状态=%s 镜像=%s %s\n", c.Name, c.State, c.Image, status)
		if c.LogDriver != "" || c.LogReadability != "" || c.LogReadabilityMessage != "" {
			fmt.Fprintf(w, "    log-driver=%s log-readable=%s note=%s\n", c.LogDriver, c.LogReadability, c.LogReadabilityMessage)
		}
		for _, match := range c.Matches {
			for _, before := range match.Before {
				fmt.Fprintf(w, "    | %s\n", before)
			}
			fmt.Fprintf(w, "  > 第 %d 行 [%s] %s\n", match.LineNumber, strings.Join(match.Keywords, ","), match.Line)
			for _, after := range match.After {
				fmt.Fprintf(w, "    | %s\n", after)
			}
		}
	}
	if len(report.Containers) == 0 {
		fmt.Fprintln(w, "未扫描任何容器。")
	}
}

func sortLogsScanReport(report *LogsScanReport) {
	sort.Slice(report.Containers, func(i, j int) bool {
		return report.Containers[i].Name < report.Containers[j].Name
	})
}

func (s *dockerLogsScanService) ListContainers(ctx context.Context, all bool) ([]container.Summary, error) {
	result, err := s.cli.ContainerList(ctx, mobyclient.ContainerListOptions{All: all})
	if err != nil {
		return nil, err
	}
	return docker.ConvertDockerType[[]container.Summary](result.Items)
}

func (s *dockerLogsScanService) InspectContainer(ctx context.Context, id string) (container.InspectResponse, error) {
	result, err := s.cli.ContainerInspect(ctx, id, mobyclient.ContainerInspectOptions{})
	if err != nil {
		return container.InspectResponse{}, err
	}
	return docker.ConvertDockerType[container.InspectResponse](result.Container)
}

func (s *dockerLogsScanService) ContainerLogs(ctx context.Context, id string, options mobyclient.ContainerLogsOptions) (io.ReadCloser, error) {
	return s.cli.ContainerLogs(ctx, id, options)
}
