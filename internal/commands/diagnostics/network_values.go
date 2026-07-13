package diagnostics

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

func sortNetworkReport(report *NetworkReport) {
	sort.Slice(report.Networks, func(i, j int) bool {
		return report.Networks[i].Name < report.Networks[j].Name
	})
	for i := range report.Networks {
		sortEndpointRefs(report.Networks[i].Containers)
	}
	sort.Slice(report.Containers, func(i, j int) bool {
		return report.Containers[i].Name < report.Containers[j].Name
	})
	for i := range report.Containers {
		sortEndpointRefs(report.Containers[i].Endpoints)
		sortPortMappingRefs(report.Containers[i].Ports)
		sortNetworkRisks(report.Containers[i].Risks)
	}
	sortPortMappingRefs(report.Ports)
	sortNetworkRisks(report.Risks)
}

func sortEndpointRefs(endpoints []EndpointRef) {
	sort.Slice(endpoints, func(i, j int) bool {
		if endpoints[i].Network == endpoints[j].Network {
			return endpoints[i].Container < endpoints[j].Container
		}
		return endpoints[i].Network < endpoints[j].Network
	})
}

func sortPortMappingRefs(ports []PortMappingRef) {
	sort.Slice(ports, func(i, j int) bool {
		if ports[i].Container != ports[j].Container {
			return ports[i].Container < ports[j].Container
		}
		if ports[i].Published != ports[j].Published {
			return ports[i].Published
		}
		if ports[i].HostIP != ports[j].HostIP {
			return ports[i].HostIP < ports[j].HostIP
		}
		if ports[i].Protocol != ports[j].Protocol {
			return ports[i].Protocol < ports[j].Protocol
		}
		if ports[i].HostPort != ports[j].HostPort {
			return ports[i].HostPort < ports[j].HostPort
		}
		return ports[i].ContainerPort < ports[j].ContainerPort
	})
}

func sortNetworkRisks(risks []NetworkRisk) {
	sort.Slice(risks, func(i, j int) bool {
		if risks[i].Type == risks[j].Type {
			return risks[i].Message < risks[j].Message
		}
		return risks[i].Type < risks[j].Type
	})
}

func groupNetworkReportByContainer(report *NetworkReport) {
	containerIndexes := make(map[string]int, len(report.Containers)*2)
	for i := range report.Containers {
		ref := &report.Containers[i]
		containerIndexes[ref.Name] = i
		if ref.ID != "" {
			containerIndexes[ref.ID] = i
		}
		for _, endpoint := range ref.Endpoints {
			if endpoint.Container != "" {
				containerIndexes[endpoint.Container] = i
			}
		}
	}
	for _, port := range report.Ports {
		if i, ok := containerIndexes[port.Container]; ok {
			report.Containers[i].Ports = append(report.Containers[i].Ports, port)
		}
	}
	for _, risk := range report.Risks {
		for _, containerName := range risk.Containers {
			if i, ok := containerIndexes[containerName]; ok {
				report.Containers[i].Risks = append(report.Containers[i].Risks, risk)
			}
		}
	}
}

func normalizeHostIP(ip any) string {
	value := strings.TrimSpace(formatNetworkValue(ip))
	switch value {
	case "", "invalid IP", "0.0.0.0", "::", "[::]":
		return "0.0.0.0"
	default:
		return value
	}
}

func formatNetworkValue(value any) string {
	if value == nil {
		return ""
	}
	switch typed := value.(type) {
	case netip.Addr:
		if !typed.IsValid() {
			return ""
		}
		return typed.String()
	case netip.Prefix:
		if !typed.IsValid() {
			return ""
		}
		return typed.String()
	}
	if stringer, ok := value.(fmt.Stringer); ok {
		return stringer.String()
	}
	return fmt.Sprint(value)
}

func cloneNetworkValueMap[T any](values map[string]T) map[string]string {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = formatNetworkValue(value)
	}
	return result
}

func isPublicHostIP(ip string) bool {
	return ip == "0.0.0.0" || ip == "::" || ip == "[::]"
}

func appendUnique(items []string, item string) []string {
	for _, existing := range items {
		if existing == item {
			return items
		}
	}
	return append(items, item)
}
