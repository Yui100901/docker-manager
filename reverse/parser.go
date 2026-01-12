package reverse

import (
	"docker-manager/docker"
	"log"

	"github.com/docker/docker/api/types/container"
)

//
// @Author yfy2001
// @Date 2026/1/12 21 06
//

type Parser struct {
}

type ParsedResult struct {
	Name    string
	Command []string
	Compose ComposeService
}

func reverse(names []string) ([]ParsedResult, error) {
	var infos []container.InspectResponse

	for _, name := range names {
		info, err := docker.ContainerInspect(name)
		if err != nil {
			log.Printf("容器 %s 解析失败: %v", name, err)
			continue
		}
		infos = append(infos, info)
	}

	var results []ParsedResult
	for _, info := range infos {
		name := trimContainerName(info.Name)
		spec := NewDockerSpec(&info)

		results = append(results, ParsedResult{
			Name:    name,
			Command: spec.ToCommand(),
			Compose: spec.ToComposeService(),
		})
	}

	return results, nil
}

func trimContainerName(name string) string {
	if len(name) > 0 && name[0] == '/' {
		return name[1:]
	}
	return name
}
