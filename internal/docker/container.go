package docker

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type ContainerManager struct {
	cli *client.Client
}

func contextWithTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, timeout)
}

func NewContainerManager() (*ContainerManager, error) {
	cli, err := initMobyClient()
	if err != nil {
		return nil, err
	}
	return &ContainerManager{cli: cli}, nil
}

func (cm *ContainerManager) ListAll() ([]container.Summary, error) {
	return cm.ListAllContext(context.Background())
}

func (cm *ContainerManager) ListAllContext(ctx context.Context) ([]container.Summary, error) {
	ctx, cancel := contextWithTimeout(ctx, 15*time.Second)
	defer cancel()
	result, err := cm.cli.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, err
	}
	return result.Items, nil
}

func (cm *ContainerManager) Stop(containerID string) error {
	return cm.StopContext(context.Background(), containerID)
}

func (cm *ContainerManager) StopContext(ctx context.Context, containerID string) error {
	ctx, cancel := contextWithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err := cm.cli.ContainerStop(ctx, containerID, client.ContainerStopOptions{})
	return err
}

func (cm *ContainerManager) Remove(containerID string, force, removeVolumes bool) error {
	return cm.RemoveContext(context.Background(), containerID, force, removeVolumes)
}

func (cm *ContainerManager) RemoveContext(ctx context.Context, containerID string, force, removeVolumes bool) error {
	ctx, cancel := contextWithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err := cm.cli.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{
		Force:         force,
		RemoveVolumes: removeVolumes,
	})
	return err
}

func (cm *ContainerManager) Inspect(containerID string) (container.InspectResponse, error) {
	return cm.InspectContext(context.Background(), containerID)
}

func (cm *ContainerManager) InspectContext(ctx context.Context, containerID string) (container.InspectResponse, error) {
	ctx, cancel := contextWithTimeout(ctx, 15*time.Second)
	defer cancel()
	result, err := cm.cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return container.InspectResponse{}, err
	}
	return result.Container, nil
}

func (cm *ContainerManager) InspectNetwork(name string) (network.Inspect, error) {
	return cm.InspectNetworkContext(context.Background(), name)
}

func (cm *ContainerManager) InspectNetworkContext(ctx context.Context, name string) (network.Inspect, error) {
	ctx, cancel := contextWithTimeout(ctx, 15*time.Second)
	defer cancel()
	result, err := cm.cli.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if err != nil {
		return network.Inspect{}, err
	}
	return result.Network, nil
}

func (cm *ContainerManager) InspectVolume(name string) (volume.Volume, error) {
	return cm.InspectVolumeContext(context.Background(), name)
}

func (cm *ContainerManager) InspectVolumeContext(ctx context.Context, name string) (volume.Volume, error) {
	ctx, cancel := contextWithTimeout(ctx, 15*time.Second)
	defer cancel()
	result, err := cm.cli.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if err != nil {
		return volume.Volume{}, err
	}
	return result.Volume, nil
}

func (cm *ContainerManager) Create(config *container.Config,
	hostConfig *container.HostConfig,
	networkingConfig *network.NetworkingConfig,
	platform *ocispec.Platform,
	containerName string) (container.CreateResponse, error) {
	return cm.CreateContext(context.Background(), config, hostConfig, networkingConfig, platform, containerName)
}

func (cm *ContainerManager) CreateContext(ctx context.Context,
	config *container.Config,
	hostConfig *container.HostConfig,
	networkingConfig *network.NetworkingConfig,
	platform *ocispec.Platform,
	containerName string) (container.CreateResponse, error) {
	ctx, cancel := contextWithTimeout(ctx, 60*time.Second)
	defer cancel()
	result, err := cm.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:           config,
		HostConfig:       hostConfig,
		NetworkingConfig: networkingConfig,
		Platform:         platform,
		Name:             containerName,
	})
	if err != nil {
		return container.CreateResponse{}, err
	}
	return container.CreateResponse{ID: result.ID, Warnings: result.Warnings}, nil
}

func (cm *ContainerManager) Start(containerID string) error {
	return cm.StartContext(context.Background(), containerID)
}

func (cm *ContainerManager) StartContext(ctx context.Context, containerID string) error {
	ctx, cancel := contextWithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := cm.cli.ContainerStart(ctx, containerID, client.ContainerStartOptions{})
	return err
}

func (cm *ContainerManager) buildNetworkingConfig(inspect container.InspectResponse) *network.NetworkingConfig {
	return cm.buildNetworkingConfigContext(context.Background(), inspect)
}

func (cm *ContainerManager) buildNetworkingConfigContext(ctx context.Context, inspect container.InspectResponse) *network.NetworkingConfig {
	ctx, cancel := contextWithTimeout(ctx, 15*time.Second)
	defer cancel()
	nc := &network.NetworkingConfig{
		EndpointsConfig: make(map[string]*network.EndpointSettings),
	}

	result, err := cm.cli.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		log.Printf("warning: list networks failed: %v", err)
		return nc
	}

	existing := make(map[string]bool)
	for _, n := range result.Items {
		existing[n.Name] = true
	}

	if inspect.NetworkSettings == nil || inspect.NetworkSettings.Networks == nil {
		return nc
	}

	for netName, netSettings := range inspect.NetworkSettings.Networks {
		if existing[netName] {
			nc.EndpointsConfig[netName] = &network.EndpointSettings{
				Aliases: netSettings.Aliases,
			}
		} else {
			log.Printf("warning: network %s does not exist, skipping", netName)
		}
	}

	return nc
}

func (cm *ContainerManager) RecreateContainer(containerID, newName string) (string, error) {
	return cm.RecreateContainerContext(context.Background(), containerID, newName)
}

func (cm *ContainerManager) RecreateContainerContext(ctx context.Context, containerID, newName string) (string, error) {
	ctx, cancel := contextWithTimeout(ctx, 180*time.Second)
	defer cancel()

	inspect, err := cm.InspectContext(ctx, containerID)
	if err != nil {
		return "", fmt.Errorf("inspect failed: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	if _, stopErr := cm.cli.ContainerStop(ctx, containerID, client.ContainerStopOptions{}); stopErr != nil {
		log.Printf("warning: stop container %s failed: %v", containerID, stopErr)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	if _, rmErr := cm.cli.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{
		Force:         true,
		RemoveVolumes: false,
	}); rmErr != nil {
		log.Printf("warning: remove container %s failed: %v", containerID, rmErr)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	resp, err := cm.CreateContext(ctx, inspect.Config, inspect.HostConfig, cm.buildNetworkingConfigContext(ctx, inspect), nil, newName)
	if err != nil {
		return "", fmt.Errorf("create container failed: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	if _, err := cm.cli.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{}); err != nil {
		return "", fmt.Errorf("start container failed: %w", err)
	}

	return resp.ID, nil
}
