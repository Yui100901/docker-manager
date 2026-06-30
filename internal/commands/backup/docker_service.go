package backup

import (
	"context"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"io"
	"log"
	"os"
)

func (s *dockerBackupService) ListContainers(ctx context.Context, all bool) ([]container.Summary, error) {
	return s.cli.ContainerList(ctx, container.ListOptions{All: all})
}

func (s *dockerBackupService) InspectContainer(ctx context.Context, name string) (container.InspectResponse, error) {
	return s.cli.ContainerInspect(ctx, name)
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

	resp, err := s.cli.ImageLoad(ctx, file, client.ImageLoadWithQuiet(false))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return backupCopyWithContext(ctx, output, resp.Body)
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
	return s.cli.NetworkInspect(ctx, name, network.InspectOptions{})
}

func (s *dockerBackupService) CreateNetwork(ctx context.Context, inspect network.Inspect) error {
	if isBuiltinNetwork(inspect.Name) {
		return nil
	}
	if _, err := s.cli.NetworkInspect(ctx, inspect.Name, network.InspectOptions{}); err == nil {
		log.Printf("Skip existing network: %s", inspect.Name)
		return nil
	} else if !client.IsErrNotFound(err) {
		return err
	}
	enableIPv4 := inspect.EnableIPv4
	enableIPv6 := inspect.EnableIPv6
	createOptions := network.CreateOptions{
		Driver:     inspect.Driver,
		Scope:      inspect.Scope,
		EnableIPv4: &enableIPv4,
		EnableIPv6: &enableIPv6,
		IPAM:       &inspect.IPAM,
		Internal:   inspect.Internal,
		Attachable: inspect.Attachable,
		Ingress:    inspect.Ingress,
		ConfigOnly: inspect.ConfigOnly,
		Options:    inspect.Options,
		Labels:     inspect.Labels,
	}
	if inspect.ConfigFrom.Network != "" {
		createOptions.ConfigFrom = &inspect.ConfigFrom
	}
	_, err := s.cli.NetworkCreate(ctx, inspect.Name, createOptions)
	return err
}

func (s *dockerBackupService) InspectVolume(ctx context.Context, name string) (volume.Volume, error) {
	return s.cli.VolumeInspect(ctx, name)
}

func (s *dockerBackupService) CreateVolume(ctx context.Context, vol volume.Volume) error {
	if _, err := s.cli.VolumeInspect(ctx, vol.Name); err == nil {
		log.Printf("Skip existing volume: %s", vol.Name)
		return nil
	} else if !client.IsErrNotFound(err) {
		return err
	}
	_, err := s.cli.VolumeCreate(ctx, volume.CreateOptions{
		Name:       vol.Name,
		Driver:     vol.Driver,
		DriverOpts: vol.Options,
		Labels:     vol.Labels,
	})
	return err
}

func (s *dockerBackupService) ContainerExists(ctx context.Context, name string) (bool, error) {
	_, err := s.cli.ContainerInspect(ctx, name)
	if err == nil {
		return true, nil
	}
	if client.IsErrNotFound(err) {
		return false, nil
	}
	return false, err
}

func (s *dockerBackupService) RemoveContainer(ctx context.Context, name string) error {
	return s.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true, RemoveVolumes: false})
}

func (s *dockerBackupService) CreateContainer(ctx context.Context, inspect container.InspectResponse, name string) (string, error) {
	networkingConfig := restoreNetworkingConfig(inspect)
	resp, err := s.cli.ContainerCreate(ctx, inspect.Config, inspect.HostConfig, networkingConfig, nil, name)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

func (s *dockerBackupService) StartContainer(ctx context.Context, id string) error {
	return s.cli.ContainerStart(ctx, id, container.StartOptions{})
}
