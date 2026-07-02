package diagnostics

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"docker-manager/internal/completion"
	"docker-manager/internal/docker"
	rpt "docker-manager/internal/report"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/spf13/cobra"
)

const (
	volumeSizeModeAPI       = "api"
	volumeSizeModeDockerRun = "docker-run"
	volumeDefaultSizeImage  = "busybox:latest"
)

type volumeDockerService interface {
	ListVolumes(ctx context.Context) (volume.ListResponse, error)
	ListContainers(ctx context.Context, all bool) ([]container.Summary, error)
	InspectContainer(ctx context.Context, id string) (container.InspectResponse, error)
	MeasureVolumeSize(ctx context.Context, volumeName, helperImage string) (int64, error)
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
	All       bool
	NoTrunc   bool
	SizeMode  string
	SizeImage string
	Filters   []string
	rpt.FormatOptions
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
	cmd.Flags().StringVar(&opts.SizeMode, "size-mode", volumeSizeModeAPI, "volume 大小探测方式: api | docker-run")
	cmd.Flags().StringVar(&opts.SizeImage, "size-image", volumeDefaultSizeImage, "docker-run 大小探测使用的 helper 镜像，必须已存在于目标 Docker")
	cmd.Flags().StringArrayVarP(&opts.Filters, "filter", "f", nil, "筛选 volume，支持名称/driver/mountpoint/label 和 * ? 通配符，可重复指定")
	_ = cmd.RegisterFlagCompletionFunc("filter", completion.LocalVolumes)
	rpt.AddFormatFlag(cmd, &opts.Format)
	return cmd
}

func normalizeVolumeOptions(opts *VolumeOptions) error {
	opts.SizeMode = strings.TrimSpace(opts.SizeMode)
	if opts.SizeMode == "" {
		opts.SizeMode = volumeSizeModeAPI
	}
	switch opts.SizeMode {
	case volumeSizeModeAPI, volumeSizeModeDockerRun:
	default:
		return fmt.Errorf("不支持的 --size-mode %q，请使用 api 或 docker-run", opts.SizeMode)
	}
	if strings.TrimSpace(opts.SizeImage) == "" {
		opts.SizeImage = volumeDefaultSizeImage
	}
	return nil
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
	refsByVolume, warnings := inspectVolumeContainerRefs(ctx, svc, containers)
	report := buildVolumeReportWithRefs(volumes, refsByVolume, warnings, opts)
	if opts.SizeMode == volumeSizeModeDockerRun {
		probeVolumeSizes(ctx, svc, &report, opts.SizeImage)
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

func probeVolumeSizes(ctx context.Context, svc volumeDockerService, report *VolumeReport, helperImage string) {
	for i := range report.Volumes {
		if err := ctx.Err(); err != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("volume 大小探测已取消: %v", err))
			return
		}
		vol := &report.Volumes[i]
		if vol.Size >= 0 || vol.Driver != "local" {
			continue
		}
		size, err := svc.MeasureVolumeSize(ctx, vol.Name, helperImage)
		if err != nil {
			vol.SizeError = err.Error()
			report.Warnings = append(report.Warnings, fmt.Sprintf("volume %s 大小探测失败: %v", vol.Name, err))
			continue
		}
		vol.Size = size
		vol.SizeSource = volumeSizeModeDockerRun
		if report.Summary.UnknownSize > 0 {
			report.Summary.UnknownSize--
		}
		if vol.Status == "unused" || vol.Status == "suspected-unused" {
			report.Summary.ReclaimableSize += size
		}
	}
}

func inspectVolumeContainerRefs(ctx context.Context, svc volumeDockerService, containers []container.Summary) (map[string][]VolumeContainerRef, []string) {
	refs := make(map[string][]VolumeContainerRef)
	var warnings []string
	for _, c := range containers {
		if err := ctx.Err(); err != nil {
			warnings = append(warnings, fmt.Sprintf("容器 inspect 已取消: %v", err))
			break
		}
		inspect, err := svc.InspectContainer(ctx, c.ID)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("inspect 容器 %s 失败，已回退到列表挂载摘要: %v", containerDisplayName(c), err))
			appendSummaryVolumeRefs(refs, c)
			continue
		}
		appendInspectVolumeRefs(refs, c, inspect)
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

func printVolumeReport(w io.Writer, report VolumeReport, opts VolumeOptions) {
	fmt.Fprintln(w, "Docker volume 报告")
	printDockerEndpoint(w, report.DockerEndpoint)
	fmt.Fprintf(w, "Volume: 总数=%d 已列出=%d 未使用=%d 疑似未使用=%d 使用中=%d 未知大小=%d 可回收=%s\n\n",
		report.Summary.Total,
		len(report.Volumes),
		report.Summary.Unused,
		report.Summary.SuspectedUnused,
		report.Summary.Used,
		report.Summary.UnknownSize,
		humanBytes(uint64FromInt64(report.Summary.ReclaimableSize)),
	)
	if len(report.Warnings) > 0 {
		fmt.Fprintln(w, "警告:")
		for _, warning := range report.Warnings {
			fmt.Fprintf(w, "  - %s\n", warning)
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "Volume:")
	if len(report.Volumes) == 0 {
		fmt.Fprintln(w, "  无")
		return
	}
	for _, vol := range report.Volumes {
		name := displayLayerText(vol.Name, opts.NoTrunc, 48)
		mountpoint := displayLayerText(vol.Mountpoint, opts.NoTrunc, 72)
		fmt.Fprintf(w, "  - %s 状态=%s driver=%s 引用=%d(%s) 大小=%s 匿名=%v\n", name, vol.Status, vol.Driver, vol.RefCount, vol.RefSource, volumeSizeText(vol), vol.Anonymous)
		if mountpoint != "" {
			fmt.Fprintf(w, "      mountpoint=%s\n", mountpoint)
		}
		if vol.SizeError != "" {
			fmt.Fprintf(w, "      size-error=%s\n", vol.SizeError)
		}
		if len(vol.Containers) == 0 {
			fmt.Fprintln(w, "      容器=无")
			continue
		}
		for _, c := range vol.Containers {
			fmt.Fprintf(w, "      容器=%s 镜像=%s 状态=%s 挂载点=%s 可写=%v 模式=%s\n", c.Name, c.Image, c.State, c.Destination, c.RW, c.Mode)
		}
	}
}

func volumeSizeText(vol VolumeRef) string {
	if vol.Size < 0 {
		return "未知"
	}
	if vol.SizeSource == "" {
		return humanBytes(uint64FromInt64(vol.Size))
	}
	return fmt.Sprintf("%s(%s)", humanBytes(uint64FromInt64(vol.Size)), vol.SizeSource)
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

func (s *dockerVolumeService) InspectContainer(ctx context.Context, id string) (container.InspectResponse, error) {
	return s.cli.ContainerInspect(ctx, id)
}

func (s *dockerVolumeService) MeasureVolumeSize(ctx context.Context, volumeName, helperImage string) (int64, error) {
	if strings.TrimSpace(helperImage) == "" {
		helperImage = volumeDefaultSizeImage
	}
	if _, err := s.cli.ImageInspect(ctx, helperImage); err != nil {
		return -1, fmt.Errorf("helper 镜像 %q 在目标 Docker 上不可用: %w", helperImage, err)
	}

	containerName := "dm_volume_size_" + time.Now().Format("20060102150405") + "_" + safeVolumeProbeName(volumeName)
	resp, err := s.cli.ContainerCreate(ctx,
		&container.Config{
			Image: helperImage,
			Cmd: []string{
				"sh",
				"-c",
				`bytes=$(du -sb /mnt/volume 2>/dev/null | awk '{print $1}'); if [ -n "$bytes" ]; then echo "$bytes"; else du -sk /mnt/volume 2>/dev/null | awk '{print $1 * 1024}'; fi`,
			},
		},
		&container.HostConfig{
			Mounts: []mount.Mount{{
				Type:     mount.TypeVolume,
				Source:   volumeName,
				Target:   "/mnt/volume",
				ReadOnly: true,
			}},
		},
		nil,
		nil,
		containerName,
	)
	if err != nil {
		return -1, fmt.Errorf("创建大小探测容器失败: %w", err)
	}
	defer removeVolumeProbeContainer(s.cli, resp.ID)

	if err := s.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return -1, fmt.Errorf("启动大小探测容器失败: %w", err)
	}
	waitC, errC := s.cli.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case waitResp := <-waitC:
		if waitResp.Error != nil {
			return -1, fmt.Errorf("大小探测容器失败: %s", waitResp.Error.Message)
		}
		if waitResp.StatusCode != 0 {
			stderr := readVolumeProbeLogs(ctx, s.cli, resp.ID, true)
			return -1, fmt.Errorf("大小探测容器退出码=%d stderr=%s", waitResp.StatusCode, strings.TrimSpace(stderr))
		}
	case err := <-errC:
		if err != nil {
			return -1, fmt.Errorf("等待大小探测容器失败: %w", err)
		}
	case <-ctx.Done():
		return -1, ctx.Err()
	}

	stdout := readVolumeProbeLogs(ctx, s.cli, resp.ID, false)
	fields := strings.Fields(stdout)
	if len(fields) == 0 {
		return -1, fmt.Errorf("大小探测容器没有输出")
	}
	size, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return -1, fmt.Errorf("解析大小探测输出 %q 失败: %w", strings.TrimSpace(stdout), err)
	}
	return size, nil
}

func readVolumeProbeLogs(ctx context.Context, cli *client.Client, containerID string, stderrOnly bool) string {
	logs, err := cli.ContainerLogs(ctx, containerID, container.LogsOptions{ShowStdout: !stderrOnly, ShowStderr: true, Tail: "all"})
	if err != nil {
		return err.Error()
	}
	defer logs.Close()
	var stdout, stderr bytes.Buffer
	_, _ = stdcopy.StdCopy(&stdout, &stderr, logs)
	if stderrOnly {
		return stderr.String()
	}
	if stdout.Len() > 0 {
		return stdout.String()
	}
	return stderr.String()
}

func removeVolumeProbeContainer(cli *client.Client, containerID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
}

func safeVolumeProbeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 40 {
			break
		}
	}
	if b.Len() == 0 {
		return "volume"
	}
	return b.String()
}
