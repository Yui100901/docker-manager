package pull

//
// @Author yfy2001
// @Date 2025/7/18 09 45
//

// OCIIndex OCI标准容器目录
// 包含了不同架构的容器清单文件信息
type OCIIndex struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	Manifests     []OCIManifestInfo `json:"manifests"`
}

// OCIManifestInfo OCI标准容器清单文件
// 包含容器平台，以及摘要信息
type OCIManifestInfo struct {
	Annotations map[string]string `json:"annotations"`
	Digest      string            `json:"digest"`
	MediaType   string            `json:"mediaType"`
	Platform    *Platform         `json:"platform,omitempty"`
	Size        int               `json:"size"`
}

// Platform 平台
type Platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
}

// OCIImageManifest OCI标准容器清单文件
// 包含配置文件信息，以及镜像层信息
type OCIImageManifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	Config        Config            `json:"config"`
	Layers        []Layer           `json:"layers"`
	Annotations   map[string]string `json:"annotations"`
}

// Config 容器配置
type Config struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int    `json:"size"`
}

// Layer 镜像层
type Layer struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int    `json:"size"`
}

// ImageManifest 导出为tar包时，包含的清单文件
type ImageManifest struct {
	Config   string   `json:"Config"`
	Layers   []string `json:"Layers"`
	RepoTags []string `json:"RepoTags"`
}
