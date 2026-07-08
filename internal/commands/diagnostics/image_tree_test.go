package diagnostics

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
)

type fakeImageTreeDockerService struct {
	mu                sync.Mutex
	inspect           image.InspectResponse
	history           []image.HistoryResponseItem
	images            []image.Summary
	containers        []container.Summary
	containerInspects map[string]container.InspectResponse
	calls             []string
}

func (f *fakeImageTreeDockerService) ImageInspect(ctx context.Context, imageRef string) (image.InspectResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "inspect:"+imageRef)
	return f.inspect, nil
}

func (f *fakeImageTreeDockerService) ImageHistory(ctx context.Context, imageRef string) ([]image.HistoryResponseItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "history:"+imageRef)
	return f.history, nil
}

func (f *fakeImageTreeDockerService) ImageList(ctx context.Context) ([]image.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "list-images")
	return f.images, nil
}

func (f *fakeImageTreeDockerService) ListContainers(ctx context.Context, all bool) ([]container.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "list-containers")
	return f.containers, nil
}

func (f *fakeImageTreeDockerService) InspectContainer(ctx context.Context, id string) (container.InspectResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "inspect-container:"+id)
	if inspect, ok := f.containerInspects[id]; ok {
		return inspect, nil
	}
	return container.InspectResponse{}, nil
}

func TestBuildImageTreeReportOrdersHistoryAndFindsLargestLayers(t *testing.T) {
	report := buildImageTreeReport("demo:latest", image.InspectResponse{
		ID:           "sha256:imageid1234567890",
		RepoTags:     []string{"demo:latest"},
		Architecture: "amd64",
		Os:           "linux",
		Size:         1000,
		RootFS:       image.RootFS{Type: "layers", Layers: []string{"layer1", "layer2"}},
	}, []image.HistoryResponseItem{
		{ID: "sha256:top", CreatedBy: "/bin/sh -c apk add curl", Size: 700},
		{ID: "<missing>", CreatedBy: "/bin/sh -c #(nop)  ENV A=B", Size: 0},
		{ID: "sha256:base", CreatedBy: "/bin/sh -c #(nop)  ADD file", Size: 300},
	}, ImageTreeOptions{Top: 1})

	if report.ID != "imageid1234567890" {
		t.Fatalf("ID = %q, want imageid1234567890", report.ID)
	}
	if report.Platform != "linux/amd64" {
		t.Fatalf("Platform = %q, want linux/amd64", report.Platform)
	}
	if len(report.History) != 3 || report.History[0].ID != "base" || report.History[2].ID != "top" {
		t.Fatalf("History = %#v, want base -> top", report.History)
	}
	if report.LayerCount != 2 || report.MetadataCount != 1 {
		t.Fatalf("LayerCount=%d MetadataCount=%d, want 2 and 1", report.LayerCount, report.MetadataCount)
	}
	if len(report.LargestLayers) != 1 || report.LargestLayers[0].ID != "top" {
		t.Fatalf("LargestLayers = %#v, want top", report.LargestLayers)
	}
	if report.HistorySize != 1000 {
		t.Fatalf("HistorySize = %d, want 1000", report.HistorySize)
	}
}

func TestRunImageTreeCallsInspectAndHistory(t *testing.T) {
	fake := &fakeImageTreeDockerService{
		inspect: image.InspectResponse{ID: "sha256:abc", Size: 1},
		history: []image.HistoryResponseItem{{ID: "sha256:abc", Size: 1}},
	}
	restore := replaceImageTreeServiceFactory(fake)
	defer restore()

	if _, err := runImageTree(context.Background(), "busybox:latest", ImageTreeOptions{Top: 5}); err != nil {
		t.Fatalf("runImageTree() error = %v", err)
	}
	wantCalls := "inspect:busybox:latest,history:busybox:latest,list-images,list-containers"
	if strings.Join(fake.calls, ",") != wantCalls {
		t.Fatalf("calls = %#v, want %s", fake.calls, wantCalls)
	}
}

func TestRunImageTreeIncludesLocalRefsAndUsedByContainers(t *testing.T) {
	imageID := "sha256:abc1234567890"
	fake := &fakeImageTreeDockerService{
		inspect: image.InspectResponse{
			ID:          imageID,
			RepoTags:    []string{"demo:latest"},
			RepoDigests: []string{"demo@sha256:one"},
			Size:        1,
		},
		history: []image.HistoryResponseItem{{ID: imageID, Size: 1}},
		images: []image.Summary{{
			ID:          imageID,
			RepoTags:    []string{"demo:latest", "demo:stable"},
			RepoDigests: []string{"demo@sha256:one", "demo@sha256:two"},
		}},
		containers: []container.Summary{
			{ID: "api-container-id", Names: []string{"/api"}, Image: "demo:stable", ImageID: imageID, State: "running", Status: "Up"},
			{ID: "other-container-id", Names: []string{"/other"}, Image: "other:latest", ImageID: "sha256:other", State: "exited"},
		},
		containerInspects: map[string]container.InspectResponse{
			"api-container-id": {
				ID:    "api-container-id",
				Name:  "/api",
				Image: imageID,
			},
		},
	}
	restore := replaceImageTreeServiceFactory(fake)
	defer restore()

	report, err := runImageTree(context.Background(), "demo:latest", ImageTreeOptions{Top: 5})
	if err != nil {
		t.Fatalf("runImageTree() error = %v", err)
	}
	if strings.Join(report.LocalRefs.RepoTags, ",") != "demo:latest,demo:stable" {
		t.Fatalf("LocalRefs.RepoTags = %#v, want latest and stable", report.LocalRefs.RepoTags)
	}
	if strings.Join(report.LocalRefs.RepoDigests, ",") != "demo@sha256:one,demo@sha256:two" {
		t.Fatalf("LocalRefs.RepoDigests = %#v, want both digests", report.LocalRefs.RepoDigests)
	}
	if len(report.UsedBy) != 1 || report.UsedBy[0].Name != "api" || report.UsedBy[0].ID != "api-container-id" {
		t.Fatalf("UsedBy = %#v, want api container", report.UsedBy)
	}
	if report.UsedBy[0].ImageID != "abc1234567890" {
		t.Fatalf("UsedBy image ID = %q, want full normalized ID", report.UsedBy[0].ImageID)
	}
}

func TestPrintImageTreeReportIncludesSummaryLargestAndHistory(t *testing.T) {
	longCommand := "RUN " + strings.Repeat("install-package-", 20)
	var out bytes.Buffer
	printImageTreeReport(&out, ImageTreeReport{
		ImageRef:      "demo:latest",
		ID:            "imageid1234567890abcdef",
		Platform:      "linux/amd64",
		Size:          1024,
		HistorySize:   1024,
		RootFSLayers:  []string{"layer1"},
		LocalRefs:     ImageLocalRefs{RepoTags: []string{"demo:latest", "demo:stable"}},
		UsedBy:        []ImageUsageRef{{ID: "container-id-full", Name: "api", Image: "demo:stable", ImageID: "imageid1234567890abcdef", State: "running", Status: "Up"}},
		History:       []ImageLayerInfo{{Index: 1, ID: "layer1-full-id", CreatedBy: longCommand, Size: 1024, SizePercent: 100}},
		LargestLayers: []ImageLayerInfo{{Index: 1, ID: "layer1-full-id", CreatedBy: longCommand, Size: 1024, SizePercent: 100}},
	}, ImageTreeOptions{Top: 5})

	got := out.String()
	for _, want := range []string{"镜像层报告: demo:latest", "最大 layer:", "构建历史 (base -> top):", "RUN install"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output = %q, want %q", got, want)
		}
	}
}

func TestPrintImageTreeReportDoesNotTruncateReportText(t *testing.T) {
	longCommand := "RUN " + strings.Repeat("install-package-", 20)
	var out bytes.Buffer
	printImageTreeReport(&out, ImageTreeReport{
		ImageRef:      "demo:latest",
		ID:            "imageid1234567890abcdef",
		Platform:      "linux/amd64",
		Size:          1024,
		HistorySize:   1024,
		RootFSLayers:  []string{"layer1"},
		LocalRefs:     ImageLocalRefs{RepoTags: []string{"demo:latest", "demo:stable"}},
		UsedBy:        []ImageUsageRef{{ID: "container-id-full", Name: "api", Image: "demo:stable", ImageID: "imageid1234567890abcdef", State: "running", Status: "Up"}},
		History:       []ImageLayerInfo{{Index: 1, ID: "layer1-full-id", CreatedBy: longCommand, Size: 1024, SizePercent: 100}},
		LargestLayers: []ImageLayerInfo{{Index: 1, ID: "layer1-full-id", CreatedBy: longCommand, Size: 1024, SizePercent: 100}},
	}, ImageTreeOptions{Top: 5})

	got := out.String()
	for _, want := range []string{longCommand, "Local refs:", "Used by containers:", "container-id-full", "imageid1234567890abcdef"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output = %q, want %q", got, want)
		}
	}
	if strings.Contains(got, "...") {
		t.Fatalf("output = %q, report text should not truncate with ellipsis", got)
	}
}

func TestDisplayLayerTextTruncatesUnlessNoTrunc(t *testing.T) {
	if got := displayLayerText("1234567890", false, 6); got != "123..." {
		t.Fatalf("displayLayerText() = %q, want 123...", got)
	}
	if got := displayLayerText("1234567890", true, 6); got != "1234567890" {
		t.Fatalf("displayLayerText(noTrunc) = %q, want full text", got)
	}
}

func replaceImageTreeServiceFactory(fake *fakeImageTreeDockerService) func() {
	previous := newImageTreeDockerService
	newImageTreeDockerService = func() (imageTreeDockerService, error) {
		return fake, nil
	}
	return func() {
		newImageTreeDockerService = previous
	}
}
