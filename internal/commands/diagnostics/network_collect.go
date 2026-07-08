package diagnostics

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"docker-manager/internal/parallel"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
)

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
	return buildNetworkReportDetailed(containers, nil, networkInspectsFromSummaries(networks))
}

func networkInspectsFromSummaries(networks []network.Summary) []network.Inspect {
	inspects := make([]network.Inspect, 0, len(networks))
	for _, net := range networks {
		inspects = append(inspects, network.Inspect{Network: net.Network})
	}
	return inspects
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

func inspectNetworkContainers(ctx context.Context, svc networkDockerService, containers []container.Summary) (map[string]container.InspectResponse, []string, error) {
	inspects := make(map[string]container.InspectResponse, len(containers))
	warningsByIndex := make([]string, len(containers))
	inspectsByIndex := make([]container.InspectResponse, len(containers))
	okByIndex := make([]bool, len(containers))
	parallel.ForEachIndex(ctx, len(containers), diagnosticsInspectConcurrency, func(ctx context.Context, i int) {
		c := containers[i]
		ref := c.ID
		if ref == "" {
			ref = networkContainerName(c)
		}
		inspect, err := svc.InspectContainer(ctx, ref)
		if err != nil {
			if ctx.Err() == nil {
				warningsByIndex[i] = fmt.Sprintf("inspect 容器 %s 失败，已使用列表信息: %v", networkContainerName(c), err)
			}
			return
		}
		inspectsByIndex[i] = inspect
		okByIndex[i] = true
	})
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var warnings []string
	for i, c := range containers {
		if warningsByIndex[i] != "" {
			warnings = append(warnings, warningsByIndex[i])
		}
		if okByIndex[i] {
			inspects[c.ID] = inspectsByIndex[i]
		}
	}
	return inspects, warnings, nil
}

func inspectNetworks(ctx context.Context, svc networkDockerService, networks []network.Summary) ([]network.Inspect, []string, error) {
	inspects := make([]network.Inspect, len(networks))
	warningsByIndex := make([]string, len(networks))
	parallel.ForEachIndex(ctx, len(networks), diagnosticsInspectConcurrency, func(ctx context.Context, i int) {
		net := networks[i]
		inspect, err := svc.InspectNetwork(ctx, net.Name)
		if err != nil {
			if ctx.Err() == nil {
				warningsByIndex[i] = fmt.Sprintf("inspect 网络 %s 失败，已使用列表信息: %v", net.Name, err)
			}
			inspects[i] = network.Inspect{Network: net.Network}
			return
		}
		inspects[i] = inspect
	})
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var warnings []string
	for _, warning := range warningsByIndex {
		if warning != "" {
			warnings = append(warnings, warning)
		}
	}
	return inspects, warnings, nil
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
			MacAddress:  formatNetworkValue(endpoint.MacAddress),
			IPv4Address: formatNetworkValue(endpoint.IPv4Address),
			IPv6Address: formatNetworkValue(endpoint.IPv6Address),
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
			Subnet:     formatNetworkValue(cfg.Subnet),
			IPRange:    formatNetworkValue(cfg.IPRange),
			Gateway:    formatNetworkValue(cfg.Gateway),
			AuxAddress: cloneNetworkValueMap(cfg.AuxAddress),
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
	if inspect.ID != "" {
		ref.ID = shortID(inspect.ID)
	}
	if name := normalizeContainerName(inspect.Name); name != "" {
		ref.Name = name
	}
	if inspect.State != nil && inspect.State.Status != "" {
		ref.State = string(inspect.State.Status)
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
	ep.IPAddress = formatNetworkValue(endpoint.IPAddress)
	ep.IPv4Address = formatNetworkValue(endpoint.IPAddress)
	ep.IPv6Address = formatNetworkValue(endpoint.GlobalIPv6Address)
	ep.Gateway = formatNetworkValue(endpoint.Gateway)
	ep.IPv6Gateway = formatNetworkValue(endpoint.IPv6Gateway)
	ep.MacAddress = formatNetworkValue(endpoint.MacAddress)
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
