package diagnostics

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"docker-manager/internal/completion"
	"docker-manager/internal/docker"
	rpt "docker-manager/internal/report"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"
)

type networkDockerService interface {
	ListContainers(ctx context.Context, all bool) ([]container.Summary, error)
	ListNetworks(ctx context.Context) ([]network.Summary, error)
}

var newNetworkDockerService = func() (networkDockerService, error) {
	cli, err := docker.NewClient()
	if err != nil {
		return nil, err
	}
	return &dockerNetworkService{cli: cli}, nil
}

type dockerNetworkService struct {
	cli *client.Client
}

type NetworkOptions struct {
	RunningOnly      bool
	ContainerFilters []string
	rpt.FormatOptions
}

type NetworkReport struct {
	Target     TargetSelection       `json:"target"`
	Networks   []NetworkRef          `json:"networks"`
	Containers []NetworkContainerRef `json:"containers"`
	Ports      []PortMappingRef      `json:"ports"`
	Risks      []NetworkRisk         `json:"risks"`
}

type NetworkRef struct {
	Name       string        `json:"name"`
	Driver     string        `json:"driver,omitempty"`
	Scope      string        `json:"scope,omitempty"`
	Internal   bool          `json:"internal,omitempty"`
	Containers []EndpointRef `json:"containers,omitempty"`
}

type EndpointRef struct {
	Container  string   `json:"container"`
	IPAddress  string   `json:"ip_address,omitempty"`
	MacAddress string   `json:"mac_address,omitempty"`
	Aliases    []string `json:"aliases,omitempty"`
}

type NetworkContainerRef struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Image    string   `json:"image,omitempty"`
	State    string   `json:"state,omitempty"`
	Networks []string `json:"networks,omitempty"`
}

type PortMappingRef struct {
	Container     string   `json:"container"`
	HostIP        string   `json:"host_ip"`
	HostPort      uint16   `json:"host_port"`
	ContainerPort uint16   `json:"container_port"`
	Protocol      string   `json:"protocol"`
	Risks         []string `json:"risks,omitempty"`
}

type NetworkRisk struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func NewNetworkCommand() *cobra.Command {
	opts := NetworkOptions{}
	cmd := &cobra.Command{
		Use:   "network [container-pattern...]",
		Short: "查看容器网络关系、端口映射和网络风险",
		RunE: func(cmd *cobra.Command, args []string) error {
			runOpts := opts
			runOpts.ContainerFilters = append(append([]string(nil), opts.ContainerFilters...), args...)
			report, err := runNetworkReport(cmd.Context(), runOpts)
			if err != nil {
				return fmt.Errorf("生成网络报告失败: %w", err)
			}
			return rpt.Print(cmd.OutOrStdout(), runOpts.Format, report, func(w io.Writer) {
				printNetworkReport(w, report)
			})
		},
		ValidArgsFunction: completion.LocalContainers,
	}
	cmd.Flags().BoolVar(&opts.RunningOnly, "running", false, "只查看正在运行的容器")
	cmd.Flags().BoolVar(&opts.RunningOnly, "running-only", false, "只查看正在运行的容器（兼容旧参数）")
	cmd.Flags().StringArrayVarP(&opts.ContainerFilters, "filter", "f", nil, "筛选容器，支持 name:/id:/image:/state:/status:/label: 和 * ? 通配符，可重复指定")
	_ = cmd.RegisterFlagCompletionFunc("filter", completion.LocalContainers)
	rpt.AddFormatFlag(cmd, &opts.Format)
	return cmd
}

func runNetworkReport(ctx context.Context, opts NetworkOptions) (NetworkReport, error) {
	svc, err := newNetworkDockerService()
	if err != nil {
		return NetworkReport{}, err
	}
	containers, err := svc.ListContainers(ctx, !opts.RunningOnly)
	if err != nil {
		return NetworkReport{}, err
	}
	hasContainerFilter := len(opts.ContainerFilters) > 0
	containers = filterContainerSummaries(containers, opts.ContainerFilters)
	networks, err := svc.ListNetworks(ctx)
	if err != nil {
		return NetworkReport{}, err
	}
	if hasContainerFilter {
		networks = filterNetworksForContainers(networks, containers)
	}
	report := buildNetworkReport(containers, networks)
	report.Target = buildContainerTargetSelection("查看", len(containers), opts.RunningOnly, opts.ContainerFilters)
	return report, nil
}

func filterNetworksForContainers(networks []network.Summary, containers []container.Summary) []network.Summary {
	used := map[string]bool{}
	for _, c := range containers {
		if c.NetworkSettings == nil {
			continue
		}
		for name := range c.NetworkSettings.Networks {
			used[name] = true
		}
	}
	var filtered []network.Summary
	for _, net := range networks {
		if used[net.Name] {
			filtered = append(filtered, net)
		}
	}
	return filtered
}

func buildNetworkReport(containers []container.Summary, networks []network.Summary) NetworkReport {
	report := NetworkReport{}
	networkByName := make(map[string]int)
	for _, net := range networks {
		ref := NetworkRef{
			Name:     net.Name,
			Driver:   net.Driver,
			Scope:    net.Scope,
			Internal: net.Internal,
		}
		report.Networks = append(report.Networks, ref)
		networkByName[net.Name] = len(report.Networks) - 1
	}

	for _, c := range containers {
		name := firstContainerName(c.Names)
		if name == "" {
			name = shortID(c.ID)
		}
		containerRef := NetworkContainerRef{
			ID:    shortID(c.ID),
			Name:  name,
			Image: c.Image,
			State: string(c.State),
		}

		if c.NetworkSettings != nil {
			for netName, endpoint := range c.NetworkSettings.Networks {
				containerRef.Networks = append(containerRef.Networks, netName)
				netIndex, ok := networkByName[netName]
				if !ok {
					report.Networks = append(report.Networks, NetworkRef{Name: netName})
					netIndex = len(report.Networks) - 1
					networkByName[netName] = netIndex
				}
				ep := EndpointRef{Container: name}
				if endpoint != nil {
					ep.IPAddress = endpoint.IPAddress
					ep.MacAddress = endpoint.MacAddress
					ep.Aliases = append([]string(nil), endpoint.Aliases...)
				}
				report.Networks[netIndex].Containers = append(report.Networks[netIndex].Containers, ep)
			}
		}
		sort.Strings(containerRef.Networks)
		report.Containers = append(report.Containers, containerRef)

		for _, port := range c.Ports {
			if port.PublicPort == 0 {
				continue
			}
			hostIP := normalizeHostIP(port.IP)
			mapping := PortMappingRef{
				Container:     name,
				HostIP:        hostIP,
				HostPort:      port.PublicPort,
				ContainerPort: port.PrivatePort,
				Protocol:      port.Type,
			}
			if isPublicHostIP(hostIP) {
				mapping.Risks = append(mapping.Risks, "public-bind")
				report.Risks = append(report.Risks, NetworkRisk{
					Type:    "public-bind",
					Message: fmt.Sprintf("%s 将 %s:%d/%s 暴露到公网监听地址", name, hostIP, port.PublicPort, port.Type),
				})
			}
			report.Ports = append(report.Ports, mapping)
		}
	}

	addPortConflictRisks(&report)
	sortNetworkReport(&report)
	return report
}

func addPortConflictRisks(report *NetworkReport) {
	type key struct {
		ip    string
		port  uint16
		proto string
	}
	groups := make(map[key][]int)
	for i, p := range report.Ports {
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
		for j := i + 1; j < len(report.Ports); j++ {
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

func printNetworkReport(w io.Writer, report NetworkReport) {
	fmt.Fprintln(w, "Docker 网络报告")
	printTargetSelection(w, report.Target)
	fmt.Fprintf(w, "网络=%d 容器=%d 端口映射=%d 风险=%d\n\n", len(report.Networks), len(report.Containers), len(report.Ports), len(report.Risks))

	fmt.Fprintln(w, "网络:")
	for _, net := range report.Networks {
		fmt.Fprintf(w, "  - %s driver=%s scope=%s 容器=%d\n", net.Name, net.Driver, net.Scope, len(net.Containers))
		for _, ep := range net.Containers {
			fmt.Fprintf(w, "      %s ip=%s aliases=%s\n", ep.Container, ep.IPAddress, strings.Join(ep.Aliases, ","))
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
		fmt.Fprintf(w, "  - %s %s:%d -> %d/%s%s\n", p.Container, p.HostIP, p.HostPort, p.ContainerPort, p.Protocol, risks)
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

func normalizeHostIP(ip string) string {
	switch strings.TrimSpace(ip) {
	case "", "0.0.0.0", "::", "[::]":
		return "0.0.0.0"
	default:
		return ip
	}
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

func (s *dockerNetworkService) ListContainers(ctx context.Context, all bool) ([]container.Summary, error) {
	return s.cli.ContainerList(ctx, container.ListOptions{All: all})
}

func (s *dockerNetworkService) ListNetworks(ctx context.Context) ([]network.Summary, error) {
	return s.cli.NetworkList(ctx, network.ListOptions{})
}
