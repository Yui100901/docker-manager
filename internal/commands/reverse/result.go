package reverse

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"docker-manager/internal/docker"

	"github.com/moby/moby/api/types/container"
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
				volumes[name] = rr.composeVolumeDefinition(name)
			}
		}

		// networks: include network_mode if it's a custom network name
		nm := svc.NetworkMode
		if nm != "" && nm != "default" && nm != "bridge" && nm != "host" && nm != "none" {
			networks[nm] = rr.composeNetworkDefinition(nm)
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

func (rr *ReverseResult) composeVolumeDefinition(name string) map[string]interface{} {
	def := map[string]interface{}{"external": false}
	meta, ok := rr.VolumeMeta[name]
	if !ok {
		return def
	}
	if meta.Driver != "" {
		def["driver"] = meta.Driver
	}
	if len(meta.Options) > 0 {
		def["driver_opts"] = sortedStringMap(meta.Options)
	}
	if len(meta.Labels) > 0 {
		def["labels"] = sortedStringMap(meta.Labels)
	}
	return def
}

func (rr *ReverseResult) composeNetworkDefinition(name string) map[string]interface{} {
	def := map[string]interface{}{"external": false}
	meta, ok := rr.NetworkMeta[name]
	if !ok {
		return def
	}
	if meta.Driver != "" {
		def["driver"] = meta.Driver
	}
	if len(meta.Options) > 0 {
		def["driver_opts"] = sortedStringMap(meta.Options)
	}
	if len(meta.Labels) > 0 {
		def["labels"] = sortedStringMap(meta.Labels)
	}
	if meta.Internal {
		def["internal"] = true
	}
	if meta.Attachable {
		def["attachable"] = true
	}
	if meta.EnableIPv6 {
		def["enable_ipv6"] = true
	}
	if ipam := composeIPAM(meta.IPAM); len(ipam) > 0 {
		def["ipam"] = ipam
	}
	return def
}

func composeIPAM(ipam network.IPAM) map[string]interface{} {
	result := map[string]interface{}{}
	if ipam.Driver != "" {
		result["driver"] = ipam.Driver
	}
	if len(ipam.Options) > 0 {
		result["options"] = sortedStringMap(ipam.Options)
	}
	var configs []map[string]interface{}
	for _, cfg := range ipam.Config {
		entry := map[string]interface{}{}
		if subnet := prefixString(cfg.Subnet); subnet != "" {
			entry["subnet"] = subnet
		}
		if ipRange := prefixString(cfg.IPRange); ipRange != "" {
			entry["ip_range"] = ipRange
		}
		if gateway := addrString(cfg.Gateway); gateway != "" {
			entry["gateway"] = gateway
		}
		if len(cfg.AuxAddress) > 0 {
			entry["aux_addresses"] = sortedAddrMap(cfg.AuxAddress)
		}
		if len(entry) > 0 {
			configs = append(configs, entry)
		}
	}
	if len(configs) > 0 {
		result["config"] = configs
	}
	return result
}

func prefixString(prefix netip.Prefix) string {
	if !prefix.IsValid() {
		return ""
	}
	return prefix.String()
}

func addrString(addr netip.Addr) string {
	if !addr.IsValid() || addr.IsUnspecified() {
		return ""
	}
	return addr.String()
}

func sortedAddrMap(src map[string]netip.Addr) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for _, key := range sortedAddrMapKeys(src) {
		if value := addrString(src[key]); value != "" {
			dst[key] = value
		}
	}
	if len(dst) == 0 {
		return nil
	}
	return dst
}

func sortedAddrMapKeys(src map[string]netip.Addr) []string {
	keys := make([]string, 0, len(src))
	for key := range src {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for _, key := range sortedMapKeys(src) {
		dst[key] = src[key]
	}
	return dst
}

func sortedMapKeys(src map[string]string) []string {
	keys := make([]string, 0, len(src))
	for key := range src {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func reverseWithOptions(ctx context.Context, names []string, options ReverseOptions) (*ReverseResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ensureContainerManager(); err != nil {
		return nil, err
	}
	inspectResults := make([]reverseInspectResult, len(names))
	runReverseParallel(ctx, len(names), reverseInspectConcurrency, func(ctx context.Context, i int) {
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

type reverseInspectResult struct {
	info   container.InspectResponse
	parsed ParsedResult
	err    error
	ok     bool
}

func collectReverseResourceNames(info container.InspectResponse, volumeNames, networkNames map[string]bool) {
	for _, name := range reverseNamedVolumeNames(info) {
		volumeNames[name] = true
	}
	for _, name := range reverseNetworkNames(info) {
		networkNames[name] = true
	}
}

func inspectReverseVolumeMetadata(ctx context.Context, names []string) (map[string]volume.Volume, error) {
	meta := map[string]volume.Volume{}
	if len(names) == 0 {
		return meta, nil
	}
	results := make([]volume.Volume, len(names))
	errs := make([]error, len(names))
	ok := make([]bool, len(names))
	runReverseParallel(ctx, len(names), reverseInspectConcurrency, func(ctx context.Context, i int) {
		result, err := containerManager.InspectVolumeContext(ctx, names[i])
		if err != nil {
			errs[i] = err
			return
		}
		results[i] = result
		ok[i] = true
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for i, name := range names {
		if errs[i] != nil {
			log.Printf("volume %s inspect failed: %v", name, errs[i])
			continue
		}
		if ok[i] {
			meta[name] = results[i]
		}
	}
	return meta, nil
}

func inspectReverseNetworkMetadata(ctx context.Context, names []string) (map[string]network.Inspect, error) {
	meta := map[string]network.Inspect{}
	if len(names) == 0 {
		return meta, nil
	}
	results := make([]network.Inspect, len(names))
	errs := make([]error, len(names))
	ok := make([]bool, len(names))
	runReverseParallel(ctx, len(names), reverseInspectConcurrency, func(ctx context.Context, i int) {
		result, err := containerManager.InspectNetworkContext(ctx, names[i])
		if err != nil {
			errs[i] = err
			return
		}
		results[i] = result
		ok[i] = true
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for i, name := range names {
		if errs[i] != nil {
			log.Printf("network %s inspect failed: %v", name, errs[i])
			continue
		}
		if ok[i] {
			meta[name] = results[i]
		}
	}
	return meta, nil
}

func reverseNamedVolumeNames(info container.InspectResponse) []string {
	seen := map[string]bool{}
	for _, mount := range info.Mounts {
		if string(mount.Type) == "volume" && mount.Name != "" {
			seen[mount.Name] = true
		}
	}
	return sortedBoolMapKeys(seen)
}

func reverseNetworkNames(info container.InspectResponse) []string {
	seen := map[string]bool{}
	if info.NetworkSettings != nil {
		for name := range info.NetworkSettings.Networks {
			if isReverseCustomNetwork(name) {
				seen[name] = true
			}
		}
	}
	if info.HostConfig != nil {
		networkMode := string(info.HostConfig.NetworkMode)
		if isReverseCustomNetwork(networkMode) {
			seen[networkMode] = true
		}
	}
	return sortedBoolMapKeys(seen)
}

func sortedBoolMapKeys(src map[string]bool) []string {
	keys := make([]string, 0, len(src))
	for key := range src {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func isReverseCustomNetwork(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	switch name {
	case "default", "bridge", "host", "none":
		return false
	default:
		return !strings.HasPrefix(name, "container:") && !strings.HasPrefix(name, "service:")
	}
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
