package diagnostics

import (
	"fmt"
	"sort"
	"strings"
	"time"

	mobyclient "github.com/moby/moby/client"
)

const (
	pruneKindContainer  = "container"
	pruneKindImage      = "image"
	pruneKindVolume     = "volume"
	pruneKindBuildCache = "build-cache"
)

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
