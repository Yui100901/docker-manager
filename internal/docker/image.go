package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	mobyimage "github.com/moby/moby/api/types/image"
	"github.com/moby/moby/client"
)

type ImageManager struct {
	cli *client.Client
}

type readOnlyReader struct {
	io.Reader
}

func NewImageManager() (*ImageManager, error) {
	cli, err := initMobyClient()
	if err != nil {
		return nil, err
	}
	return &ImageManager{cli: cli}, nil
}

func (im *ImageManager) List(all bool) ([]mobyimage.Summary, error) {
	return im.ListWithContext(context.Background(), all)
}

func (im *ImageManager) ListWithContext(ctx context.Context, all bool) ([]mobyimage.Summary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	result, err := im.cli.ImageList(ctx, client.ImageListOptions{All: all})
	if err != nil {
		return nil, err
	}
	return result.Items, nil
}

func (im *ImageManager) Save(images []string, outputFile string) error {
	return im.SaveWithContext(context.Background(), images, outputFile)
}

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
			_, _ = fmt.Fprintf(os.Stderr, "warning: close image save reader failed: %v\n", cerr)
		}
	}()

	file, err := os.Create(outputFile)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := file.Close(); cerr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "warning: close file %s failed: %v\n", outputFile, cerr)
		}
	}()

	return copyWithContext(ctx, file, reader)
}

func (im *ImageManager) Load(inputFile string) error {
	return im.LoadWithContext(context.Background(), inputFile, os.Stdout)
}

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
			_, _ = fmt.Fprintf(os.Stderr, "warning: close file %s failed: %v\n", inputFile, cerr)
		}
	}()

	resp, err := im.cli.ImageLoad(ctx, readOnlyReader{Reader: file}, client.ImageLoadWithQuiet(false))
	if err != nil {
		return err
	}
	defer func() {
		if cerr := resp.Close(); cerr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "warning: close image load response failed: %v\n", cerr)
		}
	}()

	return copyWithContext(ctx, output, resp)
}

func (im *ImageManager) Tag(ctx context.Context, source, target string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	_, err := im.cli.ImageTag(ctx, client.ImageTagOptions{Source: source, Target: target})
	return err
}

func (im *ImageManager) Push(ctx context.Context, ref string) error {
	return im.PushWithOutput(ctx, ref, os.Stdout)
}

func (im *ImageManager) PushWithOutput(ctx context.Context, ref string, output io.Writer) error {
	return im.PushWithAuthOutput(ctx, ref, "", output)
}

func (im *ImageManager) PushWithAuthOutput(ctx context.Context, ref, registryAuth string, output io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if output == nil {
		output = io.Discard
	}
	resp, err := im.cli.ImagePush(ctx, ref, client.ImagePushOptions{RegistryAuth: registryAuth})
	if err != nil {
		return err
	}
	defer func() {
		if cerr := resp.Close(); cerr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "warning: close push response failed: %v\n", cerr)
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
