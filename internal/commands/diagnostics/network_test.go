package diagnostics

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"
)

type fakeNetworkDockerService struct {
	containers  []container.Summary
	networks    []network.Summary
	inspects    map[string]container.InspectResponse
	netInspects map[string]network.Inspect
	inspectErr  error
	allFlag     bool
}

func (f *fakeNetworkDockerService) ListContainers(ctx context.Context, all bool) ([]container.Summary, error) {
	f.allFlag = all
	return f.containers, nil
}

func (f *fakeNetworkDockerService) ListNetworks(ctx context.Context) ([]network.Summary, error) {
	return f.networks, nil
}

func (f *fakeNetworkDockerService) InspectContainer(ctx context.Context, id string) (container.InspectResponse, error) {
	if f.inspectErr != nil {
		return container.InspectResponse{}, f.inspectErr
	}
	if f.inspects == nil {
		return container.InspectResponse{}, errors.New("missing fake inspect")
	}
	inspect, ok := f.inspects[id]
	if !ok {
		return container.InspectResponse{}, errors.New("missing fake inspect")
	}
	return inspect, nil
}

func (f *fakeNetworkDockerService) InspectNetwork(ctx context.Context, name string) (network.Inspect, error) {
	if f.netInspects == nil {
		for _, net := range f.networks {
			if net.Name == name {
				return net, nil
			}
		}
		return network.Inspect{}, errors.New("missing fake network inspect")
	}
	inspect, ok := f.netInspects[name]
	if !ok {
		return network.Inspect{}, errors.New("missing fake network inspect")
	}
	return inspect, nil
}

func TestBuildNetworkReportCombinesNetworksPortsAndRisks(t *testing.T) {
	report := buildNetworkReport([]container.Summary{
		{
			ID:    "container-api",
			Names: []string{"/api"},
			Image: "nginx:latest",
			State: "running",
			Ports: []container.Port{
				{IP: "0.0.0.0", PublicPort: 8080, PrivatePort: 80, Type: "tcp"},
			},
			NetworkSettings: &container.NetworkSettingsSummary{
				Networks: map[string]*network.EndpointSettings{
					"app_net": {IPAddress: "172.20.0.2", Aliases: []string{"api"}},
				},
			},
		},
		{
			ID:    "container-web",
			Names: []string{"/web"},
			Image: "nginx:latest",
			State: "exited",
			Ports: []container.Port{
				{IP: "0.0.0.0", PublicPort: 8080, PrivatePort: 8080, Type: "tcp"},
				{IP: "127.0.0.1", PublicPort: 9090, PrivatePort: 90, Type: "tcp"},
			},
			NetworkSettings: &container.NetworkSettingsSummary{
				Networks: map[string]*network.EndpointSettings{
					"app_net": {IPAddress: "172.20.0.3"},
				},
			},
		},
	}, []network.Summary{
		{Name: "app_net", Driver: "bridge", Scope: "local"},
	})

	if len(report.Networks) != 1 || report.Networks[0].Name != "app_net" || len(report.Networks[0].Containers) != 2 {
		t.Fatalf("Networks = %#v, want app_net with 2 containers", report.Networks)
	}
	if len(report.Ports) != 3 {
		t.Fatalf("Ports = %#v, want 3 mappings", report.Ports)
	}
	if !hasNetworkRisk(report, "public-bind") {
		t.Fatalf("Risks = %#v, want public-bind", report.Risks)
	}
	if !hasNetworkRisk(report, "port-conflict") {
		t.Fatalf("Risks = %#v, want port-conflict", report.Risks)
	}
}

func TestRunNetworkReportUsesInspectForNetworkMetadataAndPorts(t *testing.T) {
	fake := &fakeNetworkDockerService{
		containers: []container.Summary{{
			ID:    "container-api",
			Names: []string{"/api"},
			Image: "summary-image",
			State: "running",
		}},
		networks: []network.Summary{{Name: "app_net", Driver: "bridge"}},
		inspects: map[string]container.InspectResponse{
			"container-api": {
				ContainerJSONBase: &container.ContainerJSONBase{
					ID:         "container-api",
					Name:       "/api",
					HostConfig: &container.HostConfig{NetworkMode: "app_net"},
					State: &container.State{
						Status: container.StateRunning,
					},
				},
				Config: &container.Config{
					Image:        "nginx:latest",
					ExposedPorts: nat.PortSet{"443/tcp": struct{}{}},
				},
				NetworkSettings: &container.NetworkSettings{
					NetworkSettingsBase: container.NetworkSettingsBase{
						Ports: nat.PortMap{
							"80/tcp": []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "8080"}},
						},
					},
					Networks: map[string]*network.EndpointSettings{
						"app_net": {
							NetworkID:  "network-id",
							EndpointID: "endpoint-id",
							IPAddress:  "172.20.0.2",
							Gateway:    "172.20.0.1",
							MacAddress: "02:42:ac:14:00:02",
							Aliases:    []string{"api"},
							DNSNames:   []string{"api", "container-api"},
							DriverOpts: map[string]string{"foo": "bar"},
						},
					},
				},
			},
		},
		netInspects: map[string]network.Inspect{
			"app_net": {
				Name:       "app_net",
				ID:         "network-id",
				Driver:     "bridge",
				Scope:      "local",
				EnableIPv4: true,
				IPAM: network.IPAM{Driver: "default", Config: []network.IPAMConfig{{
					Subnet:  "172.20.0.0/16",
					Gateway: "172.20.0.1",
				}}},
				Containers: map[string]network.EndpointResource{
					"container-api": {Name: "api", EndpointID: "endpoint-id", IPv4Address: "172.20.0.2/16"},
				},
			},
		},
	}
	restore := replaceNetworkServiceFactory(fake)
	defer restore()

	report, err := runNetworkReport(context.Background(), NetworkOptions{})
	if err != nil {
		t.Fatalf("runNetworkReport() error = %v", err)
	}
	if len(report.Networks) != 1 || report.Networks[0].ID != "network-id" || report.Networks[0].IPAM.Config[0].Subnet != "172.20.0.0/16" {
		t.Fatalf("Networks = %#v, want inspect metadata", report.Networks)
	}
	if len(report.Networks[0].Containers) != 1 || report.Networks[0].Containers[0].Gateway != "172.20.0.1" || report.Networks[0].Containers[0].DriverOpts["foo"] != "bar" {
		t.Fatalf("Endpoint = %#v, want merged inspect endpoint", report.Networks[0].Containers)
	}
	if len(report.Ports) != 2 {
		t.Fatalf("Ports = %#v, want published 80 and exposed 443", report.Ports)
	}
	if !hasNetworkRisk(report, "public-bind") {
		t.Fatalf("Risks = %#v, want public-bind", report.Risks)
	}
}

func TestRunNetworkReportFallsBackToSummariesWhenInspectFails(t *testing.T) {
	fake := &fakeNetworkDockerService{
		containers: []container.Summary{{
			ID:    "container-api",
			Names: []string{"/api"},
			Image: "nginx:latest",
			State: "running",
			NetworkSettings: &container.NetworkSettingsSummary{
				Networks: map[string]*network.EndpointSettings{"app_net": {IPAddress: "172.20.0.2"}},
			},
		}},
		networks:   []network.Summary{{Name: "app_net", Driver: "bridge"}},
		inspectErr: errors.New("inspect denied"),
	}
	restore := replaceNetworkServiceFactory(fake)
	defer restore()

	report, err := runNetworkReport(context.Background(), NetworkOptions{})
	if err != nil {
		t.Fatalf("runNetworkReport() error = %v", err)
	}
	if len(report.Warnings) == 0 {
		t.Fatalf("Warnings = %#v, want inspect fallback warning", report.Warnings)
	}
	if len(report.Networks) != 1 || len(report.Networks[0].Containers) != 1 {
		t.Fatalf("Networks = %#v, want summary fallback endpoint", report.Networks)
	}
}

func TestRunNetworkReportRunningOnlyPassesContainerListFlag(t *testing.T) {
	fake := &fakeNetworkDockerService{}
	restore := replaceNetworkServiceFactory(fake)
	defer restore()

	if _, err := runNetworkReport(context.Background(), NetworkOptions{RunningOnly: true}); err != nil {
		t.Fatalf("runNetworkReport() error = %v", err)
	}
	if fake.allFlag {
		t.Fatal("ListContainers all = true, want false for running-only")
	}
}

func TestNetworkCommandRemovesRunningOnlyCompatibilityFlag(t *testing.T) {
	cmd := NewNetworkCommand()
	if flag := cmd.Flags().Lookup("running-only"); flag != nil {
		t.Fatal("running-only compatibility flag should be removed")
	}
	if flag := cmd.Flags().Lookup("running"); flag == nil {
		t.Fatal("running flag should remain available")
	}
}

func TestRunNetworkReportFiltersContainersAndRelatedNetworks(t *testing.T) {
	fake := &fakeNetworkDockerService{
		containers: []container.Summary{
			{
				ID:    "api-id",
				Names: []string{"/api-1"},
				Image: "demo/api",
				State: "running",
				NetworkSettings: &container.NetworkSettingsSummary{
					Networks: map[string]*network.EndpointSettings{"app_net": {IPAddress: "172.20.0.2"}},
				},
			},
			{
				ID:    "db-id",
				Names: []string{"/db-1"},
				Image: "demo/db",
				State: "running",
				NetworkSettings: &container.NetworkSettingsSummary{
					Networks: map[string]*network.EndpointSettings{"db_net": {IPAddress: "172.21.0.2"}},
				},
			},
		},
		networks: []network.Summary{
			{Name: "app_net", Driver: "bridge"},
			{Name: "db_net", Driver: "bridge"},
		},
	}
	restore := replaceNetworkServiceFactory(fake)
	defer restore()

	report, err := runNetworkReport(context.Background(), NetworkOptions{ContainerFilters: []string{"api-*"}})
	if err != nil {
		t.Fatalf("runNetworkReport() error = %v", err)
	}
	if len(report.Containers) != 1 || report.Containers[0].Name != "api-1" {
		t.Fatalf("Containers = %#v, want api-1", report.Containers)
	}
	if len(report.Networks) != 1 || report.Networks[0].Name != "app_net" {
		t.Fatalf("Networks = %#v, want only app_net", report.Networks)
	}
}

func TestPrintNetworkReportIncludesSections(t *testing.T) {
	var out bytes.Buffer
	printNetworkReport(&out, NetworkReport{
		Networks: []NetworkRef{{Name: "app_net", Driver: "bridge", Scope: "local", Containers: []EndpointRef{{Container: "api", IPAddress: "172.20.0.2"}}}},
		Ports:    []PortMappingRef{{Container: "api", HostIP: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp", Risks: []string{"public-bind"}}},
		Risks:    []NetworkRisk{{Type: "public-bind", Message: "api exposes 0.0.0.0:8080/tcp to public interfaces"}},
	})

	got := out.String()
	for _, want := range []string{"Docker 网络报告", "网络=1", "端口映射:", "风险:", "public-bind"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output = %q, want %q", got, want)
		}
	}
}

func hasNetworkRisk(report NetworkReport, riskType string) bool {
	for _, risk := range report.Risks {
		if risk.Type == riskType {
			return true
		}
	}
	return false
}

func replaceNetworkServiceFactory(fake *fakeNetworkDockerService) func() {
	previous := newNetworkDockerService
	newNetworkDockerService = func() (networkDockerService, error) {
		return fake, nil
	}
	return func() {
		newNetworkDockerService = previous
	}
}
