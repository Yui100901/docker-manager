package reverse

import (
	"docker-manager/docker"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/Yui100901/MyGo/command"
	"gopkg.in/yaml.v3"
)

//
// @Author yfy2001
// @Date 2026/1/13 15 21
//

type ReverseResult struct {
	ParsedResults []ParsedResult
	RunCommands   map[string][]string
	ComposeMap    map[string]ComposeService
	options       ReverseOptions
}

func NewReverseResult(results []ParsedResult, options ReverseOptions) *ReverseResult {
	rr := &ReverseResult{
		ParsedResults: results,
		options:       options,
	}
	rr.RunCommands = make(map[string][]string)
	rr.ComposeMap = make(map[string]ComposeService)

	for _, r := range results {
		rr.RunCommands[r.Name] = r.Command
		rr.ComposeMap[r.Name] = r.Compose
	}
	return rr
}

func (rr *ReverseResult) Print() {
	if rr.options.ReverseType == ReverseCmd || rr.options.ReverseType == ReverseAll {
		fmt.Println(rr.DockerRunCommandString())
	}

	if rr.options.ReverseType == ReverseCompose || rr.options.ReverseType == ReverseAll {
		fmt.Println(rr.DockerComposeFileString())
	}
}

func (rr *ReverseResult) DockerRunCommandString() string {
	var sb strings.Builder
	for name, cmd := range rr.RunCommands {
		sb.WriteString(fmt.Sprintf("# %s\n", name))
		if rr.options.PrettyFormat {
			sb.WriteString(cmd[0]) // docker
			sb.WriteString(" ")
			sb.WriteString(cmd[1]) // run
			sb.WriteString("\n")
			for _, arg := range cmd[2:] {
				sb.WriteString("  ")
				sb.WriteString(arg)
				sb.WriteString("\n")
			}
		} else {
			sb.WriteString(strings.Join(cmd, " "))
			sb.WriteString("\n\n")
		}
	}
	return sb.String()
}

func (rr *ReverseResult) DockerComposeFileString() string {
	yml, _ := yaml.Marshal(ComposeFile{Services: rr.ComposeMap})
	return string(yml)
}

func reverseWithOptions(names []string, options ReverseOptions) (*ReverseResult, error) {
	var results []ParsedResult

	for _, name := range names {
		info, err := docker.ContainerInspect(name)
		if err != nil {
			log.Printf("容器 %s 解析失败: %v", name, err)
			continue
		}

		parser := NewParser(info, options)
		results = append(results, parser.ToResult())
	}

	return NewReverseResult(results, options), nil
}

func (rr *ReverseResult) saveOutput() error {
	if rr.options.ReverseType == ReverseCmd || rr.options.ReverseType == ReverseAll {
		f, err := os.Create("docker_run_command.sh")
		if err != nil {
			return err
		}
		defer f.Close()

		fmt.Fprintln(f, "#!/bin/bash")
		fmt.Fprint(f, rr.DockerRunCommandString())
	}

	if rr.options.ReverseType == ReverseCompose || rr.options.ReverseType == ReverseAll {
		yml, _ := yaml.Marshal(ComposeFile{Services: rr.ComposeMap})
		return os.WriteFile("docker-compose.reverse.yml", yml, 0644)
	}

	return nil
}

func (rr *ReverseResult) rerunContainers() error {
	// rerun docker run
	for name, cmdSlice := range rr.RunCommands {
		docker.ContainerStop(name)
		docker.ContainerRemove(name, true, false)

		if err := command.RunCommand(cmdSlice[0], cmdSlice[1:]...); err != nil {
			return err
		}
	}

	// rerun compose
	if rr.options.ReverseType == ReverseCompose || rr.options.ReverseType == ReverseAll {
		ymlString := rr.DockerComposeFileString()
		tmp := "docker-compose.reverse.yml"
		if err := os.WriteFile(tmp, []byte(ymlString), 0644); err != nil {
			return err
		}
		return command.RunCommand("docker", "compose", "-f", tmp, "up", "-d")
	}

	return nil
}
