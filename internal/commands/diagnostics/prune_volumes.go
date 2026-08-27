package diagnostics

import (
	"context"
	"fmt"
	"maps"

	"docker-manager/internal/runcontrol"

	"github.com/moby/moby/api/types/volume"
)

type pruneVolumeSnapshot struct {
	Name       string
	CreatedAt  string
	Driver     string
	Mountpoint string
	Scope      string
	Labels     map[string]string
	Options    map[string]string
}

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
	if err := runcontrol.CheckItems(ctx, "container", len(containers)); err != nil {
		return nil, nil, err
	}
	return inspectVolumeContainerRefs(ctx, svc, containers)
}

func newPruneVolumeSnapshot(vol *volume.Volume) *pruneVolumeSnapshot {
	if vol == nil {
		return nil
	}
	return &pruneVolumeSnapshot{
		Name:       vol.Name,
		CreatedAt:  vol.CreatedAt,
		Driver:     vol.Driver,
		Mountpoint: vol.Mountpoint,
		Scope:      vol.Scope,
		Labels:     maps.Clone(vol.Labels),
		Options:    maps.Clone(vol.Options),
	}
}

func (snapshot *pruneVolumeSnapshot) matches(vol volume.Volume) bool {
	return snapshot != nil &&
		snapshot.Name == vol.Name &&
		snapshot.CreatedAt == vol.CreatedAt &&
		snapshot.Driver == vol.Driver &&
		snapshot.Mountpoint == vol.Mountpoint &&
		snapshot.Scope == vol.Scope &&
		maps.Equal(snapshot.Labels, vol.Labels) &&
		maps.Equal(snapshot.Options, vol.Options)
}
