package docker

import (
	"sync"

	"github.com/docker/docker/client"
)

//
// @Author yfy2001
// @Date 2025/12/5 14 21
//

var (
	dockerClient   *client.Client
	dockerClientMu sync.Mutex
)

// NewClient returns the shared Docker API client used by docker-manager.
func NewClient() (*client.Client, error) {
	return initDockerClient()
}

func initDockerClient() (*client.Client, error) {
	dockerClientMu.Lock()
	defer dockerClientMu.Unlock()

	if dockerClient == nil {
		cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return nil, err
		}
		dockerClient = cli
	}
	return dockerClient, nil
}
