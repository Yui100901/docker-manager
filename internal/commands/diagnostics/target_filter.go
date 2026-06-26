package diagnostics

import (
	"sort"

	"docker-manager/internal/resourcefilter"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
)

func matchesTargetFilters(candidates []string, filters []string) bool {
	keys := append([]string{}, resourcefilter.ContainerKeys...)
	keys = append(keys, resourcefilter.VolumeKeys...)
	return resourcefilter.Match(candidates, filters, keys...)
}

func filterContainerSummaries(containers []container.Summary, filters []string) []container.Summary {
	if len(filters) == 0 {
		return containers
	}
	var filtered []container.Summary
	for _, c := range containers {
		if resourcefilter.Match(resourcefilter.ContainerCandidates(c), filters, resourcefilter.ContainerKeys...) {
			filtered = append(filtered, c)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		return firstContainerName(filtered[i].Names) < firstContainerName(filtered[j].Names)
	})
	return filtered
}

func filterVolumesByPatterns(volumes []*volume.Volume, filters []string) []*volume.Volume {
	if len(filters) == 0 {
		return volumes
	}
	var filtered []*volume.Volume
	for _, vol := range volumes {
		if vol == nil {
			continue
		}
		if resourcefilter.Match(resourcefilter.VolumeCandidates(vol), filters, resourcefilter.VolumeKeys...) {
			filtered = append(filtered, vol)
		}
	}
	return filtered
}

func normalizeContainerName(name string) string {
	return resourcefilter.NormalizeContainerName(name)
}
