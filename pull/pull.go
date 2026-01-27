package pull

import (
	"crypto/sha256"
	"fmt"

	"github.com/Yui100901/MyGo/file_utils"
	"github.com/Yui100901/MyGo/network/http_utils"
	"github.com/Yui100901/MyGo/struct_utils"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"log"
	"net/http"
	"net/url"
	"os"
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
	defaultProxy    = "http://127.0.0.1:7890"
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

var platform targetPlatform

type ImageInfo struct {
	Registry   string
	Repository string
	Image      string
	Tag        string
	Digest     string
}

var httpClient *http_utils.HTTPClient

func init() {
	initHTTPClient()
}

func NewPullCommand() *cobra.Command {
	var imageNameList []string
	var targetOS string
	var arch string
	cmd := &cobra.Command{
		Use:   "pull <images...>",
		Short: "无需docker客户端，下载docker镜像",
		Long: `无需docker客户端，下载docker镜像，从官方镜像源拉取。
需要环境变量配置代理，默认代理为127.0.0.1:7890（clash）。
默认拉取linux/amd64镜像。`,
		Run: func(cmd *cobra.Command, args []string) {
			platform.targetOS = targetOS
			platform.targetArch = arch
			imageNameList = args
			for _, imageName := range imageNameList {
				getImage(imageName)
			}
		},
	}
	cmd.Flags().StringVarP(&targetOS, "os", "", "linux", "目标操作系统")
	cmd.Flags().StringVarP(&arch, "arch", "a", "amd64", "目标架构")
	return cmd
}

func getImage(imageName string) {
	imageInfo := parseImageInfo(imageName)
	log.Printf("获取镜像%s:%s,目标平台%s/%s", imageInfo.Image, imageInfo.Tag, platform.targetOS, platform.targetArch)

	token, err := getAuthToken(imageInfo)
	if err != nil {
		log.Printf("认证失败: %v", err)
		return
	}

	tempDir, err := prepareWorkspace(imageInfo)
	if err != nil {
		log.Printf("准备临时目录失败: %v", err)
		return
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			log.Printf("警告: 清理临时目录 %s 失败: %v", tempDir, err)
		}
	}()

	manifest, err := fetchManifest(imageInfo, token)
	log.Println(manifest)
	if err != nil {
		log.Printf("%s\n获取清单失败: %v", imageName, err)
		return
	}

	err = createManifestFile(imageInfo, manifest, tempDir)
	if err != nil {
		log.Printf("%s\n创建失败清单文件失败: %v", imageName, err)
		return
	}

	err = downloadConfig(imageInfo, manifest, token, tempDir)
	if err != nil {
		log.Printf("%s\n下载配置文件失败: %v", imageName, err)
		return
	}

	err = downloadLayers(imageInfo, manifest, token, tempDir)
	if err != nil {
		log.Printf("%s\n下载镜像层失败: %v", imageName, err)
		return
	}

	outputFile, err := packageImage(tempDir, imageInfo)
	if err != nil {
		log.Printf("%s\n打包镜像失败: %v", imageName, err)
		return
	}

	log.Printf("镜像拉取成功: %s", outputFile)
}

func initHTTPClient() {
	proxyURL, err := url.Parse(getProxyURL())
	if err != nil {
		log.Fatalf("无效代理地址: %v", err)
	}

	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	httpClient = &http_utils.HTTPClient{
		Client: &http.Client{
			Transport: transport,
			Timeout:   600 * time.Second,
		},
	}
}

func parseImageInfo(imageName string) *ImageInfo {
	spec := &ImageInfo{
		Registry: defaultRegistry,
		Tag:      "latest",
	}

	// 处理digest
	if parts := strings.SplitN(imageName, "@", 2); len(parts) == 2 {
		imageName = parts[0]
		spec.Digest = parts[1]
	}

	// 处理tag
	if parts := strings.SplitN(imageName, ":", 2); len(parts) == 2 {
		imageName = parts[0]
		spec.Tag = parts[1]
	}

	// 解析registry和repository
	parts := strings.Split(imageName, "/")
	switch {
	case len(parts) == 1:
		spec.Repository = "library"
		spec.Image = parts[0]
	case isRegistry(parts[0]):
		spec.Registry = parts[0]
		spec.Repository = strings.Join(parts[1:len(parts)-1], "/")
		spec.Image = parts[len(parts)-1]
	default:
		spec.Repository = strings.Join(parts[:len(parts)-1], "/")
		spec.Image = parts[len(parts)-1]
	}

	return spec
}

func isRegistry(part string) bool {
	return strings.Contains(part, ".") || strings.Contains(part, ":")
}

func getAuthToken(info *ImageInfo) (string, error) {
	authURL := "https://auth.docker.io/token"
	query := map[string]string{
		"service": "registry.docker.io",
		"scope":   fmt.Sprintf("repository:%s/%s:pull", info.Repository, info.Image),
	}

	respBytes, err := fetchWithRetry(authURL, nil, query)
	if err != nil {
		return "", fmt.Errorf("认证请求失败: %w", err)
	}

	type tokenResponse struct {
		Token string `json:"token"`
	}
	token, err := struct_utils.UnmarshalData[tokenResponse](respBytes, struct_utils.JSON)
	if err != nil {
		return "", fmt.Errorf("解析Token失败: %w", err)
	}

	return token.Token, nil
}

func fetchManifest(info *ImageInfo, token string) (*ocispec.Manifest, error) {
	manifestURL := fmt.Sprintf("https://%s/v2/%s/%s/manifests/%s",
		info.Registry,
		info.Repository,
		info.Image,
		getReference(info),
	)

	headers := map[string]string{
		"Authorization": fmt.Sprintf("Bearer %s", token),
		"Accept":        "application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.index.v1+json",
	}

	respBytes, err := fetchWithRetry(manifestURL, headers, nil)
	if err != nil {
		return nil, fmt.Errorf("获取清单失败: %w", err)
	}

	index, err := struct_utils.UnmarshalData[ocispec.Index](respBytes, struct_utils.JSON)
	if err == nil && index.SchemaVersion == 2 {
		return handleOCIIndex(info, index, token)
	}

	return struct_utils.UnmarshalData[ocispec.Manifest](respBytes, struct_utils.JSON)
}

func getReference(info *ImageInfo) string {
	if info.Digest != "" {
		return info.Digest
	}
	return info.Tag
}

func handleOCIIndex(info *ImageInfo, index *ocispec.Index, token string) (*ocispec.Manifest, error) {
	log.Println("[+] 检测到多架构镜像索引")
	var selectedDigest string

	for _, m := range index.Manifests {
		if m.Platform != nil &&
			m.Platform.OS == platform.targetOS &&
			m.Platform.Architecture == platform.targetArch {
			selectedDigest = string(m.Digest)
			break
		}
	}

	if selectedDigest == "" {
		return nil, fmt.Errorf("未找到匹配的平台: %s/%s", platform.targetOS, platform.targetArch)
	}

	manifestURL := fmt.Sprintf("https://%s/v2/%s/%s/manifests/%s",
		info.Registry,
		info.Repository,
		info.Image,
		selectedDigest,
	)

	headers := map[string]string{
		"Authorization": fmt.Sprintf("Bearer %s", token),
		"Accept":        "application/vnd.docker.distribution.manifest.v2+json",
	}

	resp, err := httpClient.Get(manifestURL, headers, nil).ReadBodyBytes()
	if err != nil {
		return nil, fmt.Errorf("获取架构清单失败: %w", err)
	}

	return struct_utils.UnmarshalData[ocispec.Manifest](resp, struct_utils.JSON)
}

func prepareWorkspace(info *ImageInfo) (string, error) {
	pattern := fmt.Sprintf("%s_%s", info.Image, info.Tag)
	return os.MkdirTemp(".", pattern)
}

// 改进版：使用 errgroup 管理并发下载，加入并发上限
func downloadLayers(info *ImageInfo, manifest *ocispec.Manifest, token, tempDir string) error {
	var g errgroup.Group
	sem := make(chan struct{}, maxLayerConcurrency)

	for _, layer := range manifest.Layers {
		l := layer // 避免闭包引用同一个变量
		// acquire
		sem <- struct{}{}
		g.Go(func() error {
			defer func() { <-sem }()
			return downloadLayer(info, l, token, tempDir)
		})
	}

	// 等待所有 goroutine 完成，如果有错误会返回第一个错误
	if err := g.Wait(); err != nil {
		return fmt.Errorf("层下载失败: %w", err)
	}
	return nil
}

func downloadConfig(info *ImageInfo, manifest *ocispec.Manifest, token, tempDir string) error {
	configURL := fmt.Sprintf("https://%s/v2/%s/%s/blobs/%s",
		info.Registry,
		info.Repository,
		info.Image,
		manifest.Config.Digest,
	)

	digest := strings.TrimPrefix(string(manifest.Config.Digest), "sha256:")
	if digest == string(manifest.Config.Digest) {
		// 没有前缀，尝试再替换常见前缀
		digest = strings.TrimPrefix(digest, "sha:")
	}
	configPath := filepath.Join(tempDir, digest+".json")
	headers := map[string]string{"Authorization": "Bearer " + token}

	return saveWithRetry(configURL, headers, nil, configPath)
}

func downloadLayer(info *ImageInfo, layer ocispec.Descriptor, token, tempDir string) error {
	layerURL := fmt.Sprintf("https://%s/v2/%s/%s/blobs/%s",
		info.Registry,
		info.Repository,
		info.Image,
		layer.Digest,
	)

	layerID := sha256Hash(string(layer.Digest))
	layerDir := filepath.Join(tempDir, layerID)
	if err := os.MkdirAll(layerDir, 0755); err != nil {
		return fmt.Errorf("创建层目录失败: %w", err)
	}

	// 下载层文件
	gzPath := filepath.Join(layerDir, "layer.tar.gz")
	headers := map[string]string{"Authorization": "Bearer " + token}

	if err := saveWithRetry(layerURL, headers, nil, gzPath); err != nil {
		return fmt.Errorf("下载层失败: %w", err)
	}

	// 解压文件
	if err := file_utils.DecompressGzip(gzPath, filepath.Join(layerDir, "layer.tar")); err != nil {
		return fmt.Errorf("解压失败: %w", err)
	}

	return os.Remove(gzPath)
}

// fetchWithRetry 会带简单的重试和指数退避，返回 body bytes
func fetchWithRetry(url string, headers map[string]string, query map[string]string) ([]byte, error) {
	var lastErr error
	backoff := initialBackoff
	for i := 0; i < maxHTTPRetries; i++ {
		resp, err := httpClient.Get(url, headers, query).ReadBodyBytes()
		if err == nil {
			return resp, nil
		}
		lastErr = err
		log.Printf("请求 %s 失败（尝试 %d/%d）: %v，稍后重试...", url, i+1, maxHTTPRetries, err)
		time.Sleep(backoff)
		backoff *= 2
	}
	return nil, lastErr
}

// saveWithRetry 将 GET 请求的响应直接保存到文件，带重试
func saveWithRetry(url string, headers map[string]string, query map[string]string, outputPath string) error {
	var lastErr error
	backoff := initialBackoff
	for i := 0; i < maxHTTPRetries; i++ {
		if err := httpClient.Get(url, headers, query).SaveToFile(outputPath); err == nil {
			return nil
		} else {
			lastErr = err
			log.Printf("保存 %s 到 %s 失败（尝试 %d/%d）: %v，稍后重试...", url, outputPath, i+1, maxHTTPRetries, err)
			time.Sleep(backoff)
			backoff *= 2
		}
	}
	return lastErr
}

func createManifestFile(info *ImageInfo, manifest *ocispec.Manifest, tempDir string) error {
	manifestContent := []*ImageManifest{
		{
			Config:   strings.TrimPrefix(string(manifest.Config.Digest), "sha256:") + ".json",
			Layers:   getLayerPaths(manifest.Layers),
			RepoTags: []string{fmt.Sprintf("%s/%s:%s", info.Repository, info.Image, info.Tag)},
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

func packageImage(tempDir string, info *ImageInfo) (string, error) {
	outputFile := fmt.Sprintf("%s_%s_%s.tar",
		strings.ReplaceAll(info.Repository, "/", "_"),
		info.Image,
		info.Tag,
	)
	return outputFile, file_utils.CreateTarArchive(tempDir, outputFile)
}

func sha256Hash(input string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(input)))
}

func getProxyURL() string {
	if proxy := os.Getenv("HTTP_PROXY"); proxy != "" {
		return proxy
	}
	return defaultProxy
}
