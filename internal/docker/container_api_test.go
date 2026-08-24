package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestContainerManagerListAllAPI(t *testing.T) {
	cli := newDockerAPITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireDockerAPIRequest(t, r, http.MethodGet, "/containers/json", url.Values{"all": {"1"}})
		writeDockerAPITestJSON(t, w, http.StatusOK, []map[string]any{{
			"Id":    "container-id",
			"Names": []string{"/demo"},
			"Image": "busybox:latest",
			"State": "running",
		}})
	}))

	items, err := (&ContainerManager{cli: cli}).ListAll()
	if err != nil {
		t.Fatalf("ListAll() error = %v", err)
	}
	if len(items) != 1 || items[0].ID != "container-id" || !reflect.DeepEqual(items[0].Names, []string{"/demo"}) {
		t.Fatalf("ListAll() = %#v, want demo container", items)
	}
}

func TestContainerManagerInspectAPIs(t *testing.T) {
	cli := newDockerAPITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v" + dockerAPITestVersion + "/containers/demo/json":
			requireDockerAPIRequest(t, r, http.MethodGet, "/containers/demo/json", url.Values{})
			writeDockerAPITestJSON(t, w, http.StatusOK, map[string]any{
				"Id": "container-id", "Name": "/demo", "Config": map[string]any{"Image": "busybox:latest"},
			})
		case "/v" + dockerAPITestVersion + "/networks/backend":
			requireDockerAPIRequest(t, r, http.MethodGet, "/networks/backend", url.Values{})
			writeDockerAPITestJSON(t, w, http.StatusOK, map[string]any{
				"Id": "network-id", "Name": "backend", "Driver": "bridge",
			})
		case "/v" + dockerAPITestVersion + "/volumes/data":
			requireDockerAPIRequest(t, r, http.MethodGet, "/volumes/data", url.Values{})
			writeDockerAPITestJSON(t, w, http.StatusOK, map[string]any{
				"Name": "data", "Driver": "local", "Mountpoint": "/var/lib/docker/volumes/data/_data",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	manager := &ContainerManager{cli: cli}

	inspect, err := manager.Inspect("demo")
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if inspect.ID != "container-id" || inspect.Config == nil || inspect.Config.Image != "busybox:latest" {
		t.Errorf("Inspect() = %#v, want decoded container", inspect)
	}

	net, err := manager.InspectNetwork("backend")
	if err != nil {
		t.Fatalf("InspectNetwork() error = %v", err)
	}
	if net.ID != "network-id" || net.Name != "backend" || net.Driver != "bridge" {
		t.Errorf("InspectNetwork() = %#v, want backend network", net)
	}

	vol, err := manager.InspectVolume("data")
	if err != nil {
		t.Fatalf("InspectVolume() error = %v", err)
	}
	if vol.Name != "data" || vol.Driver != "local" || !strings.HasSuffix(vol.Mountpoint, "/data/_data") {
		t.Errorf("InspectVolume() = %#v, want data volume", vol)
	}
}

func TestContainerManagerCreateAPI(t *testing.T) {
	cli := newDockerAPITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireDockerAPIRequest(t, r, http.MethodPost, "/containers/create", url.Values{
			"name": {"demo"}, "platform": {"linux/amd64"},
		})
		if got := r.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		var request container.CreateRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode create request: %v", err)
		}
		if request.Config == nil || request.Config.Image != "busybox:latest" || !reflect.DeepEqual(request.Config.Env, []string{"MODE=test"}) {
			t.Errorf("create config = %#v, want image and environment", request.Config)
		}
		if request.HostConfig == nil || !request.HostConfig.AutoRemove {
			t.Errorf("create host config = %#v, want AutoRemove", request.HostConfig)
		}
		endpoint := request.NetworkingConfig.EndpointsConfig["backend"]
		if endpoint == nil || !reflect.DeepEqual(endpoint.Aliases, []string{"demo"}) {
			t.Errorf("create networking config = %#v, want backend alias", request.NetworkingConfig)
		}
		writeDockerAPITestJSON(t, w, http.StatusCreated, map[string]any{
			"Id": "created-id", "Warnings": []string{"test warning"},
		})
	}))
	manager := &ContainerManager{cli: cli}

	result, err := manager.Create(
		&container.Config{Image: "busybox:latest", Env: []string{"MODE=test"}},
		&container.HostConfig{AutoRemove: true},
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			"backend": {Aliases: []string{"demo"}},
		}},
		&ocispec.Platform{OS: "linux", Architecture: "amd64"},
		"demo",
	)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if result.ID != "created-id" || !reflect.DeepEqual(result.Warnings, []string{"test warning"}) {
		t.Fatalf("Create() = %#v, want response ID and warnings", result)
	}
}

func TestContainerManagerActionAPIs(t *testing.T) {
	tests := []struct {
		name      string
		method    string
		path      string
		query     url.Values
		operation func(*ContainerManager) error
	}{
		{
			name: "start", method: http.MethodPost, path: "/containers/demo/start", query: url.Values{},
			operation: func(manager *ContainerManager) error { return manager.Start("demo") },
		},
		{
			name: "stop", method: http.MethodPost, path: "/containers/demo/stop", query: url.Values{},
			operation: func(manager *ContainerManager) error { return manager.Stop("demo") },
		},
		{
			name: "remove", method: http.MethodDelete, path: "/containers/demo", query: url.Values{"force": {"1"}, "v": {"1"}},
			operation: func(manager *ContainerManager) error { return manager.Remove("demo", true, true) },
		},
		{
			name: "rename", method: http.MethodPost, path: "/containers/demo/rename", query: url.Values{"name": {"renamed"}},
			operation: func(manager *ContainerManager) error {
				return manager.RenameContext(context.Background(), "demo", "renamed")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cli := newDockerAPITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requireDockerAPIRequest(t, r, tt.method, tt.path, tt.query)
				w.WriteHeader(http.StatusNoContent)
			}))
			if err := tt.operation(&ContainerManager{cli: cli}); err != nil {
				t.Fatalf("%s operation error = %v", tt.name, err)
			}
		})
	}
}

func TestContainerManagerBuildNetworkingConfigUsesExistingNetworks(t *testing.T) {
	cli := newDockerAPITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireDockerAPIRequest(t, r, http.MethodGet, "/networks", url.Values{})
		writeDockerAPITestJSON(t, w, http.StatusOK, []map[string]any{
			{"Id": "backend-id", "Name": "backend"},
			{"Id": "other-id", "Name": "other"},
		})
	}))
	manager := &ContainerManager{cli: cli}
	inspect := container.InspectResponse{NetworkSettings: &container.NetworkSettings{
		Networks: map[string]*network.EndpointSettings{
			"backend": {Aliases: []string{"demo", "api"}},
			"missing": {Aliases: []string{"ignored"}},
		},
	}}

	result := manager.buildNetworkingConfigContext(context.Background(), inspect)
	if len(result.EndpointsConfig) != 1 {
		t.Fatalf("EndpointsConfig = %#v, want only existing network", result.EndpointsConfig)
	}
	if endpoint := result.EndpointsConfig["backend"]; endpoint == nil || !reflect.DeepEqual(endpoint.Aliases, []string{"demo", "api"}) {
		t.Errorf("backend endpoint = %#v, want preserved aliases", endpoint)
	}
}

func TestContainerManagerPropagatesAPIError(t *testing.T) {
	cli := newDockerAPITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireDockerAPIRequest(t, r, http.MethodGet, "/containers/demo/json", url.Values{})
		writeDockerAPITestJSON(t, w, http.StatusInternalServerError, map[string]string{"message": "forced container API error"})
	}))

	_, err := (&ContainerManager{cli: cli}).Inspect("demo")
	if err == nil || !strings.Contains(err.Error(), "forced container API error") {
		t.Fatalf("Inspect() error = %v, want daemon response", err)
	}
}

func TestContainerManagerPropagatesRequestCancellation(t *testing.T) {
	started := make(chan struct{})
	cli := newDockerAPITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
			writeDockerAPITestJSON(t, w, http.StatusGatewayTimeout, map[string]string{"message": "request was not canceled"})
		}
	}))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()

	_, err := (&ContainerManager{cli: cli}).ListAllContext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ListAllContext() error = %v, want context.Canceled", err)
	}
}
