package docker

import (
	"os"
	"runtime"
	"strings"
	"sync"

	dockerclient "github.com/docker/docker/client"
	mobyclient "github.com/moby/moby/client"
)

var (
	legacyClient   *dockerclient.Client
	mobyClient     *mobyclient.Client
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

// NewClient returns the shared legacy Docker API client used by commands that
// have not yet migrated to github.com/moby/moby/client.
func NewClient() (*dockerclient.Client, error) {
	return initLegacyClient()
}

// NewMobyClient returns the shared Moby API client for migrated code paths.
func NewMobyClient() (*mobyclient.Client, error) {
	return initMobyClient()
}

// Configure sets the Docker API endpoint used by future clients. It is normally
// called once from the root command after reading config and global flags.
func Configure(opts Options) {
	dockerClientMu.Lock()
	defer dockerClientMu.Unlock()

	if sameOptions(clientOptions, opts) {
		return
	}
	if legacyClient != nil {
		_ = legacyClient.Close()
		legacyClient = nil
	}
	if mobyClient != nil {
		_ = mobyClient.Close()
		mobyClient = nil
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
		Host:       os.Getenv(mobyclient.EnvOverrideHost),
		CertPath:   os.Getenv(mobyclient.EnvOverrideCertPath),
		APIVersion: os.Getenv(mobyclient.EnvOverrideAPIVersion),
	}
	if os.Getenv(mobyclient.EnvTLSVerify) != "" {
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

func initLegacyClient() (*dockerclient.Client, error) {
	dockerClientMu.Lock()
	defer dockerClientMu.Unlock()

	if legacyClient == nil {
		restore := applyDockerEnvForClient(clientOptions)
		defer restore()

		cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
		if err != nil {
			return nil, err
		}
		legacyClient = cli
	}
	return legacyClient, nil
}

func initDockerClient() (*dockerclient.Client, error) {
	return initLegacyClient()
}

func initMobyClient() (*mobyclient.Client, error) {
	dockerClientMu.Lock()
	defer dockerClientMu.Unlock()

	if mobyClient == nil {
		restore := applyDockerEnvForClient(clientOptions)
		defer restore()

		cli, err := mobyclient.NewClientWithOpts(mobyclient.FromEnv, mobyclient.WithAPIVersionNegotiation())
		if err != nil {
			return nil, err
		}
		mobyClient = cli
	}
	return mobyClient, nil
}

func applyDockerEnvForClient(opts Options) func() {
	keys := []string{
		mobyclient.EnvOverrideHost,
		mobyclient.EnvOverrideAPIVersion,
		mobyclient.EnvOverrideCertPath,
		mobyclient.EnvTLSVerify,
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
		_ = os.Setenv(mobyclient.EnvOverrideHost, opts.Host)
	}
	if opts.APIVersion != "" {
		_ = os.Setenv(mobyclient.EnvOverrideAPIVersion, opts.APIVersion)
	}
	if opts.CertPath != "" {
		_ = os.Setenv(mobyclient.EnvOverrideCertPath, opts.CertPath)
	}
	if opts.TLSVerify != nil {
		if *opts.TLSVerify {
			_ = os.Setenv(mobyclient.EnvTLSVerify, "1")
		} else {
			_ = os.Unsetenv(mobyclient.EnvTLSVerify)
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
