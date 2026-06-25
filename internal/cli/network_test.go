package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
)

type fakeNetworkDockerService struct {
	containers []container.Summary
	networks   []network.Summary
	allFlag    bool
}

func (f *fakeNetworkDockerService) ListContainers(ctx context.Context, all bool) ([]container.Summary, error) {
	f.allFlag = all
	return f.containers, nil
}

func (f *fakeNetworkDockerService) ListNetworks(ctx context.Context) ([]network.Summary, error) {
	return f.networks, nil
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
