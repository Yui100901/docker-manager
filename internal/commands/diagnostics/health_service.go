package diagnostics

import (
	"context"
	"io"

	"docker-manager/internal/docker"

	"github.com/moby/moby/api/types/container"
	mobyclient "github.com/moby/moby/client"
)

func (s *dockerHealthService) ListContainers(ctx context.Context, all bool) ([]container.Summary, error) {
	result, err := s.cli.ContainerList(ctx, mobyclient.ContainerListOptions{All: all})
	if err != nil {
		return nil, err
	}
	return docker.ConvertDockerType[[]container.Summary](result.Items)
}

func (s *dockerHealthService) InspectContainer(ctx context.Context, id string) (container.InspectResponse, error) {
	result, err := s.cli.ContainerInspect(ctx, id, mobyclient.ContainerInspectOptions{})
	if err != nil {
		return container.InspectResponse{}, err
	}
	return docker.ConvertDockerType[container.InspectResponse](result.Container)
}

func (s *dockerHealthService) ContainerLogs(ctx context.Context, id string, options mobyclient.ContainerLogsOptions) (io.ReadCloser, error) {
	return s.cli.ContainerLogs(ctx, id, options)
}
