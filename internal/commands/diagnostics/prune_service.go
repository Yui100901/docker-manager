package diagnostics

import (
	"context"

	"docker-manager/internal/docker"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/volume"
	mobyclient "github.com/moby/moby/client"
)

type pruneDockerService interface {
	DiskUsage(ctx context.Context) (types.DiskUsage, error)
	ListContainers(ctx context.Context, all bool) ([]container.Summary, error)
	InspectContainer(ctx context.Context, id string) (container.InspectResponse, error)
	PruneContainers(ctx context.Context, pruneFilters filters.Args) (container.PruneReport, error)
	PruneImages(ctx context.Context, pruneFilters filters.Args) (image.PruneReport, error)
	PruneVolumes(ctx context.Context, pruneFilters filters.Args) (volume.PruneReport, error)
	PruneBuildCache(ctx context.Context, pruneFilters filters.Args) (*build.CachePruneReport, error)
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

func (s *dockerPruneService) DiskUsage(ctx context.Context) (types.DiskUsage, error) {
	result, err := s.cli.DiskUsage(ctx, mobyclient.DiskUsageOptions{})
	if err != nil {
		return types.DiskUsage{}, err
	}
	images, err := docker.ConvertDockerType[[]image.Summary](result.Images.Items)
	if err != nil {
		return types.DiskUsage{}, err
	}
	containers, err := docker.ConvertDockerType[[]container.Summary](result.Containers.Items)
	if err != nil {
		return types.DiskUsage{}, err
	}
	volumes, err := docker.ConvertDockerType[[]volume.Volume](result.Volumes.Items)
	if err != nil {
		return types.DiskUsage{}, err
	}
	buildCache, err := docker.ConvertDockerType[[]build.CacheRecord](result.BuildCache.Items)
	if err != nil {
		return types.DiskUsage{}, err
	}
	return types.DiskUsage{
		LayersSize: result.Images.TotalSize,
		Images:     toPointerSlice(images),
		Containers: toPointerSlice(containers),
		Volumes:    toPointerSlice(volumes),
		BuildCache: toPointerSlice(buildCache),
	}, nil
}

func (s *dockerPruneService) ListContainers(ctx context.Context, all bool) ([]container.Summary, error) {
	result, err := s.cli.ContainerList(ctx, mobyclient.ContainerListOptions{All: all})
	if err != nil {
		return nil, err
	}
	return docker.ConvertDockerType[[]container.Summary](result.Items)
}

func (s *dockerPruneService) InspectContainer(ctx context.Context, id string) (container.InspectResponse, error) {
	result, err := s.cli.ContainerInspect(ctx, id, mobyclient.ContainerInspectOptions{})
	if err != nil {
		return container.InspectResponse{}, err
	}
	return docker.ConvertDockerType[container.InspectResponse](result.Container)
}

func (s *dockerPruneService) PruneContainers(ctx context.Context, pruneFilters filters.Args) (container.PruneReport, error) {
	result, err := s.cli.ContainerPrune(ctx, mobyclient.ContainerPruneOptions{Filters: convertPruneFilters(pruneFilters)})
	if err != nil {
		return container.PruneReport{}, err
	}
	return docker.ConvertDockerType[container.PruneReport](result.Report)
}

func (s *dockerPruneService) PruneImages(ctx context.Context, pruneFilters filters.Args) (image.PruneReport, error) {
	result, err := s.cli.ImagePrune(ctx, mobyclient.ImagePruneOptions{Filters: convertPruneFilters(pruneFilters)})
	if err != nil {
		return image.PruneReport{}, err
	}
	return docker.ConvertDockerType[image.PruneReport](result.Report)
}

func (s *dockerPruneService) PruneVolumes(ctx context.Context, pruneFilters filters.Args) (volume.PruneReport, error) {
	result, err := s.cli.VolumePrune(ctx, mobyclient.VolumePruneOptions{Filters: convertPruneFilters(pruneFilters)})
	if err != nil {
		return volume.PruneReport{}, err
	}
	return docker.ConvertDockerType[volume.PruneReport](result.Report)
}

func (s *dockerPruneService) PruneBuildCache(ctx context.Context, pruneFilters filters.Args) (*build.CachePruneReport, error) {
	result, err := s.cli.BuildCachePrune(ctx, mobyclient.BuildCachePruneOptions{All: true, Filters: convertPruneFilters(pruneFilters)})
	if err != nil {
		return nil, err
	}
	report, err := docker.ConvertDockerType[build.CachePruneReport](result.Report)
	if err != nil {
		return nil, err
	}
	return &report, nil
}

func convertPruneFilters(args filters.Args) mobyclient.Filters {
	out := mobyclient.Filters{}
	for _, key := range args.Keys() {
		out.Add(key, args.Get(key)...)
	}
	return out
}

func toPointerSlice[T any](items []T) []*T {
	out := make([]*T, 0, len(items))
	for i := range items {
		out = append(out, &items[i])
	}
	return out
}
