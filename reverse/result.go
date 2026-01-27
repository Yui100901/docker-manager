package reverse

import (
	"fmt"
	"log"
	"os"
	"strings"

	"docker-manager/docker"

	"github.com/Yui100901/MyGo/command"
	"gopkg.in/yaml.v3"
)

//
// @Author yfy2001
// @Date 2026/1/13 15 21
//

var containerManager *docker.ContainerManager

// SetContainerManager allows main to inject a ContainerManager instead of using package init
func SetContainerManager(cm *docker.ContainerManager) {
	containerManager = cm
}

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

func (rr *ReverseResult) DockerRunCommandStringRaw() string {
	var sb strings.Builder
	for name, cmd := range rr.RunCommands {
		// 过滤掉分隔符
		var filtered []string
		for _, c := range cmd {
			if c == CommandSplitMarker {
				continue
			}
			filtered = append(filtered, c)
		}
		sb.WriteString(fmt.Sprintf("# %s\n%s\n\n", name, strings.Join(filtered, " ")))
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
				default:
					sb.WriteString(fmt.Sprintf("    %s %s \\\n", arg, cmd[i+1]))
					i += 2
				}
			} else {
				// 单独布尔参数，使用更常见的写法
				switch arg {
				case "-d":
					sb.WriteString("    -d \\\n")
				case "--rm":
					sb.WriteString("    --rm \\\n")
				case "--privileged":
					sb.WriteString("    --privileged \\\n")
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
		// ensure close & capture error
		var closeErr error
		defer func() {
			if cerr := f.Close(); cerr != nil && closeErr == nil {
				closeErr = cerr
			}
		}()

		if _, err := fmt.Fprintln(f, "#!/bin/bash"); err != nil {
			return err
		}
		if rr.options.PrettyFormat {
			if _, err := fmt.Fprint(f, rr.DockerRunCommandStringPretty()); err != nil {
				return err
			}
		} else {
			if _, err := fmt.Fprint(f, rr.DockerRunCommandStringRaw()); err != nil {
				return err
			}
		}
		if closeErr != nil {
			return closeErr
		}
	}

	if rr.options.ReverseType == ReverseCompose || rr.options.ReverseType == ReverseAll {
		vols, nets := rr.buildTopLevelComposeMeta()
		yml, _ := yaml.Marshal(ComposeFile{Services: rr.ComposeMap, Volumes: vols, Networks: nets})
		return os.WriteFile("docker-compose.reverse.yml", yml, 0644)
	}

	return nil
}

func (rr *ReverseResult) DockerComposeFileString() string {
	vols, nets := rr.buildTopLevelComposeMeta()
	yml, _ := yaml.Marshal(ComposeFile{Services: rr.ComposeMap, Volumes: vols, Networks: nets})
	return string(yml)
}

func (rr *ReverseResult) buildTopLevelComposeMeta() (map[string]interface{}, map[string]interface{}) {
	volumes := make(map[string]interface{})
	networks := make(map[string]interface{})

	for _, svc := range rr.ComposeMap {
		// volumes: look for named volumes like "name:dest" where name has no path separators
		for _, v := range svc.Volumes {
			parts := strings.SplitN(v, ":", 2)
			if len(parts) != 2 {
				continue
			}
			name := parts[0]
			// heuristics: treat as named volume if name does not contain path separators
			if !strings.Contains(name, "/") && !strings.Contains(name, "\\") {
				volumes[name] = map[string]interface{}{}
			}
		}

		// networks: include network_mode if it's a custom network name
		nm := svc.NetworkMode
		if nm != "" && nm != "default" && nm != "bridge" && nm != "host" && nm != "none" {
			networks[nm] = map[string]interface{}{}
		}
	}

	if len(volumes) == 0 {
		volumes = nil
	}
	if len(networks) == 0 {
		networks = nil
	}
	return volumes, networks
}

func reverseWithOptions(names []string, options ReverseOptions) (*ReverseResult, error) {
	var results []ParsedResult

	for _, name := range names {
		info, err := containerManager.Inspect(name)
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
	var firstErr error
	for name := range rr.RunCommands {
		if rr.options.DryRun {
			fmt.Printf("Dry run: recreate container %s\n", name)
		} else {
			containerID, err := containerManager.RecreateContainer(name, name)
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("重建容器 %s 失败: %w", name, err)
				}
				log.Printf("重建容器 %s 失败: %v", name, err)
				continue
			}
			fmt.Println("Recreate container", name, "id", containerID)
		}
	}

	// rerun compose
	if rr.options.ReverseType == ReverseCompose || rr.options.ReverseType == ReverseAll {
		ymlString := rr.DockerComposeFileString()
		tmp := "docker-compose.reverse.yml"
		if err := os.WriteFile(tmp, []byte(ymlString), 0644); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return err
		}
		if rr.options.DryRun {
			fmt.Printf("Dry run: docker compose -f %s up -d\n", tmp)
		} else {
			if err := command.RunCommand("docker", "compose", "-f", tmp, "up", "-d"); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return err
			}
		}
	}

	return firstErr
}
