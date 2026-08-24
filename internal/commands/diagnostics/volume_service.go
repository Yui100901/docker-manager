package diagnostics

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"docker-manager/internal/docker"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/volume"
	mobyclient "github.com/moby/moby/client"
)

const volumeProbeOwnerLabel = "com.docker-manager.volume-probe"

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

func (s *dockerVolumeService) MeasureVolumeSize(ctx context.Context, volumeName, helperImage string) (size int64, retErr error) {
	size = -1
	if strings.TrimSpace(helperImage) == "" {
		helperImage = volumeDefaultSizeImage
	}
	volumeResult, err := s.cli.VolumeInspect(ctx, volumeName, mobyclient.VolumeInspectOptions{})
	if err != nil {
		return -1, fmt.Errorf("inspect volume %q before size probe: %w", volumeName, err)
	}
	if volumeResult.Volume.Driver != "local" {
		return -1, fmt.Errorf("volume %q uses unsupported driver %q", volumeName, volumeResult.Volume.Driver)
	}
	if strings.TrimSpace(volumeResult.Volume.Mountpoint) == "" {
		return -1, fmt.Errorf("volume %q has no mountpoint", volumeName)
	}
	if _, err := s.cli.ImageInspect(ctx, helperImage); err != nil {
		return -1, fmt.Errorf("helper image %q is not available on target Docker: %w", helperImage, err)
	}

	probeID := newVolumeProbeID()
	containerName := "dm_volume_size_" + safeVolumeProbeName(volumeName) + "_" + probeID
	resp, err := s.cli.ContainerCreate(ctx, volumeProbeCreateOptions(volumeResult.Volume.Mountpoint, helperImage, containerName, probeID))
	if err != nil {
		cleanupErr := removeOwnedVolumeProbeContainer(s.cli, containerName, probeID)
		return -1, errors.Join(fmt.Errorf("create size probe container failed: %w", err), cleanupErr)
	}
	if strings.TrimSpace(resp.ID) == "" {
		cleanupErr := removeOwnedVolumeProbeContainer(s.cli, containerName, probeID)
		return -1, errors.Join(errors.New("create size probe container returned an empty ID"), cleanupErr)
	}
	defer func() {
		if cleanupErr := removeVolumeProbeContainer(s.cli, resp.ID); cleanupErr != nil {
			retErr = errors.Join(retErr, cleanupErr)
		}
	}()

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
	size, err = strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return -1, fmt.Errorf("parse size probe output %q failed: %w", strings.TrimSpace(stdout), err)
	}
	return size, nil
}

func volumeProbeCreateOptions(volumeMountpoint, helperImage, containerName, probeID string) mobyclient.ContainerCreateOptions {
	const command = `bytes=$(du -sb /mnt/volume 2>/dev/null | awk '{print $1}'); if [ -n "$bytes" ]; then echo "$bytes"; else du -sk /mnt/volume 2>/dev/null | awk '{print $1 * 1024}'; fi`
	return mobyclient.ContainerCreateOptions{
		Config: &container.Config{
			Image:      helperImage,
			Entrypoint: []string{"sh", "-c"},
			Cmd:        []string{command},
			Labels:     map[string]string{volumeProbeOwnerLabel: probeID},
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    "none",
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges:true"},
			Mounts: []mount.Mount{{
				Type:     mount.TypeBind,
				Source:   volumeMountpoint,
				Target:   "/mnt/volume",
				ReadOnly: true,
			}},
		},
		Name: containerName,
	}
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

func removeVolumeProbeContainer(cli *mobyclient.Client, containerID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, removeErr := cli.ContainerRemove(ctx, containerID, mobyclient.ContainerRemoveOptions{Force: true})
	cancel()
	if removeErr == nil || cerrdefs.IsNotFound(removeErr) {
		return nil
	}
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, inspectErr := cli.ContainerInspect(verifyCtx, containerID, mobyclient.ContainerInspectOptions{})
	verifyCancel()
	if cerrdefs.IsNotFound(inspectErr) {
		return nil
	}
	if inspectErr != nil {
		return errors.Join(fmt.Errorf("remove size probe container %s: %w", containerID, removeErr), fmt.Errorf("verify size probe cleanup %s: %w", containerID, inspectErr))
	}
	return fmt.Errorf("remove size probe container %s: %w; container still exists after verification", containerID, removeErr)
}

func removeOwnedVolumeProbeContainer(cli *mobyclient.Client, containerName, probeID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	result, err := cli.ContainerInspect(ctx, containerName, mobyclient.ContainerInspectOptions{})
	cancel()
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reconcile size probe container %s after create error: %w", containerName, err)
	}
	if result.Container.Config == nil || result.Container.Config.Labels[volumeProbeOwnerLabel] != probeID {
		return fmt.Errorf("container %s exists after probe create error but is not owned by this probe; refusing cleanup", containerName)
	}
	id := result.Container.ID
	if id == "" {
		id = containerName
	}
	return removeVolumeProbeContainer(cli, id)
}

func newVolumeProbeID() string {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err == nil {
		return hex.EncodeToString(random)
	}
	return strconv.FormatInt(time.Now().UnixNano(), 36)
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
