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

	"docker-manager/internal/completion"
	"docker-manager/internal/docker"
	rpt "docker-manager/internal/report"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"
)

type logsScanDockerService interface {
	ListContainers(ctx context.Context, all bool) ([]container.Summary, error)
	InspectContainer(ctx context.Context, id string) (container.InspectResponse, error)
	ContainerLogs(ctx context.Context, id string, options container.LogsOptions) (io.ReadCloser, error)
}

var newLogsScanDockerService = func() (logsScanDockerService, error) {
	cli, err := docker.NewClient()
	if err != nil {
		return nil, err
	}
	return &dockerLogsScanService{cli: cli}, nil
}

type dockerLogsScanService struct {
	cli *client.Client
}

type LogsScanOptions struct {
	RunningOnly   bool
	Tail          int
	Context       int
	Since         string
	Keywords      []string
	Filters       []string
	RedactSecrets bool
	rpt.FormatOptions
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
}

type LogsScanContainer struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Image   string         `json:"image,omitempty"`
	State   string         `json:"state,omitempty"`
	Error   string         `json:"error,omitempty"`
	Matches []LogScanMatch `json:"matches,omitempty"`
}

type LogScanMatch struct {
	LineNumber int      `json:"line_number"`
	Line       string   `json:"line"`
	Keywords   []string `json:"keywords"`
	Before     []string `json:"before,omitempty"`
	After      []string `json:"after,omitempty"`
}

func NewLogsScanCommand() *cobra.Command {
	opts := LogsScanOptions{
		Tail:     500,
		Context:  0,
		Keywords: []string{"error", "panic", "exception", "fatal", "oom", "killed"},
	}
	cmd := &cobra.Command{
		Use:   "logs [container-pattern...]",
		Short: "扫描容器最近日志中的错误关键词",
		RunE: func(cmd *cobra.Command, args []string) error {
			runOpts := opts
			runOpts.Filters = append(append([]string(nil), opts.Filters...), args...)
			if err := validateLogsScanArgs(runOpts); err != nil {
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
	cmd.Flags().BoolVar(&opts.RunningOnly, "running", false, "只扫描正在运行的容器")
	cmd.Flags().IntVar(&opts.Tail, "tail", opts.Tail, "每个容器扫描最近日志行数，-1 表示全部")
	cmd.Flags().IntVar(&opts.Context, "context", opts.Context, "命中日志前后各输出多少行上下文")
	cmd.Flags().StringVar(&opts.Since, "since", "", "只扫描该时间之后的日志，例如 30m、2h 或 RFC3339 时间")
	cmd.Flags().StringArrayVar(&opts.Keywords, "keyword", opts.Keywords, "日志扫描关键词，可重复指定")
	cmd.Flags().StringArrayVarP(&opts.Filters, "filter", "f", nil, "筛选容器，支持 name:/id:/image:/state:/status:/label: 和 * ? 通配符，可重复指定")
	cmd.Flags().BoolVar(&opts.RedactSecrets, "redact-secrets", false, "脱敏日志命中行和上下文中的疑似敏感信息，便于分享输出")
	_ = cmd.RegisterFlagCompletionFunc("filter", completion.LocalContainers)
	rpt.AddFormatFlag(cmd, &opts.Format)
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

func buildLogsScanReport(ctx context.Context, svc logsScanDockerService, targets []container.Summary, opts LogsScanOptions) (LogsScanReport, error) {
	keywords := normalizeKeywords(opts.Keywords)
	report := LogsScanReport{
		GeneratedAt:    time.Now().Format(time.RFC3339),
		DockerEndpoint: docker.Endpoint(),
		Keywords:       keywords,
	}
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return report, err
		}
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
		report.Summary.ScannedContainers++

		inspect, err := svc.InspectContainer(ctx, ref)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return report, ctxErr
			}
			if errors.Is(err, context.Canceled) {
				return report, err
			}
			item.Error = fmt.Sprintf("inspect 失败: %v", err)
			report.Summary.Errors++
			report.Containers = append(report.Containers, item)
			continue
		}
		if err := ctx.Err(); err != nil {
			return report, err
		}
		applyLogsScanInspect(&item, inspect)

		text, err := readContainerLogText(ctx, svc, ref, inspect, opts)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return report, ctxErr
			}
			if errors.Is(err, context.Canceled) {
				return report, err
			}
			item.Error = fmt.Sprintf("读取日志失败: %v", err)
			report.Summary.Errors++
			report.Containers = append(report.Containers, item)
			continue
		}
		item.Matches, err = findLogScanMatchesWithContext(ctx, text, keywords, opts.Context)
		if err != nil {
			return report, err
		}
		if opts.RedactSecrets {
			redactLogScanMatches(item.Matches)
		}
		if len(item.Matches) > 0 {
			report.Summary.ContainersMatched++
			report.Summary.TotalMatches += len(item.Matches)
		}
		report.Containers = append(report.Containers, item)
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	sortLogsScanReport(&report)
	return report, nil
}

func applyLogsScanInspect(item *LogsScanContainer, inspect container.InspectResponse) {
	if inspect.ContainerJSONBase != nil {
		if item.ID == "" {
			item.ID = shortID(inspect.ID)
		}
		if name := normalizeContainerName(inspect.Name); name != "" {
			item.Name = name
		}
	}
	if inspect.Config != nil && item.Image == "" {
		item.Image = inspect.Config.Image
	}
	if inspect.State != nil && inspect.State.Status != "" {
		item.State = string(inspect.State.Status)
	}
}

func readContainerLogText(ctx context.Context, svc logsScanDockerService, id string, inspect container.InspectResponse, opts LogsScanOptions) (string, error) {
	tailValue := strconv.Itoa(opts.Tail)
	if opts.Tail < 0 {
		tailValue = "all"
	}
	options := container.LogsOptions{
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

func redactLogScanMatches(matches []LogScanMatch) {
	for i := range matches {
		matches[i].Line = redactSensitiveText(matches[i].Line)
		matches[i].Before = redactStringSlice(matches[i].Before)
		matches[i].After = redactStringSlice(matches[i].After)
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
	return s.cli.ContainerList(ctx, container.ListOptions{All: all})
}

func (s *dockerLogsScanService) InspectContainer(ctx context.Context, id string) (container.InspectResponse, error) {
	return s.cli.ContainerInspect(ctx, id)
}

func (s *dockerLogsScanService) ContainerLogs(ctx context.Context, id string, options container.LogsOptions) (io.ReadCloser, error) {
	return s.cli.ContainerLogs(ctx, id, options)
}
