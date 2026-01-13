package reverse

import (
	"docker-manager/docker"
	"fmt"
	"log"
	"strings"

	"github.com/docker/docker/api/types/container"
	"gopkg.in/yaml.v3"
)

//
// @Author yfy2001
// @Date 2026/1/12 21 06
//

type ContainerSpec struct {
	Image           string
	ContainerName   string
	Privileged      bool
	PublishAllPorts bool
	AutoRemove      bool
	RestartPolicy   string
	User            string
	Envs            []string
	Mounts          []string
	PortBindings    []string
	Cmd             []string
	Entrypoint      []string
	WorkingDir      string
	NetworkMode     string
}

type ParsedResult struct {
	Name    string
	Command []string
	Compose ComposeService
}

// -------------------- Parser --------------------

type ParserOptions struct {
	PreserveVolumes   bool // true: 保留匿名卷名字，false: 简化为容器路径
	FilterDefaultEnvs bool // true: 过滤掉 Docker 固有默认 env，false: 保留全部
	PrettyFormat      bool // true: 格式化输出 docker run 命令
}

type Parser struct {
	ci      container.InspectResponse
	options ParserOptions
}

func NewParser(ci container.InspectResponse, opts ParserOptions) *Parser {
	return &Parser{ci: ci, options: opts}
}

func (p *Parser) ToSpec() *ContainerSpec {
	return &ContainerSpec{
		Image:           p.ci.Config.Image,
		ContainerName:   strings.TrimPrefix(p.ci.Name, "/"),
		Privileged:      p.ci.HostConfig.Privileged,
		PublishAllPorts: p.ci.HostConfig.PublishAllPorts,
		AutoRemove:      p.ci.HostConfig.AutoRemove,
		RestartPolicy:   p.parseRestartPolicy(),
		User:            p.ci.Config.User,
		Envs:            p.parseEnvs(),
		Mounts:          p.parseMounts(),
		PortBindings:    p.parsePortBindings(),
		Cmd:             p.ci.Config.Cmd,
		Entrypoint:      p.ci.Config.Entrypoint,
		WorkingDir:      p.ci.Config.WorkingDir,
		NetworkMode:     string(p.ci.HostConfig.NetworkMode),
	}
}

func (p *Parser) parseRestartPolicy() string {
	rp := p.ci.HostConfig.RestartPolicy
	if rp.Name == "on-failure" && rp.MaximumRetryCount > 0 {
		return fmt.Sprintf("on-failure:%d", rp.MaximumRetryCount)
	}
	return string(rp.Name)
}

var defaultEnvKeys = map[string]bool{
	"PATH":      true,
	"HOSTNAME":  true,
	"HOME":      true,
	"TERM":      true,
	"container": true,
}

func (p *Parser) parseEnvs() []string {
	envs := p.ci.Config.Env
	if !p.options.FilterDefaultEnvs {
		return envs
	}
	var result []string
	for _, e := range envs {
		kv := strings.SplitN(e, "=", 2)
		if len(kv) == 2 {
			key := kv[0]
			if defaultEnvKeys[key] {
				continue // 跳过默认变量
			}
		}
		result = append(result, e)
	}
	return result
}

func (p *Parser) parseMounts() []string {
	var mounts []string
	for _, m := range p.ci.Mounts {
		switch m.Type {
		case "volume":
			if p.options.PreserveVolumes && m.Name != "" {
				// 保留卷名，保证复现
				mounts = append(mounts, fmt.Sprintf("%s:%s", m.Name, m.Destination))
			} else {
				// 简化为容器路径，生成干净的 Compose
				mounts = append(mounts, m.Destination)
			}
		case "bind":
			mode := ""
			if m.Mode != "" {
				mode = ":" + m.Mode
			}
			mounts = append(mounts, fmt.Sprintf("%s:%s%s", m.Source, m.Destination, mode))
		default:
			mounts = append(mounts, fmt.Sprintf("%s:%s", m.Source, m.Destination))
		}
	}
	return mounts
}

func (p *Parser) parsePortBindings() []string {
	var ports []string
	for port, bindings := range p.ci.HostConfig.PortBindings {
		for _, b := range bindings {
			if b.HostIP == "" || b.HostIP == "0.0.0.0" {
				ports = append(ports, fmt.Sprintf("%s:%s", b.HostPort, port.Port()))
			} else {
				ports = append(ports, fmt.Sprintf("%s:%s:%s", b.HostIP, b.HostPort, port.Port()))
			}
		}
	}
	return ports
}

// -------------------- Formatter --------------------

type CommandFormatter struct{}

func (f CommandFormatter) Format(spec *ContainerSpec) []string {
	cmd := []string{"docker", "run", "-d"}

	add := func(args ...string) { cmd = append(cmd, args...) }

	add("--name", spec.ContainerName)

	if spec.Privileged {
		add("--privileged")
	}
	if spec.PublishAllPorts {
		add("-P")
	}
	if spec.AutoRemove {
		add("--rm")
	}
	if spec.RestartPolicy != "" {
		add("--restart", spec.RestartPolicy)
	}
	if spec.User != "" {
		add("-u", spec.User)
	}

	// 改进 Entrypoint 处理，不修改 spec.Cmd
	finalCmd := spec.Cmd
	if len(spec.Entrypoint) > 0 {
		// 第一个元素作为 entrypoint
		add("--entrypoint", spec.Entrypoint[0])
		// 剩余元素拼接到 Cmd 前面
		if len(spec.Entrypoint) > 1 {
			finalCmd = append(spec.Entrypoint[1:], finalCmd...)
		}
	}

	if spec.WorkingDir != "" {
		add("-w", spec.WorkingDir)
	}
	if spec.NetworkMode != "" && spec.NetworkMode != "default" {
		add("--network", spec.NetworkMode)
	}

	for _, e := range spec.Envs {
		add("-e", e)
	}
	for _, v := range spec.Mounts {
		add("-v", v)
	}
	for _, p := range spec.PortBindings {
		add("-p", p)
	}

	add(spec.Image)
	add(finalCmd...)

	return cmd
}

type ComposeFormatter struct{}

func (f ComposeFormatter) Format(spec *ContainerSpec) ComposeService {
	restart := spec.RestartPolicy
	// 处理 on-failure:N
	if strings.HasPrefix(restart, "on-failure:") {
		restart = "on-failure"
	}

	return ComposeService{
		Image:         spec.Image,
		ContainerName: spec.ContainerName,
		Privileged:    spec.Privileged,
		Restart:       restart,
		User:          spec.User,
		Environment:   spec.Envs,
		Volumes:       spec.Mounts,
		Ports:         spec.PortBindings,
		Entrypoint:    spec.Entrypoint,
		WorkingDir:    spec.WorkingDir,
		Command:       spec.Cmd,
		NetworkMode:   spec.NetworkMode,
	}
}

// -------------------- Parser 统一输出 --------------------

func (p *Parser) ToResult() ParsedResult {
	spec := p.ToSpec()
	cmdFormatter := CommandFormatter{}
	composeFormatter := ComposeFormatter{}

	return ParsedResult{
		Name:    trimContainerName(p.ci.Name),
		Command: cmdFormatter.Format(spec),
		Compose: composeFormatter.Format(spec),
	}
}

// -------------------- reverse --------------------

type ReverseResult struct {
	ParsedResults []ParsedResult
	RunCommands   map[string][]string
	ComposeMap    map[string]ComposeService
	options       ParserOptions
}

func NewReverseResult(results []ParsedResult, options ParserOptions) *ReverseResult {
	rr := &ReverseResult{ParsedResults: results}
	rr.RunCommands = make(map[string][]string)
	rr.ComposeMap = make(map[string]ComposeService)

	for _, r := range results {
		rr.RunCommands[r.Name] = r.Command
		rr.ComposeMap[r.Name] = r.Compose
	}
	return rr
}

func (rr *ReverseResult) Print(rt ReverseType) {
	if rt == ReverseCmd || rt == ReverseAll {
		fmt.Println(rr.DockerRunCommandString(rr.options.PrettyFormat))

	}

	if rt == ReverseCompose || rt == ReverseAll {
		fmt.Println(rr.DockerComposeFileString())
	}
}

func (rr *ReverseResult) DockerRunCommandString(pretty bool) string {
	var sb strings.Builder
	for name, cmd := range rr.RunCommands {
		sb.WriteString(fmt.Sprintf("# %s\n", name))
		if pretty {
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

func reverseWithOptions(names []string, options ParserOptions) (*ReverseResult, error) {
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

func trimContainerName(name string) string {
	if strings.HasPrefix(name, "/") {
		return name[1:]
	}
	return name
}
