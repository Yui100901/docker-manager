package docker

import (
	"github.com/docker/docker/client"
)

//
// @Author yfy2001
// @Date 2025/12/5 14 21
//

var dockerClient *client.Client

func initDockerClient() (*client.Client, error) {
	if dockerClient == nil {
		cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return nil, err
		}
		dockerClient = cli
	}
	return dockerClient, nil
}
