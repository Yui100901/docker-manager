package main

import (
	"context"
	"fmt"
	"io"
	"sort"

	"docker-manager/docker"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"
)

type volumeDockerService interface {
	ListVolumes(ctx context.Context) (volume.ListResponse, error)
	ListContainers(ctx context.Context, all bool) ([]container.Summary, error)
}

var newVolumeDockerService = func() (volumeDockerService, error) {
	cli, err := docker.NewClient()
	if err != nil {
		return nil, err
	}
	return &dockerVolumeService{cli: cli}, nil
}

type dockerVolumeService struct {
	cli *client.Client
}

type VolumeOptions struct {
	All     bool
	NoTrunc bool
}

type VolumeReport struct {
	Volumes  []VolumeRef   `json:"volumes"`
	Warnings []string      `json:"warnings,omitempty"`
	Summary  VolumeSummary `json:"summary"`
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
	RefCount   int64                `json:"ref_count"`
	Status     string               `json:"status"`
	Anonymous  bool                 `json:"anonymous"`
	Containers []VolumeContainerRef `json:"containers,omitempty"`
}

type VolumeContainerRef struct {
	Name        string `json:"name"`
	ID          string `json:"id"`
	State       string `json:"state,omitempty"`
	Destination string `json:"destination,omitempty"`
	Mode        string `json:"mode,omitempty"`
	RW          bool   `json:"rw"`
}

func newVolumeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "volume",
		Short: "Volume 分析工具",
	}
	cmd.AddCommand(newVolumeListUnusedCommand())
	return cmd
}

func newVolumeListUnusedCommand() *cobra.Command {
	opts := VolumeOptions{}
	cmd := &cobra.Command{
		Use:   "ls-unused",
		Short: "查找疑似未使用 volume，并输出关联容器信息",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			report, err := runVolumeReport(cmd.Context(), opts)
			if err != nil {
				return fmt.Errorf("volume report failed: %w", err)
			}
			printVolumeReport(cmd.OutOrStdout(), report, opts)
			return nil
		},
	}
	cmd.Flags().BoolVar(&opts.All, "all", false, "显示所有 volume，包括仍被容器引用的 volume")
	cmd.Flags().BoolVar(&opts.NoTrunc, "no-trunc", false, "显示完整 volume 名称和挂载点")
	return cmd
}

func runVolumeReport(ctx context.Context, opts VolumeOptions) (VolumeReport, error) {
	svc, err := newVolumeDockerService()
	if err != nil {
		return VolumeReport{}, err
	}
	volumes, err := svc.ListVolumes(ctx)
	if err != nil {
		return VolumeReport{}, err
	}
	containers, err := svc.ListContainers(ctx, true)
	if err != nil {
		return VolumeReport{}, err
	}
	return buildVolumeReport(volumes, containers, opts), nil
}

func buildVolumeReport(volumes volume.ListResponse, containers []container.Summary, opts VolumeOptions) VolumeReport {
	refsByVolume := volumeContainerRefs(containers)
	report := VolumeReport{Warnings: append([]string(nil), volumes.Warnings...)}

	for _, vol := range volumes.Volumes {
		if vol == nil {
			continue
		}
		ref := VolumeRef{
			Name:       vol.Name,
			Driver:     vol.Driver,
			Mountpoint: vol.Mountpoint,
			Scope:      vol.Scope,
			Labels:     cloneStringMap(vol.Labels),
			Options:    cloneStringMap(vol.Options),
			Size:       -1,
			RefCount:   -1,
			Anonymous:  isAnonymousVolumeName(vol.Name),
			Containers: refsByVolume[vol.Name],
		}
		if vol.UsageData != nil {
			ref.Size = vol.UsageData.Size
			ref.RefCount = vol.UsageData.RefCount
		}
		ref.Status = volumeUsageStatus(ref)
		report.Summary.Total++
		switch ref.Status {
		case "unused":
			report.Summary.Unused++
			if ref.Size > 0 {
				report.Summary.ReclaimableSize += ref.Size
			}
		case "suspected-unused":
			report.Summary.SuspectedUnused++
			if ref.Size > 0 {
				report.Summary.ReclaimableSize += ref.Size
			}
		case "used":
			report.Summary.Used++
		}
		if ref.Size < 0 {
			report.Summary.UnknownSize++
		}
		if opts.All || ref.Status != "used" {
			report.Volumes = append(report.Volumes, ref)
		}
	}
	sortVolumeReport(&report)
	return report
}

func volumeContainerRefs(containers []container.Summary) map[string][]VolumeContainerRef {
	refs := make(map[string][]VolumeContainerRef)
	for _, c := range containers {
		name := firstContainerName(c.Names)
		if name == "" {
			name = shortID(c.ID)
		}
		for _, mount := range c.Mounts {
			if string(mount.Type) != "volume" || mount.Name == "" {
				continue
			}
			refs[mount.Name] = append(refs[mount.Name], VolumeContainerRef{
				Name:        name,
				ID:          shortID(c.ID),
				State:       string(c.State),
				Destination: mount.Destination,
				Mode:        mount.Mode,
				RW:          mount.RW,
			})
		}
	}
	for name := range refs {
		sort.Slice(refs[name], func(i, j int) bool {
			if refs[name][i].Name == refs[name][j].Name {
				return refs[name][i].Destination < refs[name][j].Destination
			}
			return refs[name][i].Name < refs[name][j].Name
		})
	}
	return refs
}

func volumeUsageStatus(ref VolumeRef) string {
	if ref.RefCount == 0 {
		return "unused"
	}
	if ref.RefCount > 0 || len(ref.Containers) > 0 {
		return "used"
	}
	return "suspected-unused"
}

func printVolumeReport(w io.Writer, report VolumeReport, opts VolumeOptions) {
	fmt.Fprintln(w, "Docker volume report")
	fmt.Fprintf(w, "Volumes: total=%d listed=%d unused=%d suspected_unused=%d used=%d unknown_size=%d reclaimable=%s\n\n",
		report.Summary.Total,
		len(report.Volumes),
		report.Summary.Unused,
		report.Summary.SuspectedUnused,
		report.Summary.Used,
		report.Summary.UnknownSize,
		humanBytes(uint64FromInt64(report.Summary.ReclaimableSize)),
	)
	if len(report.Warnings) > 0 {
		fmt.Fprintln(w, "Warnings:")
		for _, warning := range report.Warnings {
			fmt.Fprintf(w, "  - %s\n", warning)
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "Volumes:")
	if len(report.Volumes) == 0 {
		fmt.Fprintln(w, "  none")
		return
	}
	for _, vol := range report.Volumes {
		name := displayLayerText(vol.Name, opts.NoTrunc, 48)
		mountpoint := displayLayerText(vol.Mountpoint, opts.NoTrunc, 72)
		fmt.Fprintf(w, "  - %s status=%s driver=%s refs=%d size=%s anonymous=%v\n", name, vol.Status, vol.Driver, vol.RefCount, volumeSizeText(vol.Size), vol.Anonymous)
		if mountpoint != "" {
			fmt.Fprintf(w, "      mountpoint=%s\n", mountpoint)
		}
		if len(vol.Containers) == 0 {
			fmt.Fprintln(w, "      containers=none")
			continue
		}
		for _, c := range vol.Containers {
			fmt.Fprintf(w, "      container=%s state=%s dest=%s rw=%v mode=%s\n", c.Name, c.State, c.Destination, c.RW, c.Mode)
		}
	}
}

func volumeSizeText(size int64) string {
	if size < 0 {
		return "unknown"
	}
	return humanBytes(uint64FromInt64(size))
}

func sortVolumeReport(report *VolumeReport) {
	sort.Slice(report.Volumes, func(i, j int) bool {
		if report.Volumes[i].Status == report.Volumes[j].Status {
			return report.Volumes[i].Name < report.Volumes[j].Name
		}
		return volumeStatusRank(report.Volumes[i].Status) < volumeStatusRank(report.Volumes[j].Status)
	})
}

func volumeStatusRank(status string) int {
	switch status {
	case "unused":
		return 0
	case "suspected-unused":
		return 1
	case "used":
		return 2
	default:
		return 3
	}
}

func isAnonymousVolumeName(name string) bool {
	if len(name) != 64 {
		return false
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'f') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func cloneStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func (s *dockerVolumeService) ListVolumes(ctx context.Context) (volume.ListResponse, error) {
	return s.cli.VolumeList(ctx, volume.ListOptions{Filters: filters.NewArgs()})
}

func (s *dockerVolumeService) ListContainers(ctx context.Context, all bool) ([]container.Summary, error) {
	return s.cli.ContainerList(ctx, container.ListOptions{All: all})
}
