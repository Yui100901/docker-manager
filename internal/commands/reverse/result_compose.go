package reverse

import (
	"net/netip"
	"sort"
	"strings"

	"github.com/moby/moby/api/types/network"
	"gopkg.in/yaml.v3"
)

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
