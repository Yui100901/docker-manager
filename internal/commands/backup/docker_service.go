package backup

import (
	"context"
	"io"
	"log"
	"os"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
	mobyclient "github.com/moby/moby/client"

	"docker-manager/internal/docker"
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
	return backupCopyWithContext(ctx, output, resp)
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
	if _, err := s.cli.NetworkInspect(ctx, inspect.Name, mobyclient.NetworkInspectOptions{}); err == nil {
		log.Printf("Skip existing network: %s", inspect.Name)
		return nil
	} else if !cerrdefs.IsNotFound(err) {
		return err
	}

	ipam, err := docker.ConvertDockerPointer[network.IPAM](&inspect.IPAM)
	if err != nil {
		return err
	}
	enableIPv4 := inspect.EnableIPv4
	enableIPv6 := inspect.EnableIPv6
	createOptions := mobyclient.NetworkCreateOptions{
		Driver:     inspect.Driver,
		Scope:      inspect.Scope,
		EnableIPv4: &enableIPv4,
		EnableIPv6: &enableIPv6,
		IPAM:       ipam,
		Internal:   inspect.Internal,
		Attachable: inspect.Attachable,
		Ingress:    inspect.Ingress,
		ConfigOnly: inspect.ConfigOnly,
		Options:    inspect.Options,
		Labels:     inspect.Labels,
	}
	if inspect.ConfigFrom.Network != "" {
		createOptions.ConfigFrom = inspect.ConfigFrom.Network
	}
	_, err = s.cli.NetworkCreate(ctx, inspect.Name, createOptions)
	return err
}

func (s *dockerBackupService) InspectVolume(ctx context.Context, name string) (volume.Volume, error) {
	result, err := s.cli.VolumeInspect(ctx, name, mobyclient.VolumeInspectOptions{})
	if err != nil {
		return volume.Volume{}, err
	}
	return docker.ConvertDockerType[volume.Volume](result.Volume)
}

func (s *dockerBackupService) CreateVolume(ctx context.Context, vol volume.Volume) error {
	if _, err := s.cli.VolumeInspect(ctx, vol.Name, mobyclient.VolumeInspectOptions{}); err == nil {
		log.Printf("Skip existing volume: %s", vol.Name)
		return nil
	} else if !cerrdefs.IsNotFound(err) {
		return err
	}

	_, err := s.cli.VolumeCreate(ctx, mobyclient.VolumeCreateOptions{
		Name:       vol.Name,
		Driver:     vol.Driver,
		DriverOpts: vol.Options,
		Labels:     vol.Labels,
	})
	return err
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
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

func (s *dockerBackupService) StartContainer(ctx context.Context, id string) error {
	_, err := s.cli.ContainerStart(ctx, id, mobyclient.ContainerStartOptions{})
	return err
}
