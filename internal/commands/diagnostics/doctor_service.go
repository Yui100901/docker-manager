package diagnostics

import (
	"context"

	"docker-manager/internal/docker"

	"github.com/docker/docker/api/types"
	oldbuild "github.com/docker/docker/api/types/build"
	oldswarm "github.com/docker/docker/api/types/swarm"
	mobyclient "github.com/moby/moby/client"
)

type doctorDockerService interface {
	Ping(ctx context.Context) (types.Ping, error)
	ServerVersion(ctx context.Context) (types.Version, error)
	DaemonHost() string
	ClientVersion() string
}

var newDoctorDockerService = func() (doctorDockerService, error) {
	cli, err := docker.NewMobyClient()
	if err != nil {
		return nil, err
	}
	return &dockerDoctorService{cli: cli}, nil
}

type dockerDoctorService struct {
	cli *mobyclient.Client
}

func (s *dockerDoctorService) Ping(ctx context.Context) (types.Ping, error) {
	result, err := s.cli.Ping(ctx, mobyclient.PingOptions{})
	if err != nil {
		return types.Ping{}, err
	}
	ping := types.Ping{
		APIVersion:     result.APIVersion,
		OSType:         result.OSType,
		Experimental:   result.Experimental,
		BuilderVersion: oldbuild.BuilderVersion(result.BuilderVersion),
	}
	if result.SwarmStatus != nil {
		status, err := docker.ConvertDockerType[oldswarm.Status](*result.SwarmStatus)
		if err != nil {
			return types.Ping{}, err
		}
		ping.SwarmStatus = &status
	}
	return ping, nil
}

func (s *dockerDoctorService) ServerVersion(ctx context.Context) (types.Version, error) {
	result, err := s.cli.ServerVersion(ctx, mobyclient.ServerVersionOptions{})
	if err != nil {
		return types.Version{}, err
	}
	version, err := docker.ConvertDockerType[types.Version](result)
	if err != nil {
		return types.Version{}, err
	}
	version.Platform.Name = result.Platform.Name
	return version, nil
}

func (s *dockerDoctorService) DaemonHost() string {
	return s.cli.DaemonHost()
}

func (s *dockerDoctorService) ClientVersion() string {
	return s.cli.ClientVersion()
}
