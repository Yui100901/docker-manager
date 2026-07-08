package runconfig

import (
	"fmt"
	"log"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
)

type ContainerSpec struct {
	Image           string
	ContainerName   string
	Labels          map[string]string
	DNS             []string
	DNSSearch       []string
	ExtraHosts      []string
	CapAdd          []string
	CapDrop         []string
	SecurityOpt     []string
	Devices         []string
	Ulimits         map[string]UlimitSpec
	LogDriver       string
	LogOptions      map[string]string
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

type UlimitSpec struct {
	Soft int64 `yaml:"soft"`
	Hard int64 `yaml:"hard"`
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
		Labels:          p.parseLabels(),
		DNS:             addrSliceToStrings(p.ci.HostConfig.DNS),
		DNSSearch:       copyStringSlice(p.ci.HostConfig.DNSSearch),
		ExtraHosts:      copyStringSlice(p.ci.HostConfig.ExtraHosts),
		CapAdd:          copyStringSlice(p.ci.HostConfig.CapAdd),
		CapDrop:         copyStringSlice(p.ci.HostConfig.CapDrop),
		SecurityOpt:     copyStringSlice(p.ci.HostConfig.SecurityOpt),
		Devices:         p.parseDevices(),
		Ulimits:         p.parseUlimits(),
		LogDriver:       p.ci.HostConfig.LogConfig.Type,
		LogOptions:      copyStringMap(p.ci.HostConfig.LogConfig.Config),
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
	profile, _ := normalizeRedactProfile(p.options.RedactProfile, p.options.RedactSecrets)
	redact := profile != "none"
	if !p.options.FilterDefaultEnvs && !redact {
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
		if redact {
			e = redactEnvValueWithProfile(e, profile)
		}
		result = append(result, e)
	}
	return result
}

func (p *Parser) parseLabels() map[string]string {
	labels := copyStringMap(p.ci.Config.Labels)
	profile, _ := normalizeRedactProfile(p.options.RedactProfile, p.options.RedactSecrets)
	if profile == "none" {
		return labels
	}
	return redactStringMapWithProfile(labels, profile)
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
		for _, b := range bindings {
			for _, binding := range p.resolvePublishedPort(port, b) {
				result = append(result, binding)
			}
		}
	}
	return result
}

func (p *Parser) resolvePublishedPort(port network.Port, configured network.PortBinding) []PortBindingSpec {
	proto := port.Proto()
	contPort, err := strconv.Atoi(port.Port())
	if err != nil {
		log.Printf("警告: 解析容器端口失败 %s: %v", port.Port(), err)
		return nil
	}
	if strings.TrimSpace(configured.HostPort) != "" {
		hp, err := strconv.Atoi(configured.HostPort)
		if err != nil {
			log.Printf("警告: 解析主机端口失败 %s: %v", configured.HostPort, err)
			return nil
		}
		return []PortBindingSpec{{
			HostIP:   normalizeIP(addrString(configured.HostIP)),
			HostPort: hp,
			ContPort: contPort,
			Proto:    string(proto),
		}}
	}
	if p.ci.NetworkSettings == nil {
		return nil
	}
	var result []PortBindingSpec
	for _, runtimeBinding := range p.ci.NetworkSettings.Ports[port] {
		if strings.TrimSpace(runtimeBinding.HostPort) == "" {
			continue
		}
		hp, err := strconv.Atoi(runtimeBinding.HostPort)
		if err != nil {
			log.Printf("警告: 解析运行态主机端口失败 %s: %v", runtimeBinding.HostPort, err)
			continue
		}
		hostIP := addrString(runtimeBinding.HostIP)
		if hostIP == "" {
			hostIP = addrString(configured.HostIP)
		}
		result = append(result, PortBindingSpec{
			HostIP:   normalizeIP(hostIP),
			HostPort: hp,
			ContPort: contPort,
			Proto:    string(proto),
		})
	}
	return result
}

func addrSliceToStrings(addrs []netip.Addr) []string {
	if len(addrs) == 0 {
		return nil
	}
	result := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		if value := addrString(addr); value != "" {
			result = append(result, value)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func addrString(addr netip.Addr) string {
	if !addr.IsValid() || addr.IsUnspecified() {
		return ""
	}
	return addr.String()
}

func (p *Parser) parseDevices() []string {
	var devices []string
	for _, device := range p.ci.HostConfig.Devices {
		devices = append(devices, formatDevice(device.PathOnHost, device.PathInContainer, device.CgroupPermissions))
	}
	return devices
}

func formatDevice(hostPath, containerPath, permissions string) string {
	if containerPath == "" {
		containerPath = hostPath
	}
	if permissions == "" {
		if containerPath == hostPath {
			return hostPath
		}
		return fmt.Sprintf("%s:%s", hostPath, containerPath)
	}
	return fmt.Sprintf("%s:%s:%s", hostPath, containerPath, permissions)
}

func (p *Parser) parseUlimits() map[string]UlimitSpec {
	if len(p.ci.HostConfig.Ulimits) == 0 {
		return nil
	}
	ulimits := make(map[string]UlimitSpec, len(p.ci.HostConfig.Ulimits))
	for _, ulimit := range p.ci.HostConfig.Ulimits {
		if ulimit == nil || ulimit.Name == "" {
			continue
		}
		ulimits[ulimit.Name] = UlimitSpec{Soft: ulimit.Soft, Hard: ulimit.Hard}
	}
	if len(ulimits) == 0 {
		return nil
	}
	return ulimits
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
	for _, label := range formatLabels(spec.Labels) {
		add("--label", label)
	}
	for _, dns := range spec.DNS {
		add("--dns", dns)
	}
	for _, search := range spec.DNSSearch {
		add("--dns-search", search)
	}
	for _, host := range spec.ExtraHosts {
		add("--add-host", host)
	}
	for _, cap := range spec.CapAdd {
		add("--cap-add", cap)
	}
	for _, cap := range spec.CapDrop {
		add("--cap-drop", cap)
	}
	for _, opt := range spec.SecurityOpt {
		add("--security-opt", opt)
	}
	for _, device := range spec.Devices {
		add("--device", device)
	}
	for _, ulimit := range formatUlimits(spec.Ulimits) {
		add("--ulimit", ulimit)
	}
	if spec.LogDriver != "" {
		add("--log-driver", spec.LogDriver)
	}
	for _, opt := range formatMapOptions(spec.LogOptions) {
		add("--log-opt", opt)
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

func MergePortRanges(bindings []PortBindingSpec) []string {
	return mergePortRanges(bindings)
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
		Labels:        spec.Labels,
		DNS:           spec.DNS,
		DNSSearch:     spec.DNSSearch,
		ExtraHosts:    spec.ExtraHosts,
		CapAdd:        spec.CapAdd,
		CapDrop:       spec.CapDrop,
		SecurityOpt:   spec.SecurityOpt,
		Devices:       spec.Devices,
		Ulimits:       spec.Ulimits,
		Logging:       formatComposeLogging(spec),
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

func copyStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func copyStringSlice(src []string) []string {
	if len(src) == 0 {
		return nil
	}
	return append([]string(nil), src...)
}

func formatLabels(labels map[string]string) []string {
	if len(labels) == 0 {
		return nil
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, fmt.Sprintf("%s=%s", key, labels[key]))
	}
	return result
}

func formatUlimits(ulimits map[string]UlimitSpec) []string {
	if len(ulimits) == 0 {
		return nil
	}
	keys := make([]string, 0, len(ulimits))
	for key := range ulimits {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	result := make([]string, 0, len(keys))
	for _, key := range keys {
		ulimit := ulimits[key]
		result = append(result, fmt.Sprintf("%s=%d:%d", key, ulimit.Soft, ulimit.Hard))
	}
	return result
}

func formatMapOptions(options map[string]string) []string {
	if len(options) == 0 {
		return nil
	}
	keys := make([]string, 0, len(options))
	for key := range options {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, fmt.Sprintf("%s=%s", key, options[key]))
	}
	return result
}

func formatComposeLogging(spec *ContainerSpec) *ComposeLogging {
	if spec.LogDriver == "" && len(spec.LogOptions) == 0 {
		return nil
	}
	return &ComposeLogging{
		Driver:  spec.LogDriver,
		Options: spec.LogOptions,
	}
}

func trimContainerName(name string) string {
	if strings.HasPrefix(name, "/") {
		return name[1:]
	}
	return name
}
