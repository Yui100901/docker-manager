package diagnostics

import (
	"context"

	"docker-manager/internal/docker"

	"github.com/moby/moby/api/types/registry"
	mobyclient "github.com/moby/moby/client"
)

type registryLoginDockerService interface {
	RegistryLogin(ctx context.Context, auth registry.AuthConfig) (registry.AuthResponse, error)
}

var newRegistryLoginDockerService = func() (registryLoginDockerService, error) {
	cli, err := docker.NewMobyClient()
	if err != nil {
		return nil, err
	}
	return &dockerRegistryLoginService{cli: cli}, nil
}

type dockerRegistryLoginService struct {
	cli *mobyclient.Client
}

func dockerRegistryLogin(ctx context.Context, registryName string, cred registryCredential) CheckResult {
	if !cred.Found {
		return CheckResult{Status: "skipped", Message: "没有可用于 Docker RegistryLogin 的凭据"}
	}
	svc, err := newRegistryLoginDockerService()
	if err != nil {
		return CheckResult{Status: "failed", Message: err.Error()}
	}
	auth := registry.AuthConfig{
		Username:      cred.Username,
		Password:      cred.Password,
		IdentityToken: cred.IdentityToken,
		ServerAddress: registryName,
	}
	resp, err := svc.RegistryLogin(ctx, auth)
	if err != nil {
		return CheckResult{Status: "failed", Message: err.Error()}
	}
	if resp.Status != "" {
		return CheckResult{Status: "ok", Message: resp.Status}
	}
	return CheckResult{Status: "ok", Message: "Docker registry 登录已接受"}
}

func (s *dockerRegistryLoginService) RegistryLogin(ctx context.Context, auth registry.AuthConfig) (registry.AuthResponse, error) {
	result, err := s.cli.RegistryLogin(ctx, mobyclient.RegistryLoginOptions{
		Username:      auth.Username,
		Password:      auth.Password,
		ServerAddress: auth.ServerAddress,
		IdentityToken: auth.IdentityToken,
		RegistryToken: auth.RegistryToken,
	})
	if err != nil {
		return registry.AuthResponse{}, err
	}
	return result.Auth, nil
}
