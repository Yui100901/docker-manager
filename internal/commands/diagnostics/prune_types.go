package diagnostics

import (
	"docker-manager/internal/commandflags"

	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/volume"
	mobyclient "github.com/moby/moby/client"
)

type PruneReportOptions struct {
	Apply         bool
	Confirm       bool
	Only          []string
	Filters       []string
	Until         string
	ProtectLabels []string
	commandflags.FormatOptions
}

type PruneReport struct {
	GeneratedAt       string               `json:"generated_at"`
	DockerEndpoint    string               `json:"docker_endpoint"`
	StoppedContainers []PruneContainerRef  `json:"stopped_containers,omitempty"`
	DanglingImages    []PruneImageRef      `json:"dangling_images,omitempty"`
	UnusedVolumes     []PruneVolumeRef     `json:"unused_volumes,omitempty"`
	BuildCaches       []PruneBuildCacheRef `json:"build_caches,omitempty"`
	EstimatedBytes    uint64               `json:"estimated_bytes"`
	Warnings          []string             `json:"warnings,omitempty"`
	Applied           bool                 `json:"applied"`
	Scope             PruneScope           `json:"scope"`
	ApplyResult       *PruneApplyResult    `json:"apply_result,omitempty"`
}

type PruneScope struct {
	Only          []string `json:"only,omitempty"`
	Filters       []string `json:"filters,omitempty"`
	Until         string   `json:"until,omitempty"`
	ProtectLabels []string `json:"protect_labels,omitempty"`
}

type pruneDiskUsage struct {
	LayersSize int64
	Images     []*image.Summary
	Containers []*container.Summary
	Volumes    []*volume.Volume
	BuildCache []*build.CacheRecord
}

type PruneContainerRef struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Image  string `json:"image,omitempty"`
	Status string `json:"status,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

type PruneImageRef struct {
	ID       string   `json:"id"`
	RepoTags []string `json:"repo_tags,omitempty"`
	Size     int64    `json:"size,omitempty"`
}

type PruneVolumeRef struct {
	Name     string `json:"name"`
	Driver   string `json:"driver,omitempty"`
	Size     int64  `json:"size,omitempty"`
	RefCount int64  `json:"ref_count"`
}

type PruneBuildCacheRef struct {
	ID          string `json:"id"`
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
	Size        int64  `json:"size,omitempty"`
}

type PruneApplyResult struct {
	ContainersDeleted  []string `json:"containers_deleted,omitempty"`
	ImagesDeleted      []string `json:"images_deleted,omitempty"`
	VolumesDeleted     []string `json:"volumes_deleted,omitempty"`
	BuildCachesDeleted []string `json:"build_caches_deleted,omitempty"`
	SpaceReclaimed     uint64   `json:"space_reclaimed"`
}

type pruneFilter struct {
	Key   string
	Value string
}

type pruneDockerFilters struct {
	Containers  mobyclient.Filters
	Images      mobyclient.Filters
	Volumes     mobyclient.Filters
	BuildCaches mobyclient.Filters
}
