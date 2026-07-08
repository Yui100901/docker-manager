package diagnostics

import "docker-manager/internal/commandflags"

type VolumeOptions struct {
	All       bool
	NoTrunc   bool
	SizeMode  string
	SizeImage string
	Filters   []string
	commandflags.FormatOptions
}

type VolumeReport struct {
	DockerEndpoint string        `json:"docker_endpoint"`
	Volumes        []VolumeRef   `json:"volumes"`
	Warnings       []string      `json:"warnings,omitempty"`
	Summary        VolumeSummary `json:"summary"`
}

type VolumeSummary struct {
	Total           int   `json:"total"`
	Unused          int   `json:"unused"`
	SuspectedUnused int   `json:"suspected_unused"`
	Used            int   `json:"used"`
	UnknownSize     int   `json:"unknown_size"`
	ReclaimableSize int64 `json:"reclaimable_size"`
}

type VolumeRef struct {
	Name       string               `json:"name"`
	Driver     string               `json:"driver,omitempty"`
	Mountpoint string               `json:"mountpoint,omitempty"`
	Scope      string               `json:"scope,omitempty"`
	Labels     map[string]string    `json:"labels,omitempty"`
	Options    map[string]string    `json:"options,omitempty"`
	Size       int64                `json:"size"`
	SizeSource string               `json:"size_source,omitempty"`
	SizeError  string               `json:"size_error,omitempty"`
	RefCount   int64                `json:"ref_count"`
	RefSource  string               `json:"ref_source,omitempty"`
	Status     string               `json:"status"`
	Anonymous  bool                 `json:"anonymous"`
	Containers []VolumeContainerRef `json:"containers,omitempty"`
}

type VolumeContainerRef struct {
	Name        string `json:"name"`
	ID          string `json:"id"`
	Image       string `json:"image,omitempty"`
	State       string `json:"state,omitempty"`
	MountType   string `json:"mount_type,omitempty"`
	Source      string `json:"source,omitempty"`
	Destination string `json:"destination,omitempty"`
	Mode        string `json:"mode,omitempty"`
	RW          bool   `json:"rw"`
}
