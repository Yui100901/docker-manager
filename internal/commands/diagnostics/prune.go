package diagnostics

import (
	"context"
	"docker-manager/internal/commandflags"
	"docker-manager/internal/docker"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	rpt "docker-manager/internal/report"

	"github.com/moby/moby/api/types/image"
	mobyclient "github.com/moby/moby/client"
	"github.com/spf13/cobra"
)

const (
	pruneKindContainer  = "container"
	pruneKindImage      = "image"
	pruneKindVolume     = "volume"
	pruneKindBuildCache = "build-cache"
)

func NewPruneReportCommand() *cobra.Command {
	opts := PruneReportOptions{}
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "生成 Docker 可清理资源报告，可选执行清理",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.Apply && docker.IsRemoteEndpoint() {
				fmt.Fprintf(cmd.OutOrStdout(), "Target Docker: %s\n", docker.Endpoint())
			}
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
	commandflags.AddReportFormatFlag(cmd, &opts.Format)
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
	containerFilters := make(mobyclient.Filters)
	imageFilters := make(mobyclient.Filters)
	volumeFilters := make(mobyclient.Filters)
	buildFilters := make(mobyclient.Filters)

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
	if err := ctx.Err(); err != nil {
		return PruneReport{}, err
	}
	scope, err := buildPruneScope(opts)
	if err != nil {
		return PruneReport{}, err
	}
	// Keep destructive cleanup behind an explicit confirmation even when the
	// report scope is narrow; dry-run/report output remains the default path.
	if opts.Apply && !opts.Confirm {
		message := "report prune --apply 会删除 Docker 资源"
		if docker.IsRemoteEndpoint() {
			message += "；目标 Docker: " + docker.Endpoint()
		}
		return PruneReport{}, fmt.Errorf("%s；如确认执行，请添加 --confirm", message)
	}
	svc, err := newPruneDockerService()
	if err != nil {
		return PruneReport{}, err
	}
	usage, err := svc.DiskUsage(ctx)
	if err != nil {
		return PruneReport{}, err
	}
	if err := ctx.Err(); err != nil {
		return PruneReport{}, err
	}

	var volumeRefs map[string][]VolumeContainerRef
	var volumeWarnings []string
	if scope.includes(pruneKindVolume) && len(usage.Volumes) > 0 {
		volumeRefs, volumeWarnings = inspectPruneVolumeRefs(ctx, svc)
	}

	report, err := buildPruneReportWithVolumeRefs(ctx, usage, scope, volumeRefs, volumeWarnings)
	if err != nil {
		return report, err
	}
	if opts.Apply {
		if err := ensurePruneVolumeCandidatesStillUnreferenced(ctx, svc, report.UnusedVolumes); err != nil {
			return report, err
		}
		applyResult, err := applyPruneReport(ctx, svc, scope)
		if err != nil {
			return report, err
		}
		report.Applied = true
		report.ApplyResult = &applyResult
	}
	return report, nil
}

func buildPruneReport(usage pruneDiskUsage, scope PruneScope) PruneReport {
	report, _ := buildPruneReportWithContext(context.Background(), usage, scope)
	return report
}

func buildPruneReportWithContext(ctx context.Context, usage pruneDiskUsage, scope PruneScope) (PruneReport, error) {
	return buildPruneReportWithVolumeRefs(ctx, usage, scope, nil, nil)
}

func buildPruneReportWithVolumeRefs(ctx context.Context, usage pruneDiskUsage, scope PruneScope, volumeRefs map[string][]VolumeContainerRef, warnings []string) (PruneReport, error) {
	report := PruneReport{
		GeneratedAt:    time.Now().Format(time.RFC3339),
		DockerEndpoint: docker.Endpoint(),
		Scope:          scope,
		Warnings:       append([]string(nil), warnings...),
	}
	for _, c := range usage.Containers {
		if err := ctx.Err(); err != nil {
			return report, err
		}
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
		if err := ctx.Err(); err != nil {
			return report, err
		}
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
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if vol == nil || vol.UsageData == nil || vol.UsageData.RefCount != 0 {
			continue
		}
		if !scope.includes(pruneKindVolume) || !scope.matchesLabels(vol.Labels) || !scope.matchesCreatedString(vol.CreatedAt) {
			continue
		}
		if refs := volumeRefs[vol.Name]; len(refs) > 0 {
			report.Warnings = append(report.Warnings, fmt.Sprintf("volume %s 的 DiskUsage refcount=0，但 inspect 发现仍被 %d 个容器引用，已跳过清理候选", vol.Name, len(refs)))
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
		if err := ctx.Err(); err != nil {
			return report, err
		}
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
	if err := ctx.Err(); err != nil {
		return report, err
	}
	sortPruneReport(&report)
	return report, nil
}

func inspectPruneVolumeRefs(ctx context.Context, svc pruneDockerService) (map[string][]VolumeContainerRef, []string) {
	if err := ctx.Err(); err != nil {
		return nil, []string{fmt.Sprintf("volume 引用复核已取消: %v", err)}
	}
	containers, err := svc.ListContainers(ctx, true)
	if err != nil {
		return nil, []string{fmt.Sprintf("无法列出容器复核 volume 引用，已仅使用 Docker DiskUsage: %v", err)}
	}
	return inspectVolumeContainerRefs(ctx, svc, containers)
}

func ensurePruneVolumeCandidatesStillUnreferenced(ctx context.Context, svc pruneDockerService, candidates []PruneVolumeRef) error {
	if len(candidates) == 0 {
		return nil
	}
	refsByVolume, warnings := inspectPruneVolumeRefs(ctx, svc)
	for _, warning := range warnings {
		if strings.Contains(warning, "无法列出容器") || strings.Contains(warning, "已取消") {
			return fmt.Errorf("执行 volume prune 前复核引用失败: %s", warning)
		}
	}
	for _, candidate := range candidates {
		if refs := refsByVolume[candidate.Name]; len(refs) > 0 {
			return fmt.Errorf("拒绝执行 volume prune: volume %s 在执行前复核中仍被 %d 个容器引用", candidate.Name, len(refs))
		}
	}
	return nil
}

func applyPruneReport(ctx context.Context, svc pruneDockerService, scope PruneScope) (PruneApplyResult, error) {
	var result PruneApplyResult
	pruneFilters := scope.dockerFilters()

	if err := ctx.Err(); err != nil {
		return result, err
	}
	if scope.includes(pruneKindContainer) {
		containers, err := svc.PruneContainers(ctx, pruneFilters.Containers)
		if err != nil {
			return result, fmt.Errorf("prune containers: %w", err)
		}
		result.ContainersDeleted = containers.ContainersDeleted
		result.SpaceReclaimed += containers.SpaceReclaimed
	}

	if err := ctx.Err(); err != nil {
		return result, err
	}
	if scope.includes(pruneKindImage) {
		images, err := svc.PruneImages(ctx, pruneFilters.Images)
		if err != nil {
			return result, fmt.Errorf("prune images: %w", err)
		}
		result.ImagesDeleted = imageDeleteRefs(images.ImagesDeleted)
		result.SpaceReclaimed += images.SpaceReclaimed
	}

	if err := ctx.Err(); err != nil {
		return result, err
	}
	if scope.includes(pruneKindVolume) {
		volumes, err := svc.PruneVolumes(ctx, pruneFilters.Volumes)
		if err != nil {
			return result, fmt.Errorf("prune volumes: %w", err)
		}
		result.VolumesDeleted = volumes.VolumesDeleted
		result.SpaceReclaimed += volumes.SpaceReclaimed
	}

	if err := ctx.Err(); err != nil {
		return result, err
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

	if err := ctx.Err(); err != nil {
		return result, err
	}
	sort.Strings(result.ContainersDeleted)
	sort.Strings(result.ImagesDeleted)
	sort.Strings(result.VolumesDeleted)
	sort.Strings(result.BuildCachesDeleted)
	return result, nil
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
