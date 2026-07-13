package diagnostics

import (
	"fmt"
	"io"
	"strings"
)

func printNetworkReport(w io.Writer, report NetworkReport) {
	fmt.Fprintln(w, "Docker 网络报告")
	printDockerEndpoint(w, report.DockerEndpoint)
	printTargetSelection(w, report.Target)
	fmt.Fprintf(w, "网络=%d 容器=%d 端口映射=%d 风险=%d\n\n", len(report.Networks), len(report.Containers), len(report.Ports), len(report.Risks))
	if len(report.Warnings) > 0 {
		fmt.Fprintln(w, "警告:")
		for _, warning := range report.Warnings {
			fmt.Fprintf(w, "  - %s\n", warning)
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "容器:")
	for _, c := range report.Containers {
		fmt.Fprintf(w, "  - %s 状态=%s 镜像=%s network-mode=%s 网络=%s\n", c.Name, c.State, c.Image, valueOr(c.NetworkMode, "默认"), valueOr(strings.Join(c.Networks, ","), "无"))
		for _, ep := range c.Endpoints {
			ip := ep.IPAddress
			if ip == "" {
				ip = ep.IPv4Address
			}
			fmt.Fprintf(w, "      网络=%s endpoint=%s ip=%s ipv6=%s gateway=%s aliases=%s\n", ep.Network, ep.EndpointID, ip, ep.IPv6Address, ep.Gateway, strings.Join(ep.Aliases, ","))
		}
		if len(c.Ports) > 0 {
			fmt.Fprintln(w, "      端口:")
			for _, group := range compactNetworkPortMappings(c.Ports) {
				printNetworkPortGroup(w, group, "        - ")
			}
		}
		if len(c.Risks) > 0 {
			fmt.Fprintln(w, "      风险:")
			for _, risk := range compactNetworkPortRiskGroups(c.Ports, c.Risks) {
				printNetworkPortRiskGroup(w, risk, "        - ")
			}
		}
	}
	if len(report.Containers) == 0 {
		fmt.Fprintln(w, "  无")
	}
	printDetachedNetworkRefs(w, report.Networks)
}

type compactNetworkPortGroup struct {
	HostIP         string
	HostPortStart  uint16
	HostPortEnd    uint16
	ContainerStart uint16
	ContainerEnd   uint16
	Protocol       string
	Published      bool
	Source         string
	Risks          []string
}

type compactNetworkPortRiskGroup struct {
	compactNetworkPortGroup
	Type       string
	Containers []string
}

func printNetworkPortGroup(w io.Writer, group compactNetworkPortGroup, prefix string) {
	risks := formatNetworkPortRisks(group.Risks)
	containerPorts := formatNetworkPortRange(group.ContainerStart, group.ContainerEnd)
	if group.Published {
		hostPorts := formatNetworkPortRange(group.HostPortStart, group.HostPortEnd)
		fmt.Fprintf(w, "%s%s:%s -> %s/%s source=%s%s\n", prefix, group.HostIP, hostPorts, containerPorts, group.Protocol, group.Source, risks)
		return
	}
	fmt.Fprintf(w, "%sexposed %s/%s source=%s%s\n", prefix, containerPorts, group.Protocol, group.Source, risks)
}

func printNetworkPortRiskGroup(w io.Writer, group compactNetworkPortRiskGroup, prefix string) {
	containerPorts := formatNetworkPortRange(group.ContainerStart, group.ContainerEnd)
	if group.Published {
		hostPorts := formatNetworkPortRange(group.HostPortStart, group.HostPortEnd)
		fmt.Fprintf(w, "%s[%s] %s:%s -> %s/%s %s\n", prefix, group.Type, group.HostIP, hostPorts, containerPorts, group.Protocol, networkRiskGroupMessage(group))
		return
	}
	fmt.Fprintf(w, "%s[%s] exposed %s/%s %s\n", prefix, group.Type, containerPorts, group.Protocol, networkRiskGroupMessage(group))
}

func compactNetworkPortMappings(ports []PortMappingRef) []compactNetworkPortGroup {
	if len(ports) == 0 {
		return nil
	}
	var groups []compactNetworkPortGroup
	for _, port := range ports {
		group := compactNetworkPortGroup{
			HostIP:         port.HostIP,
			HostPortStart:  port.HostPort,
			HostPortEnd:    port.HostPort,
			ContainerStart: port.ContainerPort,
			ContainerEnd:   port.ContainerPort,
			Protocol:       port.Protocol,
			Published:      port.Published,
			Source:         port.Source,
			Risks:          append([]string(nil), port.Risks...),
		}
		if len(groups) == 0 || !canMergeNetworkPortGroup(groups[len(groups)-1], group) {
			groups = append(groups, group)
			continue
		}
		groups[len(groups)-1].HostPortEnd = group.HostPortEnd
		groups[len(groups)-1].ContainerEnd = group.ContainerEnd
	}
	return groups
}

func compactNetworkPortRiskGroups(ports []PortMappingRef, risks []NetworkRisk) []compactNetworkPortRiskGroup {
	if len(ports) == 0 {
		return nil
	}
	var groups []compactNetworkPortRiskGroup
	for _, port := range ports {
		for _, riskType := range port.Risks {
			group := compactNetworkPortRiskGroup{
				compactNetworkPortGroup: compactNetworkPortGroup{
					HostIP:         port.HostIP,
					HostPortStart:  port.HostPort,
					HostPortEnd:    port.HostPort,
					ContainerStart: port.ContainerPort,
					ContainerEnd:   port.ContainerPort,
					Protocol:       port.Protocol,
					Published:      port.Published,
					Source:         port.Source,
					Risks:          []string{riskType},
				},
				Type:       riskType,
				Containers: networkRiskContainersForPort(risks, riskType, port),
			}
			if len(groups) == 0 || !canMergeNetworkPortRiskGroup(groups[len(groups)-1], group) {
				groups = append(groups, group)
				continue
			}
			groups[len(groups)-1].HostPortEnd = group.HostPortEnd
			groups[len(groups)-1].ContainerEnd = group.ContainerEnd
		}
	}
	return groups
}

func canMergeNetworkPortGroup(a, b compactNetworkPortGroup) bool {
	if a.HostIP != b.HostIP || a.Protocol != b.Protocol || a.Published != b.Published || a.Source != b.Source {
		return false
	}
	if strings.Join(a.Risks, ",") != strings.Join(b.Risks, ",") {
		return false
	}
	if a.ContainerEnd+1 != b.ContainerStart {
		return false
	}
	if !a.Published && !b.Published {
		return true
	}
	return a.HostPortEnd+1 == b.HostPortStart
}

func canMergeNetworkPortRiskGroup(a, b compactNetworkPortRiskGroup) bool {
	if a.Type != b.Type || strings.Join(a.Containers, ",") != strings.Join(b.Containers, ",") {
		return false
	}
	return canMergeNetworkPortGroup(a.compactNetworkPortGroup, b.compactNetworkPortGroup)
}

func networkRiskContainersForPort(risks []NetworkRisk, riskType string, port PortMappingRef) []string {
	var fallback []string
	hostPort := fmt.Sprintf(":%d/%s", port.HostPort, port.Protocol)
	containerPort := fmt.Sprintf("%d/%s", port.ContainerPort, port.Protocol)
	for _, risk := range risks {
		if risk.Type != riskType {
			continue
		}
		if len(fallback) == 0 {
			fallback = append([]string(nil), risk.Containers...)
		}
		if strings.Contains(risk.Message, hostPort) || strings.Contains(risk.Message, containerPort) {
			return append([]string(nil), risk.Containers...)
		}
	}
	return fallback
}

func networkRiskGroupMessage(group compactNetworkPortRiskGroup) string {
	switch group.Type {
	case "public-bind":
		return "暴露到公网监听地址"
	case "port-conflict":
		return fmt.Sprintf("发布端口冲突，相关容器=%s", valueOr(strings.Join(group.Containers, ","), "未知"))
	case "wildcard-overlap":
		return fmt.Sprintf("通配监听与指定地址监听重叠，相关容器=%s", valueOr(strings.Join(group.Containers, ","), "未知"))
	default:
		return "存在网络风险"
	}
}

func formatNetworkPortRange(start, end uint16) string {
	if start == end {
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d-%d", start, end)
}

func formatNetworkPortRisks(risks []string) string {
	if len(risks) == 0 {
		return ""
	}
	return " risks=" + strings.Join(risks, ",")
}

func printDetachedNetworkRefs(w io.Writer, networks []NetworkRef) {
	var detached []NetworkRef
	for _, net := range networks {
		if len(net.Containers) == 0 {
			detached = append(detached, net)
		}
	}
	if len(detached) == 0 {
		return
	}
	fmt.Fprintln(w, "\n未挂载容器的网络:")
	for _, net := range detached {
		fmt.Fprintf(w, "  - %s id=%s driver=%s scope=%s internal=%v ipv4=%v ipv6=%v\n", net.Name, net.ID, net.Driver, net.Scope, net.Internal, net.EnableIPv4, net.EnableIPv6)
		for _, cfg := range net.IPAM.Config {
			fmt.Fprintf(w, "      ipam subnet=%s gateway=%s range=%s\n", cfg.Subnet, cfg.Gateway, cfg.IPRange)
		}
	}
}
