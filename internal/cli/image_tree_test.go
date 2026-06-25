package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	imageapi "github.com/docker/docker/api/types/image"
)

type fakeImageTreeDockerService struct {
	inspect imageapi.InspectResponse
	history []imageapi.HistoryResponseItem
	calls   []string
}

func (f *fakeImageTreeDockerService) ImageInspect(ctx context.Context, imageRef string) (imageapi.InspectResponse, error) {
	f.calls = append(f.calls, "inspect:"+imageRef)
	return f.inspect, nil
}

func (f *fakeImageTreeDockerService) ImageHistory(ctx context.Context, imageRef string) ([]imageapi.HistoryResponseItem, error) {
	f.calls = append(f.calls, "history:"+imageRef)
	return f.history, nil
}

func TestBuildImageTreeReportOrdersHistoryAndFindsLargestLayers(t *testing.T) {
	report := buildImageTreeReport("demo:latest", imageapi.InspectResponse{
		ID:           "sha256:imageid1234567890",
		RepoTags:     []string{"demo:latest"},
		Architecture: "amd64",
		Os:           "linux",
		Size:         1000,
		RootFS:       imageapi.RootFS{Type: "layers", Layers: []string{"layer1", "layer2"}},
	}, []imageapi.HistoryResponseItem{
		{ID: "sha256:top", CreatedBy: "/bin/sh -c apk add curl", Size: 700},
		{ID: "<missing>", CreatedBy: "/bin/sh -c #(nop)  ENV A=B", Size: 0},
		{ID: "sha256:base", CreatedBy: "/bin/sh -c #(nop)  ADD file", Size: 300},
	}, ImageTreeOptions{Top: 1})

	if report.ID != "imageid12345" {
		t.Fatalf("ID = %q, want imageid12345", report.ID)
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
		inspect: imageapi.InspectResponse{ID: "sha256:abc", Size: 1},
		history: []imageapi.HistoryResponseItem{{ID: "sha256:abc", Size: 1}},
	}
	restore := replaceImageTreeServiceFactory(fake)
	defer restore()

	if _, err := runImageTree(context.Background(), "busybox:latest", ImageTreeOptions{Top: 5}); err != nil {
		t.Fatalf("runImageTree() error = %v", err)
	}
	if strings.Join(fake.calls, ",") != "inspect:busybox:latest,history:busybox:latest" {
		t.Fatalf("calls = %#v, want inspect then history", fake.calls)
	}
}

func TestPrintImageTreeReportIncludesSummaryLargestAndHistory(t *testing.T) {
	var out bytes.Buffer
	printImageTreeReport(&out, ImageTreeReport{
		ImageRef:      "demo:latest",
		ID:            "imageid123456",
		Platform:      "linux/amd64",
		Size:          1024,
		HistorySize:   1024,
		RootFSLayers:  []string{"layer1"},
		History:       []ImageLayerInfo{{Index: 1, ID: "layer1", CreatedBy: "RUN install", Size: 1024, SizePercent: 100}},
		LargestLayers: []ImageLayerInfo{{Index: 1, ID: "layer1", CreatedBy: "RUN install", Size: 1024, SizePercent: 100}},
	}, ImageTreeOptions{Top: 5})

	got := out.String()
	for _, want := range []string{"镜像层报告: demo:latest", "最大 layer:", "构建历史 (base -> top):", "RUN install"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output = %q, want %q", got, want)
		}
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
