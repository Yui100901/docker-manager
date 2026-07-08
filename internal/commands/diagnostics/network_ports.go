package diagnostics

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/container"
)

func networkPortMappings(summary container.Summary, inspect container.InspectResponse, hasInspect bool, containerName string) []PortMappingRef {
	var mappings []PortMappingRef
	seen := map[string]bool{}
	publishedPorts := map[string]bool{}
	add := func(mapping PortMappingRef) {
		if mapping.Protocol == "" {
			mapping.Protocol = "tcp"
		}
		portKey := fmt.Sprintf("%d/%s", mapping.ContainerPort, mapping.Protocol)
		if !mapping.Published && publishedPorts[portKey] {
			return
		}
		key := fmt.Sprintf("%s|%s|%d|%d|%s|%v", mapping.Container, mapping.HostIP, mapping.HostPort, mapping.ContainerPort, mapping.Protocol, mapping.Published)
		if seen[key] {
			return
		}
		seen[key] = true
		if mapping.Published {
			publishedPorts[portKey] = true
			mappings = removeExposedPortMappings(mappings, mapping.ContainerPort, mapping.Protocol)
		}
		mappings = append(mappings, mapping)
	}

	if hasInspect {
		if inspect.NetworkSettings != nil {
			for port, bindings := range inspect.NetworkSettings.Ports {
				containerPort, ok := parseNetworkPort(port.Port())
				if !ok {
					continue
				}
				if len(bindings) == 0 {
					add(PortMappingRef{Container: containerName, ContainerPort: containerPort, Protocol: string(port.Proto()), Published: false, Source: "network-settings"})
					continue
				}
				for _, binding := range bindings {
					hostPort, ok := parseNetworkPort(binding.HostPort)
					if !ok {
						add(PortMappingRef{Container: containerName, ContainerPort: containerPort, Protocol: string(port.Proto()), Published: false, Source: "network-settings"})
						continue
					}
					add(PortMappingRef{
						Container:     containerName,
						HostIP:        normalizeHostIP(binding.HostIP),
						HostPort:      hostPort,
						ContainerPort: containerPort,
						Protocol:      string(port.Proto()),
						Published:     true,
						Source:        "network-settings",
					})
				}
			}
		}
		if inspect.HostConfig != nil {
			for port, bindings := range inspect.HostConfig.PortBindings {
				containerPort, ok := parseNetworkPort(port.Port())
				if !ok {
					continue
				}
				for _, binding := range bindings {
					hostPort, ok := parseNetworkPort(binding.HostPort)
					if !ok {
						continue
					}
					add(PortMappingRef{
						Container:     containerName,
						HostIP:        normalizeHostIP(binding.HostIP),
						HostPort:      hostPort,
						ContainerPort: containerPort,
						Protocol:      string(port.Proto()),
						Published:     true,
						Source:        "host-config",
					})
				}
			}
		}
		if inspect.Config != nil {
			for port := range inspect.Config.ExposedPorts {
				containerPort, ok := parseNetworkPort(port.Port())
				if !ok {
					continue
				}
				add(PortMappingRef{Container: containerName, ContainerPort: containerPort, Protocol: string(port.Proto()), Published: false, Source: "exposed-ports"})
			}
		}
		return mappings
	}

	for _, port := range summary.Ports {
		mapping := PortMappingRef{
			Container:     containerName,
			HostIP:        normalizeHostIP(port.IP),
			HostPort:      port.PublicPort,
			ContainerPort: port.PrivatePort,
			Protocol:      port.Type,
			Published:     port.PublicPort != 0,
			Source:        "container-list",
		}
		if !mapping.Published {
			mapping.HostIP = ""
		}
		add(mapping)
	}
	return mappings
}

func removeExposedPortMappings(mappings []PortMappingRef, containerPort uint16, protocol string) []PortMappingRef {
	result := mappings[:0]
	for _, mapping := range mappings {
		if !mapping.Published && mapping.ContainerPort == containerPort && mapping.Protocol == protocol {
			continue
		}
		result = append(result, mapping)
	}
	return result
}

func parseNetworkPort(value string) (uint16, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 0 || port > 65535 {
		return 0, false
	}
	return uint16(port), true
}

func addNetworkPortMapping(report *NetworkReport, mapping PortMappingRef) {
	if mapping.Published && isPublicHostIP(mapping.HostIP) {
		mapping.Risks = append(mapping.Risks, "public-bind")
		report.Risks = append(report.Risks, NetworkRisk{
			Type:    "public-bind",
			Message: fmt.Sprintf("%s 暴露 %s:%d/%s 到公网监听地址", mapping.Container, mapping.HostIP, mapping.HostPort, mapping.Protocol),
		})
	}
	report.Ports = append(report.Ports, mapping)
}

func addPortConflictRisks(report *NetworkReport) {
	type key struct {
		ip    string
		port  uint16
		proto string
	}
	groups := make(map[key][]int)
	for i, p := range report.Ports {
		if !p.Published {
			continue
		}
		groups[key{ip: p.HostIP, port: p.HostPort, proto: p.Protocol}] = append(groups[key{ip: p.HostIP, port: p.HostPort, proto: p.Protocol}], i)
	}

	for k, indexes := range groups {
		if len(indexes) < 2 {
			continue
		}
		containers := make([]string, 0, len(indexes))
		for _, idx := range indexes {
			report.Ports[idx].Risks = appendUnique(report.Ports[idx].Risks, "port-conflict")
			containers = append(containers, report.Ports[idx].Container)
		}
		sort.Strings(containers)
		report.Risks = append(report.Risks, NetworkRisk{
			Type:    "port-conflict",
			Message: fmt.Sprintf("%s:%d/%s 被多个容器使用: %s", k.ip, k.port, k.proto, strings.Join(containers, ",")),
		})
	}

	for i := range report.Ports {
		if !report.Ports[i].Published {
			continue
		}
		for j := i + 1; j < len(report.Ports); j++ {
			if !report.Ports[j].Published {
				continue
			}
			if report.Ports[i].HostPort != report.Ports[j].HostPort || report.Ports[i].Protocol != report.Ports[j].Protocol {
				continue
			}
			if report.Ports[i].HostIP == report.Ports[j].HostIP {
				continue
			}
			if isPublicHostIP(report.Ports[i].HostIP) || isPublicHostIP(report.Ports[j].HostIP) {
				report.Ports[i].Risks = appendUnique(report.Ports[i].Risks, "wildcard-overlap")
				report.Ports[j].Risks = appendUnique(report.Ports[j].Risks, "wildcard-overlap")
				report.Risks = append(report.Risks, NetworkRisk{
					Type:    "wildcard-overlap",
					Message: fmt.Sprintf("%d/%s 同时存在通配监听和指定地址监听: %s,%s", report.Ports[i].HostPort, report.Ports[i].Protocol, report.Ports[i].Container, report.Ports[j].Container),
				})
			}
		}
	}
}
