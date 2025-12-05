package docker

import (
	"context"

	"github.com/docker/docker/api/types/container"
)

//
// @Author yfy2001
// @Date 2025/12/5 21 20
//

// ContainerStop 停止指定容器
func ContainerStop(containerID string) error {
	ctx := context.Background()
	cli, err := initDockerClient()
	if err != nil {
		return err
	}

	// 停止容器，StopOptions 可以设置超时时间
	return cli.ContainerStop(ctx, containerID, container.StopOptions{})
}

// ContainerRemove 删除指定容器
func ContainerRemove(containerID string, force bool, removeVolumes bool) error {
	ctx := context.Background()
	cli, err := initDockerClient()
	if err != nil {
		return err
	}

	// 删除容器
	return cli.ContainerRemove(ctx, containerID, container.RemoveOptions{
		Force:         force,
		RemoveVolumes: removeVolumes,
	})
}

func ContainerInspect1(containerID string) (container.InspectResponse, error) {
	ctx := context.Background()
	cli, err := initDockerClient()
	if err != nil {
		panic(err)
	}
	containerInfo, err := cli.ContainerInspect(ctx, containerID)
	return containerInfo, err
}
