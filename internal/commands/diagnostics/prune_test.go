package diagnostics

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/volume"
)

type fakePruneDockerService struct {
	usage           types.DiskUsage
	containerReport container.PruneReport
	imageReport     image.PruneReport
	volumeReport    volume.PruneReport
	cacheReport     *build.CachePruneReport
	calls           []string
}

func (f *fakePruneDockerService) DiskUsage(ctx context.Context) (types.DiskUsage, error) {
	f.calls = append(f.calls, "disk-usage")
	return f.usage, nil
}

func (f *fakePruneDockerService) PruneContainers(ctx context.Context, pruneFilters filters.Args) (container.PruneReport, error) {
	f.calls = append(f.calls, "prune-containers")
	return f.containerReport, nil
}

func (f *fakePruneDockerService) PruneImages(ctx context.Context, pruneFilters filters.Args) (image.PruneReport, error) {
	f.calls = append(f.calls, "prune-images")
	return f.imageReport, nil
}

func (f *fakePruneDockerService) PruneVolumes(ctx context.Context, pruneFilters filters.Args) (volume.PruneReport, error) {
	f.calls = append(f.calls, "prune-volumes")
	return f.volumeReport, nil
}

func (f *fakePruneDockerService) PruneBuildCache(ctx context.Context, pruneFilters filters.Args) (*build.CachePruneReport, error) {
	f.calls = append(f.calls, "prune-build-cache")
	return f.cacheReport, nil
}

func TestBuildPruneReportIncludesOnlyReclaimableResources(t *testing.T) {
	report := buildPruneReport(types.DiskUsage{
		Containers: []*container.Summary{
			{ID: "running-container", Names: []string{"/api"}, State: "running", SizeRw: 100},
			{ID: "stopped-container", Names: []string{"/old"}, State: "exited", Image: "busybox", Status: "Exited", SizeRw: 200},
		},
		Images: []*image.Summary{
			{ID: "sha256:dangling-image", RepoTags: []string{"<none>:<none>"}, Size: 300},
			{ID: "sha256:tagged-image", RepoTags: []string{"busybox:latest"}, Size: 400},
		},
		Volumes: []*volume.Volume{
			{Name: "unused", Driver: "local", UsageData: &volume.UsageData{RefCount: 0, Size: 500}},
			{Name: "used", Driver: "local", UsageData: &volume.UsageData{RefCount: 1, Size: 600}},
		},
		BuildCache: []*build.CacheRecord{
			{ID: "unused-cache", Type: "regular", Size: 700, InUse: false},
			{ID: "used-cache", Type: "regular", Size: 800, InUse: true},
		},
	}, PruneScope{})

	if len(report.StoppedContainers) != 1 || report.StoppedContainers[0].Name != "old" {
		t.Fatalf("StoppedContainers = %#v, want old", report.StoppedContainers)
	}
	if len(report.DanglingImages) != 1 || report.DanglingImages[0].ID != "dangling-ima" {
		t.Fatalf("DanglingImages = %#v, want dangling image", report.DanglingImages)
	}
	if len(report.UnusedVolumes) != 1 || report.UnusedVolumes[0].Name != "unused" {
		t.Fatalf("UnusedVolumes = %#v, want unused", report.UnusedVolumes)
	}
	if len(report.BuildCaches) != 1 || report.BuildCaches[0].ID != "unused-cache" {
		t.Fatalf("BuildCaches = %#v, want unused cache", report.BuildCaches)
	}
	if report.EstimatedBytes != 1700 {
		t.Fatalf("EstimatedBytes = %d, want 1700", report.EstimatedBytes)
	}
}

func TestRunPruneReportApplyRequiresConfirm(t *testing.T) {
	_, err := runPruneReport(context.Background(), PruneReportOptions{Apply: true})
	if err == nil {
		t.Fatal("runPruneReport() error = nil, want confirm error")
	}
	if !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("runPruneReport() error = %q, want --confirm hint", err.Error())
	}
}

func TestRunPruneReportApplyRunsAllPruneOperations(t *testing.T) {
	fake := &fakePruneDockerService{
		usage: types.DiskUsage{},
		containerReport: container.PruneReport{
			ContainersDeleted: []string{"old"},
			SpaceReclaimed:    100,
		},
		imageReport: image.PruneReport{
			ImagesDeleted:  []image.DeleteResponse{{Deleted: "image-id"}},
			SpaceReclaimed: 200,
		},
		volumeReport: volume.PruneReport{
			VolumesDeleted: []string{"unused"},
			SpaceReclaimed: 300,
		},
		cacheReport: &build.CachePruneReport{
			CachesDeleted:  []string{"cache-id"},
			SpaceReclaimed: 400,
		},
	}
	restoreFactory := replacePruneServiceFactory(fake)
	defer restoreFactory()

	report, err := runPruneReport(context.Background(), PruneReportOptions{Apply: true, Confirm: true})
	if err != nil {
		t.Fatalf("runPruneReport() error = %v", err)
	}
	if !report.Applied || report.ApplyResult == nil {
		t.Fatalf("Applied = %v ApplyResult = %#v, want apply result", report.Applied, report.ApplyResult)
	}
	if report.ApplyResult.SpaceReclaimed != 1000 {
		t.Fatalf("SpaceReclaimed = %d, want 1000", report.ApplyResult.SpaceReclaimed)
	}
	wantCalls := []string{"disk-usage", "prune-containers", "prune-images", "prune-volumes", "prune-build-cache"}
	if strings.Join(fake.calls, ",") != strings.Join(wantCalls, ",") {
		t.Fatalf("calls = %#v, want %#v", fake.calls, wantCalls)
	}
}

func TestRunPruneReportApplyOnlyRunsSelectedOperations(t *testing.T) {
	fake := &fakePruneDockerService{}
	restoreFactory := replacePruneServiceFactory(fake)
	defer restoreFactory()

	report, err := runPruneReport(context.Background(), PruneReportOptions{
		Apply:   true,
		Confirm: true,
		Only:    []string{"container,volume"},
	})
	if err != nil {
		t.Fatalf("runPruneReport() error = %v", err)
	}
	if !report.Applied {
		t.Fatal("Applied = false, want true")
	}
	wantCalls := []string{"disk-usage", "prune-containers", "prune-volumes"}
	if strings.Join(fake.calls, ",") != strings.Join(wantCalls, ",") {
		t.Fatalf("calls = %#v, want %#v", fake.calls, wantCalls)
	}
}

func TestRunPruneReportApplyWithLabelFilterSkipsBuildCache(t *testing.T) {
	fake := &fakePruneDockerService{}
	restoreFactory := replacePruneServiceFactory(fake)
	defer restoreFactory()

	_, err := runPruneReport(context.Background(), PruneReportOptions{
		Apply:   true,
		Confirm: true,
		Filters: []string{"label=dmtest=true"},
	})
	if err != nil {
		t.Fatalf("runPruneReport() error = %v", err)
	}
	wantCalls := []string{"disk-usage", "prune-containers", "prune-images", "prune-volumes"}
	if strings.Join(fake.calls, ",") != strings.Join(wantCalls, ",") {
		t.Fatalf("calls = %#v, want %#v", fake.calls, wantCalls)
	}
}

func TestBuildPruneReportAppliesLabelProtectionAndOnlyScope(t *testing.T) {
	scope, err := buildPruneScope(PruneReportOptions{
		Only:          []string{"container", "volume"},
		Filters:       []string{"label=env=test"},
		ProtectLabels: []string{"keep=true"},
	})
	if err != nil {
		t.Fatalf("buildPruneScope() error = %v", err)
	}

	report := buildPruneReport(types.DiskUsage{
		Containers: []*container.Summary{
			{ID: "keep", Names: []string{"/keep"}, State: "exited", Labels: map[string]string{"env": "test", "keep": "true"}, SizeRw: 100},
			{ID: "old", Names: []string{"/old"}, State: "exited", Labels: map[string]string{"env": "test"}, SizeRw: 200},
			{ID: "prod", Names: []string{"/prod"}, State: "exited", Labels: map[string]string{"env": "prod"}, SizeRw: 300},
		},
		Images: []*image.Summary{
			{ID: "sha256:dangling-image", RepoTags: []string{"<none>:<none>"}, Labels: map[string]string{"env": "test"}, Size: 400},
		},
		Volumes: []*volume.Volume{
			{Name: "unused", Labels: map[string]string{"env": "test"}, UsageData: &volume.UsageData{RefCount: 0, Size: 500}},
			{Name: "keep-vol", Labels: map[string]string{"env": "test", "keep": "true"}, UsageData: &volume.UsageData{RefCount: 0, Size: 600}},
		},
	}, scope)

	if len(report.StoppedContainers) != 1 || report.StoppedContainers[0].Name != "old" {
		t.Fatalf("StoppedContainers = %#v, want old only", report.StoppedContainers)
	}
	if len(report.DanglingImages) != 0 {
		t.Fatalf("DanglingImages = %#v, want none because only excludes images", report.DanglingImages)
	}
	if len(report.UnusedVolumes) != 1 || report.UnusedVolumes[0].Name != "unused" {
		t.Fatalf("UnusedVolumes = %#v, want unused only", report.UnusedVolumes)
	}
	if report.EstimatedBytes != 700 {
		t.Fatalf("EstimatedBytes = %d, want 700", report.EstimatedBytes)
	}
}

func TestPrintPruneReportIncludesSectionsAndApplyResult(t *testing.T) {
	var out bytes.Buffer
	printPruneReport(&out, PruneReport{
		GeneratedAt:       time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
		EstimatedBytes:    1024,
		StoppedContainers: []PruneContainerRef{{ID: "abc", Name: "old", Image: "busybox"}},
		ApplyResult:       &PruneApplyResult{SpaceReclaimed: 2048, ContainersDeleted: []string{"abc"}},
		Applied:           true,
	})

	got := out.String()
	for _, want := range []string{
		"已停止容器: 1",
		"预计可回收空间: 1.0 KiB",
		"执行结果:",
		"已回收空间: 2.0 KiB",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output = %q, want %q", got, want)
		}
	}
}

func replacePruneServiceFactory(fake *fakePruneDockerService) func() {
	previous := newPruneDockerService
	newPruneDockerService = func() (pruneDockerService, error) {
		return fake, nil
	}
	return func() {
		newPruneDockerService = previous
	}
}
