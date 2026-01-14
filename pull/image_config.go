package pull

//
// @Author yfy2001
// @Date 2025/7/18 09 45
//

// ImageManifest 导出为tar包时，包含的清单文件
type ImageManifest struct {
	Config   string   `json:"Config"`
	Layers   []string `json:"Layers"`
	RepoTags []string `json:"RepoTags"`
}
