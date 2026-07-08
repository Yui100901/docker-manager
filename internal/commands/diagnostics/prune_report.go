package diagnostics

import (
	"context"
	"fmt"
	"time"

	"docker-manager/internal/docker"

	mobyclient "github.com/moby/moby/client"
)

func pruneDiskUsageOptions(scope PruneScope) mobyclient.DiskUsageOptions {
	if len(scope.Only) == 0 {
		return mobyclient.DiskUsageOptions{}
	}
	return mobyclient.DiskUsageOptions{
		Containers: scope.includes(pruneKindContainer),
		Images:     scope.includes(pruneKindImage),
		Volumes:    scope.includes(pruneKindVolume),
		BuildCache: scope.includes(pruneKindBuildCache),
	}
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
