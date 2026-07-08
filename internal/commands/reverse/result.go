package reverse

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"docker-manager/internal/docker"
	"docker-manager/internal/parallel"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
	"gopkg.in/yaml.v3"
)

const inspectBackupRoot = "docker-inspect-backups"

var containerManager *docker.ContainerManager
var newContainerManager = docker.NewContainerManager

// SetContainerManager allows main to inject a ContainerManager instead of using package init
func SetContainerManager(cm *docker.ContainerManager) {
	containerManager = cm
}

func ensureContainerManager() error {
	if containerManager != nil {
		return nil
	}
	cm, err := newContainerManager()
	if err != nil {
		return fmt.Errorf("init Docker container manager: %w", err)
	}
	containerManager = cm
	return nil
}

type ReverseResult struct {
	ParsedResults  []ParsedResult
	RunCommands    map[string][]string
	ComposeMap     map[string]ComposeService
	VolumeMeta     map[string]volume.Volume
	NetworkMeta    map[string]network.Inspect
	DockerEndpoint string
	options        ReverseOptions
}

func NewReverseResult(results []ParsedResult, options ReverseOptions) *ReverseResult {
	rr := &ReverseResult{
		ParsedResults:  results,
		DockerEndpoint: docker.Endpoint(),
		options:        options,
	}
	rr.RunCommands = make(map[string][]string)
	rr.ComposeMap = make(map[string]ComposeService)
	rr.VolumeMeta = make(map[string]volume.Volume)
	rr.NetworkMeta = make(map[string]network.Inspect)

	for _, r := range results {
		rr.RunCommands[r.Name] = r.Command
		rr.ComposeMap[r.Name] = r.Compose
	}
	return rr
}

func (rr *ReverseResult) Print(w io.Writer) {
	if w == nil {
		w = io.Discard
	}
	if rr.DockerEndpoint != "" {
		fmt.Fprintf(w, "# Source Docker: %s\n", rr.DockerEndpoint)
	}
	if rr.options.ReverseType == ReverseCmd || rr.options.ReverseType == ReverseAll {
		if rr.options.PrettyFormat {
			fmt.Fprintln(w, rr.DockerRunCommandStringPretty())
		} else {
			fmt.Fprintln(w, rr.DockerRunCommandStringRaw())
		}
	}

	if rr.options.ReverseType == ReverseCompose || rr.options.ReverseType == ReverseAll {
		fmt.Fprintln(w, rr.DockerComposeFileString())
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
		sb.WriteString(fmt.Sprintf("# %s\n%s\n\n", name, shellJoin(filtered)))
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
				sb.WriteString("    " + shellJoin(cmd[i:]) + "\n\n")
				break
			}

			// 参数处理
			if i+1 < len(cmd) && !strings.HasPrefix(cmd[i+1], "-") {
				switch arg {
				case "--name", "-u", "-w", "--network", "--restart", "--entrypoint":
					sb.WriteString(fmt.Sprintf("    %s=%s \\\n", arg, shellQuote(cmd[i+1])))
					i += 2
				case "-e", "-v", "-p":
					sb.WriteString(fmt.Sprintf("    %s %s \\\n", arg, shellQuote(cmd[i+1])))
					i += 2
				default:
					sb.WriteString(fmt.Sprintf("    %s %s \\\n", shellQuote(arg), shellQuote(cmd[i+1])))
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
					sb.WriteString("    " + shellQuote(arg) + " \\\n")
				}
				i++
			}
		}
	}
	return sb.String()
}

func shellJoin(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func shellQuote(arg string) string {
	if arg == "" {
		return "''"
	}
	if isShellSafe(arg) {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
}

func isShellSafe(arg string) bool {
	for _, r := range arg {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= 'A' && r <= 'Z' {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		switch r {
		case '_', '@', '%', '+', '=', ':', ',', '.', '/', '-':
			continue
		}
		return false
	}
	return true
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

func reverseWithOptions(ctx context.Context, names []string, options ReverseOptions) (*ReverseResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ensureContainerManager(); err != nil {
		return nil, err
	}
	inspectResults := make([]reverseInspectResult, len(names))
	parallel.ForEachIndex(ctx, len(names), reverseInspectConcurrency, func(ctx context.Context, i int) {
		info, err := containerManager.InspectContext(ctx, names[i])
		if err != nil {
			inspectResults[i].err = err
			return
		}
		parser := NewParser(info, options)
		inspectResults[i] = reverseInspectResult{
			info:   info,
			parsed: parser.ToResult(),
			ok:     true,
		}
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	results := make([]ParsedResult, 0, len(names))
	volumeNames := map[string]bool{}
	networkNames := map[string]bool{}
	for i, item := range inspectResults {
		if item.err != nil {
			log.Printf("容器 %s 解析失败: %v", names[i], item.err)
			continue
		}
		if !item.ok {
			continue
		}
		results = append(results, item.parsed)
		collectReverseResourceNames(item.info, volumeNames, networkNames)
	}

	result := NewReverseResult(results, options)
	volumeMeta, err := inspectReverseVolumeMetadata(ctx, sortedBoolMapKeys(volumeNames))
	if err != nil {
		return nil, err
	}
	networkMeta, err := inspectReverseNetworkMetadata(ctx, sortedBoolMapKeys(networkNames))
	if err != nil {
		return nil, err
	}
	result.VolumeMeta = volumeMeta
	result.NetworkMeta = networkMeta
	return result, nil
}
