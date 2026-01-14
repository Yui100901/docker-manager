package docker

import (
	"context"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
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
