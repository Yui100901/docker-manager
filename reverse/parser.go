package reverse

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
)

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
	PortBindings    []PortBindingSpec
	Cmd             []string
	Entrypoint      []string
	WorkingDir      string
	NetworkMode     string
}

type PortBindingSpec struct {
	HostIP   string
	HostPort int
	ContPort int
	Proto    string
}

type ParsedResult struct {
	Name    string
	Command []string
	Compose ComposeService
}

// -------------------- Parser --------------------

type Parser struct {
	ci      container.InspectResponse
	options ReverseOptions
}

func NewParser(ci container.InspectResponse, opts ReverseOptions) *Parser {
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
				continue
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
				mounts = append(mounts, fmt.Sprintf("%s:%s", m.Name, m.Destination))
			} else {
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

// 统一归一化 IP
func normalizeIP(ip string) string {
	if ip == "" || ip == "0.0.0.0" || ip == "::" || ip == "[::]" {
		return ""
	}
	return ip
}

// 解析端口绑定为结构体
func (p *Parser) parsePortBindings() []PortBindingSpec {
	var result []PortBindingSpec
	for port, bindings := range p.ci.HostConfig.PortBindings {
		proto := port.Proto()
		contPort, _ := strconv.Atoi(port.Port())
		for _, b := range bindings {
			hp, _ := strconv.Atoi(b.HostPort)
			result = append(result, PortBindingSpec{
				HostIP:   normalizeIP(b.HostIP),
				HostPort: hp,
				ContPort: contPort,
				Proto:    proto,
			})
		}
	}
	return result
}

// -------------------- Formatter --------------------

type CommandFormatter struct{}

const CommandSplitMarker = "--__SPLIT__"

func (f CommandFormatter) Format(spec *ContainerSpec, opts ReverseOptions) []string {
	cmd := []string{"docker", "run", "-d"}
	add := func(args ...string) { cmd = append(cmd, args...) }

	// 公共选项
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
	if len(spec.Entrypoint) > 0 {
		add("--entrypoint", spec.Entrypoint[0])
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

	// 端口绑定
	if opts.MergePorts {
		for _, p := range mergePortRanges(spec.PortBindings) {
			add("-p", p)
		}
	} else {
		for _, b := range spec.PortBindings {
			if b.HostIP == "" {
				add("-p", fmt.Sprintf("%d:%d/%s", b.HostPort, b.ContPort, b.Proto))
			} else {
				add("-p", fmt.Sprintf("%s:%d:%d/%s", b.HostIP, b.HostPort, b.ContPort, b.Proto))
			}
		}
	}

	// 插入分隔符
	cmd = append(cmd, CommandSplitMarker)

	// 镜像 + 命令部分
	finalCmd := spec.Cmd
	if len(spec.Entrypoint) > 1 {
		finalCmd = append(spec.Entrypoint[1:], finalCmd...)
	}
	cmd = append(cmd, spec.Image)
	cmd = append(cmd, finalCmd...)

	return cmd
}

// 合并连续端口范围
func mergePortRanges(bindings []PortBindingSpec) []string {
	if len(bindings) == 0 {
		return nil
	}

	// 按 HostIP+Proto 分组
	groups := make(map[string][]PortBindingSpec)
	for _, b := range bindings {
		key := fmt.Sprintf("%s/%s", b.HostIP, b.Proto)
		groups[key] = append(groups[key], b)
	}

	var result []string
	for _, list := range groups {
		// 按 HostPort 和 ContPort 排序
		sort.Slice(list, func(i, j int) bool {
			if list[i].HostPort == list[j].HostPort {
				return list[i].ContPort < list[j].ContPort
			}
			return list[i].HostPort < list[j].HostPort
		})

		start := list[0]
		prev := start
		for i := 1; i < len(list); i++ {
			cur := list[i]
			// 判断是否连续
			if cur.HostPort == prev.HostPort+1 && cur.ContPort == prev.ContPort+1 {
				prev = cur
			} else {
				result = append(result, formatRange(start, prev))
				start = cur
				prev = cur
			}
		}
		result = append(result, formatRange(start, prev))
	}
	return result
}

func formatRange(start, end PortBindingSpec) string {
	if start.HostPort == end.HostPort && start.ContPort == end.ContPort {
		if start.HostIP == "" {
			return fmt.Sprintf("%d:%d/%s", start.HostPort, start.ContPort, start.Proto)
		}
		return fmt.Sprintf("%s:%d:%d/%s", start.HostIP, start.HostPort, start.ContPort, start.Proto)
	}
	if start.HostIP == "" {
		return fmt.Sprintf("%d-%d:%d-%d/%s", start.HostPort, end.HostPort, start.ContPort, end.ContPort, start.Proto)
	}
	return fmt.Sprintf("%s:%d-%d:%d-%d/%s", start.HostIP, start.HostPort, end.HostPort, start.ContPort, end.ContPort, start.Proto)
}

// -------------------- ComposeFormatter --------------------

type ComposeFormatter struct{}

func (f ComposeFormatter) Format(spec *ContainerSpec) ComposeService {
	restart := spec.RestartPolicy
	if strings.HasPrefix(restart, "on-failure:") {
		restart = "on-failure"
	}

	// Compose 不支持连续范围，逐个展开
	var ports []string
	for _, b := range spec.PortBindings {
		if b.HostIP == "" {
			ports = append(ports, fmt.Sprintf("%d:%d/%s", b.HostPort, b.ContPort, b.Proto))
		} else {
			ports = append(ports, fmt.Sprintf("%s:%d:%d/%s", b.HostIP, b.HostPort, b.ContPort, b.Proto))
		}
	}

	return ComposeService{
		Image:         spec.Image,
		ContainerName: spec.ContainerName,
		Privileged:    spec.Privileged,
		Restart:       restart,
		User:          spec.User,
		Environment:   spec.Envs,
		Volumes:       spec.Mounts,
		Ports:         ports,
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
		Command: cmdFormatter.Format(spec, p.options),
		Compose: composeFormatter.Format(spec),
	}
}

func trimContainerName(name string) string {
	if strings.HasPrefix(name, "/") {
		return name[1:]
	}
	return name
}
