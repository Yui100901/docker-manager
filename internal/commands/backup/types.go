package backup

import (
	"context"
	"docker-manager/internal/docker"
	"docker-manager/internal/version"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
	mobyclient "github.com/moby/moby/client"
	"io"
)

const (
	backupManifestName = "manifest.json"
	backupInspectName  = "container.inspect.json"
	backupComposeName  = "docker-compose.yml"
	backupReadmeName   = "README.md"
	backupRestoreName  = "restore.sh"
	backupChecksumName = "checksums.txt"
	backupRoot         = "docker-backups"
)

type backupDockerService interface {
	ListContainers(ctx context.Context, all bool) ([]container.Summary, error)
	InspectContainer(ctx context.Context, name string) (container.InspectResponse, error)
	SaveImage(ctx context.Context, refs []string, outputFile string) error
	LoadImage(ctx context.Context, inputFile string, output io.Writer) error
	ImageExists(ctx context.Context, ref string) (bool, error)
	InspectNetwork(ctx context.Context, name string) (network.Inspect, error)
	CreateNetwork(ctx context.Context, inspect network.Inspect) error
	InspectVolume(ctx context.Context, name string) (volume.Volume, error)
	CreateVolume(ctx context.Context, vol volume.Volume) error
	ContainerExists(ctx context.Context, name string) (bool, error)
	RemoveContainer(ctx context.Context, name string) error
	CreateContainer(ctx context.Context, inspect container.InspectResponse, name string) (string, error)
	StartContainer(ctx context.Context, id string) error
}

var newBackupDockerService = func() (backupDockerService, error) {
	cli, err := docker.NewMobyClient()
	if err != nil {
		return nil, err
	}
	return &dockerBackupService{cli: cli}, nil
}

type dockerBackupService struct {
	cli *mobyclient.Client
}

type BackupOptions struct {
	OutputDir      string
	IncludeImage   bool
	DryRun         bool
	Bundle         bool
	BundleOutput   string
	Encrypt        bool
	PassphraseFile string
	SplitSize      string
	Merge          bool
	Output         io.Writer
}

type RestoreOptions struct {
	Name           string
	Replace        bool
	NoStart        bool
	DryRun         bool
	Format         string
	PassphraseFile string
	SkipChecksum   bool
	Output         io.Writer
}

// BackupManifest keeps the current batch-friendly containers list while still
// accepting the legacy top-level single-container fields during restore.
type BackupManifest struct {
	Version        int                       `json:"version"`
	CreatedAt      string                    `json:"created_at"`
	Tool           version.VersionInfo       `json:"tool,omitempty"`
	SourcePlatform string                    `json:"source_platform,omitempty"`
	Containers     []BackupContainerManifest `json:"containers,omitempty"`

	ContainerName string              `json:"container_name,omitempty"`
	SourceName    string              `json:"source_name,omitempty"`
	Image         string              `json:"image,omitempty"`
	ImageArchive  string              `json:"image_archive,omitempty"`
	InspectFile   string              `json:"inspect_file,omitempty"`
	ComposeFile   string              `json:"compose_file,omitempty"`
	Networks      []BackupResourceRef `json:"networks,omitempty"`
	Volumes       []BackupResourceRef `json:"volumes,omitempty"`
}

type BackupContainerManifest struct {
	ContainerName string              `json:"container_name"`
	SourceName    string              `json:"source_name"`
	Path          string              `json:"path,omitempty"`
	Image         string              `json:"image,omitempty"`
	ImageArchive  string              `json:"image_archive,omitempty"`
	InspectFile   string              `json:"inspect_file"`
	ComposeFile   string              `json:"compose_file"`
	Networks      []BackupResourceRef `json:"networks,omitempty"`
	Volumes       []BackupResourceRef `json:"volumes,omitempty"`
	Mounts        []BackupMountRef    `json:"mounts,omitempty"`
	Devices       []BackupDeviceRef   `json:"devices,omitempty"`
}

type BackupResourceRef struct {
	Name string `json:"name"`
	File string `json:"file"`
}

type BackupMountRef struct {
	Type             string `json:"type"`
	Name             string `json:"name,omitempty"`
	Source           string `json:"source,omitempty"`
	Destination      string `json:"destination,omitempty"`
	Driver           string `json:"driver,omitempty"`
	Mode             string `json:"mode,omitempty"`
	RW               bool   `json:"rw"`
	Propagation      string `json:"propagation,omitempty"`
	Verification     string `json:"verification,omitempty"`
	HostPathExists   *bool  `json:"host_path_exists,omitempty"`
	HostPathReadable *bool  `json:"host_path_readable,omitempty"`
	HostPathWritable *bool  `json:"host_path_writable,omitempty"`
	Warning          string `json:"warning,omitempty"`
}

type BackupDeviceRef struct {
	Type              string `json:"type"`
	PathOnHost        string `json:"path_on_host,omitempty"`
	PathInContainer   string `json:"path_in_container,omitempty"`
	CgroupPermissions string `json:"cgroup_permissions,omitempty"`
	Verification      string `json:"verification,omitempty"`
	HostPathExists    *bool  `json:"host_path_exists,omitempty"`
	Warning           string `json:"warning,omitempty"`
}

type restoreDryRunPlan struct {
	ContainerName string
	SourceName    string
	EntryDir      string
	Image         string
	ImageArchive  string
	Networks      []BackupResourceRef
	Volumes       []BackupResourceRef
	Ports         []string
	Exists        bool
	Replace       bool
	NoStart       bool
	Conflicts     []string
}

type RestorePlanReport struct {
	Source         string                 `json:"source"`
	DockerEndpoint string                 `json:"docker_endpoint"`
	Checksum       string                 `json:"checksum"`
	ContainerCount int                    `json:"container_count"`
	Options        RestorePlanOptions     `json:"options"`
	Containers     []RestoreContainerPlan `json:"containers"`
	Summary        RestorePlanSummary     `json:"summary"`
	Warnings       []string               `json:"warnings,omitempty"`
}

type RestorePlanOptions struct {
	Replace bool   `json:"replace"`
	NoStart bool   `json:"no_start"`
	Name    string `json:"name,omitempty"`
}

type RestorePlanSummary struct {
	ImagesToLoad        int `json:"images_to_load"`
	ImagesPresent       int `json:"images_present"`
	NetworksToCreate    int `json:"networks_to_create"`
	NetworksPresent     int `json:"networks_present"`
	NetworksDifferent   int `json:"networks_different"`
	VolumesToCreate     int `json:"volumes_to_create"`
	VolumesPresent      int `json:"volumes_present"`
	VolumesDifferent    int `json:"volumes_different"`
	ContainersToCreate  int `json:"containers_to_create"`
	ContainersToReplace int `json:"containers_to_replace"`
	ContainerConflicts  int `json:"container_conflicts"`
	PortConflicts       int `json:"port_conflicts"`
	Warnings            int `json:"warnings"`
}

type RestoreContainerPlan struct {
	ContainerName string                `json:"container_name"`
	SourceName    string                `json:"source_name,omitempty"`
	EntryDir      string                `json:"entry_dir"`
	Image         RestoreImagePlan      `json:"image"`
	Networks      []RestoreResourcePlan `json:"networks,omitempty"`
	Volumes       []RestoreResourcePlan `json:"volumes,omitempty"`
	Ports         []string              `json:"ports,omitempty"`
	PortConflicts []RestorePortConflict `json:"port_conflicts,omitempty"`
	Container     RestoreTargetPlan     `json:"container"`
	Actions       []string              `json:"actions,omitempty"`
	Warnings      []string              `json:"warnings,omitempty"`
}

type RestoreImagePlan struct {
	Ref         string `json:"ref,omitempty"`
	Archive     string `json:"archive,omitempty"`
	Exists      bool   `json:"exists"`
	Action      string `json:"action"`
	Error       string `json:"error,omitempty"`
	ArchivePath string `json:"archive_path,omitempty"`
}

type RestoreResourcePlan struct {
	Name        string   `json:"name"`
	File        string   `json:"file,omitempty"`
	Exists      bool     `json:"exists"`
	Different   bool     `json:"different,omitempty"`
	Action      string   `json:"action"`
	Differences []string `json:"differences,omitempty"`
	Error       string   `json:"error,omitempty"`
}

type RestoreTargetPlan struct {
	Exists bool   `json:"exists"`
	Action string `json:"action"`
}

type RestorePortConflict struct {
	Port      string `json:"port"`
	Container string `json:"container"`
}

type BackupContainersResult struct {
	Paths []string
}
