package diagnostics

import (
	"io"

	"docker-manager/internal/docker"
	"docker-manager/internal/resourcefilter"
	"docker-manager/internal/targets"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/volume"
)

type TargetSelection = targets.ContainerSelection

func buildContainerTargetSelection(action string, count int, running bool, filters []string) TargetSelection {
	return targets.BuildContainerSelection(action, count, running, filters)
}

func printTargetSelection(w io.Writer, target TargetSelection) {
	if target.Message != "" {
		_, _ = io.WriteString(w, "目标: "+target.Message+"\n")
	}
}

func matchesTargetFilters(candidates []string, filters []string) bool {
	keys := append([]string{}, resourcefilter.ContainerKeys...)
	keys = append(keys, resourcefilter.VolumeKeys...)
	return resourcefilter.Match(candidates, filters, keys...)
}

func filterContainerSummaries(containers []container.Summary, filters []string) []container.Summary {
	return targets.FilterContainers(containers, filters)
}

func filterVolumesByPatterns(volumes []volume.Volume, filters []string) []volume.Volume {
	if len(filters) == 0 {
		return volumes
	}
	var filtered []volume.Volume
	for _, vol := range volumes {
		converted, err := docker.ConvertDockerType[volume.Volume](vol)
		if err != nil {
			continue
		}
		if resourcefilter.Match(resourcefilter.VolumeCandidates(&converted), filters, resourcefilter.VolumeKeys...) {
			filtered = append(filtered, vol)
		}
	}
	return filtered
}

func normalizeContainerName(name string) string {
	return resourcefilter.NormalizeContainerName(name)
}
