package docker

import (
	"context"
	"io"
	"os"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

//
// @Author yfy2001
// @Date 2025/12/5 14 21
//

var dockerClient *client.Client

func initDockerClient() (*client.Client, error) {
	if dockerClient == nil {
		cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return nil, err
		}
		dockerClient = cli
	}
	return dockerClient, nil
}

// ImageList 列出所有docker镜像
func ImageList() ([]image.Summary, error) {
	ctx := context.Background()
	cli, err := initDockerClient()
	if err != nil {
		panic(err)
	}

	// 获取镜像列表
	images, err := cli.ImageList(ctx, image.ListOptions{All: true})
	return images, err
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

// SaveImages 导出镜像
func SaveImages(images []string, outputFile string) error {
	ctx := context.Background()
	cli, err := initDockerClient()
	if err != nil {
		panic(err)
	}

	reader, err := cli.ImageSave(ctx, images)
	if err != nil {
		return err
	}
	defer reader.Close()

	file, err := os.Create(outputFile)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = io.Copy(file, reader)
	return err
}

// LoadImage 导入镜像
func LoadImage(inputFile string) error {
	ctx := context.Background()
	cli, err := initDockerClient()
	if err != nil {
		panic(err)
	}

	file, err := os.Open(inputFile)
	if err != nil {
		return err
	}
	defer file.Close()

	resp, err := cli.ImageLoad(ctx, file, client.ImageLoadWithQuiet(false))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 打印导入结果（比如镜像名）
	_, err = io.Copy(os.Stdout, resp.Body)
	return err
}
