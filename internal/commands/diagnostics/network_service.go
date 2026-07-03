package diagnostics

import (
	"context"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
)

func (s *dockerNetworkService) ListContainers(ctx context.Context, all bool) ([]container.Summary, error) {
	return s.cli.ContainerList(ctx, container.ListOptions{All: all})
}

func (s *dockerNetworkService) ListNetworks(ctx context.Context) ([]network.Summary, error) {
	return s.cli.NetworkList(ctx, network.ListOptions{})
}

func (s *dockerNetworkService) InspectContainer(ctx context.Context, id string) (container.InspectResponse, error) {
	return s.cli.ContainerInspect(ctx, id)
}

func (s *dockerNetworkService) InspectNetwork(ctx context.Context, name string) (network.Inspect, error) {
	return s.cli.NetworkInspect(ctx, name, network.InspectOptions{})
}
