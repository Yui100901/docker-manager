package diagnostics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"docker-manager/internal/commandflags"
	"docker-manager/internal/docker"
	"docker-manager/internal/parallel"
	rpt "docker-manager/internal/report"

	"github.com/spf13/cobra"
)

const (
	reportAllKindHealth  = "health"
	reportAllKindNetwork = "network"
	reportAllKindLogs    = "logs"
	reportAllKindVolumes = "volumes"
	reportAllKindPrune   = "prune"
)

var defaultReportAllKinds = []string{
	reportAllKindHealth,
	reportAllKindNetwork,
	reportAllKindLogs,
	reportAllKindVolumes,
	reportAllKindPrune,
}

type ReportAllOptions struct {
	Include       []string
	Skip          []string
	RunningOnly   bool
	Filters       []string
	RedactSecrets bool
	RedactProfile string

	HealthLogs bool

	LogTail     int
	LogContext  int
	LogSince    string
	LogKeywords []string

	VolumeAll       bool
	VolumeNoTrunc   bool
	VolumeSizeMode  string
	VolumeSizeImage string

	PruneOnly          []string
	PruneFilters       []string
	PruneUntil         string
	PruneProtectLabels []string

	commandflags.FormatOptions
}

type ReportAllReport struct {
	GeneratedAt    string             `json:"generated_at"`
	DockerEndpoint string             `json:"docker_endpoint"`
	Selected       []string           `json:"selected"`
	Sections       []ReportAllSection `json:"sections"`
	Health         *HealthReport      `json:"health,omitempty"`
	Network        *NetworkReport     `json:"network,omitempty"`
	Logs           *LogsScanReport    `json:"logs,omitempty"`
	Volumes        *VolumeReport      `json:"volumes,omitempty"`
	Prune          *PruneReport       `json:"prune,omitempty"`
}

type ReportAllSection struct {
	Name           string `json:"name"`
	Status         string `json:"status"`
	DurationMillis int64  `json:"duration_millis,omitempty"`
	Error          string `json:"error,omitempty"`
}

type reportAllSectionResult struct {
	section ReportAllSection
	err     error

	health  *HealthReport
	network *NetworkReport
	logs    *LogsScanReport
	volumes *VolumeReport
	prune   *PruneReport
}

func NewReportAllCommand() *cobra.Command {
	opts := defaultReportAllOptions()
	cmd := &cobra.Command{
		Use:   "all",
		Short: "聚合输出 health、network、logs、volumes 和 prune dry-run 报告",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			report, err := runReportAll(cmd.Context(), opts)
			if report.GeneratedAt == "" {
				return err
			}
			if printErr := rpt.Print(cmd.OutOrStdout(), opts.Format, report, func(w io.Writer) {
				printReportAll(w, report, opts)
			}); printErr != nil {
				return printErr
			}
			if err != nil {
				return fmt.Errorf("聚合报告存在失败项: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&opts.Include, "include", nil, "只运行指定报告，支持逗号分隔: health,network,logs,volumes,prune")
	cmd.Flags().StringArrayVar(&opts.Skip, "skip", nil, "跳过指定报告，支持逗号分隔: health,network,logs,volumes,prune")
	commandflags.AddContainerFilterFlags(cmd, &opts.RunningOnly, &opts.Filters, "容器类报告只处理运行中的容器")
	commandflags.AddRedactFlags(cmd, &opts.RedactSecrets, &opts.RedactProfile, "对 health/logs 中的日志命中内容进行脱敏")
	cmd.Flags().BoolVar(&opts.HealthLogs, "health-logs", false, "health 子报告也扫描容器日志；默认由 logs 子报告统一扫描")
	cmd.Flags().IntVar(&opts.LogTail, "log-tail", opts.LogTail, "logs 子报告每个容器扫描最近日志行数，-1 表示全部")
	cmd.Flags().IntVar(&opts.LogContext, "log-context", 0, "logs 子报告命中日志前后输出多少行上下文")
	cmd.Flags().StringVar(&opts.LogSince, "log-since", "", "logs 子报告只扫描该时间之后的日志，例如 30m、2h 或 RFC3339")
	cmd.Flags().StringArrayVar(&opts.LogKeywords, "log-keyword", opts.LogKeywords, "logs/health 日志扫描关键词，可重复指定")
	cmd.Flags().BoolVar(&opts.VolumeAll, "volume-all", false, "volumes 子报告显示所有 volume")
	cmd.Flags().BoolVar(&opts.VolumeNoTrunc, "volume-no-trunc", false, "volumes 子报告显示完整 volume 名称和挂载点")
	commandflags.AddReportAllVolumeSizeFlags(cmd, &opts.VolumeSizeMode, opts.VolumeSizeMode, &opts.VolumeSizeImage, opts.VolumeSizeImage)
	commandflags.AddReportAllPruneScopeFlags(cmd, &opts.PruneOnly, &opts.PruneFilters, &opts.PruneUntil, &opts.PruneProtectLabels)
	commandflags.AddReportFormatFlag(cmd, &opts.Format)
	return cmd
}

func runReportAll(ctx context.Context, opts ReportAllOptions) (ReportAllReport, error) {
	if _, err := normalizeRedactProfile(opts.RedactProfile, opts.RedactSecrets); err != nil {
		return ReportAllReport{}, err
	}
	selected, err := selectReportAllKinds(opts.Include, opts.Skip)
	if err != nil {
		return ReportAllReport{}, err
	}
	report := ReportAllReport{
		GeneratedAt:    time.Now().Format(time.RFC3339),
		DockerEndpoint: docker.Endpoint(),
		Selected:       selected,
	}
	results := make([]reportAllSectionResult, len(selected))
	parallel.ForEachIndex(ctx, len(selected), len(selected), func(ctx context.Context, i int) {
		results[i] = runReportAllSection(ctx, selected[i], opts)
	})
	if err := ctx.Err(); err != nil {
		return report, err
	}

	var errs []error
	for _, result := range results {
		report.Sections = append(report.Sections, result.section)
		switch result.section.Name {
		case reportAllKindHealth:
			report.Health = result.health
		case reportAllKindNetwork:
			report.Network = result.network
		case reportAllKindLogs:
			report.Logs = result.logs
		case reportAllKindVolumes:
			report.Volumes = result.volumes
		case reportAllKindPrune:
			report.Prune = result.prune
		}
		if result.err != nil {
			if errors.Is(result.err, context.Canceled) || errors.Is(result.err, context.DeadlineExceeded) {
				return report, result.err
			}
			errs = append(errs, fmt.Errorf("%s: %w", result.section.Name, result.err))
		}
	}
	return report, errors.Join(errs...)
}

func runReportAllSection(ctx context.Context, kind string, opts ReportAllOptions) (result reportAllSectionResult) {
	result.section = ReportAllSection{Name: kind, Status: "ok"}
	start := time.Now()
	defer func() {
		result.section.DurationMillis = time.Since(start).Milliseconds()
		if result.err != nil {
			result.section.Status = "failed"
			result.section.Error = result.err.Error()
		}
	}()
	switch kind {
	case reportAllKindHealth:
		childOpts := defaultHealthOptions()
		childOpts.RunningOnly = opts.RunningOnly
		childOpts.NoLogs = !opts.HealthLogs
		childOpts.LogTail = opts.LogTail
		childOpts.Keywords = append([]string(nil), opts.LogKeywords...)
		childOpts.ContainerFilters = append([]string(nil), opts.Filters...)
		childOpts.RedactSecrets = opts.RedactSecrets
		childOpts.RedactProfile = opts.RedactProfile
		child, runErr := runHealthReport(ctx, childOpts)
		result.health = &child
		result.err = runErr
	case reportAllKindNetwork:
		child, runErr := runNetworkReport(ctx, NetworkOptions{
			RunningOnly:      opts.RunningOnly,
			ContainerFilters: append([]string(nil), opts.Filters...),
		})
		result.network = &child
		result.err = runErr
	case reportAllKindLogs:
		childOpts := defaultLogsScanOptions()
		childOpts.RunningOnly = opts.RunningOnly
		childOpts.Tail = opts.LogTail
		childOpts.Context = opts.LogContext
		childOpts.Since = opts.LogSince
		childOpts.Keywords = append([]string(nil), opts.LogKeywords...)
		childOpts.Filters = append([]string(nil), opts.Filters...)
		childOpts.RedactSecrets = opts.RedactSecrets
		childOpts.RedactProfile = opts.RedactProfile
		if validateErr := validateLogsScanArgs(childOpts); validateErr != nil {
			result.err = validateErr
			break
		}
		child, runErr := runLogsScan(ctx, childOpts)
		result.logs = &child
		result.err = runErr
	case reportAllKindVolumes:
		childOpts := defaultVolumeOptions()
		childOpts.All = opts.VolumeAll
		childOpts.NoTrunc = opts.VolumeNoTrunc
		childOpts.SizeMode = opts.VolumeSizeMode
		childOpts.SizeImage = opts.VolumeSizeImage
		if normalizeErr := normalizeVolumeOptions(&childOpts); normalizeErr != nil {
			result.err = normalizeErr
			break
		}
		child, runErr := runVolumeReport(ctx, childOpts)
		result.volumes = &child
		result.err = runErr
	case reportAllKindPrune:
		child, runErr := runPruneReport(ctx, PruneReportOptions{
			Only:          append([]string(nil), opts.PruneOnly...),
			Filters:       append([]string(nil), opts.PruneFilters...),
			Until:         opts.PruneUntil,
			ProtectLabels: append([]string(nil), opts.PruneProtectLabels...),
		})
		result.prune = &child
		result.err = runErr
	default:
		result.err = fmt.Errorf("unsupported report kind %q", kind)
	}
	return result
}

func selectReportAllKinds(include, skip []string) ([]string, error) {
	selected := append([]string(nil), defaultReportAllKinds...)
	if len(include) > 0 {
		kinds, err := normalizeReportAllKinds(include)
		if err != nil {
			return nil, err
		}
		selected = kinds
	}
	if len(skip) > 0 {
		skipped, err := normalizeReportAllKinds(skip)
		if err != nil {
			return nil, err
		}
		skipSet := map[string]bool{}
		for _, kind := range skipped {
			skipSet[kind] = true
		}
		filtered := selected[:0]
		for _, kind := range selected {
			if !skipSet[kind] {
				filtered = append(filtered, kind)
			}
		}
		selected = filtered
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("没有可运行的报告，请调整 --include 或 --skip")
	}
	return selected, nil
}

func normalizeReportAllKinds(values []string) ([]string, error) {
	seen := map[string]bool{}
	var kinds []string
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			kind := strings.ToLower(strings.TrimSpace(part))
			if kind == "" {
				continue
			}
			switch kind {
			case reportAllKindHealth, reportAllKindNetwork, reportAllKindLogs, reportAllKindVolumes, reportAllKindPrune:
			case "volume":
				kind = reportAllKindVolumes
			case "log":
				kind = reportAllKindLogs
			default:
				return nil, fmt.Errorf("不支持的聚合报告类型 %q，请使用 health、network、logs、volumes 或 prune", part)
			}
			if !seen[kind] {
				seen[kind] = true
				kinds = append(kinds, kind)
			}
		}
	}
	sort.SliceStable(kinds, func(i, j int) bool {
		return reportAllKindRank(kinds[i]) < reportAllKindRank(kinds[j])
	})
	return kinds, nil
}

func reportAllKindRank(kind string) int {
	for i, item := range defaultReportAllKinds {
		if item == kind {
			return i
		}
	}
	return len(defaultReportAllKinds)
}

func printReportAll(w io.Writer, report ReportAllReport, opts ReportAllOptions) {
	fmt.Fprintf(w, "Docker 聚合报告 (%s)\n", report.GeneratedAt)
	printDockerEndpoint(w, report.DockerEndpoint)
	ok, failed := reportAllSectionCounts(report.Sections)
	fmt.Fprintf(w, "摘要: 报告=%d 成功=%d 失败=%d\n", len(report.Sections), ok, failed)
	fmt.Fprintf(w, "包含: %s\n\n", strings.Join(report.Selected, ", "))

	for _, section := range report.Sections {
		fmt.Fprintf(w, "## %s [%s] %dms\n", section.Name, section.Status, section.DurationMillis)
		if section.Error != "" {
			fmt.Fprintf(w, "错误: %s\n\n", section.Error)
			continue
		}
		switch section.Name {
		case reportAllKindHealth:
			if report.Health != nil {
				printHealthReport(w, *report.Health)
			}
		case reportAllKindNetwork:
			if report.Network != nil {
				printNetworkReport(w, *report.Network)
			}
		case reportAllKindLogs:
			if report.Logs != nil {
				printLogsScanReport(w, *report.Logs)
			}
		case reportAllKindVolumes:
			if report.Volumes != nil {
				printVolumeReport(w, *report.Volumes, VolumeOptions{NoTrunc: opts.VolumeNoTrunc})
			}
		case reportAllKindPrune:
			if report.Prune != nil {
				printPruneReport(w, *report.Prune)
			}
		}
		fmt.Fprintln(w)
	}
}

func reportAllSectionCounts(sections []ReportAllSection) (ok, failed int) {
	for _, section := range sections {
		switch section.Status {
		case "ok":
			ok++
		case "failed":
			failed++
		}
	}
	return ok, failed
}
