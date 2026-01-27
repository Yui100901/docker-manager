package docker

import (
	"context"
	"fmt"
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
func NewImageManager() (*ImageManager, error) {
	cli, err := initDockerClient()
	if err != nil {
		return nil, err
	}

	return &ImageManager{cli: cli}, nil
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
	defer func() {
		if cerr := reader.Close(); cerr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "警告: 关闭 reader 失败: %v\n", cerr)
		}
	}()

	file, err := os.Create(outputFile)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := file.Close(); cerr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "警告: 关闭文件 %s 失败: %v\n", outputFile, cerr)
		}
	}()

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
	defer func() {
		if cerr := file.Close(); cerr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "警告: 关闭文件 %s 失败: %v\n", inputFile, cerr)
		}
	}()

	resp, err := im.cli.ImageLoad(ctx, file, client.ImageLoadWithQuiet(false))
	if err != nil {
		return err
	}
	defer func() {
		if resp.Body != nil {
			if cerr := resp.Body.Close(); cerr != nil {
				_, _ = fmt.Fprintf(os.Stderr, "警告: 关闭 resp.Body 失败: %v\n", cerr)
			}
		}
	}()

	_, err = io.Copy(os.Stdout, resp.Body)
	return err
}
