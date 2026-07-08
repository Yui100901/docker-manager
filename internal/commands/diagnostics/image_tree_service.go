package diagnostics

import (
	"context"

	"docker-manager/internal/docker"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	mobyclient "github.com/moby/moby/client"
)

type imageTreeDockerService interface {
	ImageInspect(ctx context.Context, imageRef string) (image.InspectResponse, error)
	ImageHistory(ctx context.Context, imageRef string) ([]image.HistoryResponseItem, error)
	ImageList(ctx context.Context) ([]image.Summary, error)
	ListContainers(ctx context.Context, all bool) ([]container.Summary, error)
	InspectContainer(ctx context.Context, id string) (container.InspectResponse, error)
}

var newImageTreeDockerService = func() (imageTreeDockerService, error) {
	cli, err := docker.NewMobyClient()
	if err != nil {
		return nil, err
	}
	return &dockerImageTreeService{cli: cli}, nil
}

type dockerImageTreeService struct {
	cli *mobyclient.Client
}

func (s *dockerImageTreeService) ImageInspect(ctx context.Context, imageRef string) (image.InspectResponse, error) {
	result, err := s.cli.ImageInspect(ctx, imageRef)
	if err != nil {
		return image.InspectResponse{}, err
	}
	return docker.ConvertDockerType[image.InspectResponse](result)
}

func (s *dockerImageTreeService) ImageHistory(ctx context.Context, imageRef string) ([]image.HistoryResponseItem, error) {
	result, err := s.cli.ImageHistory(ctx, imageRef)
	if err != nil {
		return nil, err
	}
	return docker.ConvertDockerType[[]image.HistoryResponseItem](result.Items)
}

func (s *dockerImageTreeService) ImageList(ctx context.Context) ([]image.Summary, error) {
	result, err := s.cli.ImageList(ctx, mobyclient.ImageListOptions{All: true})
	if err != nil {
		return nil, err
	}
	return docker.ConvertDockerType[[]image.Summary](result.Items)
}

func (s *dockerImageTreeService) ListContainers(ctx context.Context, all bool) ([]container.Summary, error) {
	result, err := s.cli.ContainerList(ctx, mobyclient.ContainerListOptions{All: all})
	if err != nil {
		return nil, err
	}
	return docker.ConvertDockerType[[]container.Summary](result.Items)
}

func (s *dockerImageTreeService) InspectContainer(ctx context.Context, id string) (container.InspectResponse, error) {
	result, err := s.cli.ContainerInspect(ctx, id, mobyclient.ContainerInspectOptions{})
	if err != nil {
		return container.InspectResponse{}, err
	}
	return docker.ConvertDockerType[container.InspectResponse](result.Container)
}
