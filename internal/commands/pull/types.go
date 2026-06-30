package pull

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/Yui100901/MyGo/network/http_utils"
)

const (
	defaultRegistry = "registry-1.docker.io"
	dockerHubDomain = "docker.io"
	// Docker schema v2 media types. OCI media types are provided by ocispec.
	dockerManifestV2     = "application/vnd.docker.distribution.manifest.v2+json"
	dockerManifestListV2 = "application/vnd.docker.distribution.manifest.list.v2+json"
	// Maximum number of image layers downloaded in parallel.
	maxLayerConcurrency = 4
	// HTTP retry/backoff config.
	maxHTTPRetries           = 3
	initialBackoff           = 1 * time.Second
	defaultPullTimeout       = 30 * time.Second
	downloadProgressInterval = 5 * time.Second
)

var pullProgressMu sync.Mutex

type targetPlatform struct {
	targetOS   string
	targetArch string
}

type ImageInfo struct {
	Registry   string
	Repository string
	Image      string
	Tag        string
	Digest     string
}

type PullOptions struct {
	Context        context.Context
	Output         string
	OutputDir      string
	Load           bool
	To             string
	DockerConfig   string
	PlainHTTP      bool
	ProgressOutput io.Writer
}

type CommandDefaults struct {
	Proxy     string
	TargetOS  string
	Arch      string
	OutputDir string
}

// PullRunner owns all side-effectful dependencies for pull. Tests replace
// Docker and credential-helper functions here without having to shell out.
type PullRunner struct {
	platform            targetPlatform
	httpClient          *http_utils.HTTPClient
	loadPulledImage     func(ctx context.Context, path string, output io.Writer) error
	tagPulledImage      func(ctx context.Context, source, target string) error
	pushPulledImage     func(ctx context.Context, target, registryAuth string, output io.Writer) error
	runCredentialHelper func(ctx context.Context, helper, server string) (pullRegistryCredential, error)
}
