package docker

import (
	"context"
	"io"
	"os"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

//
// @Author yfy2001
// @Date 2025/12/5 21 20
//

type ImageManager struct {
	cli *client.Client
}

// NewImageManager 构造函数
func NewImageManager() *ImageManager {
	cli, _ := initDockerClient()

	return &ImageManager{cli: cli}
}

// List 列出所有镜像
func (im *ImageManager) List(all bool) ([]image.Summary, error) {
	ctx := context.Background()
	return im.cli.ImageList(ctx, image.ListOptions{All: all})
}

// Save 导出镜像
func (im *ImageManager) Save(images []string, outputFile string) error {
	ctx := context.Background()
	reader, err := im.cli.ImageSave(ctx, images)
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

// Load 导入镜像
func (im *ImageManager) Load(inputFile string) error {
	ctx := context.Background()
	file, err := os.Open(inputFile)
	if err != nil {
		return err
	}
	defer file.Close()

	resp, err := im.cli.ImageLoad(ctx, file, client.ImageLoadWithQuiet(false))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	_, err = io.Copy(os.Stdout, resp.Body)
	return err
}
