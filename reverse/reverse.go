package reverse

import (
	"docker-manager/docker"
	"encoding/json"
	"fmt"
	"github.com/Yui100901/MyGo/command"
	"github.com/Yui100901/MyGo/log_utils"
	"github.com/spf13/cobra"
	"os"
	"strings"
)

//
// @Author yfy2001
// @Date 2025/7/18 09 48
//

func NewReverseCommand() *cobra.Command {
	var rerun bool
	cmd := &cobra.Command{
		Use:   "reverse <name...>",
		Short: "逆向Docker容器到启动命令",
		Run: func(cmd *cobra.Command, args []string) {
			containers := args
			if cmds, err := reverse(containers); err != nil {
				log_utils.Error.Fatalf("Error to reverse container: %v", err)
			} else {
				file, err := os.Create("docker_commands.sh")
				if err != nil {
					log_utils.Error.Fatalf("Failed to create file: %v", err)
				}
				defer file.Close()
				fmt.Fprintln(file, "#!/bin/bash")
				for name, cmd := range cmds {
					fmt.Fprintf(file, "# %s\n", name)
					fmt.Fprintln(file, strings.Join(cmd, " "))
					log_utils.Info.Printf("Generated docker command:\n%s", strings.Join(cmd, " "))
					if rerun {
						docker.ContainerStop(name)
						docker.ContainerRemove(name)
						command.RunCommand(cmd[0], cmd[1:]...)
					}
				}
				log_utils.Info.Println("Save command to docker_commands.sh successfully!")
			}
		},
	}
	cmd.Flags().BoolVarP(&rerun, "rerun", "r", false, "逆向解析完成后以解析出的命令重新创建容器")
	return cmd
}

func reverse(names []string) (map[string][]string, error) {
	data, err := docker.ContainerInspect(names...)
	if err != nil {
		log_utils.Error.Println("Reverse Error", err)
		return nil, err
	}
	var containerInfoList []docker.ContainerInfo
	cleanData := strings.ReplaceAll(data, "\n", "")
	if err := json.Unmarshal([]byte(cleanData), &containerInfoList); err != nil {
		return nil, err
	}
	commandMap := make(map[string][]string)
	for _, containerInfo := range containerInfoList {
		name := containerInfo.Name
		dockerCommand := docker.NewDockerCommand(&containerInfo)
		commandStr := dockerCommand.ToCommand()
		commandMap[name] = commandStr
	}
	return commandMap, nil
}
