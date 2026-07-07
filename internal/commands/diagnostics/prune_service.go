package diagnostics

import (
	"context"

	"docker-manager/internal/docker"

	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/volume"
	mobyclient "github.com/moby/moby/client"
)

type pruneDockerService interface {
	DiskUsage(ctx context.Context) (pruneDiskUsage, error)
	ListContainers(ctx context.Context, all bool) ([]container.Summary, error)
	InspectContainer(ctx context.Context, id string) (container.InspectResponse, error)
	PruneContainers(ctx context.Context, pruneFilters mobyclient.Filters) (container.PruneReport, error)
	PruneImages(ctx context.Context, pruneFilters mobyclient.Filters) (image.PruneReport, error)
	PruneVolumes(ctx context.Context, pruneFilters mobyclient.Filters) (volume.PruneReport, error)
	PruneBuildCache(ctx context.Context, pruneFilters mobyclient.Filters) (*build.CachePruneReport, error)
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

func (s *dockerPruneService) DiskUsage(ctx context.Context) (pruneDiskUsage, error) {
	result, err := s.cli.DiskUsage(ctx, mobyclient.DiskUsageOptions{})
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

func (s *dockerPruneService) PruneContainers(ctx context.Context, pruneFilters mobyclient.Filters) (container.PruneReport, error) {
	result, err := s.cli.ContainerPrune(ctx, mobyclient.ContainerPruneOptions{Filters: pruneFilters})
	if err != nil {
		return container.PruneReport{}, err
	}
	return result.Report, nil
}

func (s *dockerPruneService) PruneImages(ctx context.Context, pruneFilters mobyclient.Filters) (image.PruneReport, error) {
	result, err := s.cli.ImagePrune(ctx, mobyclient.ImagePruneOptions{Filters: pruneFilters})
	if err != nil {
		return image.PruneReport{}, err
	}
	return result.Report, nil
}

func (s *dockerPruneService) PruneVolumes(ctx context.Context, pruneFilters mobyclient.Filters) (volume.PruneReport, error) {
	result, err := s.cli.VolumePrune(ctx, mobyclient.VolumePruneOptions{Filters: pruneFilters})
	if err != nil {
		return volume.PruneReport{}, err
	}
	return result.Report, nil
}

func (s *dockerPruneService) PruneBuildCache(ctx context.Context, pruneFilters mobyclient.Filters) (*build.CachePruneReport, error) {
	result, err := s.cli.BuildCachePrune(ctx, mobyclient.BuildCachePruneOptions{All: true, Filters: pruneFilters})
	if err != nil {
		return nil, err
	}
	return &result.Report, nil
}

func toPointerSlice[T any](items []T) []*T {
	out := make([]*T, 0, len(items))
	for i := range items {
		out = append(out, &items[i])
	}
	return out
}
