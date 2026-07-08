package pull

import (
	"context"
	"fmt"
	"github.com/Yui100901/MyGo/network/http_utils"
	"io"
	"log"
	"os"
	"time"
)

func NewPullRunner(proxy, targetOS, arch string) (*PullRunner, error) {
	return NewPullRunnerWithTimeout(proxy, targetOS, arch, defaultPullTimeout)
}

func NewPullRunnerWithTimeout(proxy, targetOS, arch string, timeout time.Duration) (*PullRunner, error) {
	client, err := newPullHTTPClient(proxy, timeout)
	if err != nil {
		return nil, err
	}
	return &PullRunner{
		platform:            targetPlatform{targetOS: targetOS, targetArch: arch},
		httpClient:          client,
		loadPulledImage:     loadImageTar,
		tagPulledImage:      tagImage,
		pushPulledImage:     pushImage,
		runCredentialHelper: defaultRunPullCredentialHelper,
	}, nil
}

func (r *PullRunner) PullImage(imageName string, opts PullOptions) error {
	return r.getImage(imageName, opts)
}

func (r *PullRunner) getImage(imageName string, opts PullOptions) error {
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	imageInfo, err := parseImageInfo(imageName)
	if err != nil {
		return fmt.Errorf("镜像名称解析失败: %w", err)
	}
	log.Printf("获取镜像%s:%s,目标平台%s/%s", imageInfo.Image, imageInfo.Tag, r.platform.targetOS, r.platform.targetArch)

	if err := ctx.Err(); err != nil {
		return err
	}

	tempDir, err := prepareWorkspace(imageInfo)
	if err != nil {
		return fmt.Errorf("准备临时目录失败: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			log.Printf("警告: 清理临时目录 %s 失败: %v", tempDir, err)
		}
	}()

	manifest, auth, err := r.fetchManifest(ctx, imageInfo, opts)
	if err != nil {
		return fmt.Errorf("获取清单失败: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	err = createManifestFile(imageInfo, manifest, tempDir)
	if err != nil {
		return fmt.Errorf("创建清单文件失败: %w", err)
	}

	err = r.downloadConfig(ctx, imageInfo, manifest, auth, opts, tempDir)
	if err != nil {
		return fmt.Errorf("下载配置文件失败: %w", err)
	}

	err = r.downloadLayers(ctx, imageInfo, manifest, auth, opts, tempDir)
	if err != nil {
		return fmt.Errorf("下载镜像层失败: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	outputFile, err := resolveOutputFile(imageInfo, opts)
	if err != nil {
		return fmt.Errorf("解析输出路径失败: %w", err)
	}
	err = packageImage(ctx, tempDir, outputFile)
	if err != nil {
		return fmt.Errorf("打包镜像失败: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	return r.completePulledImage(outputFile, imageInfo, opts)
}

func configureHTTPLogging(verbose bool) {
	if verbose {
		http_utils.Logger.SetOutput(os.Stdout)
		return
	}
	http_utils.Logger.SetOutput(io.Discard)
}
