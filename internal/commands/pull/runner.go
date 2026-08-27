package pull

import (
	"context"
	"fmt"
	"github.com/Yui100901/MyGo/network/http_utils"
	"io"
	"log"
	"os"
	"time"

	"docker-manager/internal/sensitive"
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
		baseProxy:           proxy,
		baseTimeout:         timeout,
		policyClients:       &registryPolicyClientCache{clients: make(map[registryPolicyClientKey]*http_utils.HTTPClient)},
		loadPulledImage:     loadImageTar,
		tagPulledImage:      tagImage,
		pushPulledImage:     pushImage,
		runCredentialHelper: defaultRunPullCredentialHelper,
	}, nil
}

func (r *PullRunner) getImage(imageName string, opts PullOptions) error {
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validatePullResourceLimits(opts.Limits); err != nil {
		return err
	}
	opts.Limits = effectivePullResourceLimits(opts.Limits)
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, opts.Limits.TotalTimeout)
	defer cancel()
	opts.Context = ctx

	imageInfo, err := parseImageInfo(imageName)
	if err != nil {
		return fmt.Errorf("镜像名称解析失败: %w", err)
	}
	baseOpts := opts
	sourceRunner, sourceOpts, err := r.bindRegistryPolicy(imageInfo.Registry, registryCredentialPull, opts)
	if err != nil {
		return fmt.Errorf("应用 registry %s 策略失败: %w", imageInfo.Registry, err)
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

	manifest, auth, err := sourceRunner.fetchManifest(ctx, imageInfo, sourceOpts)
	if err != nil {
		return fmt.Errorf("获取清单失败: %w", err)
	}
	if err := validateManifestLayers(manifest, sourceOpts.Limits); err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	err = createManifestFile(imageInfo, manifest, tempDir)
	if err != nil {
		return fmt.Errorf("创建清单文件失败: %w", err)
	}

	err = sourceRunner.downloadConfig(ctx, imageInfo, manifest, auth, sourceOpts, tempDir)
	if err != nil {
		return fmt.Errorf("下载配置文件失败: %w", err)
	}
	initialTemporaryBytes, err := workspaceRegularBytes(ctx, tempDir)
	if err != nil {
		return fmt.Errorf("统计临时文件失败: %w", err)
	}
	sourceOpts.resourceBudget, err = newPullResourceBudget(sourceOpts.Limits, initialTemporaryBytes)
	if err != nil {
		return err
	}

	err = sourceRunner.downloadLayers(ctx, imageInfo, manifest, auth, sourceOpts, tempDir)
	if err != nil {
		return fmt.Errorf("下载镜像层失败: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	outputFile, err := resolveOutputFile(imageInfo, sourceOpts)
	if err != nil {
		return fmt.Errorf("解析输出路径失败: %w", err)
	}
	if err := validatePackageTemporaryBudget(ctx, tempDir, sourceOpts.Limits.TemporaryBytes); err != nil {
		return err
	}
	baseOpts.Limits = sourceOpts.Limits
	preparedTarget, err := r.preparePulledImageMutation(outputFile, imageInfo, baseOpts)
	if err != nil {
		return err
	}
	baseOpts.preparedTarget = preparedTarget
	baseOpts.mutationAuthorized = true
	err = packageImage(ctx, tempDir, outputFile)
	if err != nil {
		return fmt.Errorf("打包镜像失败: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	return r.completePulledImage(outputFile, imageInfo, baseOpts)
}

func configureHTTPLogging(verbose bool) {
	configureHTTPLoggingTo(verbose, os.Stdout)
}

func configureHTTPLoggingTo(verbose bool, output io.Writer) {
	if !verbose || output == nil {
		http_utils.Logger.SetOutput(io.Discard)
		return
	}
	http_utils.Logger.SetOutput(sensitive.NewDynamicWriter(output))
}
