package docker

import (
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/docker/docker/client"
)

var (
	dockerClient   *client.Client
	dockerClientMu sync.Mutex
	clientOptions  Options
)

// Options describes the Docker daemon endpoint selected for Docker API calls.
// Empty fields preserve Docker SDK defaults and the corresponding DOCKER_* env
// values.
type Options struct {
	Host       string
	TLSVerify  *bool
	CertPath   string
	APIVersion string
}

// NewClient returns the shared Docker API client used by docker-manager.
func NewClient() (*client.Client, error) {
	return initDockerClient()
}

// Configure sets the Docker API endpoint used by future clients. It is normally
// called once from the root command after reading config and global flags.
func Configure(opts Options) {
	dockerClientMu.Lock()
	defer dockerClientMu.Unlock()

	if sameOptions(clientOptions, opts) {
		return
	}
	if dockerClient != nil {
		_ = dockerClient.Close()
		dockerClient = nil
	}
	clientOptions = opts
}

// CurrentOptions returns the effective explicit options. Environment-derived
// values are intentionally not expanded here.
func CurrentOptions() Options {
	dockerClientMu.Lock()
	defer dockerClientMu.Unlock()
	return clientOptions
}

// Endpoint returns the currently selected Docker endpoint. It prefers explicit
// config and DOCKER_* env values, then falls back to the platform local daemon.
func Endpoint() string {
	opts := EffectiveOptions()
	if strings.TrimSpace(opts.Host) != "" {
		return strings.TrimSpace(opts.Host)
	}
	return defaultLocalEndpoint()
}

// IsRemoteEndpoint reports whether the selected endpoint is not the platform
// local Docker socket or named pipe.
func IsRemoteEndpoint() bool {
	host := strings.ToLower(strings.TrimSpace(Endpoint()))
	return !(strings.HasPrefix(host, "unix://") || strings.HasPrefix(host, "npipe://"))
}

// EffectiveOptions resolves explicit options over Docker's DOCKER_* environment
// variables. SDK defaults such as the platform-specific local socket are not
// expanded here; callers can read Client.DaemonHost after NewClient.
func EffectiveOptions() Options {
	dockerClientMu.Lock()
	defer dockerClientMu.Unlock()

	opts := Options{
		Host:       os.Getenv(client.EnvOverrideHost),
		CertPath:   os.Getenv(client.EnvOverrideCertPath),
		APIVersion: os.Getenv(client.EnvOverrideAPIVersion),
	}
	if os.Getenv(client.EnvTLSVerify) != "" {
		value := true
		opts.TLSVerify = &value
	}
	if clientOptions.Host != "" {
		opts.Host = clientOptions.Host
	}
	if clientOptions.CertPath != "" {
		opts.CertPath = clientOptions.CertPath
	}
	if clientOptions.APIVersion != "" {
		opts.APIVersion = clientOptions.APIVersion
	}
	if clientOptions.TLSVerify != nil {
		value := *clientOptions.TLSVerify
		opts.TLSVerify = &value
	}
	return opts
}

func defaultLocalEndpoint() string {
	if runtime.GOOS == "windows" {
		return "npipe:////./pipe/docker_engine"
	}
	return "unix:///var/run/docker.sock"
}

func sameOptions(a, b Options) bool {
	if a.Host != b.Host || a.CertPath != b.CertPath || a.APIVersion != b.APIVersion {
		return false
	}
	switch {
	case a.TLSVerify == nil && b.TLSVerify == nil:
		return true
	case a.TLSVerify != nil && b.TLSVerify != nil:
		return *a.TLSVerify == *b.TLSVerify
	default:
		return false
	}
}

func initDockerClient() (*client.Client, error) {
	dockerClientMu.Lock()
	defer dockerClientMu.Unlock()

	if dockerClient == nil {
		restore := applyDockerEnvForClient(clientOptions)
		defer restore()

		cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return nil, err
		}
		dockerClient = cli
	}
	return dockerClient, nil
}

func applyDockerEnvForClient(opts Options) func() {
	keys := []string{
		client.EnvOverrideHost,
		client.EnvOverrideAPIVersion,
		client.EnvOverrideCertPath,
		client.EnvTLSVerify,
	}
	previous := make(map[string]*string, len(keys))
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			copied := value
			previous[key] = &copied
		} else {
			previous[key] = nil
		}
	}

	if opts.Host != "" {
		_ = os.Setenv(client.EnvOverrideHost, opts.Host)
	}
	if opts.APIVersion != "" {
		_ = os.Setenv(client.EnvOverrideAPIVersion, opts.APIVersion)
	}
	if opts.CertPath != "" {
		_ = os.Setenv(client.EnvOverrideCertPath, opts.CertPath)
	}
	if opts.TLSVerify != nil {
		if *opts.TLSVerify {
			_ = os.Setenv(client.EnvTLSVerify, "1")
		} else {
			_ = os.Unsetenv(client.EnvTLSVerify)
		}
	}

	return func() {
		for _, key := range keys {
			if value := previous[key]; value != nil {
				_ = os.Setenv(key, *value)
			} else {
				_ = os.Unsetenv(key)
			}
		}
	}
}
