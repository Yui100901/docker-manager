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
		if matchesOneTargetFilter(candidates, filter) {
			return true
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
	cleanID := strings.TrimPrefix(c.ID, "sha256:")
	candidates := []string{
		c.ID,
		cleanID,
		"id:" + c.ID,
		"id:" + cleanID,
		c.Image,
		"image:" + c.Image,
		string(c.State),
		"state:" + string(c.State),
		c.Status,
		"status:" + c.Status,
	}
	if short := shortID(c.ID); short != "" && short != cleanID {
		candidates = append(candidates, short, "id:"+short)
	}
	for _, name := range c.Names {
		name = strings.TrimPrefix(name, "/")
		candidates = append(candidates, name, "name:"+name)
	}
	if name := firstContainerName(c.Names); name != "" {
		candidates = append(candidates, name, "name:"+name)
	}
	if c.Image != "" {
		repo, tag := splitRepoTag(c.Image)
		if repo != "" {
			candidates = append(candidates, repo, "image:"+repo)
			if slash := strings.Index(repo, "/"); slash >= 0 && slash < len(repo)-1 {
				candidates = append(candidates, repo[slash+1:], "image:"+repo[slash+1:])
			}
			if slash := strings.LastIndex(repo, "/"); slash >= 0 && slash < len(repo)-1 {
				candidates = append(candidates, repo[slash+1:], "image:"+repo[slash+1:])
			}
		}
		if tag != "" {
			candidates = append(candidates, tag, "image:"+tag)
		}
	}
	for key, value := range c.Labels {
		candidates = append(candidates, key, "label:"+key)
		if value != "" {
			candidates = append(candidates, value, key+"="+value, "label:"+key+"="+value)
		}
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
	candidates := []string{
		vol.Name,
		"name:" + vol.Name,
		vol.Driver,
		"driver:" + vol.Driver,
		vol.Mountpoint,
		"mountpoint:" + vol.Mountpoint,
		vol.Scope,
		"scope:" + vol.Scope,
	}
	for key, value := range vol.Labels {
		candidates = append(candidates, key, "label:"+key, value, key+"="+value, "label:"+key+"="+value)
	}
	for key, value := range vol.Options {
		candidates = append(candidates, key, "option:"+key, value, key+"="+value, "option:"+key+"="+value)
	}
	return uniqueNonEmptyStrings(candidates)
}

func matchesOneTargetFilter(candidates []string, filter string) bool {
	key, pattern, keyed := splitTargetFilter(filter)
	if strings.TrimSpace(pattern) == "" {
		return false
	}
	for _, candidate := range candidates {
		candidateKey, candidateValue, candidateKeyed := splitTargetFilter(candidate)
		if keyed {
			if !candidateKeyed || candidateKey != key {
				continue
			}
			if targetValueMatches(pattern, candidateValue) {
				return true
			}
			continue
		}
		if targetValueMatches(pattern, candidate) {
			return true
		}
	}
	return false
}

func splitTargetFilter(filter string) (string, string, bool) {
	filter = strings.TrimSpace(filter)
	for _, sep := range []string{":", "="} {
		if idx := strings.Index(filter, sep); idx > 0 {
			key := strings.ToLower(strings.TrimSpace(filter[:idx]))
			if isTargetFilterKey(key) {
				return key, strings.TrimSpace(filter[idx+1:]), true
			}
		}
	}
	return "", filter, false
}

func isTargetFilterKey(key string) bool {
	switch key {
	case "name", "id", "image", "state", "status", "label", "driver", "mountpoint", "scope", "option":
		return true
	default:
		return false
	}
}

func targetValueMatches(pattern, value string) bool {
	pattern = strings.TrimSpace(pattern)
	value = strings.TrimSpace(value)
	if pattern == "" || value == "" {
		return false
	}
	if wildcardMatch(pattern, value) || strings.EqualFold(pattern, value) || strings.HasPrefix(strings.ToLower(value), strings.ToLower(pattern)) {
		return true
	}
	if strings.ContainsAny(pattern, "*?") {
		return wildcardMatch(strings.ToLower(pattern), strings.ToLower(value))
	}
	return false
}

func splitRepoTag(ref string) (string, string) {
	lastSlash := strings.LastIndex(ref, "/")
	lastColon := strings.LastIndex(ref, ":")
	if lastColon > lastSlash {
		return ref[:lastColon], ref[lastColon+1:]
	}
	return ref, ""
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
