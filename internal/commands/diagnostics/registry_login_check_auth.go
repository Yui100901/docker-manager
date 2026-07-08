package diagnostics

import (
	"context"

	"docker-manager/internal/registryauth"
)

var runDockerCredentialHelper registryauth.HelperRunner = defaultRunDockerCredentialHelper

func defaultDockerConfigPath() string {
	return registryauth.DefaultConfigPath()
}

func readDockerConfig(path string) (dockerConfigFile, bool, error) {
	return registryauth.ReadConfig(path)
}

func resolveRegistryCredential(ctx context.Context, cfg dockerConfigFile, registryName string) registryCredential {
	return registryauth.ResolveCredential(ctx, cfg, registryName, runDockerCredentialHelper)
}

func findCredentialHelper(cfg dockerConfigFile, keys []string) (string, string) {
	return registryauth.FindCredentialHelper(cfg, keys)
}

func registryConfigKeys(registryName string) []string {
	return registryauth.ConfigKeys(registryName)
}

func credentialFromAuthEntry(entry dockerAuthEntry) registryCredential {
	return registryauth.CredentialFromAuthEntry(entry)
}

func defaultRunDockerCredentialHelper(ctx context.Context, helper, server string) (registryCredential, error) {
	return registryauth.DefaultRunCredentialHelper(ctx, helper, server)
}
