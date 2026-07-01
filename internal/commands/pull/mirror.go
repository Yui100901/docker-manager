package pull

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/url"
	"strings"

	"docker-manager/internal/docker"

	"github.com/distribution/reference"
)

// completePulledImage is the post-download pipeline. A plain pull stops after
// producing the tar file; --load imports it; --to imports, tags, validates the
// target registry, and pushes with Docker-compatible registry auth.
func (r *PullRunner) completePulledImage(outputFile string, info *ImageInfo, opts PullOptions) error {
	log.Printf("镜像拉取成功: %s", outputFile)
	if !opts.Load && opts.To == "" {
		return nil
	}

	var target string
	if opts.To != "" {
		var err error
		target, err = resolvePushTarget(info, opts.To)
		if err != nil {
			return err
		}
		if err := r.checkPushTargetRegistry(opts.Context, target, opts); err != nil {
			return err
		}
	}

	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	progressOutput := opts.ProgressOutput
	if progressOutput == nil {
		progressOutput = io.Discard
	}
	log.Printf("Load pulled image: %s", outputFile)
	if err := r.loadPulledImage(ctx, outputFile, progressOutput); err != nil {
		return fmt.Errorf("导入镜像失败: %w", err)
	}
	log.Printf("镜像导入成功: %s", outputFile)

	if opts.To == "" {
		return nil
	}
	source := localImageRef(info)
	if err := ctx.Err(); err != nil {
		return err
	}
	log.Printf("Tag pulled image: %s -> %s", source, target)
	if err := r.tagPulledImage(ctx, source, target); err != nil {
		return fmt.Errorf("tag 镜像失败: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	log.Printf("Push pulled image: %s", target)
	registryAuth, err := r.dockerPushRegistryAuth(ctx, target, opts)
	if err != nil {
		return fmt.Errorf("解析 push registry 认证失败: %w", err)
	}
	if err := r.pushPulledImage(ctx, target, registryAuth, progressOutput); err != nil {
		return fmt.Errorf("push 镜像失败: %w", err)
	}
	log.Printf("镜像推送成功: %s", target)
	return nil
}

func (r *PullRunner) checkPushTargetRegistry(ctx context.Context, target string, opts PullOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	info, err := parseImageInfo(target)
	if err != nil {
		return fmt.Errorf("解析目标 registry 失败: %w", err)
	}
	registryName := info.Registry
	cred, credErr := r.loadPullRegistryCredential(ctx, registryName, opts.DockerConfig)
	targetOpts := opts
	targetOpts.PlainHTTP = pushTargetUsesPlainHTTP(opts)
	result := r.pingRegistryV2(ctx, registryName, targetOpts, cred, info)
	switch result.status {
	case registryPingOK:
		log.Printf("Push target registry check passed: registry=%s credential=%v", registryName, cred.Found)
		return nil
	case registryPingAuthRequired:
		if credErr != nil {
			return fmt.Errorf("目标 registry %s 需要认证，但读取 Docker 凭据失败: %w", registryName, credErr)
		}
		return fmt.Errorf("目标 registry %s 需要认证，但未找到 Docker 凭据；请先执行 docker login %s，或通过 --docker-config 指定配置", registryName, registryName)
	default:
		if credErr != nil {
			return fmt.Errorf("目标 registry %s 推送前检查失败: %s；同时读取 Docker 凭据失败: %w", registryName, result.message, credErr)
		}
		return fmt.Errorf("目标 registry %s 推送前检查失败: %s", registryName, result.message)
	}
}

func (r *PullRunner) dockerPushRegistryAuth(ctx context.Context, target string, opts PullOptions) (string, error) {
	info, err := parseImageInfo(target)
	if err != nil {
		return "", err
	}
	cred, err := r.loadPullRegistryCredential(ctx, info.Registry, opts.DockerConfig)
	if err != nil {
		return "", err
	}
	if !cred.Found {
		return "", nil
	}
	payload := map[string]string{
		"serveraddress": info.Registry,
	}
	if cred.IdentityToken != "" {
		payload["identitytoken"] = cred.IdentityToken
	} else {
		payload["username"] = cred.Username
		payload["password"] = cred.Password
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(data), nil
}

func localImageRef(info *ImageInfo) string {
	return fmt.Sprintf("%s:%s", imagePath(info), info.Tag)
}

func resolvePushTarget(info *ImageInfo, target string) (string, error) {
	target = strings.Trim(strings.TrimSpace(target), "/")
	if target == "" {
		return "", fmt.Errorf("--to 不能为空")
	}
	var err error
	target, err = stripPushTargetScheme(target)
	if err != nil {
		return "", err
	}
	if strings.Contains(target, "@") {
		return "", fmt.Errorf("--to 不支持 digest 目标: %s", target)
	}
	if isTaggedImageRef(target) {
		return validateImageRef(target)
	}

	registry, namespace, hasNamespace := strings.Cut(target, "/")
	var ref string
	if hasNamespace {
		ref = fmt.Sprintf("%s/%s/%s:%s", registry, strings.Trim(namespace, "/"), info.Image, info.Tag)
	} else {
		ref = fmt.Sprintf("%s/%s:%s", registry, imagePath(info), info.Tag)
	}
	return validateImageRef(ref)
}

func stripPushTargetScheme(target string) (string, error) {
	if !strings.Contains(target, "://") {
		return target, nil
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return "", fmt.Errorf("invalid --to target %q: %w", target, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("--to only supports http:// or https:// targets: %s", target)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("--to target is missing registry host: %s", target)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return "", fmt.Errorf("--to target does not support user info, query, or fragment: %s", target)
	}
	path := strings.Trim(parsed.EscapedPath(), "/")
	if path == "" {
		return parsed.Host, nil
	}
	return parsed.Host + "/" + path, nil
}

func pushTargetUsesPlainHTTP(opts PullOptions) bool {
	switch pushTargetScheme(opts.To) {
	case "http":
		return true
	case "https":
		return false
	default:
		return opts.PlainHTTP
	}
}

func pushTargetScheme(target string) string {
	target = strings.TrimSpace(target)
	if !strings.Contains(target, "://") {
		return ""
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Scheme)
}

func isTaggedImageRef(ref string) bool {
	lastSlash := strings.LastIndex(ref, "/")
	if lastSlash < 0 || strings.LastIndex(ref, ":") <= lastSlash {
		return false
	}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return false
	}
	_, ok := named.(reference.Tagged)
	return ok
}

func validateImageRef(ref string) (string, error) {
	if _, err := reference.ParseNormalizedNamed(ref); err != nil {
		return "", fmt.Errorf("无效目标镜像 %q: %w", ref, err)
	}
	return ref, nil
}

func loadImageTar(ctx context.Context, path string, output io.Writer) error {
	im, err := docker.NewImageManager()
	if err != nil {
		return err
	}
	return im.LoadWithContext(ctx, path, output)
}

func tagImage(ctx context.Context, source, target string) error {
	im, err := docker.NewImageManager()
	if err != nil {
		return err
	}
	return im.Tag(ctx, source, target)
}

func pushImage(ctx context.Context, target, registryAuth string, output io.Writer) error {
	im, err := docker.NewImageManager()
	if err != nil {
		return err
	}
	return im.PushWithAuthOutput(ctx, target, registryAuth, output)
}
