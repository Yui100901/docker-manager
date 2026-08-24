package diagnostics

import (
	"docker-manager/internal/commandflags"
	"time"

	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/volume"
)

type PruneReportOptions struct {
	Apply                bool
	Confirm              bool
	AllowNonAtomicDelete bool
	Only                 []string
	Filters              []string
	Until                string
	UntilValues          []string
	ProtectLabels        []string
	commandflags.FormatOptions
}

type PruneReport struct {
	GeneratedAt                 string               `json:"generated_at"`
	DockerEndpoint              string               `json:"docker_endpoint"`
	StoppedContainers           []PruneContainerRef  `json:"stopped_containers,omitempty"`
	DanglingImages              []PruneImageRef      `json:"dangling_images,omitempty"`
	UnusedVolumes               []PruneVolumeRef     `json:"unused_volumes,omitempty"`
	BuildCaches                 []PruneBuildCacheRef `json:"build_caches,omitempty"`
	EstimatedBytes              uint64               `json:"estimated_bytes"`
	Warnings                    []string             `json:"warnings,omitempty"`
	Applied                     bool                 `json:"applied"`
	Scope                       PruneScope           `json:"scope"`
	ApplyResult                 *PruneApplyResult    `json:"apply_result,omitempty"`
	NonAtomicDeleteAcknowledged bool                 `json:"non_atomic_delete_acknowledged,omitempty"`
}

type PruneScope struct {
	Only          []string `json:"only,omitempty"`
	Filters       []string `json:"filters,omitempty"`
	Until         string   `json:"until,omitempty"`
	ProtectLabels []string `json:"protect_labels,omitempty"`

	untilCutoff    time.Time
	hasUntilCutoff bool
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

	snapshot *pruneVolumeSnapshot
}

type PruneBuildCacheRef struct {
	ID          string `json:"id"`
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
	Size        int64  `json:"size,omitempty"`

	snapshot *pruneBuildCacheSnapshot
}

type PruneApplyResult struct {
	ContainersDeleted  []string                   `json:"containers_deleted,omitempty"`
	ImagesDeleted      []string                   `json:"images_deleted,omitempty"`
	VolumesDeleted     []string                   `json:"volumes_deleted,omitempty"`
	BuildCachesDeleted []string                   `json:"build_caches_deleted,omitempty"`
	Failures           []PruneApplyFailure        `json:"failures,omitempty"`
	UnknownOutcomes    []PruneApplyUnknownOutcome `json:"unknown_outcomes,omitempty"`
	// SpaceReclaimed contains only bytes confirmed by Docker (currently build cache).
	SpaceReclaimed uint64 `json:"space_reclaimed"`
	// EstimatedBytesReclaimed sums snapshot sizes for every successful candidate.
	EstimatedBytesReclaimed uint64 `json:"estimated_bytes_reclaimed"`
}

type PruneApplyFailure struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Error string `json:"error"`
}

type PruneApplyUnknownOutcome struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

type pruneFilter struct {
	Key   string
	Value string
}
