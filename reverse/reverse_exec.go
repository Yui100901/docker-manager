package reverse

import (
	"fmt"
	"os"
	"strings"

	"github.com/Yui100901/MyGo/command"
	"gopkg.in/yaml.v3"
)

//
// @Author yfy2001
// @Date 2026/1/12 22 26
//

func rerunContainers(cmdMap map[string]string, composeMap map[string]ComposeService, rt ReverseType) error {
	// rerun docker run
	for name, cmdStr := range cmdMap {
		parts := strings.Split(cmdStr, " ")
		command.RunCommand("docker", "stop", name)
		command.RunCommand("docker", "rm", "-f", name)

		if err := command.RunCommand(parts[0], parts[1:]...); err != nil {
			return err
		}
	}

	// rerun compose
	if rt == ReverseCompose || rt == ReverseAll {
		yml, _ := yaml.Marshal(ComposeFile{Services: composeMap})
		tmp := "docker-compose.reverse.yml"
		if err := os.WriteFile(tmp, yml, 0644); err != nil {
			return err
		}
		return command.RunCommand("docker", "compose", "-f", tmp, "up", "-d")
	}

	return nil
}

func saveOutput(cmdMap map[string]string, composeMap map[string]ComposeService, rt ReverseType) error {
	if rt == ReverseCmd || rt == ReverseAll {
		f, err := os.Create("docker_run_command.sh")
		if err != nil {
			return err
		}
		defer f.Close()

		fmt.Fprintln(f, "#!/bin/bash")
		for name, cmd := range cmdMap {
			fmt.Fprintf(f, "# %s\n%s\n\n", name, cmd)
		}
	}

	if rt == ReverseCompose || rt == ReverseAll {
		yml, _ := yaml.Marshal(ComposeFile{Services: composeMap})
		return os.WriteFile("docker-compose.reverse.yml", yml, 0644)
	}

	return nil
}
