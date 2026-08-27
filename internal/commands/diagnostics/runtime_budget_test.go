package diagnostics

import (
	"context"
	"strings"
	"testing"

	"docker-manager/internal/runcontrol"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
)

func runtimeBudgetContext(t *testing.T, maxItems int) context.Context {
	t.Helper()
	controller, err := runcontrol.New(runcontrol.Limits{MaxItems: maxItems})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := controller.Context(context.Background())
	t.Cleanup(cancel)
	return ctx
}

func TestBuildHealthReportRejectsMaxItemsBeforeInspect(t *testing.T) {
	ctx := runtimeBudgetContext(t, 1)
	fake := &fakeHealthDockerService{}
	_, err := buildHealthReport(ctx, fake, []container.Summary{{ID: "one"}, {ID: "two"}}, HealthOptions{NoLogs: true})
	if err == nil || !strings.Contains(err.Error(), "item budget exceeded") {
		t.Fatalf("buildHealthReport() error = %v, want max-items rejection", err)
	}
}

func TestNetworkInspectionUsesCumulativeResourceBudget(t *testing.T) {
	ctx := runtimeBudgetContext(t, 2)
	fake := &fakeNetworkDockerService{
		inspects: map[string]container.InspectResponse{"container": {ID: "container"}},
	}
	if _, _, err := inspectNetworkContainers(ctx, fake, []container.Summary{{ID: "container"}}); err != nil {
		t.Fatalf("inspectNetworkContainers() error = %v", err)
	}
	_, _, err := inspectNetworks(ctx, fake, []network.Summary{
		{Network: network.Network{Name: "one"}},
		{Network: network.Network{Name: "two"}},
	})
	if err == nil || !strings.Contains(err.Error(), "item budget exceeded") {
		t.Fatalf("inspectNetworks() error = %v, want cumulative max-items rejection", err)
	}
}

func TestVolumeReportBudgetsSelectedVolumesAndContainersBeforeInspect(t *testing.T) {
	ctx := runtimeBudgetContext(t, 1)
	fake := &fakeVolumeDockerService{
		volumes:    volume.ListResponse{Volumes: []volume.Volume{{Name: "data", Driver: "local"}}},
		containers: []container.Summary{{ID: "container"}},
	}
	restore := replaceVolumeServiceFactory(fake)
	defer restore()
	_, err := runVolumeReport(ctx, VolumeOptions{SizeMode: volumeSizeModeAPI})
	if err == nil || !strings.Contains(err.Error(), "item budget exceeded") {
		t.Fatalf("runVolumeReport() error = %v, want max-items rejection", err)
	}
}

func TestImageTreeBudgetsRelatedResourcesBeforeContainerInspect(t *testing.T) {
	ctx := runtimeBudgetContext(t, 1)
	fake := &fakeImageTreeDockerService{
		inspect:    image.InspectResponse{ID: "sha256:target"},
		containers: []container.Summary{{ID: "one"}, {ID: "two"}},
	}
	restore := replaceImageTreeServiceFactory(fake)
	defer restore()

	_, err := runImageTree(ctx, "target:latest", ImageTreeOptions{})
	if err == nil || !strings.Contains(err.Error(), "item budget exceeded") {
		t.Fatalf("runImageTree() error = %v, want max-items rejection", err)
	}
	for _, call := range fake.calls {
		if strings.HasPrefix(call, "inspect-container:") {
			t.Fatalf("calls = %#v, container inspect started after max-items rejection", fake.calls)
		}
	}
}
