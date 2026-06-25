package diagnostics

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
)

type fakeVolumeDockerService struct {
	volumes    volume.ListResponse
	containers []container.Summary
	allFlag    bool
}

func (f *fakeVolumeDockerService) ListVolumes(ctx context.Context) (volume.ListResponse, error) {
	return f.volumes, nil
}

func (f *fakeVolumeDockerService) ListContainers(ctx context.Context, all bool) ([]container.Summary, error) {
	f.allFlag = all
	return f.containers, nil
}

func TestBuildVolumeReportClassifiesUnusedSuspectedAndUsedVolumes(t *testing.T) {
	report := buildVolumeReport(volume.ListResponse{
		Volumes: []*volume.Volume{
			{Name: "unused", Driver: "local", Mountpoint: "/var/lib/docker/volumes/unused/_data", UsageData: &volume.UsageData{RefCount: 0, Size: 1024}},
			{Name: "unknown", Driver: "local", Mountpoint: "/var/lib/docker/volumes/unknown/_data"},
			{Name: "used", Driver: "local", Mountpoint: "/var/lib/docker/volumes/used/_data", UsageData: &volume.UsageData{RefCount: 1, Size: 2048}},
		},
	}, []container.Summary{
		{
			ID:    "container-used",
			Names: []string{"/db"},
			State: "running",
			Mounts: []container.MountPoint{
				{Type: mount.TypeVolume, Name: "used", Destination: "/var/lib/postgresql/data", RW: true},
			},
		},
	}, VolumeOptions{})

	if report.Summary.Total != 3 || report.Summary.Unused != 1 || report.Summary.SuspectedUnused != 1 || report.Summary.Used != 1 {
		t.Fatalf("Summary = %#v, want total=3 unused=1 suspected=1 used=1", report.Summary)
	}
	if len(report.Volumes) != 2 {
		t.Fatalf("listed volumes = %#v, want only unused and suspected", report.Volumes)
	}
	if report.Volumes[0].Name != "unused" || report.Volumes[0].Status != "unused" {
		t.Fatalf("first volume = %#v, want unused", report.Volumes[0])
	}
	if report.Volumes[1].Name != "unknown" || report.Volumes[1].Status != "suspected-unused" {
		t.Fatalf("second volume = %#v, want suspected-unused", report.Volumes[1])
	}
	if report.Summary.ReclaimableSize != 1024 {
		t.Fatalf("ReclaimableSize = %d, want 1024", report.Summary.ReclaimableSize)
	}
}

func TestBuildVolumeReportAllIncludesUsedVolumeContainers(t *testing.T) {
	report := buildVolumeReport(volume.ListResponse{
		Volumes: []*volume.Volume{
			{Name: "used", Driver: "local", UsageData: &volume.UsageData{RefCount: 1, Size: 2048}},
		},
	}, []container.Summary{
		{
			ID:    "container-used",
			Names: []string{"/db"},
			State: "exited",
			Mounts: []container.MountPoint{
				{Type: mount.TypeVolume, Name: "used", Destination: "/data", Mode: "z", RW: false},
			},
		},
	}, VolumeOptions{All: true})

	if len(report.Volumes) != 1 {
		t.Fatalf("Volumes = %#v, want used volume included", report.Volumes)
	}
	refs := report.Volumes[0].Containers
	if len(refs) != 1 || refs[0].Name != "db" || refs[0].Destination != "/data" || refs[0].RW {
		t.Fatalf("Containers = %#v, want db /data rw=false", refs)
	}
}

func TestBuildVolumeReportFiltersVolumesByWildcard(t *testing.T) {
	report := buildVolumeReport(volume.ListResponse{
		Volumes: []*volume.Volume{
			{Name: "app_data", Driver: "local", UsageData: &volume.UsageData{RefCount: 0, Size: 1024}},
			{Name: "db_data", Driver: "local", UsageData: &volume.UsageData{RefCount: 0, Size: 2048}},
		},
	}, nil, VolumeOptions{Filters: []string{"app_*"}})

	if report.Summary.Total != 1 || len(report.Volumes) != 1 || report.Volumes[0].Name != "app_data" {
		t.Fatalf("report = %#v, want only app_data", report)
	}
}

func TestRunVolumeReportListsAllContainers(t *testing.T) {
	fake := &fakeVolumeDockerService{}
	restore := replaceVolumeServiceFactory(fake)
	defer restore()

	if _, err := runVolumeReport(context.Background(), VolumeOptions{}); err != nil {
		t.Fatalf("runVolumeReport() error = %v", err)
	}
	if !fake.allFlag {
		t.Fatal("ListContainers all = false, want true")
	}
}

func TestPrintVolumeReportIncludesSummaryAndContainers(t *testing.T) {
	var out bytes.Buffer
	printVolumeReport(&out, VolumeReport{
		Summary: VolumeSummary{Total: 1, Unused: 1, ReclaimableSize: 1024},
		Volumes: []VolumeRef{{
			Name:       "unused",
			Driver:     "local",
			Mountpoint: "/var/lib/docker/volumes/unused/_data",
			Size:       1024,
			RefCount:   0,
			Status:     "unused",
		}},
	}, VolumeOptions{})

	got := out.String()
	for _, want := range []string{"Docker volume 报告", "未使用=1", "可回收=1.0 KiB", "容器=无"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output = %q, want %q", got, want)
		}
	}
}

func TestIsAnonymousVolumeName(t *testing.T) {
	if !isAnonymousVolumeName(strings.Repeat("a", 64)) {
		t.Fatal("64 hex chars should be anonymous volume name")
	}
	if isAnonymousVolumeName("named-volume") {
		t.Fatal("named-volume should not be anonymous volume name")
	}
}

func replaceVolumeServiceFactory(fake *fakeVolumeDockerService) func() {
	previous := newVolumeDockerService
	newVolumeDockerService = func() (volumeDockerService, error) {
		return fake, nil
	}
	return func() {
		newVolumeDockerService = previous
	}
}
