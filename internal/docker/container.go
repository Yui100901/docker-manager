package docker

import (
	"context"
	"fmt"
	"log"
	"time"

	oldcontainer "github.com/docker/docker/api/types/container"
	oldnetwork "github.com/docker/docker/api/types/network"
	oldvolume "github.com/docker/docker/api/types/volume"
	mobycontainer "github.com/moby/moby/api/types/container"
	mobynetwork "github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type ContainerManager struct {
	cli *client.Client
}

func NewContainerManager() (*ContainerManager, error) {
	cli, err := initMobyClient()
	if err != nil {
		return nil, err
	}
	return &ContainerManager{cli: cli}, nil
}

func (cm *ContainerManager) ListAll() ([]oldcontainer.Summary, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := cm.cli.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, err
	}
	return convertDockerType[[]oldcontainer.Summary](result.Items)
}

func (cm *ContainerManager) Stop(containerID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := cm.cli.ContainerStop(ctx, containerID, client.ContainerStopOptions{})
	return err
}

func (cm *ContainerManager) Remove(containerID string, force, removeVolumes bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := cm.cli.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{
		Force:         force,
		RemoveVolumes: removeVolumes,
	})
	return err
}

func (cm *ContainerManager) Inspect(containerID string) (oldcontainer.InspectResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := cm.cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return oldcontainer.InspectResponse{}, err
	}
	return convertDockerType[oldcontainer.InspectResponse](result.Container)
}

func (cm *ContainerManager) InspectNetwork(name string) (oldnetwork.Inspect, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := cm.cli.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if err != nil {
		return oldnetwork.Inspect{}, err
	}
	return convertDockerType[oldnetwork.Inspect](result.Network)
}

func (cm *ContainerManager) InspectVolume(name string) (oldvolume.Volume, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := cm.cli.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if err != nil {
		return oldvolume.Volume{}, err
	}
	return convertDockerType[oldvolume.Volume](result.Volume)
}

func (cm *ContainerManager) Create(config *oldcontainer.Config,
	hostConfig *oldcontainer.HostConfig,
	networkingConfig *oldnetwork.NetworkingConfig,
	platform *ocispec.Platform,
	containerName string) (oldcontainer.CreateResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	mobyConfig, err := convertDockerPointer[mobycontainer.Config](config)
	if err != nil {
		return oldcontainer.CreateResponse{}, err
	}
	mobyHostConfig, err := convertDockerPointer[mobycontainer.HostConfig](hostConfig)
	if err != nil {
		return oldcontainer.CreateResponse{}, err
	}
	mobyNetworkingConfig, err := convertDockerPointer[mobynetwork.NetworkingConfig](networkingConfig)
	if err != nil {
		return oldcontainer.CreateResponse{}, err
	}
	result, err := cm.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:           mobyConfig,
		HostConfig:       mobyHostConfig,
		NetworkingConfig: mobyNetworkingConfig,
		Platform:         platform,
		Name:             containerName,
	})
	if err != nil {
		return oldcontainer.CreateResponse{}, err
	}
	return oldcontainer.CreateResponse{ID: result.ID, Warnings: result.Warnings}, nil
}

func (cm *ContainerManager) Start(containerID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := cm.cli.ContainerStart(ctx, containerID, client.ContainerStartOptions{})
	return err
}

func (cm *ContainerManager) buildNetworkingConfig(inspect oldcontainer.InspectResponse) *oldnetwork.NetworkingConfig {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	nc := &oldnetwork.NetworkingConfig{
		EndpointsConfig: make(map[string]*oldnetwork.EndpointSettings),
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
			nc.EndpointsConfig[netName] = &oldnetwork.EndpointSettings{
				Aliases: netSettings.Aliases,
			}
		} else {
			log.Printf("warning: network %s does not exist, skipping", netName)
		}
	}

	return nc
}

func (cm *ContainerManager) RecreateContainer(containerID, newName string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	inspect, err := cm.Inspect(containerID)
	if err != nil {
		return "", fmt.Errorf("inspect failed: %w", err)
	}

	if _, stopErr := cm.cli.ContainerStop(ctx, containerID, client.ContainerStopOptions{}); stopErr != nil {
		log.Printf("warning: stop container %s failed: %v", containerID, stopErr)
	}

	if _, rmErr := cm.cli.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{
		Force:         true,
		RemoveVolumes: false,
	}); rmErr != nil {
		log.Printf("warning: remove container %s failed: %v", containerID, rmErr)
	}

	resp, err := cm.Create(inspect.Config, inspect.HostConfig, cm.buildNetworkingConfig(inspect), nil, newName)
	if err != nil {
		return "", fmt.Errorf("create container failed: %w", err)
	}

	if _, err := cm.cli.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{}); err != nil {
		return "", fmt.Errorf("start container failed: %w", err)
	}

	return resp.ID, nil
}
