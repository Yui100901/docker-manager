package diagnostics

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"docker-manager/internal/commandflags"
	"docker-manager/internal/completion"
	"docker-manager/internal/docker"
	rpt "docker-manager/internal/report"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/volume"
	mobyclient "github.com/moby/moby/client"
	"github.com/spf13/cobra"
)

const (
	volumeSizeModeAPI       = "api"
	volumeSizeModeDockerRun = "docker-run"
	volumeSizeModeLocalGo   = "local-go"
	volumeSizeModeAuto      = "auto"
	volumeDefaultSizeImage  = "busybox:latest"
)

type volumeDockerService interface {
	ListVolumes(ctx context.Context) (volume.ListResponse, error)
	ListContainers(ctx context.Context, all bool) ([]container.Summary, error)
	InspectContainer(ctx context.Context, id string) (container.InspectResponse, error)
	MeasureVolumeSize(ctx context.Context, volumeName, helperImage string) (int64, error)
}

type containerInspectService interface {
	InspectContainer(ctx context.Context, id string) (container.InspectResponse, error)
}

var newVolumeDockerService = func() (volumeDockerService, error) {
	cli, err := docker.NewMobyClient()
	if err != nil {
		return nil, err
	}
	return &dockerVolumeService{cli: cli}, nil
}

var measureLocalVolumeSize = measureLocalVolumeSizeWithGo

type dockerVolumeService struct {
	cli *mobyclient.Client
}

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

func NewVolumesReportCommand() *cobra.Command {
	cmd := newVolumeListUnusedCommand()
	cmd.Use = "volumes [volume-pattern...]"
	cmd.Short = "查找疑似未使用 volume，并输出关联容器信息"
	return cmd
}

func newVolumeListUnusedCommand() *cobra.Command {
	opts := VolumeOptions{SizeMode: volumeSizeModeAPI, SizeImage: volumeDefaultSizeImage}
	cmd := &cobra.Command{
		Use:   "ls-unused [volume-pattern...]",
		Short: "查找疑似未使用 volume，并输出关联容器信息",
		RunE: func(cmd *cobra.Command, args []string) error {
			runOpts := opts
			if err := normalizeVolumeOptions(&runOpts); err != nil {
				return err
			}
			runOpts.Filters = append(append([]string(nil), opts.Filters...), args...)
			report, err := runVolumeReport(cmd.Context(), runOpts)
			if err != nil {
				return fmt.Errorf("生成 volume 报告失败: %w", err)
			}
			return rpt.Print(cmd.OutOrStdout(), runOpts.Format, report, func(w io.Writer) {
				printVolumeReport(w, report, runOpts)
			})
		},
		ValidArgsFunction: completion.LocalVolumes,
	}
	cmd.Flags().BoolVar(&opts.All, "all", false, "显示所有 volume，包括仍被容器引用的 volume")
	cmd.Flags().BoolVar(&opts.NoTrunc, "no-trunc", false, "显示完整 volume 名称和挂载点")
	cmd.Flags().StringVar(&opts.SizeMode, "size-mode", volumeSizeModeAPI, "volume 大小探测方式: api | local-go | docker-run | auto")
	cmd.Flags().StringVar(&opts.SizeImage, "size-image", volumeDefaultSizeImage, "docker-run/auto 大小探测使用的 helper 镜像，必须已存在于目标 Docker")
	cmd.Flags().StringArrayVarP(&opts.Filters, "filter", "f", nil, "筛选 volume，支持名称/driver/mountpoint/label 和 * ? 通配符，可重复指定")
	_ = cmd.RegisterFlagCompletionFunc("filter", completion.LocalVolumes)
	commandflags.AddReportFormatFlag(cmd, &opts.Format)
	return cmd
}

func normalizeVolumeOptions(opts *VolumeOptions) error {
	opts.SizeMode = strings.TrimSpace(opts.SizeMode)
	if opts.SizeMode == "" {
		opts.SizeMode = volumeSizeModeAPI
	}
	switch opts.SizeMode {
	case volumeSizeModeAPI, volumeSizeModeLocalGo, volumeSizeModeDockerRun, volumeSizeModeAuto:
	default:
		return fmt.Errorf("不支持的 --size-mode %q，请使用 api、local-go、docker-run 或 auto", opts.SizeMode)
	}
	if strings.TrimSpace(opts.SizeImage) == "" {
		opts.SizeImage = volumeDefaultSizeImage
	}
	return nil
}

func runVolumeReport(ctx context.Context, opts VolumeOptions) (VolumeReport, error) {
	if err := normalizeVolumeOptions(&opts); err != nil {
		return VolumeReport{}, err
	}
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
	refsByVolume, warnings := inspectVolumeContainerRefs(ctx, svc, containers)
	report := buildVolumeReportWithRefs(volumes, refsByVolume, warnings, opts)
	if opts.SizeMode != volumeSizeModeAPI {
		probeVolumeSizes(ctx, svc, &report, opts)
	}
	return report, nil
}

func buildVolumeReport(volumes volume.ListResponse, containers []container.Summary, opts VolumeOptions) VolumeReport {
	return buildVolumeReportWithRefs(volumes, volumeContainerRefs(containers), nil, opts)
}

func buildVolumeReportWithRefs(volumes volume.ListResponse, refsByVolume map[string][]VolumeContainerRef, warnings []string, opts VolumeOptions) VolumeReport {
	report := VolumeReport{DockerEndpoint: docker.Endpoint(), Warnings: append([]string(nil), volumes.Warnings...)}
	report.Warnings = append(report.Warnings, warnings...)

	for _, vol := range filterVolumesByPatterns(volumes.Volumes, opts.Filters) {
		ref := VolumeRef{
			Name:       vol.Name,
			Driver:     vol.Driver,
			Mountpoint: vol.Mountpoint,
			Scope:      vol.Scope,
			Labels:     cloneStringMap(vol.Labels),
			Options:    cloneStringMap(vol.Options),
			Size:       -1,
			SizeSource: "unknown",
			RefCount:   -1,
			RefSource:  "unknown",
			Anonymous:  isAnonymousVolumeName(vol.Name),
			Containers: refsByVolume[vol.Name],
		}
		if vol.UsageData != nil {
			ref.Size = vol.UsageData.Size
			ref.SizeSource = "api"
			ref.RefCount = vol.UsageData.RefCount
			ref.RefSource = "api"
		}
		if len(ref.Containers) > 0 && ref.RefCount < int64(len(ref.Containers)) {
			ref.RefCount = int64(len(ref.Containers))
			ref.RefSource = "inspect"
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

func probeVolumeSizes(ctx context.Context, svc volumeDockerService, report *VolumeReport, opts VolumeOptions) {
	type sizeResult struct {
		index  int
		size   int64
		source string
		err    error
	}
	results := make([]sizeResult, len(report.Volumes))
	for i := range results {
		results[i].index = i
	}
	runDiagnosticsParallel(ctx, len(report.Volumes), diagnosticsProbeConcurrency, func(ctx context.Context, i int) {
		vol := &report.Volumes[i]
		if vol.Size >= 0 || vol.Driver != "local" {
			return
		}
		size, source, err := measureVolumeSize(ctx, svc, vol, opts)
		results[i] = sizeResult{index: i, size: size, source: source, err: err}
	})
	if err := ctx.Err(); err != nil {
		report.Warnings = append(report.Warnings, fmt.Sprintf("volume ???????: %v", err))
		return
	}
	for _, result := range results {
		vol := &report.Volumes[result.index]
		if vol.Size >= 0 || vol.Driver != "local" {
			continue
		}
		if result.err != nil {
			vol.SizeError = result.err.Error()
			report.Warnings = append(report.Warnings, fmt.Sprintf("volume %s ??????: %v", vol.Name, result.err))
			continue
		}
		vol.Size = result.size
		vol.SizeSource = result.source
		if report.Summary.UnknownSize > 0 {
			report.Summary.UnknownSize--
		}
		if vol.Status == "unused" || vol.Status == "suspected-unused" {
			report.Summary.ReclaimableSize += result.size
		}
	}
}

func measureVolumeSize(ctx context.Context, svc volumeDockerService, vol *VolumeRef, opts VolumeOptions) (int64, string, error) {
	switch opts.SizeMode {
	case volumeSizeModeLocalGo:
		size, err := measureLocalVolumeSize(ctx, vol)
		return size, volumeSizeModeLocalGo, err
	case volumeSizeModeDockerRun:
		size, err := svc.MeasureVolumeSize(ctx, vol.Name, opts.SizeImage)
		return size, volumeSizeModeDockerRun, err
	case volumeSizeModeAuto:
		size, err := measureLocalVolumeSize(ctx, vol)
		if err == nil {
			return size, volumeSizeModeLocalGo, nil
		}
		localErr := err
		size, err = svc.MeasureVolumeSize(ctx, vol.Name, opts.SizeImage)
		if err == nil {
			return size, volumeSizeModeDockerRun, nil
		}
		return -1, "", fmt.Errorf("local-go 不可用: %v; docker-run 失败: %w", localErr, err)
	default:
		return -1, "", fmt.Errorf("不支持的大小探测方式 %q", opts.SizeMode)
	}
}

func measureLocalVolumeSizeWithGo(ctx context.Context, vol *VolumeRef) (int64, error) {
	if runtime.GOOS != "linux" {
		return -1, fmt.Errorf("local-go 仅支持 Linux 本机 Docker，当前系统为 %s", runtime.GOOS)
	}
	if docker.IsRemoteEndpoint() || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(docker.Endpoint())), "unix://") {
		return -1, fmt.Errorf("local-go 仅支持 unix socket 本机 Docker，当前 endpoint=%s", docker.Endpoint())
	}
	if strings.TrimSpace(vol.Mountpoint) == "" {
		return -1, fmt.Errorf("volume %s 没有 mountpoint", vol.Name)
	}
	root := filepath.Clean(vol.Mountpoint)
	info, err := os.Stat(root)
	if err != nil {
		return -1, fmt.Errorf("读取 mountpoint %s 失败: %w", root, err)
	}
	if !info.IsDir() {
		return -1, fmt.Errorf("mountpoint %s 不是目录", root)
	}

	var total int64
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += localFileDiskUsage(info)
		return nil
	})
	if err != nil {
		return -1, fmt.Errorf("遍历 mountpoint %s 失败: %w", root, err)
	}
	return total, nil
}

func inspectVolumeContainerRefs(ctx context.Context, svc containerInspectService, containers []container.Summary) (map[string][]VolumeContainerRef, []string) {
	refs := make(map[string][]VolumeContainerRef)
	refsByIndex := make([]map[string][]VolumeContainerRef, len(containers))
	warningsByIndex := make([]string, len(containers))
	runDiagnosticsParallel(ctx, len(containers), diagnosticsInspectConcurrency, func(ctx context.Context, i int) {
		c := containers[i]
		localRefs := make(map[string][]VolumeContainerRef)
		inspect, err := svc.InspectContainer(ctx, c.ID)
		if err != nil {
			if ctx.Err() == nil {
				warningsByIndex[i] = fmt.Sprintf("inspect ?? %s ?????????????: %v", containerDisplayName(c), err)
			}
			appendSummaryVolumeRefs(localRefs, c)
			refsByIndex[i] = localRefs
			return
		}
		appendInspectVolumeRefs(localRefs, c, inspect)
		refsByIndex[i] = localRefs
	})
	var warnings []string
	if err := ctx.Err(); err != nil {
		warnings = append(warnings, fmt.Sprintf("?? inspect ???: %v", err))
	}
	for i := range containers {
		if warningsByIndex[i] != "" {
			warnings = append(warnings, warningsByIndex[i])
		}
		for volumeName, volumeRefs := range refsByIndex[i] {
			refs[volumeName] = append(refs[volumeName], volumeRefs...)
		}
	}
	sortVolumeContainerRefs(refs)
	return refs, warnings
}

func volumeContainerRefs(containers []container.Summary) map[string][]VolumeContainerRef {
	refs := make(map[string][]VolumeContainerRef)
	for _, c := range containers {
		appendSummaryVolumeRefs(refs, c)
	}
	sortVolumeContainerRefs(refs)
	return refs
}

func appendSummaryVolumeRefs(refs map[string][]VolumeContainerRef, c container.Summary) {
	for _, m := range c.Mounts {
		if m.Type != mount.TypeVolume || m.Name == "" {
			continue
		}
		refs[m.Name] = append(refs[m.Name], VolumeContainerRef{
			Name:        containerDisplayName(c),
			ID:          shortID(c.ID),
			Image:       c.Image,
			State:       string(c.State),
			MountType:   string(m.Type),
			Source:      m.Source,
			Destination: m.Destination,
			Mode:        m.Mode,
			RW:          m.RW,
		})
	}
}

func appendInspectVolumeRefs(refs map[string][]VolumeContainerRef, c container.Summary, inspect container.InspectResponse) {
	name := strings.TrimPrefix(inspect.Name, "/")
	if name == "" {
		name = containerDisplayName(c)
	}
	id := inspect.ID
	if id == "" {
		id = c.ID
	}
	state := string(c.State)
	if inspect.State != nil && inspect.State.Status != "" {
		state = string(inspect.State.Status)
	}
	image := c.Image
	if inspect.Config != nil && inspect.Config.Image != "" {
		image = inspect.Config.Image
	}
	for _, m := range inspect.Mounts {
		if m.Type != mount.TypeVolume || m.Name == "" {
			continue
		}
		refs[m.Name] = append(refs[m.Name], VolumeContainerRef{
			Name:        name,
			ID:          shortID(id),
			Image:       image,
			State:       state,
			MountType:   string(m.Type),
			Source:      m.Source,
			Destination: m.Destination,
			Mode:        m.Mode,
			RW:          m.RW,
		})
	}
}

func containerDisplayName(c container.Summary) string {
	name := firstContainerName(c.Names)
	if name == "" {
		name = shortID(c.ID)
	}
	return name
}

func sortVolumeContainerRefs(refs map[string][]VolumeContainerRef) {
	for name := range refs {
		sort.Slice(refs[name], func(i, j int) bool {
			if refs[name][i].Name == refs[name][j].Name {
				return refs[name][i].Destination < refs[name][j].Destination
			}
			return refs[name][i].Name < refs[name][j].Name
		})
	}
}

func volumeUsageStatus(ref VolumeRef) string {
	if len(ref.Containers) > 0 || ref.RefCount > 0 {
		return "used"
	}
	if ref.RefCount == 0 {
		return "unused"
	}
	return "suspected-unused"
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
