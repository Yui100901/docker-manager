package reverse

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"docker-manager/internal/docker"

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

func (rr *ReverseResult) Print(w io.Writer) {
	if w == nil {
		w = io.Discard
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
	if err := ensureContainerManager(); err != nil {
		return nil, err
	}
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

func backupContainerInspect(name, backupDir string) (string, error) {
	inspect, err := containerManager.Inspect(name)
	if err != nil {
		return "", err
	}

	data, err := json.MarshalIndent(inspect, "", "  ")
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return "", err
	}

	backupPath := inspectBackupPath(backupDir, name)
	if err := os.WriteFile(backupPath, append(data, '\n'), 0644); err != nil {
		return "", err
	}
	return backupPath, nil
}

func inspectBackupDir(now time.Time) string {
	return filepath.Join(inspectBackupRoot, now.Format("20060102-150405"))
}

func inspectBackupPath(backupDir, name string) string {
	return filepath.Join(backupDir, sanitizeBackupFileName(name)+".inspect.json")
}

func sanitizeBackupFileName(name string) string {
	name = strings.TrimPrefix(name, "/")
	var sb strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' ||
			r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' {
			sb.WriteRune(r)
			continue
		}
		switch r {
		case '.', '-', '_':
			sb.WriteRune(r)
		default:
			sb.WriteRune('_')
		}
	}
	if sb.Len() == 0 {
		return "container"
	}
	return sb.String()
}
