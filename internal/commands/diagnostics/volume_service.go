package diagnostics

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

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
