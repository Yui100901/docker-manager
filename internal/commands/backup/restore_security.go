package backup

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
)

var (
	restoreDefaultMaskedPaths = []string{
		"/proc/asound",
		"/proc/acpi",
		"/proc/interrupts",
		"/proc/kcore",
		"/proc/keys",
		"/proc/latency_stats",
		"/proc/timer_list",
		"/proc/timer_stats",
		"/proc/sched_debug",
		"/proc/scsi",
		"/sys/firmware",
		"/sys/devices/virtual/powercap",
	}
	restoreDefaultReadonlyPaths = []string{
		"/proc/bus",
		"/proc/fs",
		"/proc/irq",
		"/proc/sys",
		"/proc/sysrq-trigger",
	}
)

func unsafeRestoreHostConfig(inspect container.InspectResponse) []string {
	host := inspect.HostConfig
	if host == nil {
		return nil
	}

	var risks []string
	add := func(format string, args ...interface{}) {
		risks = append(risks, fmt.Sprintf(format, args...))
	}
	if host.Privileged {
		add("privileged=true")
	}
	if host.PublishAllPorts {
		add("publish-all-ports=true")
	}
	// Compare the serialized value directly so a Windows-built client still
	// blocks a Linux backup that requests host networking.
	if strings.EqualFold(strings.TrimSpace(string(host.NetworkMode)), "host") {
		add("host network namespace")
	}
	if host.NetworkMode.IsContainer() {
		add("network namespace joined from %q", host.NetworkMode)
	}
	if host.PidMode.IsHost() {
		add("host PID namespace")
	}
	if host.PidMode.IsContainer() {
		add("PID namespace joined from %q", host.PidMode)
	}
	if host.IpcMode.IsHost() {
		add("host IPC namespace")
	}
	if host.IpcMode.IsContainer() {
		add("IPC namespace joined from %q", host.IpcMode)
	}
	if host.UTSMode.IsHost() {
		add("host UTS namespace")
	}
	if host.CgroupnsMode.IsHost() {
		add("host cgroup namespace")
	}
	if host.UsernsMode.IsHost() {
		add("host user namespace")
	}
	if host.Cgroup.IsContainer() {
		add("cgroup joined from %q", host.Cgroup)
	}
	for _, bind := range host.Binds {
		if isHostBindSpec(bind) {
			add("host bind mount %q", bind)
		}
	}
	for _, spec := range host.Mounts {
		switch spec.Type {
		case mount.TypeBind:
			add("host bind mount %q -> %q", spec.Source, spec.Target)
		case mount.TypeNamedPipe:
			add("host named-pipe mount %q -> %q", spec.Source, spec.Target)
		case mount.TypeVolume:
			if spec.VolumeOptions != nil && spec.VolumeOptions.DriverConfig != nil {
				driver := spec.VolumeOptions.DriverConfig
				if !isSafeRestoreVolumeDriver(driver.Name) || len(driver.Options) > 0 {
					add("volume mount %q uses driver %q with host-controlled options", spec.Source, driver.Name)
				}
			}
		case mount.TypeTmpfs:
			// A tmpfs is created inside the container and does not reference a host path.
		default:
			add("unsupported mount type %q for %q -> %q", spec.Type, spec.Source, spec.Target)
		}
	}
	if !isSafeRestoreVolumeDriver(host.VolumeDriver) {
		add("custom default volume driver %q", host.VolumeDriver)
	}
	if len(host.Devices) > 0 {
		add("host device mappings (%d)", len(host.Devices))
	}
	if len(host.DeviceRequests) > 0 {
		add("device requests (%d)", len(host.DeviceRequests))
	}
	if len(host.DeviceCgroupRules) > 0 {
		add("device cgroup rules (%d)", len(host.DeviceCgroupRules))
	}
	if len(host.CapAdd) > 0 {
		add("added Linux capabilities: %s", strings.Join(host.CapAdd, ","))
	}
	for _, option := range host.SecurityOpt {
		if !isSafeRestoreSecurityOption(option) {
			add("custom security option %q", option)
		}
	}
	risks = append(risks, unsafeRestoreSystemPaths(host)...)
	if len(host.Sysctls) > 0 {
		keys := make([]string, 0, len(host.Sysctls))
		for key := range host.Sysctls {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		add("custom sysctls: %s", strings.Join(keys, ","))
	}
	if host.ContainerIDFile != "" {
		add("host container ID file %q", host.ContainerIDFile)
	}
	if host.CgroupParent != "" {
		add("custom cgroup parent %q", host.CgroupParent)
	}
	if len(host.StorageOpt) > 0 {
		add("storage driver options: %s", sortedRestoreMapKeys(host.StorageOpt))
	}
	logDriver := strings.ToLower(strings.TrimSpace(host.LogConfig.Type))
	if logDriver != "" && logDriver != "json-file" && logDriver != "local" && logDriver != "none" {
		add("custom log driver %q", host.LogConfig.Type)
	}
	if len(host.LogConfig.Config) > 0 {
		add("log driver options: %s", sortedRestoreMapKeys(host.LogConfig.Config))
	}
	if len(host.VolumesFrom) > 0 {
		add("volumes-from references: %s", strings.Join(host.VolumesFrom, ","))
	}
	if runtime := strings.TrimSpace(strings.ToLower(host.Runtime)); runtime != "" && runtime != "runc" && runtime != "io.containerd.runc.v2" {
		add("custom runtime %q", host.Runtime)
	}
	if inspect.NetworkSettings != nil {
		endpointNames := make([]string, 0, len(inspect.NetworkSettings.Networks))
		for name := range inspect.NetworkSettings.Networks {
			endpointNames = append(endpointNames, name)
		}
		sort.Strings(endpointNames)
		for _, name := range endpointNames {
			settings := inspect.NetworkSettings.Networks[name]
			if settings == nil {
				continue
			}
			if len(settings.DriverOpts) > 0 {
				add("network endpoint %q has driver options: %s", name, sortedRestoreMapKeys(settings.DriverOpts))
			}
			if len(settings.Links) > 0 {
				add("network endpoint %q has legacy links: %s", name, strings.Join(settings.Links, ","))
			}
		}
	}

	sort.Strings(risks)
	return risks
}

func unsafeRestoreSystemPaths(host *container.HostConfig) []string {
	if host == nil {
		return nil
	}
	masked := restoreStringSet(host.MaskedPaths)
	readonly := restoreStringSet(host.ReadonlyPaths)
	var risks []string

	// nil asks the daemon to install its defaults. A non-nil slice replaces
	// the full default set, so every baseline path must remain masked.
	if host.MaskedPaths != nil {
		var missing, downgraded []string
		for _, path := range restoreDefaultMaskedPaths {
			if _, exists := masked[path]; exists {
				continue
			}
			if _, exists := readonly[path]; exists {
				downgraded = append(downgraded, path)
				continue
			}
			missing = append(missing, path)
		}
		if len(downgraded) > 0 {
			risks = append(risks, "masked system paths downgraded to read-only: "+strings.Join(downgraded, ","))
		}
		if len(missing) > 0 {
			risks = append(risks, "masked system paths missing default protections: "+strings.Join(missing, ","))
		}
	}

	// A default read-only path may be made stricter by masking it, but it must
	// not become writable when ReadonlyPaths explicitly overrides the defaults.
	if host.ReadonlyPaths != nil {
		var missing []string
		for _, path := range restoreDefaultReadonlyPaths {
			if _, exists := readonly[path]; exists {
				continue
			}
			if _, exists := masked[path]; exists {
				continue
			}
			missing = append(missing, path)
		}
		if len(missing) > 0 {
			risks = append(risks, "read-only system paths missing default protections: "+strings.Join(missing, ","))
		}
	}
	return risks
}

func restoreStringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func unsafeRestoreNetworkConfig(value network.Inspect) []string {
	var risks []string
	driver := strings.ToLower(strings.TrimSpace(value.Driver))
	if driver != "" && driver != "bridge" && driver != "overlay" {
		risks = append(risks, fmt.Sprintf("network %q uses custom driver %q", value.Name, value.Driver))
	}
	if len(value.Options) > 0 {
		risks = append(risks, fmt.Sprintf("network %q has driver options: %s", value.Name, sortedRestoreMapKeys(value.Options)))
	}
	ipamDriver := strings.ToLower(strings.TrimSpace(value.IPAM.Driver))
	if ipamDriver != "" && ipamDriver != "default" {
		risks = append(risks, fmt.Sprintf("network %q uses custom IPAM driver %q", value.Name, value.IPAM.Driver))
	}
	if len(value.IPAM.Options) > 0 {
		risks = append(risks, fmt.Sprintf("network %q has IPAM options: %s", value.Name, sortedRestoreMapKeys(value.IPAM.Options)))
	}
	return risks
}

func unsafeRestoreVolumeConfig(value volume.Volume) []string {
	var risks []string
	if !isSafeRestoreVolumeDriver(value.Driver) {
		risks = append(risks, fmt.Sprintf("volume %q uses custom driver %q", value.Name, value.Driver))
	}
	if len(value.Options) > 0 {
		risks = append(risks, fmt.Sprintf("volume %q has host mount/driver options: %s", value.Name, sortedRestoreMapKeys(value.Options)))
	}
	return risks
}

func isSafeRestoreVolumeDriver(driver string) bool {
	driver = strings.ToLower(strings.TrimSpace(driver))
	return driver == "" || driver == "local"
}

func sortedRestoreMapKeys(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func isHostBindSpec(spec string) bool {
	source := strings.TrimSpace(bindSource(spec))
	if source == "" {
		return false
	}
	if filepath.IsAbs(source) || strings.HasPrefix(source, "/") || strings.HasPrefix(source, `\\`) {
		return true
	}
	if len(source) >= 3 && unicode.IsLetter(rune(source[0])) && source[1] == ':' && (source[2] == '\\' || source[2] == '/') {
		return true
	}
	return source == "." || strings.HasPrefix(source, "./") || strings.HasPrefix(source, `.\`)
}

func bindSource(spec string) string {
	spec = strings.TrimSpace(spec)
	if len(spec) >= 3 && unicode.IsLetter(rune(spec[0])) && spec[1] == ':' && (spec[2] == '\\' || spec[2] == '/') {
		if index := strings.Index(spec[3:], ":"); index >= 0 {
			return spec[:index+3]
		}
		return spec
	}
	if source, _, ok := strings.Cut(spec, ":"); ok {
		return source
	}
	return spec
}

func isSafeRestoreSecurityOption(option string) bool {
	normalized := strings.ToLower(strings.TrimSpace(option))
	return normalized == "no-new-privileges" || normalized == "no-new-privileges=true"
}
