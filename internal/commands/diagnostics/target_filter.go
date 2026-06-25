package diagnostics

import (
	"regexp"
	"sort"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
)

func matchesTargetFilters(candidates []string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	for _, filter := range filters {
		filter = strings.TrimSpace(filter)
		if filter == "" {
			continue
		}
		for _, candidate := range candidates {
			if candidate == "" {
				continue
			}
			if wildcardMatch(filter, candidate) || candidate == filter || strings.HasPrefix(candidate, filter) {
				return true
			}
		}
	}
	return false
}

func filterContainerSummaries(containers []container.Summary, filters []string) []container.Summary {
	if len(filters) == 0 {
		return containers
	}
	var filtered []container.Summary
	for _, c := range containers {
		if matchesTargetFilters(containerFilterCandidates(c), filters) {
			filtered = append(filtered, c)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		return firstContainerName(filtered[i].Names) < firstContainerName(filtered[j].Names)
	})
	return filtered
}

func containerFilterCandidates(c container.Summary) []string {
	candidates := []string{
		c.ID,
		strings.TrimPrefix(c.ID, "sha256:"),
		c.Image,
		string(c.State),
	}
	if short := shortID(c.ID); short != "" && short != c.ID {
		candidates = append(candidates, short)
	}
	for _, name := range c.Names {
		name = strings.TrimPrefix(name, "/")
		candidates = append(candidates, name)
	}
	if name := firstContainerName(c.Names); name != "" {
		candidates = append(candidates, name)
	}
	return uniqueNonEmptyStrings(candidates)
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
		if matchesTargetFilters(volumeFilterCandidates(vol), filters) {
			filtered = append(filtered, vol)
		}
	}
	return filtered
}

func volumeFilterCandidates(vol *volume.Volume) []string {
	candidates := []string{vol.Name, vol.Driver, vol.Mountpoint, vol.Scope}
	for key, value := range vol.Labels {
		candidates = append(candidates, key, value, key+"="+value)
	}
	for key, value := range vol.Options {
		candidates = append(candidates, key, value, key+"="+value)
	}
	return uniqueNonEmptyStrings(candidates)
}

func uniqueNonEmptyStrings(values []string) []string {
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

func normalizeContainerName(name string) string {
	return strings.TrimPrefix(strings.TrimSpace(name), "/")
}

func wildcardMatch(pattern, value string) bool {
	re, err := regexp.Compile("^" + wildcardToRegex(pattern) + "$")
	if err != nil {
		return false
	}
	return re.MatchString(value)
}

func wildcardToRegex(pattern string) string {
	var sb strings.Builder
	for _, r := range pattern {
		switch r {
		case '*':
			sb.WriteString(".*")
		case '?':
			sb.WriteByte('.')
		default:
			sb.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	return sb.String()
}
