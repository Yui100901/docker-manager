package reverse

import (
	"docker-manager/docker"
	"os"

	"github.com/Yui100901/MyGo/command"
)

//
// @Author yfy2001
// @Date 2026/1/12 22 26
//

func rerunContainers(reverseResult *ReverseResult, rt ReverseType) error {
	// rerun docker run
	for name, cmdSlice := range reverseResult.RunCommands {
		docker.ContainerStop(name)
		docker.ContainerRemove(name, true, false)

		if err := command.RunCommand(cmdSlice[0], cmdSlice[1:]...); err != nil {
			return err
		}
	}

	// rerun compose
	if rt == ReverseCompose || rt == ReverseAll {
		ymlString := reverseResult.DockerComposeFileString()
		tmp := "docker-compose.reverse.yml"
		if err := os.WriteFile(tmp, []byte(ymlString), 0644); err != nil {
			return err
		}
		return command.RunCommand("docker", "compose", "-f", tmp, "up", "-d")
	}

	return nil
}
