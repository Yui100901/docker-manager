package diagnostics

import (
	"context"

	"docker-manager/internal/commandflags"
	"docker-manager/internal/docker"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	mobyclient "github.com/moby/moby/client"
)

type networkDockerService interface {
	ListContainers(ctx context.Context, all bool) ([]container.Summary, error)
	ListNetworks(ctx context.Context) ([]network.Summary, error)
	InspectContainer(ctx context.Context, id string) (container.InspectResponse, error)
	InspectNetwork(ctx context.Context, name string) (network.Inspect, error)
}

var newNetworkDockerService = func() (networkDockerService, error) {
	cli, err := docker.NewMobyClient()
	if err != nil {
		return nil, err
	}
	return &dockerNetworkService{cli: cli}, nil
}

type dockerNetworkService struct {
	cli *mobyclient.Client
}

type NetworkOptions struct {
	RunningOnly      bool
	ContainerFilters []string
	commandflags.FormatOptions
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
