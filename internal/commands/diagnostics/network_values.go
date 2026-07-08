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
		sort.Slice(report.Networks[i].Containers, func(a, b int) bool {
			return report.Networks[i].Containers[a].Container < report.Networks[i].Containers[b].Container
		})
	}
	sort.Slice(report.Containers, func(i, j int) bool {
		return report.Containers[i].Name < report.Containers[j].Name
	})
	sort.Slice(report.Ports, func(i, j int) bool {
		if report.Ports[i].HostPort == report.Ports[j].HostPort {
			return report.Ports[i].Container < report.Ports[j].Container
		}
		return report.Ports[i].HostPort < report.Ports[j].HostPort
	})
	sort.Slice(report.Risks, func(i, j int) bool {
		if report.Risks[i].Type == report.Risks[j].Type {
			return report.Risks[i].Message < report.Risks[j].Message
		}
		return report.Risks[i].Type < report.Risks[j].Type
	})
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
