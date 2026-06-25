package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"docker-manager/docker"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/spf13/cobra"
)

type healthDockerService interface {
	ListContainers(ctx context.Context, all bool) ([]container.Summary, error)
	InspectContainer(ctx context.Context, id string) (container.InspectResponse, error)
	ContainerLogs(ctx context.Context, id string, options container.LogsOptions) (io.ReadCloser, error)
}

var newHealthDockerService = func() (healthDockerService, error) {
	cli, err := docker.NewClient()
	if err != nil {
		return nil, err
	}
	return &dockerHealthService{cli: cli}, nil
}

type dockerHealthService struct {
	cli *client.Client
}

type HealthOptions struct {
	RunningOnly      bool
	NoLogs           bool
	LogTail          int
	RestartThreshold int
	Keywords         []string
	ContainerFilters []string
	RedactSecrets    bool
	ReportFormatOptions
}

type HealthReport struct {
	GeneratedAt string            `json:"generated_at"`
	Summary     HealthSummary     `json:"summary"`
	Containers  []HealthContainer `json:"containers"`
	Issues      []HealthIssue     `json:"issues,omitempty"`
}

type HealthSummary struct {
	Total           int `json:"total"`
	Running         int `json:"running"`
	Stopped         int `json:"stopped"`
	Restarting      int `json:"restarting"`
	Unhealthy       int `json:"unhealthy"`
	RestartWarnings int `json:"restart_warnings"`
	LogWarnings     int `json:"log_warnings"`
	PublicBindings  int `json:"public_bindings"`
}

type HealthContainer struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Image         string     `json:"image,omitempty"`
	State         string     `json:"state,omitempty"`
	Status        string     `json:"status,omitempty"`
	RestartCount  int        `json:"restart_count"`
	HealthStatus  string     `json:"health_status,omitempty"`
	FailingStreak int        `json:"failing_streak,omitempty"`
	ExitCode      int        `json:"exit_code,omitempty"`
	Error         string     `json:"error,omitempty"`
	PublicPorts   []string   `json:"public_ports,omitempty"`
	LogMatches    []LogMatch `json:"log_matches,omitempty"`
}

type LogMatch struct {
	Line     string   `json:"line"`
	Keywords []string `json:"keywords"`
}

type HealthIssue struct {
	Severity  string `json:"severity"`
	Container string `json:"container,omitempty"`
	Type      string `json:"type"`
	Message   string `json:"message"`
}

func newHealthCommand() *cobra.Command {
	opts := HealthOptions{
		LogTail:          100,
		RestartThreshold: 3,
		Keywords:         []string{"error", "panic", "exception", "fatal", "oom", "killed"},
	}
	cmd := &cobra.Command{
		Use:   "health [container-pattern...]",
		Short: "输出本机 Docker 体检报告",
		RunE: func(cmd *cobra.Command, args []string) error {
			runOpts := opts
			runOpts.ContainerFilters = append(append([]string(nil), opts.ContainerFilters...), args...)
			report, err := runHealthReport(cmd.Context(), runOpts)
			if err != nil {
				return fmt.Errorf("生成体检报告失败: %w", err)
			}
			return printReport(cmd.OutOrStdout(), runOpts.Format, report, func(w io.Writer) {
				printHealthReport(w, report)
			})
		},
		ValidArgsFunction: completeLocalContainers,
	}
	cmd.Flags().BoolVar(&opts.RunningOnly, "running-only", false, "只检查正在运行的容器")
	cmd.Flags().BoolVar(&opts.NoLogs, "no-logs", false, "不扫描容器日志")
	cmd.Flags().IntVar(&opts.LogTail, "log-tail", opts.LogTail, "每个容器扫描最近日志行数")
	cmd.Flags().IntVar(&opts.RestartThreshold, "restart-threshold", opts.RestartThreshold, "restart 次数达到该阈值时报告风险")
	cmd.Flags().StringArrayVar(&opts.Keywords, "keyword", opts.Keywords, "日志扫描关键词，可重复指定")
	cmd.Flags().StringArrayVarP(&opts.ContainerFilters, "filter", "f", nil, "筛选容器，支持名称/ID/镜像和 * ? 通配符，可重复指定")
	cmd.Flags().BoolVar(&opts.RedactSecrets, "redact-secrets", false, "脱敏日志命中行中的疑似敏感信息，便于分享输出")
	_ = cmd.RegisterFlagCompletionFunc("filter", completeLocalContainers)
	addReportFormatFlag(cmd, &opts.Format)
	return cmd
}

func runHealthReport(ctx context.Context, opts HealthOptions) (HealthReport, error) {
	svc, err := newHealthDockerService()
	if err != nil {
		return HealthReport{}, err
	}
	containers, err := svc.ListContainers(ctx, !opts.RunningOnly)
	if err != nil {
		return HealthReport{}, err
	}
	containers = filterContainerSummaries(containers, opts.ContainerFilters)
	return buildHealthReport(ctx, svc, containers, opts), nil
}

func buildHealthReport(ctx context.Context, svc healthDockerService, containers []container.Summary, opts HealthOptions) HealthReport {
	report := HealthReport{GeneratedAt: time.Now().Format(time.RFC3339)}
	keywords := normalizeKeywords(opts.Keywords)

	for _, summary := range containers {
		name := firstContainerName(summary.Names)
		if name == "" {
			name = shortID(summary.ID)
		}
		ref := summary.ID
		if ref == "" {
			ref = name
		}
		item := HealthContainer{
			ID:          shortID(summary.ID),
			Name:        name,
			Image:       summary.Image,
			State:       string(summary.State),
			Status:      summary.Status,
			PublicPorts: publicPortBindings(summary.Ports),
		}
		report.Summary.Total++
		switch summary.State {
		case "running":
			report.Summary.Running++
		case "restarting":
			report.Summary.Restarting++
		default:
			report.Summary.Stopped++
		}

		inspect, err := svc.InspectContainer(ctx, ref)
		if err != nil {
			report.Issues = append(report.Issues, HealthIssue{
				Severity:  "error",
				Container: name,
				Type:      "inspect-failed",
				Message:   fmt.Sprintf("inspect 失败: %v", err),
			})
			report.Containers = append(report.Containers, item)
			continue
		}
		applyInspectHealth(&item, inspect)
		addStateIssues(&report, item, inspect, opts)

		for _, port := range item.PublicPorts {
			report.Summary.PublicBindings++
			report.Issues = append(report.Issues, HealthIssue{
				Severity:  "warn",
				Container: item.Name,
				Type:      "public-port",
				Message:   fmt.Sprintf("public port binding: %s", port),
			})
		}

		if !opts.NoLogs && opts.LogTail != 0 && len(keywords) > 0 {
			matches, err := scanHealthLogs(ctx, svc, ref, inspect, opts.LogTail, keywords, opts.RedactSecrets)
			if err != nil {
				report.Issues = append(report.Issues, HealthIssue{
					Severity:  "warn",
					Container: item.Name,
					Type:      "logs-unavailable",
					Message:   fmt.Sprintf("扫描日志失败: %v", err),
				})
			} else if len(matches) > 0 {
				item.LogMatches = matches
				report.Summary.LogWarnings += len(matches)
				report.Issues = append(report.Issues, HealthIssue{
					Severity:  "warn",
					Container: item.Name,
					Type:      "log-keyword",
					Message:   fmt.Sprintf("matched %d recent log lines", len(matches)),
				})
			}
		}

		report.Containers = append(report.Containers, item)
	}
	sortHealthReport(&report)
	return report
}

func applyInspectHealth(item *HealthContainer, inspect container.InspectResponse) {
	if inspect.ContainerJSONBase != nil {
		if item.ID == "" {
			item.ID = shortID(inspect.ID)
		}
		if item.Name == "" {
			item.Name = normalizeContainerName(inspect.Name)
		}
		item.RestartCount = inspect.RestartCount
	}
	if inspect.Config != nil && item.Image == "" {
		item.Image = inspect.Config.Image
	}
	if inspect.State == nil {
		return
	}
	if inspect.State.Status != "" {
		item.State = string(inspect.State.Status)
	}
	item.ExitCode = inspect.State.ExitCode
	item.Error = inspect.State.Error
	if inspect.State.Health != nil {
		item.HealthStatus = string(inspect.State.Health.Status)
		item.FailingStreak = inspect.State.Health.FailingStreak
	}
}

func addStateIssues(report *HealthReport, item HealthContainer, inspect container.InspectResponse, opts HealthOptions) {
	if item.RestartCount >= opts.RestartThreshold && opts.RestartThreshold > 0 {
		report.Summary.RestartWarnings++
		report.Issues = append(report.Issues, HealthIssue{
			Severity:  "warn",
			Container: item.Name,
			Type:      "restart-count",
			Message:   fmt.Sprintf("restart count is %d", item.RestartCount),
		})
	}
	if item.HealthStatus == "unhealthy" {
		report.Summary.Unhealthy++
		report.Issues = append(report.Issues, HealthIssue{
			Severity:  "error",
			Container: item.Name,
			Type:      "unhealthy",
			Message:   fmt.Sprintf("healthcheck unhealthy, failing streak=%d", item.FailingStreak),
		})
	} else if item.HealthStatus == "starting" {
		report.Issues = append(report.Issues, HealthIssue{
			Severity:  "warn",
			Container: item.Name,
			Type:      "health-starting",
			Message:   "healthcheck is still starting",
		})
	}
	if item.State == "restarting" || item.State == "dead" {
		report.Issues = append(report.Issues, HealthIssue{
			Severity:  "error",
			Container: item.Name,
			Type:      "bad-state",
			Message:   fmt.Sprintf("container state is %s", item.State),
		})
	}
	if inspect.State != nil && inspect.State.OOMKilled {
		report.Issues = append(report.Issues, HealthIssue{
			Severity:  "error",
			Container: item.Name,
			Type:      "oom-killed",
			Message:   "container was OOM killed",
		})
	}
	if item.State != "running" && item.ExitCode != 0 {
		report.Issues = append(report.Issues, HealthIssue{
			Severity:  "warn",
			Container: item.Name,
			Type:      "non-zero-exit",
			Message:   fmt.Sprintf("container exited with code %d", item.ExitCode),
		})
	}
}

func scanHealthLogs(ctx context.Context, svc healthDockerService, id string, inspect container.InspectResponse, tail int, keywords []string, redactSecrets bool) ([]LogMatch, error) {
	tailValue := strconv.Itoa(tail)
	if tail < 0 {
		tailValue = "all"
	}
	reader, err := svc.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       tailValue,
	})
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	text, err := readDockerLogs(reader, inspect.Config != nil && inspect.Config.Tty)
	if err != nil {
		return nil, err
	}
	matches := findLogMatches(text, keywords)
	if redactSecrets {
		for i := range matches {
			matches[i].Line = redactSensitiveText(matches[i].Line)
		}
	}
	return matches, nil
}

func readDockerLogs(reader io.Reader, tty bool) (string, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	if tty {
		return string(data), nil
	}
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, bytes.NewReader(data)); err != nil {
		return string(data), nil
	}
	return stdout.String() + stderr.String(), nil
}

func findLogMatches(text string, keywords []string) []LogMatch {
	var matches []LogMatch
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		var found []string
		for _, keyword := range keywords {
			if strings.Contains(lower, keyword) {
				found = append(found, keyword)
			}
		}
		if len(found) > 0 {
			matches = append(matches, LogMatch{Line: line, Keywords: found})
		}
	}
	return matches
}

func publicPortBindings(ports []container.Port) []string {
	var result []string
	for _, port := range ports {
		if port.PublicPort == 0 {
			continue
		}
		hostIP := normalizeHostIP(port.IP)
		if !isPublicHostIP(hostIP) {
			continue
		}
		result = append(result, fmt.Sprintf("%s:%d->%d/%s", hostIP, port.PublicPort, port.PrivatePort, port.Type))
	}
	sort.Strings(result)
	return result
}

func normalizeKeywords(keywords []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, keyword := range keywords {
		keyword = strings.ToLower(strings.TrimSpace(keyword))
		if keyword == "" || seen[keyword] {
			continue
		}
		seen[keyword] = true
		result = append(result, keyword)
	}
	sort.Strings(result)
	return result
}

func printHealthReport(w io.Writer, report HealthReport) {
	fmt.Fprintf(w, "Docker 体检报告 (%s)\n", report.GeneratedAt)
	fmt.Fprintf(w, "容器: 总数=%d 运行中=%d 已停止=%d 重启中=%d 不健康=%d\n", report.Summary.Total, report.Summary.Running, report.Summary.Stopped, report.Summary.Restarting, report.Summary.Unhealthy)
	fmt.Fprintf(w, "风险: 重启=%d 日志=%d 公网端口=%d 问题=%d\n\n", report.Summary.RestartWarnings, report.Summary.LogWarnings, report.Summary.PublicBindings, len(report.Issues))

	fmt.Fprintln(w, "问题:")
	if len(report.Issues) == 0 {
		fmt.Fprintln(w, "  无")
	} else {
		for _, issue := range report.Issues {
			containerName := ""
			if issue.Container != "" {
				containerName = " " + issue.Container
			}
			fmt.Fprintf(w, "  - [%s] %s%s: %s\n", issue.Severity, issue.Type, containerName, issue.Message)
		}
	}

	fmt.Fprintln(w, "\n容器:")
	for _, c := range report.Containers {
		health := c.HealthStatus
		if health == "" {
			health = "none"
		}
		fmt.Fprintf(w, "  - %s 状态=%s 健康=%s 重启次数=%d 镜像=%s\n", c.Name, c.State, health, c.RestartCount, c.Image)
		for _, port := range c.PublicPorts {
			fmt.Fprintf(w, "      公网端口=%s\n", port)
		}
		for _, match := range c.LogMatches {
			fmt.Fprintf(w, "      日志[%s] %s\n", strings.Join(match.Keywords, ","), match.Line)
		}
	}
}

func sortHealthReport(report *HealthReport) {
	sort.Slice(report.Containers, func(i, j int) bool {
		return report.Containers[i].Name < report.Containers[j].Name
	})
	sort.Slice(report.Issues, func(i, j int) bool {
		if report.Issues[i].Severity == report.Issues[j].Severity {
			if report.Issues[i].Type == report.Issues[j].Type {
				return report.Issues[i].Container < report.Issues[j].Container
			}
			return report.Issues[i].Type < report.Issues[j].Type
		}
		return report.Issues[i].Severity < report.Issues[j].Severity
	})
}

func (s *dockerHealthService) ListContainers(ctx context.Context, all bool) ([]container.Summary, error) {
	return s.cli.ContainerList(ctx, container.ListOptions{All: all})
}

func (s *dockerHealthService) InspectContainer(ctx context.Context, id string) (container.InspectResponse, error) {
	return s.cli.ContainerInspect(ctx, id)
}

func (s *dockerHealthService) ContainerLogs(ctx context.Context, id string, options container.LogsOptions) (io.ReadCloser, error) {
	return s.cli.ContainerLogs(ctx, id, options)
}
