package docker

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

//
// @Author yfy2001
// @Date 2025/12/5 21 20
//

// ContainerManager 封装容器相关操作
type ContainerManager struct {
	cli *client.Client
}

// NewContainerManager 构造函数，初始化 DockerClient
func NewContainerManager() *ContainerManager {
	cli, _ := initDockerClient()
	return &ContainerManager{cli: cli}
}

// ListAll 列出所有容器
func (cm *ContainerManager) ListAll() ([]container.Summary, error) {
	ctx := context.Background()
	return cm.cli.ContainerList(ctx, container.ListOptions{All: true})
}

// Stop 停止指定容器
func (cm *ContainerManager) Stop(containerID string) error {
	ctx := context.Background()
	return cm.cli.ContainerStop(ctx, containerID, container.StopOptions{})
}

// Remove 删除指定容器
func (cm *ContainerManager) Remove(containerID string, force, removeVolumes bool) error {
	ctx := context.Background()
	return cm.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{
		Force:         force,
		RemoveVolumes: removeVolumes,
	})
}

// Inspect 获取容器信息
func (cm *ContainerManager) Inspect(containerID string) (container.InspectResponse, error) {
	ctx := context.Background()
	return cm.cli.ContainerInspect(ctx, containerID)
}

// Create 获取容器信息
func (cm *ContainerManager) Create(config *container.Config,
	hostConfig *container.HostConfig,
	networkingConfig *network.NetworkingConfig,
	platform *ocispec.Platform,
	containerName string) (container.CreateResponse, error) {
	ctx := context.Background()
	return cm.cli.ContainerCreate(ctx, config, hostConfig, networkingConfig, platform, containerName)
}

func (cm *ContainerManager) Start(containerID string) error {
	ctx := context.Background()
	return cm.cli.ContainerStart(ctx, containerID, container.StartOptions{})
}

// buildNetworkingConfig 从 Inspect.NetworkSettings 构造 NetworkingConfig
func (cm *ContainerManager) buildNetworkingConfig(inspect container.InspectResponse) *network.NetworkingConfig {
	ctx := context.Background()
	nc := &network.NetworkingConfig{
		EndpointsConfig: make(map[string]*network.EndpointSettings),
	}

	// 获取当前存在的网络列表
	nets, err := cm.cli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		fmt.Printf("警告: 获取网络列表失败: %v\n", err)
		return nc
	}

	existing := make(map[string]bool)
	for _, n := range nets {
		existing[n.Name] = true
	}

	// 遍历原容器网络配置
	for netName, netSettings := range inspect.NetworkSettings.Networks {
		if existing[netName] {
			nc.EndpointsConfig[netName] = &network.EndpointSettings{
				Aliases: netSettings.Aliases,
			}
		} else {
			fmt.Printf("警告: 网络 %s 已不存在，跳过\n", netName)
		}
	}

	return nc
}

// RecreateContainer 根据现有容器配置删除并重新创建运行
func (cm *ContainerManager) RecreateContainer(containerID, newName string) (string, error) {
	ctx := context.Background()

	// 1. Inspect 原容器
	inspect, err := cm.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", fmt.Errorf("inspect失败: %w", err)
	}

	// 2. 停止容器（忽略错误）
	if stopErr := cm.cli.ContainerStop(ctx, containerID, container.StopOptions{}); stopErr != nil {
		fmt.Printf("警告: 停止容器 %s 失败: %v\n", containerID, stopErr)
	}

	// 3. 删除容器（不删除挂载卷，忽略错误）
	if rmErr := cm.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{
		Force:         true,
		RemoveVolumes: false,
	}); rmErr != nil {
		fmt.Printf("警告: 删除容器 %s 失败: %v\n", containerID, rmErr)
	}

	// 4. 构造 NetworkingConfig
	networkingConfig := cm.buildNetworkingConfig(inspect)

	// 5. 重新创建容器
	resp, err := cm.cli.ContainerCreate(
		ctx,
		inspect.Config,     // 原来的 Config
		inspect.HostConfig, // 原来的 HostConfig
		networkingConfig,   // 尝试还原网络配置
		nil,                // Platform 可选，通常传 nil 即可
		newName,            // 新容器名
	)
	if err != nil {
		return "", fmt.Errorf("创建容器失败: %w", err)
	}

	// 6. 启动新容器
	if err := cm.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("启动容器失败: %w", err)
	}

	return resp.ID, nil
}
