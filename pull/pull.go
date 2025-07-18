package pull

import (
	"crypto/sha256"
	"fmt"
	"github.com/Yui100901/MyGo/file_utils"
	"github.com/Yui100901/MyGo/network/http_utils"
	"github.com/Yui100901/MyGo/struct_utils"
	"github.com/spf13/cobra"
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
	defaultRegistry   = "registry-1.docker.io"
	defaultProxy      = "http://127.0.0.1:7890"
	defaultTargetOS   = "linux"
	defaultTargetArch = "amd64"
)

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
	cmd := &cobra.Command{
		Use:   "pull <images...>",
		Short: "无需docker客户端，下载docker镜像",
		Long: `无需docker客户端，下载docker镜像，从官方镜像源拉取。
需要环境变量配置代理，默认代理为127.0.0.1:7890（clash）。`,
		Run: func(cmd *cobra.Command, args []string) {
			imageNameList = args
			for _, imageName := range imageNameList {
				getImage(imageName)
			}
		},
	}
	return cmd
}

func getImage(imageName string) {
	imageInfo := parseImageInfo(imageName)
	log.Printf("获取镜像%s:%s", imageInfo.Image, imageInfo.Tag)

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
	defer os.RemoveAll(tempDir)

	manifest, err := fetchManifest(imageInfo, token)
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
			Timeout:   30 * time.Second,
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

	resp, err := httpClient.Get(authURL, nil, query).ReadBodyBytes()
	if err != nil {
		return "", fmt.Errorf("认证请求失败: %w", err)
	}

	type tokenResponse struct {
		Token string `json:"token"`
	}
	token, err := struct_utils.UnmarshalData[tokenResponse](resp, struct_utils.JSON)
	if err != nil {
		return "", fmt.Errorf("解析Token失败: %w", err)
	}

	return token.Token, nil
}

func fetchManifest(info *ImageInfo, token string) (*OCIImageManifest, error) {
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

	resp, err := httpClient.Get(manifestURL, headers, nil).ReadBodyBytes()
	if err != nil {
		return nil, fmt.Errorf("获取清单失败: %w", err)
	}

	index, err := struct_utils.UnmarshalData[OCIIndex](resp, struct_utils.JSON)
	if err == nil && index.SchemaVersion == 2 {
		return handleOCIIndex(info, index, token)
	}

	return struct_utils.UnmarshalData[OCIImageManifest](resp, struct_utils.JSON)
}

func getReference(info *ImageInfo) string {
	if info.Digest != "" {
		return info.Digest
	}
	return info.Tag
}

func handleOCIIndex(info *ImageInfo, index *OCIIndex, token string) (*OCIImageManifest, error) {
	log.Println("[+] 检测到多架构镜像索引")
	var selectedDigest string

	for _, m := range index.Manifests {
		if m.Platform != nil &&
			m.Platform.OS == defaultTargetOS &&
			m.Platform.Architecture == defaultTargetArch {
			selectedDigest = m.Digest
			break
		}
	}

	if selectedDigest == "" {
		return nil, fmt.Errorf("未找到匹配的平台: %s/%s", defaultTargetOS, defaultTargetArch)
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

	return struct_utils.UnmarshalData[OCIImageManifest](resp, struct_utils.JSON)
}

func prepareWorkspace(info *ImageInfo) (string, error) {
	pattern := fmt.Sprintf("%s_%s", info.Image, info.Tag)
	return os.MkdirTemp(".", pattern)
}

func downloadLayers(info *ImageInfo, manifest *OCIImageManifest, token, tempDir string) error {

	errChan := make(chan error, len(manifest.Layers))
	for _, layer := range manifest.Layers {
		go func(l Layer) {
			errChan <- downloadLayer(info, l, token, tempDir)
		}(layer)
	}

	for range manifest.Layers {
		if err := <-errChan; err != nil {
			return fmt.Errorf("层下载失败: %w", err)
		}
	}

	return nil
}

func downloadConfig(info *ImageInfo, manifest *OCIImageManifest, token, tempDir string) error {
	configURL := fmt.Sprintf("https://%s/v2/%s/%s/blobs/%s",
		info.Registry,
		info.Repository,
		info.Image,
		manifest.Config.Digest,
	)

	configPath := filepath.Join(tempDir, manifest.Config.Digest[7:]+".json")
	headers := map[string]string{"Authorization": "Bearer " + token}

	return httpClient.Get(configURL, headers, nil).SaveToFile(configPath)
}

func downloadLayer(info *ImageInfo, layer Layer, token, tempDir string) error {
	layerURL := fmt.Sprintf("https://%s/v2/%s/%s/blobs/%s",
		info.Registry,
		info.Repository,
		info.Image,
		layer.Digest,
	)

	layerID := sha256Hash(layer.Digest)
	layerDir := filepath.Join(tempDir, layerID)
	if err := os.Mkdir(layerDir, 0755); err != nil {
		return fmt.Errorf("创建层目录失败: %w", err)
	}

	// 下载层文件
	gzPath := filepath.Join(layerDir, "layer.tar.gz")
	headers := map[string]string{"Authorization": "Bearer " + token}

	if err := httpClient.Get(layerURL, headers, nil).SaveToFile(gzPath); err != nil {
		return fmt.Errorf("下载层失败: %w", err)
	}

	// 解压文件
	if err := file_utils.DecompressGzip(gzPath, filepath.Join(layerDir, "layer.tar")); err != nil {
		return fmt.Errorf("解压失败: %w", err)
	}

	return os.Remove(gzPath)
}

func createManifestFile(info *ImageInfo, manifest *OCIImageManifest, tempDir string) error {
	manifestContent := []*ImageManifest{
		{
			Config:   manifest.Config.Digest[7:] + ".json",
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

func getLayerPaths(layers []Layer) []string {
	paths := make([]string, 0, len(layers))
	for _, layer := range layers {
		//这里不用filepath.Join
		//filepath.Join会导致在windows下的反斜杠在docker导入时无法识别
		paths = append(paths, fmt.Sprintf("%s/layer.tar", sha256Hash(layer.Digest)))
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
