package backup

import (
	"context"
	"fmt"
	"sort"

	"docker-manager/internal/docker"
	"docker-manager/internal/resourcefilter"

	"github.com/moby/moby/api/types/container"
)

func resolveBackupContainerTargets(ctx context.Context, patterns []string) ([]string, error) {
	if err := checkBackupContext(ctx); err != nil {
		return nil, err
	}
	svc, err := newBackupDockerService()
	if err != nil {
		return nil, err
	}
	containers, err := svc.ListContainers(ctx, true)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var targets []string
	for _, pattern := range patterns {
		if err := checkBackupContext(ctx); err != nil {
			return nil, err
		}
		matches := matchBackupContainerTargets(containers, pattern)
		if len(matches) == 0 {
			return nil, fmt.Errorf("容器 %q 未匹配任何容器", pattern)
		}
		for _, match := range matches {
			if !seen[match] {
				seen[match] = true
				targets = append(targets, match)
			}
		}
	}
	return targets, nil
}

func matchBackupContainerTargets(containers []container.Summary, pattern string) []string {
	var matches []string
	for _, c := range containers {
		if !backupContainerMatchesPattern(c, pattern) {
			continue
		}
		name := backupContainerTargetName(c)
		if name != "" {
			matches = append(matches, name)
		}
	}
	sort.Strings(matches)
	return matches
}

func backupContainerMatchesPattern(c container.Summary, pattern string) bool {
	converted, err := docker.ConvertDockerType[container.Summary](c)
	if err != nil {
		return false
	}
	return resourcefilter.Match(resourcefilter.ContainerCandidates(converted), []string{pattern}, resourcefilter.ContainerKeys...)
}

func firstContainerName(names []string) string {
	return resourcefilter.FirstContainerName(names)
}

func backupContainerTargetName(c container.Summary) string {
	name := firstContainerName(c.Names)
	if name != "" {
		return name
	}
	return c.ID
}
