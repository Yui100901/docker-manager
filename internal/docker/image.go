package docker

import (
	"bufio"
	"context"
	"encoding/json"
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

type readOnlyReader struct {
	io.Reader
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
	return im.ListWithContext(context.Background(), all)
}

// ListWithContext lists images with caller-provided cancellation.
func (im *ImageManager) ListWithContext(ctx context.Context, all bool) ([]image.Summary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return im.cli.ImageList(ctx, image.ListOptions{All: all})
}

// Save 导出镜像
func (im *ImageManager) Save(images []string, outputFile string) error {
	return im.SaveWithContext(context.Background(), images, outputFile)
}

// SaveWithContext exports images with caller-provided cancellation.
func (im *ImageManager) SaveWithContext(ctx context.Context, images []string, outputFile string) error {
	if ctx == nil {
		ctx = context.Background()
	}
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

	return copyWithContext(ctx, file, reader)
}

// Load 导入镜像
func (im *ImageManager) Load(inputFile string) error {
	return im.LoadWithContext(context.Background(), inputFile, os.Stdout)
}

// LoadWithContext imports an image archive with caller-provided cancellation and output.
func (im *ImageManager) LoadWithContext(ctx context.Context, inputFile string, output io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if output == nil {
		output = io.Discard
	}
	file, err := os.Open(inputFile)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := file.Close(); cerr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "警告: 关闭文件 %s 失败: %v\n", inputFile, cerr)
		}
	}()

	resp, err := im.cli.ImageLoad(ctx, readOnlyReader{Reader: file}, client.ImageLoadWithQuiet(false))
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

	return copyWithContext(ctx, output, resp.Body)
}

// Tag tags an image in the local Docker engine.
func (im *ImageManager) Tag(ctx context.Context, source, target string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return im.cli.ImageTag(ctx, source, target)
}

// Push pushes an image from the local Docker engine to a registry.
func (im *ImageManager) Push(ctx context.Context, ref string) error {
	return im.PushWithOutput(ctx, ref, os.Stdout)
}

// PushWithOutput pushes an image and writes Docker's progress stream to output.
func (im *ImageManager) PushWithOutput(ctx context.Context, ref string, output io.Writer) error {
	return im.PushWithAuthOutput(ctx, ref, "", output)
}

// PushWithAuthOutput pushes an image with optional registry auth and writes Docker's progress stream to output.
func (im *ImageManager) PushWithAuthOutput(ctx context.Context, ref, registryAuth string, output io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if output == nil {
		output = io.Discard
	}
	resp, err := im.cli.ImagePush(ctx, ref, image.PushOptions{RegistryAuth: registryAuth})
	if err != nil {
		return err
	}
	defer func() {
		if cerr := resp.Close(); cerr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "警告: 关闭 push response 失败: %v\n", cerr)
		}
	}()
	return copyDockerPushStream(ctx, output, resp)
}

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) error {
	if ctx == nil {
		ctx = context.Background()
	}
	buf := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}

type dockerPushMessage struct {
	Error       string `json:"error"`
	ErrorDetail struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
}

func copyDockerPushStream(ctx context.Context, dst io.Writer, src io.Reader) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if dst == nil {
		dst = io.Discard
	}
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Text()
		if _, err := io.WriteString(dst, line+"\n"); err != nil {
			return err
		}
		var msg dockerPushMessage
		if err := json.Unmarshal([]byte(line), &msg); err == nil {
			if msg.ErrorDetail.Message != "" {
				return fmt.Errorf("docker push failed: %s", msg.ErrorDetail.Message)
			}
			if msg.Error != "" {
				return fmt.Errorf("docker push failed: %s", msg.Error)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return ctx.Err()
}
