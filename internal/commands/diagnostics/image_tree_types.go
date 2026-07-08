package diagnostics

import "docker-manager/internal/commandflags"

type ImageTreeOptions struct {
	NoTrunc bool
	Top     int
	commandflags.FormatOptions
}

type ImageTreeReport struct {
	DockerEndpoint string           `json:"docker_endpoint"`
	ImageRef       string           `json:"image_ref"`
	ID             string           `json:"id"`
	RepoTags       []string         `json:"repo_tags,omitempty"`
	RepoDigests    []string         `json:"repo_digests,omitempty"`
	Platform       string           `json:"platform,omitempty"`
	Created        string           `json:"created,omitempty"`
	Size           int64            `json:"size"`
	RootFSType     string           `json:"rootfs_type,omitempty"`
	RootFSLayers   []string         `json:"rootfs_layers,omitempty"`
	HistorySize    int64            `json:"history_size"`
	LayerCount     int              `json:"layer_count"`
	MetadataCount  int              `json:"metadata_count"`
	LocalRefs      ImageLocalRefs   `json:"local_refs,omitempty"`
	UsedBy         []ImageUsageRef  `json:"used_by_containers,omitempty"`
	History        []ImageLayerInfo `json:"history"`
	LargestLayers  []ImageLayerInfo `json:"largest_layers,omitempty"`
}

type ImageLocalRefs struct {
	ID          string   `json:"id,omitempty"`
	RepoTags    []string `json:"repo_tags,omitempty"`
	RepoDigests []string `json:"repo_digests,omitempty"`
}

type ImageUsageRef struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Image   string `json:"image,omitempty"`
	ImageID string `json:"image_id,omitempty"`
	State   string `json:"state,omitempty"`
	Status  string `json:"status,omitempty"`
}

type ImageLayerInfo struct {
	Index       int      `json:"index"`
	ID          string   `json:"id"`
	Created     string   `json:"created,omitempty"`
	CreatedBy   string   `json:"created_by,omitempty"`
	Size        int64    `json:"size"`
	SizePercent float64  `json:"size_percent,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Comment     string   `json:"comment,omitempty"`
	Metadata    bool     `json:"metadata"`
}
