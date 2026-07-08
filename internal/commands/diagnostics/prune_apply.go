package diagnostics

import (
	"context"
	"fmt"
	"sort"
)

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
