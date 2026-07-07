package diagnostics

import (
	"context"

	"docker-manager/internal/docker"

	mobyclient "github.com/moby/moby/client"
)

type doctorDockerService interface {
	Ping(ctx context.Context) (mobyclient.PingResult, error)
	ServerVersion(ctx context.Context) (mobyclient.ServerVersionResult, error)
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

func (s *dockerDoctorService) Ping(ctx context.Context) (mobyclient.PingResult, error) {
	return s.cli.Ping(ctx, mobyclient.PingOptions{})
}

func (s *dockerDoctorService) ServerVersion(ctx context.Context) (mobyclient.ServerVersionResult, error) {
	return s.cli.ServerVersion(ctx, mobyclient.ServerVersionOptions{})
}

func (s *dockerDoctorService) DaemonHost() string {
	return s.cli.DaemonHost()
}

func (s *dockerDoctorService) ClientVersion() string {
	return s.cli.ClientVersion()
}
