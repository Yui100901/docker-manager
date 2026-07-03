package diagnostics

import (
	"context"
	"io"

	"github.com/docker/docker/api/types/container"
)

func (s *dockerHealthService) ListContainers(ctx context.Context, all bool) ([]container.Summary, error) {
	return s.cli.ContainerList(ctx, container.ListOptions{All: all})
}

func (s *dockerHealthService) InspectContainer(ctx context.Context, id string) (container.InspectResponse, error) {
	return s.cli.ContainerInspect(ctx, id)
}

func (s *dockerHealthService) ContainerLogs(ctx context.Context, id string, options container.LogsOptions) (io.ReadCloser, error) {
	return s.cli.ContainerLogs(ctx, id, options)
}
