package diagnostics

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"docker-manager/internal/docker"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/volume"
	mobyclient "github.com/moby/moby/client"
)

func (s *dockerVolumeService) ListVolumes(ctx context.Context) (volume.ListResponse, error) {
	result, err := s.cli.VolumeList(ctx, mobyclient.VolumeListOptions{Filters: mobyclient.Filters{}})
	if err != nil {
		return volume.ListResponse{}, err
	}
	return volume.ListResponse{Volumes: result.Items, Warnings: result.Warnings}, nil
}

func (s *dockerVolumeService) ListContainers(ctx context.Context, all bool) ([]container.Summary, error) {
	result, err := s.cli.ContainerList(ctx, mobyclient.ContainerListOptions{All: all})
	if err != nil {
		return nil, err
	}
	return docker.ConvertDockerType[[]container.Summary](result.Items)
}

func (s *dockerVolumeService) InspectContainer(ctx context.Context, id string) (container.InspectResponse, error) {
	result, err := s.cli.ContainerInspect(ctx, id, mobyclient.ContainerInspectOptions{})
	if err != nil {
		return container.InspectResponse{}, err
	}
	return docker.ConvertDockerType[container.InspectResponse](result.Container)
}

func (s *dockerVolumeService) MeasureVolumeSize(ctx context.Context, volumeName, helperImage string) (int64, error) {
	if strings.TrimSpace(helperImage) == "" {
		helperImage = volumeDefaultSizeImage
	}
	if _, err := s.cli.ImageInspect(ctx, helperImage); err != nil {
		return -1, fmt.Errorf("helper image %q is not available on target Docker: %w", helperImage, err)
	}

	containerName := "dm_volume_size_" + time.Now().Format("20060102150405") + "_" + safeVolumeProbeName(volumeName)
	resp, err := s.cli.ContainerCreate(ctx, mobyclient.ContainerCreateOptions{
		Config: &container.Config{
			Image: helperImage,
			Cmd: []string{
				"sh",
				"-c",
				`bytes=$(du -sb /mnt/volume 2>/dev/null | awk '{print $1}'); if [ -n "$bytes" ]; then echo "$bytes"; else du -sk /mnt/volume 2>/dev/null | awk '{print $1 * 1024}'; fi`,
			},
		},
		HostConfig: &container.HostConfig{
			Mounts: []mount.Mount{{
				Type:     mount.TypeVolume,
				Source:   volumeName,
				Target:   "/mnt/volume",
				ReadOnly: true,
			}},
		},
		Name: containerName,
	})
	if err != nil {
		return -1, fmt.Errorf("create size probe container failed: %w", err)
	}
	defer removeVolumeProbeContainer(s.cli, resp.ID)

	if _, err := s.cli.ContainerStart(ctx, resp.ID, mobyclient.ContainerStartOptions{}); err != nil {
		return -1, fmt.Errorf("start size probe container failed: %w", err)
	}
	waitResult := s.cli.ContainerWait(ctx, resp.ID, mobyclient.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case waitResp := <-waitResult.Result:
		if waitResp.Error != nil {
			return -1, fmt.Errorf("size probe container failed: %s", waitResp.Error.Message)
		}
		if waitResp.StatusCode != 0 {
			stderr := readVolumeProbeLogs(ctx, s.cli, resp.ID, true)
			return -1, fmt.Errorf("size probe container exit_code=%d stderr=%s", waitResp.StatusCode, strings.TrimSpace(stderr))
		}
	case err := <-waitResult.Error:
		if err != nil {
			return -1, fmt.Errorf("wait size probe container failed: %w", err)
		}
	case <-ctx.Done():
		return -1, ctx.Err()
	}

	stdout := readVolumeProbeLogs(ctx, s.cli, resp.ID, false)
	fields := strings.Fields(stdout)
	if len(fields) == 0 {
		return -1, fmt.Errorf("size probe container produced no output")
	}
	size, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return -1, fmt.Errorf("parse size probe output %q failed: %w", strings.TrimSpace(stdout), err)
	}
	return size, nil
}

func readVolumeProbeLogs(ctx context.Context, cli *mobyclient.Client, containerID string, stderrOnly bool) string {
	logs, err := cli.ContainerLogs(ctx, containerID, mobyclient.ContainerLogsOptions{ShowStdout: !stderrOnly, ShowStderr: true, Tail: "all"})
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

func removeVolumeProbeContainer(cli *mobyclient.Client, containerID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = cli.ContainerRemove(ctx, containerID, mobyclient.ContainerRemoveOptions{Force: true})
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
