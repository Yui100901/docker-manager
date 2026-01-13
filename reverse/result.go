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
		if rr.options.PrettyFormat {
			fmt.Println(rr.DockerRunCommandStringPretty())
		} else {
			fmt.Println(rr.DockerRunCommandStringRaw())
		}
	}

	if rr.options.ReverseType == ReverseCompose || rr.options.ReverseType == ReverseAll {
		fmt.Println(rr.DockerComposeFileString())
	}
}

// 原始单行输出
func (rr *ReverseResult) DockerRunCommandStringRaw() string {
	var sb strings.Builder
	for name, cmd := range rr.RunCommands {
		sb.WriteString(fmt.Sprintf("# %s\n%s\n\n", name, strings.Join(cmd, " ")))
	}
	return sb.String()
}

func (rr *ReverseResult) DockerRunCommandStringPretty() string {
	var sb strings.Builder
	for name, cmd := range rr.RunCommands {
		sb.WriteString(fmt.Sprintf("# %s\n", name))
		sb.WriteString("docker run \\\n")

		foundSplit := false
		for i := 2; i < len(cmd); {
			arg := cmd[i]

			if arg == CommandSplitMarker {
				foundSplit = true
				i++
				continue
			}

			if foundSplit {
				// 镜像及后续命令在同一行
				sb.WriteString("    " + strings.Join(cmd[i:], " ") + "\n\n")
				break
			}

			// 参数处理
			if i+1 < len(cmd) && !strings.HasPrefix(cmd[i+1], "-") {
				switch arg {
				case "--name", "-u", "-w", "--network", "--restart", "--entrypoint":
					sb.WriteString(fmt.Sprintf("    %s=%s \\\n", arg, cmd[i+1]))
					i += 2
				case "-e", "-v", "-p":
					sb.WriteString(fmt.Sprintf("    %s %s \\\n", arg, cmd[i+1]))
					i += 2
				case "-d":
					sb.WriteString("    --detach=true \\\n")
					i++
				default:
					sb.WriteString(fmt.Sprintf("    %s %s \\\n", arg, cmd[i+1]))
					i += 2
				}
			} else {
				// 单独布尔参数
				switch arg {
				case "-d":
					sb.WriteString("    --detach=true \\\n")
				case "--rm":
					sb.WriteString("    --rm=true \\\n")
				case "--privileged":
					sb.WriteString("    --privileged=true \\\n")
				default:
					sb.WriteString("    " + arg + " \\\n")
				}
				i++
			}
		}
	}
	return sb.String()
}

func (rr *ReverseResult) saveOutput() error {
	if rr.options.ReverseType == ReverseCmd || rr.options.ReverseType == ReverseAll {
		f, err := os.Create("docker_run_command.sh")
		if err != nil {
			return err
		}
		defer f.Close()

		fmt.Fprintln(f, "#!/bin/bash")
		if rr.options.PrettyFormat {
			fmt.Fprint(f, rr.DockerRunCommandStringPretty())
		} else {
			fmt.Fprint(f, rr.DockerRunCommandStringRaw())
		}
	}

	if rr.options.ReverseType == ReverseCompose || rr.options.ReverseType == ReverseAll {
		yml, _ := yaml.Marshal(ComposeFile{Services: rr.ComposeMap})
		return os.WriteFile("docker-compose.reverse.yml", yml, 0644)
	}

	return nil
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
