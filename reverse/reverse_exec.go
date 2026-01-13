package reverse

import (
	"docker-manager/docker"
	"fmt"
	"os"

	"github.com/Yui100901/MyGo/command"
	"gopkg.in/yaml.v3"
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

func saveOutput(reverseResult *ReverseResult, rt ReverseType) error {
	if rt == ReverseCmd || rt == ReverseAll {
		f, err := os.Create("docker_run_command.sh")
		if err != nil {
			return err
		}
		defer f.Close()

		fmt.Fprintln(f, "#!/bin/bash")
		fmt.Fprint(f, reverseResult.DockerRunCommandString(reverseResult.options.PrettyFormat))
	}

	if rt == ReverseCompose || rt == ReverseAll {
		yml, _ := yaml.Marshal(ComposeFile{Services: reverseResult.ComposeMap})
		return os.WriteFile("docker-compose.reverse.yml", yml, 0644)
	}

	return nil
}
