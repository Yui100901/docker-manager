package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"docker-manager/docker"

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
	PruneContainers(ctx context.Context) (container.PruneReport, error)
	PruneImages(ctx context.Context) (image.PruneReport, error)
	PruneVolumes(ctx context.Context) (volume.PruneReport, error)
	PruneBuildCache(ctx context.Context) (*build.CachePruneReport, error)
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
	Apply bool
}

type PruneReport struct {
	GeneratedAt       string               `json:"generated_at"`
	StoppedContainers []PruneContainerRef  `json:"stopped_containers,omitempty"`
	DanglingImages    []PruneImageRef      `json:"dangling_images,omitempty"`
	UnusedVolumes     []PruneVolumeRef     `json:"unused_volumes,omitempty"`
	BuildCaches       []PruneBuildCacheRef `json:"build_caches,omitempty"`
	EstimatedBytes    uint64               `json:"estimated_bytes"`
	Applied           bool                 `json:"applied"`
	ApplyResult       *PruneApplyResult    `json:"apply_result,omitempty"`
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

func newPruneReportCommand() *cobra.Command {
	opts := PruneReportOptions{}
	cmd := &cobra.Command{
		Use:   "prune-report",
		Short: "生成 Docker 可清理资源报告，可选执行清理",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			report, err := runPruneReport(cmd.Context(), opts)
			if err != nil {
				return fmt.Errorf("prune report failed: %w", err)
			}
			printPruneReport(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().BoolVar(&opts.Apply, "apply", false, "根据报告执行清理")
	return cmd
}

func runPruneReport(ctx context.Context, opts PruneReportOptions) (PruneReport, error) {
	svc, err := newPruneDockerService()
	if err != nil {
		return PruneReport{}, err
	}
	usage, err := svc.DiskUsage(ctx)
	if err != nil {
		return PruneReport{}, err
	}

	report := buildPruneReport(usage)
	if opts.Apply {
		applyResult, err := applyPruneReport(ctx, svc)
		if err != nil {
			return report, err
		}
		report.Applied = true
		report.ApplyResult = &applyResult
	}
	return report, nil
}

func buildPruneReport(usage types.DiskUsage) PruneReport {
	report := PruneReport{
		GeneratedAt: time.Now().Format(time.RFC3339),
	}
	for _, c := range usage.Containers {
		if c == nil || c.State == "running" {
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

func applyPruneReport(ctx context.Context, svc pruneDockerService) (PruneApplyResult, error) {
	var result PruneApplyResult

	containers, err := svc.PruneContainers(ctx)
	if err != nil {
		return result, fmt.Errorf("prune containers: %w", err)
	}
	result.ContainersDeleted = containers.ContainersDeleted
	result.SpaceReclaimed += containers.SpaceReclaimed

	images, err := svc.PruneImages(ctx)
	if err != nil {
		return result, fmt.Errorf("prune images: %w", err)
	}
	result.ImagesDeleted = imageDeleteRefs(images.ImagesDeleted)
	result.SpaceReclaimed += images.SpaceReclaimed

	volumes, err := svc.PruneVolumes(ctx)
	if err != nil {
		return result, fmt.Errorf("prune volumes: %w", err)
	}
	result.VolumesDeleted = volumes.VolumesDeleted
	result.SpaceReclaimed += volumes.SpaceReclaimed

	caches, err := svc.PruneBuildCache(ctx)
	if err != nil {
		return result, fmt.Errorf("prune build cache: %w", err)
	}
	if caches != nil {
		result.BuildCachesDeleted = caches.CachesDeleted
		result.SpaceReclaimed += caches.SpaceReclaimed
	}

	sort.Strings(result.ContainersDeleted)
	sort.Strings(result.ImagesDeleted)
	sort.Strings(result.VolumesDeleted)
	sort.Strings(result.BuildCachesDeleted)
	return result, nil
}

func printPruneReport(w io.Writer, report PruneReport) {
	fmt.Fprintf(w, "Docker prune report (%s)\n", report.GeneratedAt)
	fmt.Fprintf(w, "Estimated reclaimable: %s\n\n", humanBytes(report.EstimatedBytes))

	printPruneSection(w, "Stopped containers", len(report.StoppedContainers), func() {
		for _, c := range report.StoppedContainers {
			fmt.Fprintf(w, "  - %s %s image=%s size=%s status=%s\n", c.ID, c.Name, c.Image, humanBytes(uint64FromInt64(c.Size)), c.Status)
		}
	})
	printPruneSection(w, "Dangling images", len(report.DanglingImages), func() {
		for _, img := range report.DanglingImages {
			fmt.Fprintf(w, "  - %s size=%s tags=%s\n", img.ID, humanBytes(uint64FromInt64(img.Size)), strings.Join(img.RepoTags, ","))
		}
	})
	printPruneSection(w, "Unused volumes", len(report.UnusedVolumes), func() {
		for _, vol := range report.UnusedVolumes {
			fmt.Fprintf(w, "  - %s driver=%s size=%s\n", vol.Name, vol.Driver, humanBytes(uint64FromInt64(vol.Size)))
		}
	})
	printPruneSection(w, "Build cache", len(report.BuildCaches), func() {
		for _, cache := range report.BuildCaches {
			fmt.Fprintf(w, "  - %s type=%s size=%s %s\n", cache.ID, cache.Type, humanBytes(uint64FromInt64(cache.Size)), cache.Description)
		}
	})

	if report.Applied && report.ApplyResult != nil {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Apply result:")
		fmt.Fprintf(w, "  containers deleted: %d\n", len(report.ApplyResult.ContainersDeleted))
		fmt.Fprintf(w, "  images deleted/untagged: %d\n", len(report.ApplyResult.ImagesDeleted))
		fmt.Fprintf(w, "  volumes deleted: %d\n", len(report.ApplyResult.VolumesDeleted))
		fmt.Fprintf(w, "  build cache deleted: %d\n", len(report.ApplyResult.BuildCachesDeleted))
		fmt.Fprintf(w, "  space reclaimed: %s\n", humanBytes(report.ApplyResult.SpaceReclaimed))
	}
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

func (s *dockerPruneService) PruneContainers(ctx context.Context) (container.PruneReport, error) {
	return s.cli.ContainersPrune(ctx, filters.NewArgs())
}

func (s *dockerPruneService) PruneImages(ctx context.Context) (image.PruneReport, error) {
	f := filters.NewArgs()
	f.Add("dangling", "true")
	return s.cli.ImagesPrune(ctx, f)
}

func (s *dockerPruneService) PruneVolumes(ctx context.Context) (volume.PruneReport, error) {
	f := filters.NewArgs()
	f.Add("all", "true")
	return s.cli.VolumesPrune(ctx, f)
}

func (s *dockerPruneService) PruneBuildCache(ctx context.Context) (*build.CachePruneReport, error) {
	return s.cli.BuildCachePrune(ctx, build.CachePruneOptions{All: true})
}
