package diagnostics

import (
	"context"

	"docker-manager/internal/docker"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	mobyclient "github.com/moby/moby/client"
)

func (s *dockerNetworkService) ListContainers(ctx context.Context, all bool) ([]container.Summary, error) {
	result, err := s.cli.ContainerList(ctx, mobyclient.ContainerListOptions{All: all})
	if err != nil {
		return nil, err
	}
	return docker.ConvertDockerType[[]container.Summary](result.Items)
}

func (s *dockerNetworkService) ListNetworks(ctx context.Context) ([]network.Summary, error) {
	result, err := s.cli.NetworkList(ctx, mobyclient.NetworkListOptions{})
	if err != nil {
		return nil, err
	}
	return docker.ConvertDockerType[[]network.Summary](result.Items)
}

func (s *dockerNetworkService) InspectContainer(ctx context.Context, id string) (container.InspectResponse, error) {
	result, err := s.cli.ContainerInspect(ctx, id, mobyclient.ContainerInspectOptions{})
	if err != nil {
		return container.InspectResponse{}, err
	}
	return docker.ConvertDockerType[container.InspectResponse](result.Container)
}

func (s *dockerNetworkService) InspectNetwork(ctx context.Context, name string) (network.Inspect, error) {
	result, err := s.cli.NetworkInspect(ctx, name, mobyclient.NetworkInspectOptions{})
	if err != nil {
		return network.Inspect{}, err
	}
	return docker.ConvertDockerType[network.Inspect](result.Network)
}
