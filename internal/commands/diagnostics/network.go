package diagnostics

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"docker-manager/internal/commandflags"
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
	InspectContainer(ctx context.Context, id string) (container.InspectResponse, error)
	InspectNetwork(ctx context.Context, name string) (network.Inspect, error)
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
	DockerEndpoint string                `json:"docker_endpoint"`
	Target         TargetSelection       `json:"target"`
	Networks       []NetworkRef          `json:"networks"`
	Containers     []NetworkContainerRef `json:"containers"`
	Ports          []PortMappingRef      `json:"ports"`
	Risks          []NetworkRisk         `json:"risks"`
	Warnings       []string              `json:"warnings,omitempty"`
}

type NetworkRef struct {
	ID         string            `json:"id,omitempty"`
	Name       string            `json:"name"`
	Driver     string            `json:"driver,omitempty"`
	Scope      string            `json:"scope,omitempty"`
	Created    string            `json:"created,omitempty"`
	Internal   bool              `json:"internal,omitempty"`
	Attachable bool              `json:"attachable,omitempty"`
	Ingress    bool              `json:"ingress,omitempty"`
	ConfigOnly bool              `json:"config_only,omitempty"`
	EnableIPv4 bool              `json:"enable_ipv4,omitempty"`
	EnableIPv6 bool              `json:"enable_ipv6,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Options    map[string]string `json:"options,omitempty"`
	IPAM       NetworkIPAMRef    `json:"ipam,omitempty"`
	Containers []EndpointRef     `json:"containers,omitempty"`
}

type NetworkIPAMRef struct {
	Driver  string                 `json:"driver,omitempty"`
	Options map[string]string      `json:"options,omitempty"`
	Config  []NetworkIPAMConfigRef `json:"config,omitempty"`
}

type NetworkIPAMConfigRef struct {
	Subnet     string            `json:"subnet,omitempty"`
	IPRange    string            `json:"ip_range,omitempty"`
	Gateway    string            `json:"gateway,omitempty"`
	AuxAddress map[string]string `json:"aux_address,omitempty"`
}

type EndpointRef struct {
	Container   string            `json:"container"`
	ID          string            `json:"id,omitempty"`
	EndpointID  string            `json:"endpoint_id,omitempty"`
	NetworkID   string            `json:"network_id,omitempty"`
	IPAddress   string            `json:"ip_address,omitempty"`
	IPv4Address string            `json:"ipv4_address,omitempty"`
	IPv6Address string            `json:"ipv6_address,omitempty"`
	Gateway     string            `json:"gateway,omitempty"`
	IPv6Gateway string            `json:"ipv6_gateway,omitempty"`
	MacAddress  string            `json:"mac_address,omitempty"`
	Aliases     []string          `json:"aliases,omitempty"`
	Links       []string          `json:"links,omitempty"`
	DNSNames    []string          `json:"dns_names,omitempty"`
	DriverOpts  map[string]string `json:"driver_opts,omitempty"`
}

type NetworkContainerRef struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Image       string   `json:"image,omitempty"`
	State       string   `json:"state,omitempty"`
	NetworkMode string   `json:"network_mode,omitempty"`
	Networks    []string `json:"networks,omitempty"`
}

type PortMappingRef struct {
	Container     string   `json:"container"`
	HostIP        string   `json:"host_ip,omitempty"`
	HostPort      uint16   `json:"host_port,omitempty"`
	ContainerPort uint16   `json:"container_port"`
	Protocol      string   `json:"protocol"`
	Published     bool     `json:"published"`
	Source        string   `json:"source,omitempty"`
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
	cmd.Flags().StringArrayVarP(&opts.ContainerFilters, "filter", "f", nil, "筛选容器，支持 name:/id:/image:/state:/status:/label: 和 * ? 通配符，可重复指定")
	_ = cmd.RegisterFlagCompletionFunc("filter", completion.LocalContainers)
	commandflags.AddReportFormatFlag(cmd, &opts.Format)
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
	inspectByID, inspectWarnings := inspectNetworkContainers(ctx, svc, containers)
	networks, err := svc.ListNetworks(ctx)
	if err != nil {
		return NetworkReport{}, err
	}
	if hasContainerFilter {
		networks = filterNetworksForContainersWithInspect(networks, containers, inspectByID)
	}
	inspectedNetworks, networkWarnings := inspectNetworks(ctx, svc, networks)
	report := buildNetworkReportDetailed(containers, inspectByID, inspectedNetworks)
	report.DockerEndpoint = docker.Endpoint()
	report.Target = buildContainerTargetSelection("查看", len(containers), opts.RunningOnly, opts.ContainerFilters)
	report.Warnings = append(report.Warnings, inspectWarnings...)
	report.Warnings = append(report.Warnings, networkWarnings...)
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

func filterNetworksForContainersWithInspect(networks []network.Summary, containers []container.Summary, inspectByID map[string]container.InspectResponse) []network.Summary {
	used := map[string]bool{}
	for _, c := range containers {
		if inspect, ok := inspectByID[c.ID]; ok && inspect.NetworkSettings != nil {
			for name := range inspect.NetworkSettings.Networks {
				used[name] = true
			}
			continue
		}
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
	return buildNetworkReportDetailed(containers, nil, networks)
}

func buildNetworkReportDetailed(containers []container.Summary, inspectByID map[string]container.InspectResponse, networks []network.Inspect) NetworkReport {
	report := NetworkReport{}
	selected := selectedNetworkContainers(containers)
	networkByName := make(map[string]int)
	for _, net := range networks {
		ref := networkRefFromInspect(net, selected)
		report.Networks = append(report.Networks, ref)
		networkByName[net.Name] = len(report.Networks) - 1
	}

	for _, c := range containers {
		name := networkContainerName(c)
		inspect, hasInspect := inspectByID[c.ID]
		containerRef := networkContainerRefFromSummary(c, name)
		if hasInspect {
			applyNetworkContainerInspect(&containerRef, inspect)
		}

		if hasInspect && inspect.NetworkSettings != nil {
			for netName, endpoint := range inspect.NetworkSettings.Networks {
				containerRef.Networks = append(containerRef.Networks, netName)
				netIndex, ok := networkByName[netName]
				if !ok {
					report.Networks = append(report.Networks, NetworkRef{Name: netName})
					netIndex = len(report.Networks) - 1
					networkByName[netName] = netIndex
				}
				ep := endpointRefFromSettings(name, c.ID, endpoint)
				upsertEndpointRef(&report.Networks[netIndex].Containers, ep)
			}
		} else if c.NetworkSettings != nil {
			for netName, endpoint := range c.NetworkSettings.Networks {
				containerRef.Networks = append(containerRef.Networks, netName)
				netIndex, ok := networkByName[netName]
				if !ok {
					report.Networks = append(report.Networks, NetworkRef{Name: netName})
					netIndex = len(report.Networks) - 1
					networkByName[netName] = netIndex
				}
				ep := endpointRefFromSettings(name, c.ID, endpoint)
				upsertEndpointRef(&report.Networks[netIndex].Containers, ep)
			}
		}
		sort.Strings(containerRef.Networks)
		report.Containers = append(report.Containers, containerRef)

		for _, mapping := range networkPortMappings(c, inspect, hasInspect, name) {
			addNetworkPortMapping(&report, mapping)
		}
	}

	addPortConflictRisks(&report)
	sortNetworkReport(&report)
	return report
}

func inspectNetworkContainers(ctx context.Context, svc networkDockerService, containers []container.Summary) (map[string]container.InspectResponse, []string) {
	inspects := make(map[string]container.InspectResponse, len(containers))
	var warnings []string
	for _, c := range containers {
		if err := ctx.Err(); err != nil {
			warnings = append(warnings, fmt.Sprintf("容器 inspect 已取消: %v", err))
			break
		}
		ref := c.ID
		if ref == "" {
			ref = networkContainerName(c)
		}
		inspect, err := svc.InspectContainer(ctx, ref)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("inspect 容器 %s 失败，已回退到列表摘要: %v", networkContainerName(c), err))
			continue
		}
		inspects[c.ID] = inspect
	}
	return inspects, warnings
}

func inspectNetworks(ctx context.Context, svc networkDockerService, networks []network.Summary) ([]network.Inspect, []string) {
	inspects := make([]network.Inspect, 0, len(networks))
	var warnings []string
	for _, net := range networks {
		if err := ctx.Err(); err != nil {
			warnings = append(warnings, fmt.Sprintf("network inspect 已取消: %v", err))
			break
		}
		inspect, err := svc.InspectNetwork(ctx, net.Name)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("inspect network %s 失败，已回退到列表摘要: %v", net.Name, err))
			inspects = append(inspects, net)
			continue
		}
		inspects = append(inspects, inspect)
	}
	return inspects, warnings
}

func selectedNetworkContainers(containers []container.Summary) map[string]bool {
	selected := make(map[string]bool, len(containers)*3)
	for _, c := range containers {
		if c.ID != "" {
			selected[c.ID] = true
			selected[shortID(c.ID)] = true
		}
		name := networkContainerName(c)
		if name != "" {
			selected[name] = true
			selected["/"+name] = true
		}
		for _, raw := range c.Names {
			name = strings.TrimPrefix(raw, "/")
			if name != "" {
				selected[name] = true
				selected["/"+name] = true
			}
		}
	}
	return selected
}

func networkRefFromInspect(net network.Inspect, selected map[string]bool) NetworkRef {
	ref := NetworkRef{
		ID:         shortID(net.ID),
		Name:       net.Name,
		Driver:     net.Driver,
		Scope:      net.Scope,
		Internal:   net.Internal,
		Attachable: net.Attachable,
		Ingress:    net.Ingress,
		ConfigOnly: net.ConfigOnly,
		EnableIPv4: net.EnableIPv4,
		EnableIPv6: net.EnableIPv6,
		Labels:     cloneStringMap(net.Labels),
		Options:    cloneStringMap(net.Options),
		IPAM:       networkIPAMRef(net.IPAM),
	}
	if !net.Created.IsZero() {
		ref.Created = net.Created.Format(time.RFC3339)
	}
	for containerID, endpoint := range net.Containers {
		if !networkEndpointSelected(selected, containerID, endpoint.Name) {
			continue
		}
		ep := EndpointRef{
			Container:   endpoint.Name,
			ID:          shortID(containerID),
			EndpointID:  endpoint.EndpointID,
			MacAddress:  endpoint.MacAddress,
			IPv4Address: endpoint.IPv4Address,
			IPv6Address: endpoint.IPv6Address,
		}
		if ep.Container == "" {
			ep.Container = shortID(containerID)
		}
		ref.Containers = append(ref.Containers, ep)
	}
	return ref
}

func networkEndpointSelected(selected map[string]bool, id, name string) bool {
	if len(selected) == 0 {
		return true
	}
	if selected[id] || selected[shortID(id)] || selected[name] || selected[strings.TrimPrefix(name, "/")] {
		return true
	}
	return false
}

func networkIPAMRef(ipam network.IPAM) NetworkIPAMRef {
	ref := NetworkIPAMRef{
		Driver:  ipam.Driver,
		Options: cloneStringMap(ipam.Options),
	}
	for _, cfg := range ipam.Config {
		ref.Config = append(ref.Config, NetworkIPAMConfigRef{
			Subnet:     cfg.Subnet,
			IPRange:    cfg.IPRange,
			Gateway:    cfg.Gateway,
			AuxAddress: cloneStringMap(cfg.AuxAddress),
		})
	}
	return ref
}

func networkContainerName(c container.Summary) string {
	name := firstContainerName(c.Names)
	if name == "" {
		name = shortID(c.ID)
	}
	return name
}

func networkContainerRefFromSummary(c container.Summary, name string) NetworkContainerRef {
	return NetworkContainerRef{
		ID:    shortID(c.ID),
		Name:  name,
		Image: c.Image,
		State: string(c.State),
	}
}

func applyNetworkContainerInspect(ref *NetworkContainerRef, inspect container.InspectResponse) {
	if inspect.ContainerJSONBase != nil {
		if inspect.ID != "" {
			ref.ID = shortID(inspect.ID)
		}
		if name := normalizeContainerName(inspect.Name); name != "" {
			ref.Name = name
		}
		if inspect.State != nil && inspect.State.Status != "" {
			ref.State = string(inspect.State.Status)
		}
	}
	if inspect.Config != nil && inspect.Config.Image != "" {
		ref.Image = inspect.Config.Image
	}
	if inspect.HostConfig != nil {
		ref.NetworkMode = string(inspect.HostConfig.NetworkMode)
	}
}

func endpointRefFromSettings(containerName, containerID string, endpoint *network.EndpointSettings) EndpointRef {
	ep := EndpointRef{Container: containerName, ID: shortID(containerID)}
	if endpoint == nil {
		return ep
	}
	ep.EndpointID = endpoint.EndpointID
	ep.NetworkID = endpoint.NetworkID
	ep.IPAddress = endpoint.IPAddress
	ep.IPv4Address = endpoint.IPAddress
	ep.IPv6Address = endpoint.GlobalIPv6Address
	ep.Gateway = endpoint.Gateway
	ep.IPv6Gateway = endpoint.IPv6Gateway
	ep.MacAddress = endpoint.MacAddress
	ep.Aliases = sortedStrings(endpoint.Aliases)
	ep.Links = sortedStrings(endpoint.Links)
	ep.DNSNames = sortedStrings(endpoint.DNSNames)
	ep.DriverOpts = cloneStringMap(endpoint.DriverOpts)
	return ep
}

func upsertEndpointRef(items *[]EndpointRef, incoming EndpointRef) {
	for i := range *items {
		existing := &(*items)[i]
		if !sameEndpointRef(*existing, incoming) {
			continue
		}
		if incoming.ID != "" {
			existing.ID = incoming.ID
		}
		if incoming.EndpointID != "" {
			existing.EndpointID = incoming.EndpointID
		}
		if incoming.NetworkID != "" {
			existing.NetworkID = incoming.NetworkID
		}
		if incoming.IPAddress != "" {
			existing.IPAddress = incoming.IPAddress
		}
		if incoming.IPv4Address != "" {
			existing.IPv4Address = incoming.IPv4Address
		}
		if incoming.IPv6Address != "" {
			existing.IPv6Address = incoming.IPv6Address
		}
		if incoming.Gateway != "" {
			existing.Gateway = incoming.Gateway
		}
		if incoming.IPv6Gateway != "" {
			existing.IPv6Gateway = incoming.IPv6Gateway
		}
		if incoming.MacAddress != "" {
			existing.MacAddress = incoming.MacAddress
		}
		if len(incoming.Aliases) > 0 {
			existing.Aliases = incoming.Aliases
		}
		if len(incoming.Links) > 0 {
			existing.Links = incoming.Links
		}
		if len(incoming.DNSNames) > 0 {
			existing.DNSNames = incoming.DNSNames
		}
		if len(incoming.DriverOpts) > 0 {
			existing.DriverOpts = incoming.DriverOpts
		}
		return
	}
	*items = append(*items, incoming)
}

func sameEndpointRef(a, b EndpointRef) bool {
	if a.Container != "" && b.Container != "" && a.Container == b.Container {
		return true
	}
	if a.ID != "" && b.ID != "" && a.ID == b.ID {
		return true
	}
	if a.EndpointID != "" && b.EndpointID != "" && a.EndpointID == b.EndpointID {
		return true
	}
	return false
}

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
					add(PortMappingRef{Container: containerName, ContainerPort: containerPort, Protocol: port.Proto(), Published: false, Source: "network-settings"})
					continue
				}
				for _, binding := range bindings {
					hostPort, ok := parseNetworkPort(binding.HostPort)
					if !ok {
						add(PortMappingRef{Container: containerName, ContainerPort: containerPort, Protocol: port.Proto(), Published: false, Source: "network-settings"})
						continue
					}
					add(PortMappingRef{
						Container:     containerName,
						HostIP:        normalizeHostIP(binding.HostIP),
						HostPort:      hostPort,
						ContainerPort: containerPort,
						Protocol:      port.Proto(),
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
						Protocol:      port.Proto(),
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
				add(PortMappingRef{Container: containerName, ContainerPort: containerPort, Protocol: port.Proto(), Published: false, Source: "exposed-ports"})
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
			Message: fmt.Sprintf("%s 将 %s:%d/%s 暴露到公网监听地址", mapping.Container, mapping.HostIP, mapping.HostPort, mapping.Protocol),
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

func (s *dockerNetworkService) InspectContainer(ctx context.Context, id string) (container.InspectResponse, error) {
	return s.cli.ContainerInspect(ctx, id)
}

func (s *dockerNetworkService) InspectNetwork(ctx context.Context, name string) (network.Inspect, error) {
	return s.cli.NetworkInspect(ctx, name, network.InspectOptions{})
}
