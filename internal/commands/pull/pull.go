package pull

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"docker-manager/internal/docker"

	"github.com/Yui100901/MyGo/file_utils"
	"github.com/Yui100901/MyGo/network/http_utils"
	"github.com/Yui100901/MyGo/struct_utils"
	"github.com/distribution/reference"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
)

//
// @Author yfy2001
// @Date 2025/7/18 09 46
//

const (
	defaultRegistry = "registry-1.docker.io"
	dockerHubDomain = "docker.io"
	// Docker schema v2 media types. OCI media types are provided by ocispec.
	dockerManifestV2     = "application/vnd.docker.distribution.manifest.v2+json"
	dockerManifestListV2 = "application/vnd.docker.distribution.manifest.list.v2+json"
	// 并发下载层的最大并发数
	maxLayerConcurrency = 4
	// HTTP retry/backoff config
	maxHTTPRetries = 3
	initialBackoff = 1 * time.Second
)

type targetPlatform struct {
	targetOS   string
	targetArch string
}

type ImageInfo struct {
	Registry   string
	Repository string
	Image      string
	Tag        string
	Digest     string
}

type PullOptions struct {
	Context        context.Context
	Output         string
	OutputDir      string
	Load           bool
	To             string
	DockerConfig   string
	PlainHTTP      bool
	ProgressOutput io.Writer
}

type CommandDefaults struct {
	Proxy     string
	TargetOS  string
	Arch      string
	OutputDir string
}

type PullRunner struct {
	platform            targetPlatform
	httpClient          *http_utils.HTTPClient
	loadPulledImage     func(ctx context.Context, path string, output io.Writer) error
	tagPulledImage      func(ctx context.Context, source, target string) error
	pushPulledImage     func(ctx context.Context, target string, output io.Writer) error
	runCredentialHelper func(ctx context.Context, helper, server string) (pullRegistryCredential, error)
}

func NewPullRunner(proxy, targetOS, arch string) (*PullRunner, error) {
	client, err := newPullHTTPClient(proxy)
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

func NewPullCommand() *cobra.Command {
	return NewPullCommandWithDefaults(nil)
}

func NewPullCommandWithDefaults(defaults func() CommandDefaults) *cobra.Command {
	var imageNameList []string
	var targetOS string
	var arch string
	var proxy string
	var output string
	var outputDir string
	var load bool
	var to string
	var dockerConfig string
	var plainHTTP bool
	var verboseHTTP bool
	cmd := &cobra.Command{
		Use:   "pull <images...>",
		Short: "无需 Docker 客户端下载 Docker 镜像",
		Long: `无需 Docker 客户端下载 Docker 镜像，从官方镜像源拉取。
默认使用 HTTP_PROXY/HTTPS_PROXY 环境变量代理；未设置则直连。可通过 --proxy 强制指定代理。
默认拉取 linux/amd64 镜像。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()

			applyCommandDefaults(cmd, defaults, &proxy, &targetOS, &arch, &outputDir)
			configureHTTPLogging(verboseHTTP)
			runner, err := NewPullRunner(proxy, targetOS, arch)
			if err != nil {
				return fmt.Errorf("配置代理失败: %w", err)
			}
			if output != "" && len(args) > 1 {
				return fmt.Errorf("--output 只能在拉取单个镜像时使用，请改用 --output-dir")
			}
			imageNameList = args
			opts := PullOptions{
				Context:        ctx,
				Output:         output,
				OutputDir:      outputDir,
				Load:           load,
				To:             to,
				DockerConfig:   dockerConfig,
				PlainHTTP:      plainHTTP,
				ProgressOutput: cmd.OutOrStdout(),
			}
			var pullErrs []error
			success := 0
			total := len(imageNameList)
			log.Printf("Pull images: total=%d os=%s arch=%s output=%s outputDir=%s to=%s plainHTTP=%v", total, targetOS, arch, output, outputDir, to, plainHTTP)
			for i, imageName := range imageNameList {
				log.Printf("Pull image [%d/%d]: %s", i+1, total, imageName)
				if err := runner.getImage(imageName, opts); err != nil {
					log.Printf("%s 拉取失败: %v", imageName, err)
					pullErrs = append(pullErrs, fmt.Errorf("%s: %w", imageName, err))
					continue
				}
				success++
			}
			log.Printf("Pull summary: total=%d success=%d failed=%d", total, success, len(pullErrs))
			return errors.Join(pullErrs...)
		},
	}
	cmd.Flags().StringVarP(&targetOS, "os", "", "linux", "目标操作系统")
	cmd.Flags().StringVarP(&arch, "arch", "a", "amd64", "目标架构")
	cmd.Flags().StringVar(&proxy, "proxy", "", "强制指定 HTTP 代理，例如 http://127.0.0.1:7890；为空时使用环境变量代理")
	cmd.Flags().StringVarP(&output, "output", "o", "", "输出 tar 文件路径，仅支持单个镜像")
	cmd.Flags().StringVar(&outputDir, "output-dir", ".", "输出 tar 文件目录")
	cmd.Flags().BoolVar(&load, "load", false, "拉取并打包完成后自动导入 Docker")
	cmd.Flags().BoolVar(&verboseHTTP, "verbose-http", false, "输出底层 HTTP 请求调试日志")
	cmd.Flags().StringVar(&to, "to", "", "pull 后导入 Docker、tag 并 push 到目标 registry/repository")
	cmd.Flags().StringVar(&dockerConfig, "docker-config", "", "Docker config.json 路径，默认使用 DOCKER_CONFIG/config.json 或 ~/.docker/config.json")
	cmd.Flags().BoolVar(&plainHTTP, "plain-http", false, "使用 http:// 拉取 registry，适用于未启用 TLS 的内网 registry")
	_ = cmd.RegisterFlagCompletionFunc("os", completePullValues("linux", "windows"))
	_ = cmd.RegisterFlagCompletionFunc("arch", completePullValues("amd64", "arm64", "arm", "386", "ppc64le", "s390x"))
	return cmd
}

func completePullValues(values ...string) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		var suggestions []string
		for _, value := range values {
			if strings.HasPrefix(value, toComplete) {
				suggestions = append(suggestions, value)
			}
		}
		return suggestions, cobra.ShellCompDirectiveNoFileComp
	}
}

func applyCommandDefaults(cmd *cobra.Command, defaults func() CommandDefaults, proxy, targetOS, arch, outputDir *string) {
	if defaults == nil {
		return
	}
	cfg := defaults()
	flags := cmd.Flags()
	if cfg.Proxy != "" && !flags.Changed("proxy") {
		*proxy = cfg.Proxy
	}
	if cfg.TargetOS != "" && !flags.Changed("os") {
		*targetOS = cfg.TargetOS
	}
	if cfg.Arch != "" && !flags.Changed("arch") {
		*arch = cfg.Arch
	}
	if cfg.OutputDir != "" && !flags.Changed("output-dir") {
		*outputDir = cfg.OutputDir
	}
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
	log.Println(manifest)
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
	if err := r.pushPulledImage(ctx, target, progressOutput); err != nil {
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
	result := r.pingRegistryV2(ctx, registryName, opts, cred, info)
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

type registryPingStatus string

const (
	registryPingOK           registryPingStatus = "ok"
	registryPingAuthRequired registryPingStatus = "auth-required"
	registryPingFailed       registryPingStatus = "failed"
)

type registryPingResult struct {
	status     registryPingStatus
	message    string
	httpStatus int
}

func (r *PullRunner) pingRegistryV2(ctx context.Context, registryName string, opts PullOptions, cred pullRegistryCredential, info *ImageInfo) registryPingResult {
	scheme := "https"
	if opts.PlainHTTP {
		scheme = "http"
	}
	rawURL := fmt.Sprintf("%s://%s/v2/", scheme, registryName)
	result := r.pingRegistryV2Once(ctx, rawURL, nil)
	if result.status == registryPingOK {
		return result
	}
	if result.httpStatus != http.StatusUnauthorized {
		return result
	}
	if !cred.Found {
		return registryPingResult{status: registryPingAuthRequired, httpStatus: result.httpStatus, message: "registry 可访问但需要认证"}
	}
	auth, err := r.resolveRegistryAuth(ctx, result.message, info, opts)
	if err != nil {
		return registryPingResult{status: registryPingFailed, httpStatus: result.httpStatus, message: err.Error()}
	}
	retry := r.pingRegistryV2Once(ctx, rawURL, auth)
	if retry.status == registryPingOK {
		return retry
	}
	return registryPingResult{status: registryPingFailed, httpStatus: retry.httpStatus, message: "已配置凭据未被 registry /v2/ 接受: " + retry.message}
}

func (r *PullRunner) pingRegistryV2Once(ctx context.Context, rawURL string, auth *pullRegistryAuth) registryPingResult {
	req, err := buildGETRequest(ctx, rawURL, authHeaders(nil, auth), nil)
	if err != nil {
		return registryPingResult{status: registryPingFailed, message: err.Error()}
	}
	resp, err := r.httpClient.Client.Do(req)
	if err != nil {
		return registryPingResult{status: registryPingFailed, message: err.Error()}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return registryPingResult{status: registryPingOK, httpStatus: resp.StatusCode, message: resp.Status}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return registryPingResult{status: registryPingAuthRequired, httpStatus: resp.StatusCode, message: resp.Header.Get("WWW-Authenticate")}
	}
	return registryPingResult{status: registryPingFailed, httpStatus: resp.StatusCode, message: resp.Status}
}

func localImageRef(info *ImageInfo) string {
	return fmt.Sprintf("%s:%s", imagePath(info), info.Tag)
}

func resolvePushTarget(info *ImageInfo, target string) (string, error) {
	target = strings.Trim(strings.TrimSpace(target), "/")
	if target == "" {
		return "", fmt.Errorf("--to 不能为空")
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

func newPullHTTPClient(proxy string) (*http_utils.HTTPClient, error) {
	proxyFunc, err := proxyFuncFromSetting(proxy)
	if err != nil {
		return nil, err
	}

	transport := &http.Transport{Proxy: proxyFunc}
	return &http_utils.HTTPClient{
		Client: &http.Client{
			Transport: transport,
			Timeout:   600 * time.Second,
		},
	}, nil
}

func parseImageInfo(imageName string) (*ImageInfo, error) {
	spec := &ImageInfo{
		Registry: defaultRegistry,
		Tag:      "latest",
	}

	named, err := reference.ParseNormalizedNamed(imageName)
	if err != nil {
		return nil, err
	}

	domain := reference.Domain(named)
	if domain != "" && domain != dockerHubDomain {
		spec.Registry = domain
	}

	path := reference.Path(named)
	spec.Repository, spec.Image = splitRepositoryImage(path)

	if tagged, ok := named.(reference.Tagged); ok {
		spec.Tag = tagged.Tag()
	}
	if digested, ok := named.(reference.Digested); ok {
		spec.Digest = digested.Digest().String()
	}

	return spec, nil
}

func (r *PullRunner) fetchManifest(ctx context.Context, info *ImageInfo, opts PullOptions) (*ocispec.Manifest, *pullRegistryAuth, error) {
	manifestURL := registryAPIURL(opts, info, "manifests", getReference(info))
	headers := map[string]string{
		"Accept": strings.Join([]string{
			dockerManifestV2,
			dockerManifestListV2,
			ocispec.MediaTypeImageManifest,
			ocispec.MediaTypeImageIndex,
		}, ", "),
	}

	respBytes, auth, err := r.fetchRegistryBytesWithRetry(ctx, manifestURL, headers, nil, info, opts, nil)
	if err != nil {
		return nil, auth, fmt.Errorf("获取清单失败: %w", err)
	}

	isIndex, err := isManifestIndex(respBytes)
	if err != nil {
		return nil, auth, fmt.Errorf("解析清单类型失败: %w", err)
	}
	if isIndex {
		index, err := struct_utils.UnmarshalData[ocispec.Index](respBytes, struct_utils.JSON)
		if err != nil {
			return nil, auth, fmt.Errorf("解析多架构清单失败: %w", err)
		}
		return r.handleOCIIndex(ctx, info, index, auth, opts)
	}

	manifest, err := struct_utils.UnmarshalData[ocispec.Manifest](respBytes, struct_utils.JSON)
	return manifest, auth, err
}

func getReference(info *ImageInfo) string {
	if info.Digest != "" {
		return info.Digest
	}
	return info.Tag
}

func (r *PullRunner) handleOCIIndex(ctx context.Context, info *ImageInfo, index *ocispec.Index, auth *pullRegistryAuth, opts PullOptions) (*ocispec.Manifest, *pullRegistryAuth, error) {
	log.Println("[+] 检测到多架构镜像索引")
	var selectedDigest string

	for _, m := range index.Manifests {
		if m.Platform != nil &&
			m.Platform.OS == r.platform.targetOS &&
			m.Platform.Architecture == r.platform.targetArch {
			selectedDigest = string(m.Digest)
			break
		}
	}

	if selectedDigest == "" {
		return nil, auth, fmt.Errorf("未找到匹配的平台: %s/%s", r.platform.targetOS, r.platform.targetArch)
	}

	manifestURL := registryAPIURL(opts, info, "manifests", selectedDigest)
	headers := map[string]string{
		"Accept": strings.Join([]string{
			dockerManifestV2,
			ocispec.MediaTypeImageManifest,
		}, ", "),
	}

	resp, auth, err := r.fetchRegistryBytesWithRetry(ctx, manifestURL, headers, nil, info, opts, auth)
	if err != nil {
		return nil, auth, fmt.Errorf("获取架构清单失败: %w", err)
	}

	manifest, err := struct_utils.UnmarshalData[ocispec.Manifest](resp, struct_utils.JSON)
	return manifest, auth, err
}

func prepareWorkspace(info *ImageInfo) (string, error) {
	pattern := fmt.Sprintf("%s_%s", info.Image, info.Tag)
	return os.MkdirTemp(".", pattern)
}

// 改进版：使用 errgroup 管理并发下载，加入并发上限
func (r *PullRunner) downloadLayers(ctx context.Context, info *ImageInfo, manifest *ocispec.Manifest, auth *pullRegistryAuth, opts PullOptions, tempDir string) error {
	g, ctx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, maxLayerConcurrency)

	for _, layer := range manifest.Layers {
		l := layer // 避免闭包引用同一个变量
		// acquire
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		g.Go(func() error {
			defer func() { <-sem }()
			return r.downloadLayer(ctx, info, l, auth, opts, tempDir)
		})
	}

	// 等待所有 goroutine 完成，如果有错误会返回第一个错误
	if err := g.Wait(); err != nil {
		return fmt.Errorf("层下载失败: %w", err)
	}
	return nil
}

func (r *PullRunner) downloadConfig(ctx context.Context, info *ImageInfo, manifest *ocispec.Manifest, auth *pullRegistryAuth, opts PullOptions, tempDir string) error {
	configURL := registryAPIURL(opts, info, "blobs", string(manifest.Config.Digest))
	digest := strings.TrimPrefix(string(manifest.Config.Digest), "sha256:")
	if digest == string(manifest.Config.Digest) {
		// 没有前缀，尝试再替换常见前缀
		digest = strings.TrimPrefix(digest, "sha:")
	}
	configPath := filepath.Join(tempDir, digest+".json")

	_, err := r.saveRegistryFileWithRetry(ctx, configURL, nil, nil, info, opts, auth, configPath)
	return err
}

func (r *PullRunner) downloadLayer(ctx context.Context, info *ImageInfo, layer ocispec.Descriptor, auth *pullRegistryAuth, opts PullOptions, tempDir string) error {
	layerURL := registryAPIURL(opts, info, "blobs", string(layer.Digest))
	layerID := sha256Hash(string(layer.Digest))
	layerDir := filepath.Join(tempDir, layerID)
	if err := os.MkdirAll(layerDir, 0755); err != nil {
		return fmt.Errorf("创建层目录失败: %w", err)
	}

	// 下载层文件
	gzPath := filepath.Join(layerDir, "layer.tar.gz")

	if _, err := r.saveRegistryFileWithRetry(ctx, layerURL, nil, nil, info, opts, auth, gzPath); err != nil {
		return fmt.Errorf("下载层失败: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := verifyFileDigest(gzPath, layer.Digest); err != nil {
		return fmt.Errorf("校验层 digest 失败: %w", err)
	}

	// 解压文件
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := file_utils.DecompressGzip(gzPath, filepath.Join(layerDir, "layer.tar")); err != nil {
		return fmt.Errorf("解压失败: %w", err)
	}

	return os.Remove(gzPath)
}

type pullRegistryAuth struct {
	Authorization string
}

type pullRegistryCredential struct {
	Found         bool
	Username      string
	Password      string
	IdentityToken string
	Source        string
	Message       string
}

type pullDockerConfigFile struct {
	Auths       map[string]pullDockerAuthEntry `json:"auths"`
	CredsStore  string                         `json:"credsStore"`
	CredHelpers map[string]string              `json:"credHelpers"`
}

type pullDockerAuthEntry struct {
	Auth          string `json:"auth"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
}

type pullCredentialHelperResponse struct {
	ServerURL string `json:"ServerURL"`
	Username  string `json:"Username"`
	Secret    string `json:"Secret"`
}

type authChallenge struct {
	Scheme string
	Params map[string]string
}

type httpStatusError struct {
	StatusCode int
	Status     string
	Header     http.Header
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("HTTP %d %s", e.StatusCode, e.Status)
}

func registryAPIURL(opts PullOptions, info *ImageInfo, kind, ref string) string {
	scheme := "https"
	if opts.PlainHTTP {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s/v2/%s/%s/%s", scheme, info.Registry, imagePath(info), kind, ref)
}

func (r *PullRunner) fetchRegistryBytesWithRetry(ctx context.Context, rawURL string, headers map[string]string, query map[string]string, info *ImageInfo, opts PullOptions, auth *pullRegistryAuth) ([]byte, *pullRegistryAuth, error) {
	var lastErr error
	currentAuth := auth
	backoff := initialBackoff
	for i := 0; i < maxHTTPRetries; i++ {
		if err := ctx.Err(); err != nil {
			return nil, currentAuth, err
		}
		body, nextAuth, err := r.fetchRegistryBytesOnce(ctx, rawURL, headers, query, info, opts, currentAuth)
		if err == nil {
			return body, nextAuth, nil
		}
		currentAuth = nextAuth
		lastErr = err
		log.Printf("请求 %s 失败（尝试 %d/%d）: %v，稍后重试...", rawURL, i+1, maxHTTPRetries, err)
		if err := sleepWithContext(ctx, backoff); err != nil {
			return nil, currentAuth, err
		}
		backoff *= 2
	}
	return nil, currentAuth, lastErr
}

func (r *PullRunner) fetchRegistryBytesOnce(ctx context.Context, rawURL string, headers map[string]string, query map[string]string, info *ImageInfo, opts PullOptions, auth *pullRegistryAuth) ([]byte, *pullRegistryAuth, error) {
	body, err := r.httpGetBytesWithStatus(ctx, rawURL, authHeaders(headers, auth), query)
	if err == nil {
		return body, auth, nil
	}
	statusErr, ok := err.(*httpStatusError)
	if !ok || statusErr.StatusCode != http.StatusUnauthorized {
		return nil, auth, err
	}
	nextAuth, err := r.resolveRegistryAuth(ctx, statusErr.Header.Get("WWW-Authenticate"), info, opts)
	if err != nil {
		return nil, auth, err
	}
	body, err = r.httpGetBytesWithStatus(ctx, rawURL, authHeaders(headers, nextAuth), query)
	if err != nil {
		return nil, nextAuth, err
	}
	return body, nextAuth, nil
}

func (r *PullRunner) saveRegistryFileWithRetry(ctx context.Context, rawURL string, headers map[string]string, query map[string]string, info *ImageInfo, opts PullOptions, auth *pullRegistryAuth, outputPath string) (*pullRegistryAuth, error) {
	var lastErr error
	currentAuth := auth
	backoff := initialBackoff
	for i := 0; i < maxHTTPRetries; i++ {
		if err := ctx.Err(); err != nil {
			_ = removePartialDownload(outputPath)
			return currentAuth, err
		}
		nextAuth, err := r.saveRegistryFileOnce(ctx, rawURL, headers, query, info, opts, currentAuth, outputPath)
		if err == nil {
			return nextAuth, nil
		}
		currentAuth = nextAuth
		_ = removePartialDownload(outputPath)
		lastErr = err
		log.Printf("保存 %s 到 %s 失败（尝试 %d/%d）: %v，稍后重试...", rawURL, outputPath, i+1, maxHTTPRetries, err)
		if err := sleepWithContext(ctx, backoff); err != nil {
			_ = removePartialDownload(outputPath)
			return currentAuth, err
		}
		backoff *= 2
	}
	return currentAuth, lastErr
}

func (r *PullRunner) saveRegistryFileOnce(ctx context.Context, rawURL string, headers map[string]string, query map[string]string, info *ImageInfo, opts PullOptions, auth *pullRegistryAuth, outputPath string) (*pullRegistryAuth, error) {
	err := r.httpSaveToFileWithStatus(ctx, rawURL, authHeaders(headers, auth), query, outputPath)
	if err == nil {
		return auth, nil
	}
	statusErr, ok := err.(*httpStatusError)
	if !ok || statusErr.StatusCode != http.StatusUnauthorized {
		return auth, err
	}
	nextAuth, err := r.resolveRegistryAuth(ctx, statusErr.Header.Get("WWW-Authenticate"), info, opts)
	if err != nil {
		return auth, err
	}
	err = r.httpSaveToFileWithStatus(ctx, rawURL, authHeaders(headers, nextAuth), query, outputPath)
	return nextAuth, err
}

func authHeaders(headers map[string]string, auth *pullRegistryAuth) map[string]string {
	result := map[string]string{}
	for key, value := range headers {
		result[key] = value
	}
	if auth != nil && auth.Authorization != "" {
		result["Authorization"] = auth.Authorization
	}
	return result
}

func (r *PullRunner) resolveRegistryAuth(ctx context.Context, header string, info *ImageInfo, opts PullOptions) (*pullRegistryAuth, error) {
	challenge := parseAuthChallenge(header)
	cred, credErr := r.loadPullRegistryCredential(ctx, info.Registry, opts.DockerConfig)
	switch strings.ToLower(challenge.Scheme) {
	case "bearer":
		token, err := r.fetchBearerToken(ctx, challenge, info, cred)
		if err != nil {
			if credErr != nil {
				return nil, fmt.Errorf("获取 Bearer token 失败: %w；读取 Docker 凭据也失败: %v", err, credErr)
			}
			return nil, err
		}
		return &pullRegistryAuth{Authorization: "Bearer " + token}, nil
	case "basic":
		if credErr != nil {
			return nil, credErr
		}
		if cred.Username == "" && cred.Password == "" {
			return nil, fmt.Errorf("registry %s 需要 Basic 认证，但未找到 Docker 凭据", info.Registry)
		}
		return &pullRegistryAuth{Authorization: basicAuthHeader(cred.Username, cred.Password)}, nil
	default:
		if credErr == nil {
			if cred.IdentityToken != "" {
				return &pullRegistryAuth{Authorization: "Bearer " + cred.IdentityToken}, nil
			}
			if cred.Username != "" || cred.Password != "" {
				return &pullRegistryAuth{Authorization: basicAuthHeader(cred.Username, cred.Password)}, nil
			}
		}
		if strings.TrimSpace(header) == "" {
			return nil, fmt.Errorf("registry %s 返回 401 但没有 WWW-Authenticate challenge", info.Registry)
		}
		return nil, fmt.Errorf("不支持的 registry 认证方式 %q", challenge.Scheme)
	}
}

func parseAuthChallenge(header string) authChallenge {
	header = strings.TrimSpace(header)
	if header == "" {
		return authChallenge{Params: map[string]string{}}
	}
	scheme, rest, _ := strings.Cut(header, " ")
	return authChallenge{
		Scheme: strings.TrimSpace(scheme),
		Params: parseChallengeParams(rest),
	}
}

func parseChallengeParams(input string) map[string]string {
	params := map[string]string{}
	for len(input) > 0 {
		input = strings.TrimLeft(input, " ,")
		if input == "" {
			break
		}
		key, rest, ok := strings.Cut(input, "=")
		if !ok {
			break
		}
		key = strings.TrimSpace(key)
		rest = strings.TrimLeft(rest, " ")
		var value string
		if strings.HasPrefix(rest, "\"") {
			value, rest = readQuotedChallengeValue(rest[1:])
		} else {
			value, rest, _ = strings.Cut(rest, ",")
		}
		if key != "" {
			params[strings.ToLower(key)] = value
		}
		input = rest
	}
	return params
}

func readQuotedChallengeValue(input string) (string, string) {
	var sb strings.Builder
	escaped := false
	for i, r := range input {
		if escaped {
			sb.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r == '"' {
			return sb.String(), input[i+1:]
		}
		sb.WriteRune(r)
	}
	return sb.String(), ""
}

func (r *PullRunner) fetchBearerToken(ctx context.Context, challenge authChallenge, info *ImageInfo, cred pullRegistryCredential) (string, error) {
	realm := challenge.Params["realm"]
	if realm == "" {
		return "", fmt.Errorf("Bearer challenge 缺少 realm")
	}
	query := map[string]string{}
	if service := challenge.Params["service"]; service != "" {
		query["service"] = service
	}
	scope := challenge.Params["scope"]
	if scope == "" {
		scope = fmt.Sprintf("repository:%s:pull", imagePath(info))
	}
	query["scope"] = scope
	headers := map[string]string{}
	if cred.IdentityToken != "" {
		headers["Authorization"] = "Bearer " + cred.IdentityToken
	} else if cred.Username != "" || cred.Password != "" {
		headers["Authorization"] = basicAuthHeader(cred.Username, cred.Password)
	}
	respBytes, err := r.fetchWithRetry(ctx, realm, headers, query)
	if err != nil {
		return "", fmt.Errorf("认证请求失败: %w", err)
	}
	var token struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(respBytes, &token); err != nil {
		return "", fmt.Errorf("解析 token 失败: %w", err)
	}
	if token.Token != "" {
		return token.Token, nil
	}
	if token.AccessToken != "" {
		return token.AccessToken, nil
	}
	return "", fmt.Errorf("认证响应不包含 token")
}

func (r *PullRunner) loadPullRegistryCredential(ctx context.Context, registryName, configPath string) (pullRegistryCredential, error) {
	if configPath == "" {
		configPath = defaultPullDockerConfigPath()
	}
	cfg, err := readPullDockerConfig(configPath)
	if err != nil {
		return pullRegistryCredential{}, err
	}
	keys := pullRegistryConfigKeys(registryName)
	if helper, server := findPullCredentialHelper(cfg, keys); helper != "" {
		cred, err := r.runCredentialHelper(ctx, helper, server)
		if err != nil {
			return pullRegistryCredential{Source: "credential-helper", Message: err.Error()}, err
		}
		cred.Found = true
		cred.Source = "credential-helper"
		return cred, nil
	}
	for _, key := range keys {
		entry, ok := cfg.Auths[key]
		if !ok {
			continue
		}
		cred := pullCredentialFromAuthEntry(entry)
		cred.Found = cred.Username != "" || cred.Password != "" || cred.IdentityToken != ""
		cred.Source = "auths"
		return cred, nil
	}
	return pullRegistryCredential{}, nil
}

func defaultPullDockerConfigPath() string {
	if dir := strings.TrimSpace(os.Getenv("DOCKER_CONFIG")); dir != "" {
		return filepath.Join(dir, "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".docker", "config.json")
	}
	return filepath.Join(home, ".docker", "config.json")
}

func readPullDockerConfig(path string) (pullDockerConfigFile, error) {
	var cfg pullDockerConfigFile
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func findPullCredentialHelper(cfg pullDockerConfigFile, keys []string) (string, string) {
	for _, key := range keys {
		if helper := strings.TrimSpace(cfg.CredHelpers[key]); helper != "" {
			return helper, key
		}
	}
	if helper := strings.TrimSpace(cfg.CredsStore); helper != "" {
		return helper, keys[0]
	}
	return "", ""
}

func pullRegistryConfigKeys(registryName string) []string {
	keys := []string{
		registryName,
		"https://" + registryName,
		"http://" + registryName,
		"https://" + registryName + "/v1/",
	}
	if registryName == "docker.io" || registryName == "registry-1.docker.io" || registryName == "index.docker.io" {
		keys = append(keys, "https://index.docker.io/v1/", "index.docker.io", "docker.io", "registry-1.docker.io")
	}
	return uniquePullStrings(keys)
}

func pullCredentialFromAuthEntry(entry pullDockerAuthEntry) pullRegistryCredential {
	cred := pullRegistryCredential{
		Username:      entry.Username,
		Password:      entry.Password,
		IdentityToken: entry.IdentityToken,
	}
	if cred.Username == "" && cred.Password == "" && entry.Auth != "" {
		decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
		if err == nil {
			username, password, ok := strings.Cut(string(decoded), ":")
			if ok {
				cred.Username = username
				cred.Password = password
			}
		}
	}
	return cred
}

func defaultRunPullCredentialHelper(ctx context.Context, helper, server string) (pullRegistryCredential, error) {
	cmd := exec.CommandContext(ctx, "docker-credential-"+helper, "get")
	cmd.Stdin = strings.NewReader(server)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return pullRegistryCredential{}, fmt.Errorf("docker-credential-%s get failed: %s", helper, msg)
	}
	var resp pullCredentialHelperResponse
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return pullRegistryCredential{}, err
	}
	cred := pullRegistryCredential{Username: resp.Username, Password: resp.Secret}
	if resp.Username == "<token>" {
		cred.Username = ""
		cred.Password = ""
		cred.IdentityToken = resp.Secret
	}
	return cred, nil
}

func basicAuthHeader(username, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}

func uniquePullStrings(values []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

// fetchWithRetry 会带简单的重试和指数退避，返回 body bytes
func fetchWithRetry(ctx context.Context, url string, headers map[string]string, query map[string]string) ([]byte, error) {
	runner, err := NewPullRunner("", "linux", "amd64")
	if err != nil {
		return nil, err
	}
	return runner.fetchWithRetry(ctx, url, headers, query)
}

// saveWithRetry 将 GET 请求的响应直接保存到文件，带重试
func saveWithRetry(ctx context.Context, url string, headers map[string]string, query map[string]string, outputPath string) error {
	runner, err := NewPullRunner("", "linux", "amd64")
	if err != nil {
		return err
	}
	return runner.saveWithRetry(ctx, url, headers, query, outputPath)
}

func (r *PullRunner) fetchWithRetry(ctx context.Context, url string, headers map[string]string, query map[string]string) ([]byte, error) {
	var lastErr error
	backoff := initialBackoff
	for i := 0; i < maxHTTPRetries; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		resp, err := r.httpGetBytes(ctx, url, headers, query)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		log.Printf("请求 %s 失败（尝试 %d/%d）: %v，稍后重试...", url, i+1, maxHTTPRetries, err)
		if err := sleepWithContext(ctx, backoff); err != nil {
			return nil, err
		}
		backoff *= 2
	}
	return nil, lastErr
}

func (r *PullRunner) saveWithRetry(ctx context.Context, url string, headers map[string]string, query map[string]string, outputPath string) error {
	var lastErr error
	backoff := initialBackoff
	for i := 0; i < maxHTTPRetries; i++ {
		if err := ctx.Err(); err != nil {
			_ = removePartialDownload(outputPath)
			return err
		}
		if err := r.httpSaveToFile(ctx, url, headers, query, outputPath); err == nil {
			return nil
		} else {
			_ = removePartialDownload(outputPath)
			lastErr = err
			log.Printf("保存 %s 到 %s 失败（尝试 %d/%d）: %v，稍后重试...", url, outputPath, i+1, maxHTTPRetries, err)
			if err := sleepWithContext(ctx, backoff); err != nil {
				_ = removePartialDownload(outputPath)
				return err
			}
			backoff *= 2
		}
	}
	return lastErr
}

func (r *PullRunner) httpGetBytes(ctx context.Context, rawURL string, headers map[string]string, query map[string]string) ([]byte, error) {
	return r.httpGetBytesWithStatus(ctx, rawURL, headers, query)
}

func (r *PullRunner) httpGetBytesWithStatus(ctx context.Context, rawURL string, headers map[string]string, query map[string]string) ([]byte, error) {
	req, err := buildGETRequest(ctx, rawURL, headers, query)
	if err != nil {
		return nil, err
	}
	resp, err := r.httpClient.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			log.Printf("警告: 关闭 HTTP response body 失败: %v", cerr)
		}
	}()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, &httpStatusError{StatusCode: resp.StatusCode, Status: resp.Status, Header: resp.Header.Clone()}
	}
	return io.ReadAll(resp.Body)
}

func (r *PullRunner) httpSaveToFile(ctx context.Context, rawURL string, headers map[string]string, query map[string]string, outputPath string) error {
	return r.httpSaveToFileWithStatus(ctx, rawURL, headers, query, outputPath)
}

func (r *PullRunner) httpSaveToFileWithStatus(ctx context.Context, rawURL string, headers map[string]string, query map[string]string, outputPath string) error {
	req, err := buildGETRequest(ctx, rawURL, headers, query)
	if err != nil {
		return err
	}
	resp, err := r.httpClient.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			log.Printf("警告: 关闭 HTTP response body 失败: %v", cerr)
		}
	}()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return &httpStatusError{StatusCode: resp.StatusCode, Status: resp.Status, Header: resp.Header.Clone()}
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		return err
	}

	partPath := partialDownloadPath(outputPath)
	file, err := os.Create(partPath)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, resp.Body)
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(partPath)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(partPath)
		return closeErr
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(partPath)
		return err
	}
	_ = os.Remove(outputPath)
	return os.Rename(partPath, outputPath)
}

func buildGETRequest(ctx context.Context, rawURL string, headers map[string]string, query map[string]string) (*http.Request, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	values := parsedURL.Query()
	for key, value := range query {
		values.Set(key, value)
	}
	parsedURL.RawQuery = values.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return req, nil
}

func sleepWithContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func removePartialDownload(outputPath string) error {
	return os.Remove(partialDownloadPath(outputPath))
}

func partialDownloadPath(outputPath string) string {
	return outputPath + ".part"
}

func createManifestFile(info *ImageInfo, manifest *ocispec.Manifest, tempDir string) error {
	manifestContent := []*ImageManifest{
		{
			Config:   strings.TrimPrefix(string(manifest.Config.Digest), "sha256:") + ".json",
			Layers:   getLayerPaths(manifest.Layers),
			RepoTags: []string{fmt.Sprintf("%s:%s", imagePath(info), info.Tag)},
		},
	}

	data, err := struct_utils.MarshalData(manifestContent, struct_utils.JSON)
	if err != nil {
		return fmt.Errorf("序列化清单失败: %w", err)
	}

	return os.WriteFile(filepath.Join(tempDir, "manifest.json"), data, 0644)
}

func getLayerPaths(layers []ocispec.Descriptor) []string {
	paths := make([]string, 0, len(layers))
	for _, layer := range layers {
		//这里不用filepath.Join
		//filepath.Join会导致在windows下的反斜杠在docker导入时无法识别
		paths = append(paths, fmt.Sprintf("%s/layer.tar", sha256Hash(string(layer.Digest))))
	}
	return paths
}

func packageImage(ctx context.Context, tempDir, outputFile string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := os.MkdirAll(filepath.Dir(outputFile), 0755); err != nil {
		return fmt.Errorf("创建输出目录失败: %w", err)
	}
	return createTarArchiveWithContext(ctx, tempDir, outputFile)
}

func createTarArchiveWithContext(ctx context.Context, sourceDir, outputFile string) error {
	partPath := partialDownloadPath(outputFile)
	_ = os.Remove(partPath)

	file, err := os.Create(partPath)
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(partPath)
		}
	}()

	tw := tar.NewWriter(file)
	walkErr := filepath.WalkDir(sourceDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == sourceDir {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if entry.IsDir() && !strings.HasSuffix(header.Name, "/") {
			header.Name += "/"
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() {
			if cerr := src.Close(); cerr != nil {
				log.Printf("璀﹀憡: 鍏抽棴鏂囦欢 %s 澶辫触: %v", path, cerr)
			}
		}()
		return copyWithContext(ctx, tw, src)
	})
	closeTarErr := tw.Close()
	closeFileErr := file.Close()
	if walkErr != nil {
		return walkErr
	}
	if closeTarErr != nil {
		return closeTarErr
	}
	if closeFileErr != nil {
		return closeFileErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_ = os.Remove(outputFile)
	if err := os.Rename(partPath, outputFile); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) error {
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

func pushImage(ctx context.Context, target string, output io.Writer) error {
	im, err := docker.NewImageManager()
	if err != nil {
		return err
	}
	return im.PushWithOutput(ctx, target, output)
}

func sha256Hash(input string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(input)))
}

func verifyFileDigest(path string, expected digest.Digest) error {
	if expected == "" {
		return nil
	}
	if err := expected.Validate(); err != nil {
		return err
	}

	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := file.Close(); cerr != nil {
			log.Printf("警告: 关闭文件 %s 失败: %v", path, cerr)
		}
	}()

	verifier := expected.Verifier()
	if _, err := io.Copy(verifier, file); err != nil {
		return err
	}
	if !verifier.Verified() {
		return fmt.Errorf("digest 校验失败 %s: 期望 %s", path, expected)
	}
	return nil
}

func resolveOutputFile(info *ImageInfo, opts PullOptions) (string, error) {
	if opts.Output != "" {
		return opts.Output, nil
	}

	outputDir := opts.OutputDir
	if outputDir == "" {
		outputDir = "."
	}
	return filepath.Join(outputDir, defaultOutputFileName(info)), nil
}

func defaultOutputFileName(info *ImageInfo) string {
	name := strings.ReplaceAll(imagePath(info), "/", "_")
	tag := sanitizeOutputName(info.Tag)
	if tag == "" {
		tag = "latest"
	}
	return fmt.Sprintf("%s_%s.tar", name, tag)
}

func sanitizeOutputName(name string) string {
	var sb strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' ||
			r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' {
			sb.WriteRune(r)
			continue
		}
		switch r {
		case '.', '-', '_':
			sb.WriteRune(r)
		default:
			sb.WriteRune('_')
		}
	}
	return sb.String()
}

func proxyFuncFromSetting(proxy string) (func(*http.Request) (*url.URL, error), error) {
	if proxy == "" {
		return proxyFromEnvironment, nil
	}

	proxyURL, err := url.Parse(proxy)
	if err != nil {
		return nil, fmt.Errorf("无效代理地址 %q: %w", proxy, err)
	}
	if proxyURL.Scheme == "" || proxyURL.Host == "" {
		return nil, fmt.Errorf("无效代理地址 %q: 必须包含 scheme 和 host，例如 http://127.0.0.1:7890", proxy)
	}
	return http.ProxyURL(proxyURL), nil
}

func proxyFromEnvironment(req *http.Request) (*url.URL, error) {
	if req == nil || req.URL == nil {
		return nil, nil
	}
	if shouldBypassProxy(req.URL.Hostname()) {
		return nil, nil
	}

	proxy := proxyEnvForScheme(req.URL.Scheme)
	if proxy == "" {
		return nil, nil
	}

	proxyURL, err := url.Parse(proxy)
	if err != nil {
		return nil, err
	}
	if proxyURL.Scheme == "" || proxyURL.Host == "" {
		return nil, fmt.Errorf("无效环境变量代理地址 %q: 必须包含 scheme 和 host", proxy)
	}
	return proxyURL, nil
}

func proxyEnvForScheme(scheme string) string {
	switch strings.ToLower(scheme) {
	case "https":
		return firstEnv("HTTPS_PROXY", "https_proxy")
	case "http":
		return firstEnv("HTTP_PROXY", "http_proxy")
	default:
		return ""
	}
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}

func shouldBypassProxy(host string) bool {
	noProxy := firstEnv("NO_PROXY", "no_proxy")
	if noProxy == "" || host == "" {
		return false
	}

	for _, item := range strings.Split(noProxy, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if item == "*" || item == host {
			return true
		}
		if strings.HasPrefix(item, ".") && strings.HasSuffix(host, item) {
			return true
		}
		if strings.HasPrefix(host, item+".") {
			return true
		}
	}
	return false
}

func splitRepositoryImage(path string) (string, string) {
	parts := strings.Split(path, "/")
	if len(parts) == 1 {
		return "", parts[0]
	}
	return strings.Join(parts[:len(parts)-1], "/"), parts[len(parts)-1]
}

func imagePath(info *ImageInfo) string {
	if info.Repository == "" {
		return info.Image
	}
	return info.Repository + "/" + info.Image
}

func isManifestIndex(data []byte) (bool, error) {
	var probe struct {
		MediaType string            `json:"mediaType"`
		Manifests []json.RawMessage `json:"manifests"`
		Config    *json.RawMessage  `json:"config"`
		Layers    []json.RawMessage `json:"layers"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return false, err
	}

	switch probe.MediaType {
	case ocispec.MediaTypeImageIndex, dockerManifestListV2:
		return true, nil
	case ocispec.MediaTypeImageManifest, dockerManifestV2:
		return false, nil
	}

	if len(probe.Manifests) > 0 {
		return true, nil
	}
	if probe.Config != nil || len(probe.Layers) > 0 {
		return false, nil
	}
	return false, nil
}
