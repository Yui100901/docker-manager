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
	"github.com/docker/docker/client"
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
	cli, err := docker.NewClient()
	if err != nil {
		return nil, err
	}
	return &dockerPruneService{cli: cli}, nil
}

type dockerPruneService struct {
	cli *client.Client
}

func (s *dockerPruneService) DiskUsage(ctx context.Context) (types.DiskUsage, error) {
	return s.cli.DiskUsage(ctx, types.DiskUsageOptions{})
}

func (s *dockerPruneService) ListContainers(ctx context.Context, all bool) ([]container.Summary, error) {
	return s.cli.ContainerList(ctx, container.ListOptions{All: all})
}

func (s *dockerPruneService) InspectContainer(ctx context.Context, id string) (container.InspectResponse, error) {
	return s.cli.ContainerInspect(ctx, id)
}

func (s *dockerPruneService) PruneContainers(ctx context.Context, pruneFilters filters.Args) (container.PruneReport, error) {
	return s.cli.ContainersPrune(ctx, pruneFilters)
}

func (s *dockerPruneService) PruneImages(ctx context.Context, pruneFilters filters.Args) (image.PruneReport, error) {
	return s.cli.ImagesPrune(ctx, pruneFilters)
}

func (s *dockerPruneService) PruneVolumes(ctx context.Context, pruneFilters filters.Args) (volume.PruneReport, error) {
	return s.cli.VolumesPrune(ctx, pruneFilters)
}

func (s *dockerPruneService) PruneBuildCache(ctx context.Context, pruneFilters filters.Args) (*build.CachePruneReport, error) {
	return s.cli.BuildCachePrune(ctx, build.CachePruneOptions{All: true, Filters: pruneFilters})
}
