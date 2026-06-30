package diagnostics

import (
	"context"

	"docker-manager/internal/docker"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

type doctorDockerService interface {
	Ping(ctx context.Context) (types.Ping, error)
	ServerVersion(ctx context.Context) (types.Version, error)
}

var newDoctorDockerService = func() (doctorDockerService, error) {
	cli, err := docker.NewClient()
	if err != nil {
		return nil, err
	}
	return &dockerDoctorService{cli: cli}, nil
}

type dockerDoctorService struct {
	cli *client.Client
}

func (s *dockerDoctorService) Ping(ctx context.Context) (types.Ping, error) {
	return s.cli.Ping(ctx)
}

func (s *dockerDoctorService) ServerVersion(ctx context.Context) (types.Version, error) {
	return s.cli.ServerVersion(ctx)
}
