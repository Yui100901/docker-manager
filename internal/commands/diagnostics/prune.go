package diagnostics

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"docker-manager/internal/docker"
	rpt "docker-manager/internal/report"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"
)

type pruneDockerService interface {
	DiskUsage(ctx context.Context) (types.DiskUsage, error)
	PruneContainers(ctx context.Context, pruneFilters filters.Args) (container.PruneReport, error)
	PruneImages(ctx context.Context, pruneFilters filters.Args) (image.PruneReport, error)
	PruneVolumes(ctx context.Context, pruneFilters filters.Args) (volume.PruneReport, error)
	PruneBuildCache(ctx context.Context, pruneFilters filters.Args) (*build.CachePruneReport, error)
}

var newPruneDockerService = func() (pruneDockerService, error) {
	cli, err := docker.NewClient()
	if err != nil {
		return nil, err
	}
	return &dockerPruneService{cli: cli}, nil
}

type dockerPruneService struct {
	cli *client.Client
}

type PruneReportOptions struct {
	Apply         bool
	Confirm       bool
	Only          []string
	Filters       []string
	Until         string
	ProtectLabels []string
	rpt.FormatOptions
}

type PruneReport struct {
	GeneratedAt       string               `json:"generated_at"`
	StoppedContainers []PruneContainerRef  `json:"stopped_containers,omitempty"`
	DanglingImages    []PruneImageRef      `json:"dangling_images,omitempty"`
	UnusedVolumes     []PruneVolumeRef     `json:"unused_volumes,omitempty"`
	BuildCaches       []PruneBuildCacheRef `json:"build_caches,omitempty"`
	EstimatedBytes    uint64               `json:"estimated_bytes"`
	Applied           bool                 `json:"applied"`
	Scope             PruneScope           `json:"scope"`
	ApplyResult       *PruneApplyResult    `json:"apply_result,omitempty"`
}

type PruneScope struct {
	Only          []string `json:"only,omitempty"`
	Filters       []string `json:"filters,omitempty"`
	Until         string   `json:"until,omitempty"`
	ProtectLabels []string `json:"protect_labels,omitempty"`
}

type PruneContainerRef struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Image  string `json:"image,omitempty"`
	Status string `json:"status,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

type PruneImageRef struct {
	ID       string   `json:"id"`
	RepoTags []string `json:"repo_tags,omitempty"`
	Size     int64    `json:"size,omitempty"`
}

type PruneVolumeRef struct {
	Name     string `json:"name"`
	Driver   string `json:"driver,omitempty"`
	Size     int64  `json:"size,omitempty"`
	RefCount int64  `json:"ref_count"`
}

type PruneBuildCacheRef struct {
	ID          string `json:"id"`
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
	Size        int64  `json:"size,omitempty"`
}

type PruneApplyResult struct {
	ContainersDeleted  []string `json:"containers_deleted,omitempty"`
	ImagesDeleted      []string `json:"images_deleted,omitempty"`
	VolumesDeleted     []string `json:"volumes_deleted,omitempty"`
	BuildCachesDeleted []string `json:"build_caches_deleted,omitempty"`
	SpaceReclaimed     uint64   `json:"space_reclaimed"`
}

const (
	pruneKindContainer  = "container"
	pruneKindImage      = "image"
	pruneKindVolume     = "volume"
	pruneKindBuildCache = "build-cache"
)

type pruneFilter struct {
	Key   string
	Value string
}

type pruneDockerFilters struct {
	Containers  filters.Args
	Images      filters.Args
	Volumes     filters.Args
	BuildCaches filters.Args
}

func NewPruneReportCommand() *cobra.Command {
	opts := PruneReportOptions{}
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "生成 Docker 可清理资源报告，可选执行清理",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			report, err := runPruneReport(cmd.Context(), opts)
			if err != nil {
				return fmt.Errorf("生成清理报告失败: %w", err)
			}
			return rpt.Print(cmd.OutOrStdout(), opts.Format, report, func(w io.Writer) {
				printPruneReport(w, report)
			})
		},
	}
	cmd.Flags().BoolVar(&opts.Apply, "apply", false, "根据报告执行清理")
	cmd.Flags().BoolVar(&opts.Confirm, "confirm", false, "确认执行 --apply 清理操作")
	cmd.Flags().StringArrayVar(&opts.Only, "only", nil, "只处理指定资源类型，可重复指定: container | image | volume | build-cache")
	cmd.Flags().StringArrayVarP(&opts.Filters, "filter", "f", nil, "清理筛选条件，支持 label=key、label=key=value、label!=key、until=<duration|timestamp>，可重复指定")
	cmd.Flags().StringVar(&opts.Until, "until", "", "仅清理该时间之前创建的资源，例如 24h、168h 或 RFC3339 时间")
	cmd.Flags().StringArrayVar(&opts.ProtectLabels, "protect-label", nil, "保护带有指定 label 的资源，例如 keep 或 env=prod，可重复指定")
	rpt.AddFormatFlag(cmd, &opts.Format)
	return cmd
}

func buildPruneScope(opts PruneReportOptions) (PruneScope, error) {
	only, err := normalizePruneKinds(opts.Only)
	if err != nil {
		return PruneScope{}, err
	}
	scope := PruneScope{
		Only:          only,
		Filters:       uniquePruneStrings(opts.Filters),
		Until:         strings.TrimSpace(opts.Until),
		ProtectLabels: uniquePruneStrings(opts.ProtectLabels),
	}
	if err := validatePruneScope(scope); err != nil {
		return PruneScope{}, err
	}
	return scope, nil
}

func normalizePruneKinds(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	var kinds []string
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			part = strings.ToLower(strings.TrimSpace(part))
			if part == "" {
				continue
			}
			var kind string
			switch part {
			case "container", "containers":
				kind = pruneKindContainer
			case "image", "images":
				kind = pruneKindImage
			case "volume", "volumes":
				kind = pruneKindVolume
			case "build-cache", "build-cachees", "build", "cache", "builder":
				kind = pruneKindBuildCache
			default:
				return nil, fmt.Errorf("不支持的 --only 资源类型 %q，请使用 container、image、volume 或 build-cache", part)
			}
			if !seen[kind] {
				seen[kind] = true
				kinds = append(kinds, kind)
			}
		}
	}
	sort.Strings(kinds)
	return kinds, nil
}

func validatePruneScope(scope PruneScope) error {
	if scope.Until != "" {
		if _, err := parsePruneUntil(scope.Until); err != nil {
			return err
		}
	}
	for _, filter := range scope.Filters {
		key, _, err := parsePruneFilter(filter)
		if err != nil {
			return err
		}
		switch key {
		case "label", "label!", "until":
			// ok
		default:
			return fmt.Errorf("不支持的 prune filter %q，仅支持 label=、label!= 和 until=", filter)
		}
	}
	for _, label := range scope.ProtectLabels {
		if strings.TrimSpace(label) == "" {
			return fmt.Errorf("--protect-label 不能为空")
		}
	}
	return nil
}

func (s PruneScope) includes(kind string) bool {
	if len(s.Only) == 0 {
		return true
	}
	for _, item := range s.Only {
		if item == kind {
			return true
		}
	}
	return false
}

func (s PruneScope) includesBuildCache() bool {
	if !s.includes(pruneKindBuildCache) {
		return false
	}
	return len(s.labelFilters()) == 0
}

func (s PruneScope) dockerFilters() pruneDockerFilters {
	containerFilters := filters.NewArgs()
	imageFilters := filters.NewArgs()
	volumeFilters := filters.NewArgs()
	buildFilters := filters.NewArgs()

	imageFilters.Add("dangling", "true")
	volumeFilters.Add("all", "true")

	for _, item := range s.allFilterItems() {
		switch item.Key {
		case "label", "label!":
			containerFilters.Add(item.Key, item.Value)
			imageFilters.Add(item.Key, item.Value)
			volumeFilters.Add(item.Key, item.Value)
		case "until":
			containerFilters.Add("until", item.Value)
			imageFilters.Add("until", item.Value)
			buildFilters.Add("until", item.Value)
		}
	}
	return pruneDockerFilters{
		Containers:  containerFilters,
		Images:      imageFilters,
		Volumes:     volumeFilters,
		BuildCaches: buildFilters,
	}
}

func (s PruneScope) allFilterItems() []pruneFilter {
	var result []pruneFilter
	for _, filter := range s.Filters {
		key, value, err := parsePruneFilter(filter)
		if err == nil {
			result = append(result, pruneFilter{Key: key, Value: value})
		}
	}
	if s.Until != "" {
		result = append(result, pruneFilter{Key: "until", Value: s.Until})
	}
	for _, label := range s.ProtectLabels {
		result = append(result, pruneFilter{Key: "label!", Value: label})
	}
	return result
}

func (s PruneScope) labelFilters() []pruneFilter {
	var result []pruneFilter
	for _, item := range s.allFilterItems() {
		if item.Key == "label" || item.Key == "label!" {
			result = append(result, item)
		}
	}
	return result
}

func (s PruneScope) matchesLabels(labels map[string]string) bool {
	for _, item := range s.labelFilters() {
		matches := labelSetMatches(labels, item.Value)
		switch item.Key {
		case "label":
			if !matches {
				return false
			}
		case "label!":
			if matches {
				return false
			}
		}
	}
	return true
}

func (s PruneScope) matchesCreatedUnix(created int64) bool {
	until, ok := s.untilTime()
	if !ok {
		return true
	}
	if created <= 0 {
		return false
	}
	return time.Unix(created, 0).Before(until)
}

func (s PruneScope) matchesCreatedString(created string) bool {
	until, ok := s.untilTime()
	if !ok {
		return true
	}
	created = strings.TrimSpace(created)
	if created == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339Nano, created)
	if err != nil {
		t, err = time.Parse(time.RFC3339, created)
	}
	return err == nil && t.Before(until)
}

func (s PruneScope) untilTime() (time.Time, bool) {
	value := strings.TrimSpace(s.Until)
	for _, filter := range s.Filters {
		key, parsed, err := parsePruneFilter(filter)
		if err == nil && key == "until" {
			value = parsed
		}
	}
	if value == "" {
		return time.Time{}, false
	}
	t, err := parsePruneUntil(value)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func parsePruneUntil(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, fmt.Errorf("until 不能为空")
	}
	if duration, err := time.ParseDuration(value); err == nil {
		return time.Now().Add(-duration), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("无法解析 until=%q，请使用 duration（如 24h）或 RFC3339 时间", value)
}

func parsePruneFilter(filter string) (string, string, error) {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return "", "", fmt.Errorf("--filter 不能为空")
	}
	if strings.HasPrefix(filter, "label!=") {
		value := strings.TrimSpace(strings.TrimPrefix(filter, "label!="))
		if value == "" {
			return "", "", fmt.Errorf("label!= 的值不能为空")
		}
		return "label!", value, nil
	}
	if strings.HasPrefix(filter, "label=") {
		value := strings.TrimSpace(strings.TrimPrefix(filter, "label="))
		if value == "" {
			return "", "", fmt.Errorf("label= 的值不能为空")
		}
		return "label", value, nil
	}
	if strings.HasPrefix(filter, "until=") {
		value := strings.TrimSpace(strings.TrimPrefix(filter, "until="))
		if _, err := parsePruneUntil(value); err != nil {
			return "", "", err
		}
		return "until", value, nil
	}
	return "", "", fmt.Errorf("不支持的 prune filter %q，仅支持 label=、label!= 和 until=", filter)
}

func labelSetMatches(labels map[string]string, expr string) bool {
	key, value, hasValue := strings.Cut(expr, "=")
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if key == "" {
		return false
	}
	actual, ok := labels[key]
	if !ok {
		return false
	}
	if !hasValue {
		return true
	}
	return actual == value
}

func uniquePruneStrings(values []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func runPruneReport(ctx context.Context, opts PruneReportOptions) (PruneReport, error) {
	scope, err := buildPruneScope(opts)
	if err != nil {
		return PruneReport{}, err
	}
	if opts.Apply && !opts.Confirm {
		return PruneReport{}, fmt.Errorf("report prune --apply 会删除 Docker 资源；如确认执行，请添加 --confirm")
	}
	svc, err := newPruneDockerService()
	if err != nil {
		return PruneReport{}, err
	}
	usage, err := svc.DiskUsage(ctx)
	if err != nil {
		return PruneReport{}, err
	}

	report := buildPruneReport(usage, scope)
	if opts.Apply {
		applyResult, err := applyPruneReport(ctx, svc, scope)
		if err != nil {
			return report, err
		}
		report.Applied = true
		report.ApplyResult = &applyResult
	}
	return report, nil
}

func buildPruneReport(usage types.DiskUsage, scope PruneScope) PruneReport {
	report := PruneReport{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Scope:       scope,
	}
	for _, c := range usage.Containers {
		if c == nil || c.State == "running" {
			continue
		}
		if !scope.includes(pruneKindContainer) || !scope.matchesLabels(c.Labels) || !scope.matchesCreatedUnix(c.Created) {
			continue
		}
		report.StoppedContainers = append(report.StoppedContainers, PruneContainerRef{
			ID:     shortID(c.ID),
			Name:   firstContainerName(c.Names),
			Image:  c.Image,
			Status: c.Status,
			Size:   c.SizeRw,
		})
		addPositiveSize(&report.EstimatedBytes, c.SizeRw)
	}
	for _, img := range usage.Images {
		if img == nil || !isDanglingImage(img) {
			continue
		}
		if !scope.includes(pruneKindImage) || !scope.matchesLabels(img.Labels) || !scope.matchesCreatedUnix(img.Created) {
			continue
		}
		report.DanglingImages = append(report.DanglingImages, PruneImageRef{
			ID:       shortID(img.ID),
			RepoTags: cleanRepoTags(img.RepoTags),
			Size:     img.Size,
		})
		addPositiveSize(&report.EstimatedBytes, img.Size)
	}
	for _, vol := range usage.Volumes {
		if vol == nil || vol.UsageData == nil || vol.UsageData.RefCount != 0 {
			continue
		}
		if !scope.includes(pruneKindVolume) || !scope.matchesLabels(vol.Labels) || !scope.matchesCreatedString(vol.CreatedAt) {
			continue
		}
		report.UnusedVolumes = append(report.UnusedVolumes, PruneVolumeRef{
			Name:     vol.Name,
			Driver:   vol.Driver,
			Size:     vol.UsageData.Size,
			RefCount: vol.UsageData.RefCount,
		})
		addPositiveSize(&report.EstimatedBytes, vol.UsageData.Size)
	}
	for _, cache := range usage.BuildCache {
		if cache == nil || cache.InUse {
			continue
		}
		if !scope.includesBuildCache() {
			continue
		}
		report.BuildCaches = append(report.BuildCaches, PruneBuildCacheRef{
			ID:          shortID(cache.ID),
			Type:        cache.Type,
			Description: cache.Description,
			Size:        cache.Size,
		})
		addPositiveSize(&report.EstimatedBytes, cache.Size)
	}
	sortPruneReport(&report)
	return report
}

func applyPruneReport(ctx context.Context, svc pruneDockerService, scope PruneScope) (PruneApplyResult, error) {
	var result PruneApplyResult
	pruneFilters := scope.dockerFilters()

	if scope.includes(pruneKindContainer) {
		containers, err := svc.PruneContainers(ctx, pruneFilters.Containers)
		if err != nil {
			return result, fmt.Errorf("prune containers: %w", err)
		}
		result.ContainersDeleted = containers.ContainersDeleted
		result.SpaceReclaimed += containers.SpaceReclaimed
	}

	if scope.includes(pruneKindImage) {
		images, err := svc.PruneImages(ctx, pruneFilters.Images)
		if err != nil {
			return result, fmt.Errorf("prune images: %w", err)
		}
		result.ImagesDeleted = imageDeleteRefs(images.ImagesDeleted)
		result.SpaceReclaimed += images.SpaceReclaimed
	}

	if scope.includes(pruneKindVolume) {
		volumes, err := svc.PruneVolumes(ctx, pruneFilters.Volumes)
		if err != nil {
			return result, fmt.Errorf("prune volumes: %w", err)
		}
		result.VolumesDeleted = volumes.VolumesDeleted
		result.SpaceReclaimed += volumes.SpaceReclaimed
	}

	if scope.includesBuildCache() {
		caches, err := svc.PruneBuildCache(ctx, pruneFilters.BuildCaches)
		if err != nil {
			return result, fmt.Errorf("prune build cache: %w", err)
		}
		if caches != nil {
			result.BuildCachesDeleted = caches.CachesDeleted
			result.SpaceReclaimed += caches.SpaceReclaimed
		}
	}

	sort.Strings(result.ContainersDeleted)
	sort.Strings(result.ImagesDeleted)
	sort.Strings(result.VolumesDeleted)
	sort.Strings(result.BuildCachesDeleted)
	return result, nil
}

func printPruneReport(w io.Writer, report PruneReport) {
	fmt.Fprintf(w, "Docker 清理报告 (%s)\n", report.GeneratedAt)
	printPruneScope(w, report.Scope)
	fmt.Fprintf(w, "预计可回收空间: %s\n\n", humanBytes(report.EstimatedBytes))

	printPruneSection(w, "已停止容器", len(report.StoppedContainers), func() {
		for _, c := range report.StoppedContainers {
			fmt.Fprintf(w, "  - %s %s image=%s size=%s status=%s\n", c.ID, c.Name, c.Image, humanBytes(uint64FromInt64(c.Size)), c.Status)
		}
	})
	printPruneSection(w, "悬空镜像", len(report.DanglingImages), func() {
		for _, img := range report.DanglingImages {
			fmt.Fprintf(w, "  - %s size=%s tags=%s\n", img.ID, humanBytes(uint64FromInt64(img.Size)), strings.Join(img.RepoTags, ","))
		}
	})
	printPruneSection(w, "未使用 volume", len(report.UnusedVolumes), func() {
		for _, vol := range report.UnusedVolumes {
			fmt.Fprintf(w, "  - %s driver=%s size=%s\n", vol.Name, vol.Driver, humanBytes(uint64FromInt64(vol.Size)))
		}
	})
	printPruneSection(w, "构建缓存", len(report.BuildCaches), func() {
		for _, cache := range report.BuildCaches {
			fmt.Fprintf(w, "  - %s type=%s size=%s %s\n", cache.ID, cache.Type, humanBytes(uint64FromInt64(cache.Size)), cache.Description)
		}
	})

	if report.Applied && report.ApplyResult != nil {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "执行结果:")
		fmt.Fprintf(w, "  已删除容器: %d\n", len(report.ApplyResult.ContainersDeleted))
		fmt.Fprintf(w, "  已删除/取消标记镜像: %d\n", len(report.ApplyResult.ImagesDeleted))
		fmt.Fprintf(w, "  已删除 volume: %d\n", len(report.ApplyResult.VolumesDeleted))
		fmt.Fprintf(w, "  已删除构建缓存: %d\n", len(report.ApplyResult.BuildCachesDeleted))
		fmt.Fprintf(w, "  已回收空间: %s\n", humanBytes(report.ApplyResult.SpaceReclaimed))
	}
}

func printPruneScope(w io.Writer, scope PruneScope) {
	var parts []string
	if len(scope.Only) > 0 {
		parts = append(parts, "only="+strings.Join(scope.Only, ","))
	}
	if len(scope.Filters) > 0 {
		parts = append(parts, "filter="+strings.Join(scope.Filters, ","))
	}
	if scope.Until != "" {
		parts = append(parts, "until="+scope.Until)
	}
	if len(scope.ProtectLabels) > 0 {
		parts = append(parts, "protect-label="+strings.Join(scope.ProtectLabels, ","))
	}
	if len(parts) == 0 {
		fmt.Fprintln(w, "范围: 全部可清理资源")
		return
	}
	fmt.Fprintf(w, "范围: %s\n", strings.Join(parts, " "))
}

func printPruneSection(w io.Writer, title string, count int, printItems func()) {
	fmt.Fprintf(w, "%s: %d\n", title, count)
	if count > 0 {
		printItems()
	}
}

func sortPruneReport(report *PruneReport) {
	sort.Slice(report.StoppedContainers, func(i, j int) bool {
		return report.StoppedContainers[i].Name < report.StoppedContainers[j].Name
	})
	sort.Slice(report.DanglingImages, func(i, j int) bool {
		return report.DanglingImages[i].ID < report.DanglingImages[j].ID
	})
	sort.Slice(report.UnusedVolumes, func(i, j int) bool {
		return report.UnusedVolumes[i].Name < report.UnusedVolumes[j].Name
	})
	sort.Slice(report.BuildCaches, func(i, j int) bool {
		return report.BuildCaches[i].ID < report.BuildCaches[j].ID
	})
}

func firstContainerName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[0], "/")
}

func isDanglingImage(img *image.Summary) bool {
	if len(img.RepoTags) == 0 {
		return true
	}
	for _, tag := range img.RepoTags {
		if tag != "" && tag != "<none>:<none>" {
			return false
		}
	}
	return true
}

func cleanRepoTags(tags []string) []string {
	var cleaned []string
	for _, tag := range tags {
		if tag == "" {
			continue
		}
		cleaned = append(cleaned, tag)
	}
	return cleaned
}

func imageDeleteRefs(items []image.DeleteResponse) []string {
	refs := make([]string, 0, len(items))
	for _, item := range items {
		if item.Deleted != "" {
			refs = append(refs, item.Deleted)
		}
		if item.Untagged != "" {
			refs = append(refs, item.Untagged)
		}
	}
	return refs
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func addPositiveSize(total *uint64, size int64) {
	if size > 0 {
		*total += uint64(size)
	}
}

func uint64FromInt64(size int64) uint64 {
	if size <= 0 {
		return 0
	}
	return uint64(size)
}

func humanBytes(size uint64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	value := float64(size)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PiB", value/unit)
}

func (s *dockerPruneService) DiskUsage(ctx context.Context) (types.DiskUsage, error) {
	return s.cli.DiskUsage(ctx, types.DiskUsageOptions{})
}

func (s *dockerPruneService) PruneContainers(ctx context.Context, pruneFilters filters.Args) (container.PruneReport, error) {
	return s.cli.ContainersPrune(ctx, pruneFilters)
}

func (s *dockerPruneService) PruneImages(ctx context.Context, pruneFilters filters.Args) (image.PruneReport, error) {
	return s.cli.ImagesPrune(ctx, pruneFilters)
}

func (s *dockerPruneService) PruneVolumes(ctx context.Context, pruneFilters filters.Args) (volume.PruneReport, error) {
	return s.cli.VolumesPrune(ctx, pruneFilters)
}

func (s *dockerPruneService) PruneBuildCache(ctx context.Context, pruneFilters filters.Args) (*build.CachePruneReport, error) {
	return s.cli.BuildCachePrune(ctx, build.CachePruneOptions{All: true, Filters: pruneFilters})
}
