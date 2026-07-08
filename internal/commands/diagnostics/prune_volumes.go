package diagnostics

import (
	"context"
	"fmt"
	"strings"
)

func inspectPruneVolumeRefs(ctx context.Context, svc pruneDockerService) (map[string][]VolumeContainerRef, []string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	containers, err := svc.ListContainers(ctx, true)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, ctxErr
		}
		return nil, []string{fmt.Sprintf("无法列出容器复核 volume 引用，已仅使用 Docker DiskUsage: %v", err)}, nil
	}
	return inspectVolumeContainerRefs(ctx, svc, containers)
}

func ensurePruneVolumeCandidatesStillUnreferenced(ctx context.Context, svc pruneDockerService, candidates []PruneVolumeRef) error {
	if len(candidates) == 0 {
		return nil
	}
	refsByVolume, warnings, err := inspectPruneVolumeRefs(ctx, svc)
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("volume prune preflight canceled: %w", err)
	}
	if err != nil {
		return fmt.Errorf("执行 volume prune 前复核引用失败: %w", err)
	}
	for _, warning := range warnings {
		if strings.Contains(warning, "无法列出容器") {
			return fmt.Errorf("执行 volume prune 前复核引用失败: %s", warning)
		}
	}
	for _, candidate := range candidates {
		if refs := refsByVolume[candidate.Name]; len(refs) > 0 {
			return fmt.Errorf("拒绝执行 volume prune: volume %s 在执行前复核中仍被 %d 个容器引用", candidate.Name, len(refs))
		}
	}
	return nil
}
