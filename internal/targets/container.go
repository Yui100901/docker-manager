package targets

import (
	"sort"
	"strings"

	"docker-manager/internal/resourcefilter"

	"github.com/moby/moby/api/types/container"
)

func RunningContainers(containers []container.Summary) []container.Summary {
	running := make([]container.Summary, 0, len(containers))
	for _, c := range containers {
		if c.State == "running" {
			running = append(running, c)
		}
	}
	return running
}

func FilterContainers(containers []container.Summary, filters []string) []container.Summary {
	filtered := containers
	if len(filters) > 0 {
		filtered = make([]container.Summary, 0, len(containers))
		for _, c := range containers {
			if resourcefilter.Match(resourcefilter.ContainerCandidates(c), filters, resourcefilter.ContainerKeys...) {
				filtered = append(filtered, c)
			}
		}
	}
	SortContainers(filtered)
	return filtered
}

func SortContainers(containers []container.Summary) {
	sort.Slice(containers, func(i, j int) bool {
		return ContainerDisplayName(containers[i]) < ContainerDisplayName(containers[j])
	})
}

func ContainerNames(containers []container.Summary) []string {
	names := make([]string, 0, len(containers))
	for _, c := range containers {
		name := ContainerDisplayName(c)
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func ContainerDisplayName(c container.Summary) string {
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	return c.ID
}

func ContainerMatchesFilter(c container.Summary, filter string) bool {
	return resourcefilter.Match(resourcefilter.ContainerCandidates(c), []string{filter}, resourcefilter.ContainerKeys...)
}

func ContainerMatchesFilters(c container.Summary, filters []string) bool {
	return resourcefilter.Match(resourcefilter.ContainerCandidates(c), filters, resourcefilter.ContainerKeys...)
}
