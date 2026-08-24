package backup

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
	mobyclient "github.com/moby/moby/client"

	"docker-manager/internal/docker"
)

const (
	restoreResourceVerifyTimeout  = 3 * time.Second
	restoreResourceVerifyInterval = 100 * time.Millisecond
)

func (s *dockerBackupService) ListContainers(ctx context.Context, all bool) ([]container.Summary, error) {
	result, err := s.cli.ContainerList(ctx, mobyclient.ContainerListOptions{All: all})
	if err != nil {
		return nil, err
	}
	return docker.ConvertDockerType[[]container.Summary](result.Items)
}

func (s *dockerBackupService) InspectContainer(ctx context.Context, name string) (container.InspectResponse, error) {
	result, err := s.cli.ContainerInspect(ctx, name, mobyclient.ContainerInspectOptions{})
	if err != nil {
		return container.InspectResponse{}, err
	}
	return docker.ConvertDockerType[container.InspectResponse](result.Container)
}

func (s *dockerBackupService) SaveImage(ctx context.Context, refs []string, outputFile string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	reader, err := s.cli.ImageSave(ctx, refs)
	if err != nil {
		return err
	}
	defer reader.Close()

	file, err := os.Create(outputFile)
	if err != nil {
		return err
	}
	defer file.Close()

	return backupCopyWithContext(ctx, file, reader)
}

func (s *dockerBackupService) LoadImage(ctx context.Context, inputFile string, output io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if output == nil {
		output = io.Discard
	}
	file, err := os.Open(inputFile)
	if err != nil {
		return err
	}
	defer file.Close()

	resp, err := s.cli.ImageLoad(ctx, file, mobyclient.ImageLoadWithQuiet(false))
	if err != nil {
		return err
	}
	defer resp.Close()
	return copyDockerLoadStream(ctx, output, resp)
}

type dockerLoadMessage struct {
	Error       string `json:"error"`
	ErrorDetail struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
}

func copyDockerLoadStream(ctx context.Context, dst io.Writer, src io.Reader) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if dst == nil {
		dst = io.Discard
	}
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Text()
		if _, err := io.WriteString(dst, line+"\n"); err != nil {
			return err
		}
		var message dockerLoadMessage
		if err := json.Unmarshal([]byte(line), &message); err == nil {
			if message.ErrorDetail.Message != "" {
				return fmt.Errorf("docker image load failed: %s", message.ErrorDetail.Message)
			}
			if message.Error != "" {
				return fmt.Errorf("docker image load failed: %s", message.Error)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return ctx.Err()
}

func (s *dockerBackupService) ImageExists(ctx context.Context, ref string) (bool, error) {
	if ref == "" {
		return false, nil
	}
	_, err := s.cli.ImageInspect(ctx, ref)
	if err == nil {
		return true, nil
	}
	if cerrdefs.IsNotFound(err) {
		return false, nil
	}
	return false, err
}

func backupCopyWithContext(ctx context.Context, dst io.Writer, src io.Reader) error {
	if ctx == nil {
		ctx = context.Background()
	}
	buf := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}

func (s *dockerBackupService) InspectNetwork(ctx context.Context, name string) (network.Inspect, error) {
	result, err := s.cli.NetworkInspect(ctx, name, mobyclient.NetworkInspectOptions{})
	if err != nil {
		return network.Inspect{}, err
	}
	return docker.ConvertDockerType[network.Inspect](result.Network)
}

func (s *dockerBackupService) CreateNetwork(ctx context.Context, inspect network.Inspect) error {
	if isBuiltinNetwork(inspect.Name) {
		return nil
	}
	if result, err := s.cli.NetworkInspect(ctx, inspect.Name, mobyclient.NetworkInspectOptions{}); err == nil {
		actual, err := docker.ConvertDockerType[network.Inspect](result.Network)
		if err != nil {
			return err
		}
		if !restoreNetworkFingerprintsEqual(newRestoreNetworkFingerprint(inspect), newRestoreNetworkFingerprint(actual)) {
			return fmt.Errorf("existing network %s differs from the restore definition", inspect.Name)
		}
		log.Printf("Skip existing network: %s", inspect.Name)
		return nil
	} else if !cerrdefs.IsNotFound(err) {
		return err
	}

	fingerprint := newRestoreNetworkFingerprint(inspect)
	createIPAM := restoreNetworkCreateIPAM(inspect.IPAM)
	ipam, err := docker.ConvertDockerPointer[network.IPAM](&createIPAM)
	if err != nil {
		return err
	}
	enableIPv4 := fingerprint.EnableIPv4
	enableIPv6 := fingerprint.EnableIPv6
	createOptions := mobyclient.NetworkCreateOptions{
		Driver:     inspect.Driver,
		Scope:      fingerprint.Scope,
		EnableIPv4: &enableIPv4,
		EnableIPv6: &enableIPv6,
		IPAM:       ipam,
		Internal:   inspect.Internal,
		Attachable: inspect.Attachable,
		Ingress:    inspect.Ingress,
		ConfigOnly: inspect.ConfigOnly,
		Options:    fingerprint.Options,
		Labels:     fingerprint.Labels,
	}
	if inspect.ConfigFrom.Network != "" {
		createOptions.ConfigFrom = inspect.ConfigFrom.Network
	}
	if _, err = s.cli.NetworkCreate(ctx, inspect.Name, createOptions); err != nil {
		return err
	}
	verifyCtx, cancel := context.WithTimeout(ctx, restoreResourceVerifyTimeout)
	defer cancel()
	return waitForRestoredResource(
		verifyCtx,
		"network",
		inspect.Name,
		restoreResourceVerifyInterval,
		func(ctx context.Context) (network.Inspect, error) { return s.InspectNetwork(ctx, inspect.Name) },
		func(actual network.Inspect) bool { return restoredNetworkMatchesCreateRequest(inspect, actual) },
	)
}

func (s *dockerBackupService) InspectVolume(ctx context.Context, name string) (volume.Volume, error) {
	result, err := s.cli.VolumeInspect(ctx, name, mobyclient.VolumeInspectOptions{})
	if err != nil {
		return volume.Volume{}, err
	}
	return docker.ConvertDockerType[volume.Volume](result.Volume)
}

func (s *dockerBackupService) CreateVolume(ctx context.Context, vol volume.Volume) error {
	if result, err := s.cli.VolumeInspect(ctx, vol.Name, mobyclient.VolumeInspectOptions{}); err == nil {
		actual, err := docker.ConvertDockerType[volume.Volume](result.Volume)
		if err != nil {
			return err
		}
		if !restoreVolumeFingerprintsEqual(newRestoreVolumeFingerprint(vol), newRestoreVolumeFingerprint(actual)) {
			return fmt.Errorf("existing volume %s differs from the restore definition", vol.Name)
		}
		log.Printf("Skip existing volume: %s", vol.Name)
		return nil
	} else if !cerrdefs.IsNotFound(err) {
		return err
	}

	if _, err := s.cli.VolumeCreate(ctx, mobyclient.VolumeCreateOptions{
		Name:       vol.Name,
		Driver:     vol.Driver,
		DriverOpts: vol.Options,
		Labels:     vol.Labels,
	}); err != nil {
		return err
	}
	verifyCtx, cancel := context.WithTimeout(ctx, restoreResourceVerifyTimeout)
	defer cancel()
	return waitForRestoredResource(
		verifyCtx,
		"volume",
		vol.Name,
		restoreResourceVerifyInterval,
		func(ctx context.Context) (volume.Volume, error) { return s.InspectVolume(ctx, vol.Name) },
		func(actual volume.Volume) bool { return restoredVolumeMatchesCreateRequest(vol, actual) },
	)
}

func waitForRestoredResource[T any](ctx context.Context, kind, name string, interval time.Duration, inspect func(context.Context) (T, error), matches func(T) bool) error {
	if interval <= 0 {
		interval = time.Millisecond
	}
	lastStatus := "resource was not visible"
	for {
		actual, err := inspect(ctx)
		switch {
		case err == nil && matches(actual):
			return nil
		case err == nil:
			lastStatus = "definition still differs from the create request"
		case cerrdefs.IsNotFound(err):
			lastStatus = "resource was not visible"
		default:
			return fmt.Errorf("inspect restored %s %s: %w", kind, name, err)
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("verify restored %s %s: %s: %w", kind, name, lastStatus, ctx.Err())
		case <-timer.C:
		}
	}
}

func (s *dockerBackupService) ContainerExists(ctx context.Context, name string) (bool, error) {
	_, err := s.cli.ContainerInspect(ctx, name, mobyclient.ContainerInspectOptions{})
	if err == nil {
		return true, nil
	}
	if cerrdefs.IsNotFound(err) {
		return false, nil
	}
	return false, err
}

func (s *dockerBackupService) RemoveContainer(ctx context.Context, name string) error {
	_, err := s.cli.ContainerRemove(ctx, name, mobyclient.ContainerRemoveOptions{Force: true, RemoveVolumes: false})
	return err
}

func (s *dockerBackupService) StopContainer(ctx context.Context, id string) error {
	_, err := s.cli.ContainerStop(ctx, id, mobyclient.ContainerStopOptions{})
	return err
}

func (s *dockerBackupService) RenameContainer(ctx context.Context, id, name string) error {
	_, err := s.cli.ContainerRename(ctx, id, mobyclient.ContainerRenameOptions{NewName: name})
	return err
}

func (s *dockerBackupService) CreateContainer(ctx context.Context, inspect container.InspectResponse, name string) (string, error) {
	networkingConfig := restoreNetworkingConfig(inspect)
	mobyConfig, err := docker.ConvertDockerPointer[container.Config](inspect.Config)
	if err != nil {
		return "", err
	}
	mobyHostConfig, err := docker.ConvertDockerPointer[container.HostConfig](inspect.HostConfig)
	if err != nil {
		return "", err
	}
	mobyNetworkingConfig, err := docker.ConvertDockerPointer[network.NetworkingConfig](networkingConfig)
	if err != nil {
		return "", err
	}
	resp, err := s.cli.ContainerCreate(ctx, mobyclient.ContainerCreateOptions{
		Config:           mobyConfig,
		HostConfig:       mobyHostConfig,
		NetworkingConfig: mobyNetworkingConfig,
		Name:             name,
	})
	return resp.ID, err
}

func (s *dockerBackupService) StartContainer(ctx context.Context, id string) error {
	_, err := s.cli.ContainerStart(ctx, id, mobyclient.ContainerStartOptions{})
	return err
}

func (s *dockerBackupService) WaitContainerReady(ctx context.Context, id string, requireHealthy bool) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		inspect, err := s.InspectContainer(ctx, id)
		if err != nil {
			return err
		}
		if inspect.State == nil || !inspect.State.Running {
			status := "unknown"
			if inspect.State != nil {
				status = string(inspect.State.Status)
			}
			return fmt.Errorf("container is not running after start (status=%s)", status)
		}
		if !requireHealthy {
			return nil
		}
		if inspect.State.Health != nil {
			switch inspect.State.Health.Status {
			case container.Healthy:
				return nil
			case container.Unhealthy:
				return fmt.Errorf("container healthcheck reported unhealthy")
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for container readiness: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
