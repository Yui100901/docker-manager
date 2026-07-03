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

	fmt.Fprintln(w, "网络:")
	for _, net := range report.Networks {
		fmt.Fprintf(w, "  - %s id=%s driver=%s scope=%s internal=%v ipv4=%v ipv6=%v 容器=%d\n", net.Name, net.ID, net.Driver, net.Scope, net.Internal, net.EnableIPv4, net.EnableIPv6, len(net.Containers))
		for _, cfg := range net.IPAM.Config {
			fmt.Fprintf(w, "      ipam subnet=%s gateway=%s range=%s\n", cfg.Subnet, cfg.Gateway, cfg.IPRange)
		}
		for _, ep := range net.Containers {
			ip := ep.IPAddress
			if ip == "" {
				ip = ep.IPv4Address
			}
			fmt.Fprintf(w, "      %s endpoint=%s ip=%s ipv6=%s gateway=%s aliases=%s\n", ep.Container, ep.EndpointID, ip, ep.IPv6Address, ep.Gateway, strings.Join(ep.Aliases, ","))
		}
	}
	if len(report.Networks) == 0 {
		fmt.Fprintln(w, "  无")
	}

	fmt.Fprintln(w, "\n端口映射:")
	for _, p := range report.Ports {
		risks := ""
		if len(p.Risks) > 0 {
			risks = " risks=" + strings.Join(p.Risks, ",")
		}
		if p.Published {
			fmt.Fprintf(w, "  - %s %s:%d -> %d/%s source=%s%s\n", p.Container, p.HostIP, p.HostPort, p.ContainerPort, p.Protocol, p.Source, risks)
		} else {
			fmt.Fprintf(w, "  - %s exposed %d/%s source=%s%s\n", p.Container, p.ContainerPort, p.Protocol, p.Source, risks)
		}
	}
	if len(report.Ports) == 0 {
		fmt.Fprintln(w, "  无")
	}

	fmt.Fprintln(w, "\n风险:")
	for _, risk := range report.Risks {
		fmt.Fprintf(w, "  - [%s] %s\n", risk.Type, risk.Message)
	}
	if len(report.Risks) == 0 {
		fmt.Fprintln(w, "  无")
	}
}
