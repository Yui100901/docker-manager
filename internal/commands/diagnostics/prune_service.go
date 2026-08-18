package diagnostics

import (
	"context"
	"regexp"
	"time"

	"docker-manager/internal/docker"

	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/volume"
	mobyclient "github.com/moby/moby/client"
)

type pruneDockerService interface {
	DiskUsage(ctx context.Context, opts mobyclient.DiskUsageOptions) (pruneDiskUsage, error)
	ListContainers(ctx context.Context, all bool) ([]container.Summary, error)
	InspectContainer(ctx context.Context, id string) (container.InspectResponse, error)
	InspectImage(ctx context.Context, id string) (image.InspectResponse, error)
	InspectVolume(ctx context.Context, name string) (volume.Volume, error)
	RemoveContainer(ctx context.Context, id string) error
	RemoveImage(ctx context.Context, id string) error
	RemoveVolume(ctx context.Context, name string) error
	RemoveBuildCache(ctx context.Context, id string, untilCutoff time.Time, hasUntilCutoff bool) (*build.CachePruneReport, error)
}

var newPruneDockerService = func() (pruneDockerService, error) {
	cli, err := docker.NewMobyClient()
	if err != nil {
		return nil, err
	}
	return &dockerPruneService{cli: cli}, nil
}

type dockerPruneService struct {
	cli *mobyclient.Client
}

func (s *dockerPruneService) DiskUsage(ctx context.Context, opts mobyclient.DiskUsageOptions) (pruneDiskUsage, error) {
	result, err := s.cli.DiskUsage(ctx, opts)
	if err != nil {
		return pruneDiskUsage{}, err
	}
	return pruneDiskUsage{
		LayersSize: result.Images.TotalSize,
		Images:     toPointerSlice(result.Images.Items),
		Containers: toPointerSlice(result.Containers.Items),
		Volumes:    toPointerSlice(result.Volumes.Items),
		BuildCache: toPointerSlice(result.BuildCache.Items),
	}, nil
}

func (s *dockerPruneService) ListContainers(ctx context.Context, all bool) ([]container.Summary, error) {
	result, err := s.cli.ContainerList(ctx, mobyclient.ContainerListOptions{All: all})
	if err != nil {
		return nil, err
	}
	return result.Items, nil
}

func (s *dockerPruneService) InspectContainer(ctx context.Context, id string) (container.InspectResponse, error) {
	result, err := s.cli.ContainerInspect(ctx, id, mobyclient.ContainerInspectOptions{})
	if err != nil {
		return container.InspectResponse{}, err
	}
	return result.Container, nil
}

func (s *dockerPruneService) InspectImage(ctx context.Context, id string) (image.InspectResponse, error) {
	result, err := s.cli.ImageInspect(ctx, id)
	if err != nil {
		return image.InspectResponse{}, err
	}
	return result.InspectResponse, nil
}

func (s *dockerPruneService) InspectVolume(ctx context.Context, name string) (volume.Volume, error) {
	result, err := s.cli.VolumeInspect(ctx, name, mobyclient.VolumeInspectOptions{})
	if err != nil {
		return volume.Volume{}, err
	}
	return result.Volume, nil
}

func (s *dockerPruneService) RemoveContainer(ctx context.Context, id string) error {
	_, err := s.cli.ContainerRemove(ctx, id, mobyclient.ContainerRemoveOptions{
		Force:         false,
		RemoveVolumes: false,
	})
	return err
}

func (s *dockerPruneService) RemoveImage(ctx context.Context, id string) error {
	_, err := s.cli.ImageRemove(ctx, id, mobyclient.ImageRemoveOptions{
		Force:         false,
		PruneChildren: false,
	})
	return err
}

func (s *dockerPruneService) RemoveVolume(ctx context.Context, name string) error {
	_, err := s.cli.VolumeRemove(ctx, name, mobyclient.VolumeRemoveOptions{Force: false})
	return err
}

func (s *dockerPruneService) RemoveBuildCache(ctx context.Context, id string, untilCutoff time.Time, hasUntilCutoff bool) (*build.CachePruneReport, error) {
	result, err := s.cli.BuildCachePrune(ctx, pruneBuildCacheOptions(id, untilCutoff, hasUntilCutoff))
	return &result.Report, err
}

func pruneBuildCacheOptions(id string, untilCutoff time.Time, hasUntilCutoff bool) mobyclient.BuildCachePruneOptions {
	filters := make(mobyclient.Filters)
	// The daemon forwards this filter to BuildKit as a regular expression.
	filters.Add("id", "^"+regexp.QuoteMeta(id)+"$")
	if hasUntilCutoff {
		filters.Add("until", pruneBuildCacheUntilDuration(untilCutoff, time.Now()))
	}
	return mobyclient.BuildCachePruneOptions{All: true, Filters: filters}
}

func pruneBuildCacheUntilDuration(cutoff, now time.Time) string {
	age := now.Sub(cutoff)
	if age < 0 {
		age = 0
	}
	return age.String()
}

func toPointerSlice[T any](items []T) []*T {
	out := make([]*T, 0, len(items))
	for i := range items {
		out = append(out, &items[i])
	}
	return out
}
