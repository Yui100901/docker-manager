package diagnostics

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"docker-manager/internal/docker"
	"docker-manager/internal/resourcefilter"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
	mobycontainer "github.com/moby/moby/api/types/container"
	mobyvolume "github.com/moby/moby/api/types/volume"
)

type TargetSelection struct {
	Count      int      `json:"count"`
	DefaultAll bool     `json:"default_all"`
	Running    bool     `json:"running"`
	Filters    []string `json:"filters,omitempty"`
	Message    string   `json:"message"`
}

func buildContainerTargetSelection(action string, count int, running bool, filters []string) TargetSelection {
	target := TargetSelection{
		Count:      count,
		DefaultAll: len(filters) == 0 && !running,
		Running:    running,
		Filters:    append([]string(nil), filters...),
	}
	switch {
	case target.DefaultAll:
		target.Message = fmt.Sprintf("未指定容器筛选，默认%s全部本地容器 %d 个", action, count)
	case running && len(filters) == 0:
		target.Message = fmt.Sprintf("仅%s运行中容器 %d 个", action, count)
	case running:
		target.Message = fmt.Sprintf("在运行中容器内按筛选条件 %q 选中 %d 个", strings.Join(filters, ", "), count)
	default:
		target.Message = fmt.Sprintf("按筛选条件 %q 选中 %d 个容器", strings.Join(filters, ", "), count)
	}
	return target
}

func printTargetSelection(w io.Writer, target TargetSelection) {
	if target.Message != "" {
		fmt.Fprintf(w, "目标: %s\n", target.Message)
	}
}

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
		converted, err := docker.ConvertDockerType[mobycontainer.Summary](c)
		if err != nil {
			continue
		}
		if resourcefilter.Match(resourcefilter.ContainerCandidates(converted), filters, resourcefilter.ContainerKeys...) {
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
		converted, err := docker.ConvertDockerType[mobyvolume.Volume](*vol)
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
