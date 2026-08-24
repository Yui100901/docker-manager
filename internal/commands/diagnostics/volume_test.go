package diagnostics

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/volume"
	mobyclient "github.com/moby/moby/client"
)

type fakeVolumeDockerService struct {
	volumes       volume.ListResponse
	containers    []container.Summary
	inspects      map[string]container.InspectResponse
	inspectErr    error
	measuredSizes map[string]int64
	measureErr    error
	allFlag       bool
}

func (f *fakeVolumeDockerService) ListVolumes(ctx context.Context) (volume.ListResponse, error) {
	return f.volumes, nil
}

func (f *fakeVolumeDockerService) ListContainers(ctx context.Context, all bool) ([]container.Summary, error) {
	f.allFlag = all
	return f.containers, nil
}

func (f *fakeVolumeDockerService) InspectContainer(ctx context.Context, id string) (container.InspectResponse, error) {
	if f.inspectErr != nil {
		return container.InspectResponse{}, f.inspectErr
	}
	if f.inspects == nil {
		return container.InspectResponse{}, nil
	}
	return f.inspects[id], nil
}

func (f *fakeVolumeDockerService) MeasureVolumeSize(ctx context.Context, volumeName, helperImage string) (int64, error) {
	if f.measureErr != nil {
		return -1, f.measureErr
	}
	if f.measuredSizes == nil {
		return -1, errors.New("missing fake measured size")
	}
	return f.measuredSizes[volumeName], nil
}

func TestVolumeProbeCreateOptionsAreRestrictedAndDigestPinned(t *testing.T) {
	opts := volumeProbeCreateOptions("/var/lib/docker/volumes/data/_data", volumeDefaultSizeImage, "probe", "probe-id")
	if opts.Config == nil || opts.Config.Image != volumeDefaultSizeImage || !strings.Contains(opts.Config.Image, "@sha256:") {
		t.Fatalf("probe image = %#v, want digest-pinned default", opts.Config)
	}
	if opts.HostConfig == nil {
		t.Fatal("probe HostConfig = nil")
	}
	host := opts.HostConfig
	if host.NetworkMode != "none" || !host.ReadonlyRootfs {
		t.Fatalf("probe isolation = network=%q readonly=%v", host.NetworkMode, host.ReadonlyRootfs)
	}
	if len(host.CapDrop) != 1 || host.CapDrop[0] != "ALL" {
		t.Fatalf("probe CapDrop = %#v, want ALL", host.CapDrop)
	}
	if len(host.SecurityOpt) != 1 || host.SecurityOpt[0] != "no-new-privileges:true" {
		t.Fatalf("probe SecurityOpt = %#v", host.SecurityOpt)
	}
	if len(host.Mounts) != 1 || host.Mounts[0].Type != mount.TypeBind || host.Mounts[0].Source != "/var/lib/docker/volumes/data/_data" || !host.Mounts[0].ReadOnly {
		t.Fatalf("probe mounts = %#v, want one read-only data volume", host.Mounts)
	}
	if opts.Config.Labels[volumeProbeOwnerLabel] != "probe-id" || len(opts.Config.Entrypoint) == 0 {
		t.Fatalf("probe ownership/entrypoint = labels=%#v entrypoint=%#v", opts.Config.Labels, opts.Config.Entrypoint)
	}
}

func TestDockerVolumeServiceMeasuresThroughBindMountAndCleansProbe(t *testing.T) {
	var created, removed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1.49")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && path == "/volumes/data":
			_ = json.NewEncoder(w).Encode(volume.Volume{Name: "data", Driver: "local", Mountpoint: "/var/lib/docker/volumes/data/_data"})
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && path == "/containers/create":
			var request struct {
				Image      string                `json:"Image"`
				Entrypoint []string              `json:"Entrypoint"`
				Labels     map[string]string     `json:"Labels"`
				HostConfig *container.HostConfig `json:"HostConfig"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode create request: %v", err)
			}
			if request.HostConfig == nil || len(request.HostConfig.Mounts) != 1 {
				t.Errorf("create HostConfig = %#v", request.HostConfig)
			} else if got := request.HostConfig.Mounts[0]; got.Type != mount.TypeBind || got.Source != "/var/lib/docker/volumes/data/_data" || !got.ReadOnly {
				t.Errorf("create probe mount = %#v", got)
			}
			if len(request.Entrypoint) == 0 || request.Labels[volumeProbeOwnerLabel] == "" {
				t.Errorf("create entrypoint/labels = %#v %#v", request.Entrypoint, request.Labels)
			}
			created.Store(true)
			_, _ = w.Write([]byte(`{"Id":"probe-container","Warnings":[]}`))
		case r.Method == http.MethodPost && path == "/containers/probe-container/start":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && path == "/containers/probe-container/wait":
			_, _ = w.Write([]byte(`{"StatusCode":0}`))
		case r.Method == http.MethodGet && path == "/containers/probe-container/logs":
			w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
			payload := []byte("12345\n")
			header := make([]byte, 8)
			header[0] = 1
			binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
			_, _ = w.Write(header)
			_, _ = w.Write(payload)
		case r.Method == http.MethodDelete && path == "/containers/probe-container":
			removed.Store(true)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected Docker API request: %s %s", r.Method, r.URL.String())
			http.Error(w, `{"message":"unexpected request"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()

	cli, err := mobyclient.New(mobyclient.WithHost(server.URL), mobyclient.WithAPIVersion("1.49"))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	service := &dockerVolumeService{cli: cli}
	size, err := service.MeasureVolumeSize(context.Background(), "data", volumeDefaultSizeImage)
	if err != nil || size != 12345 {
		t.Fatalf("MeasureVolumeSize() = %d, %v", size, err)
	}
	if !created.Load() || !removed.Load() {
		t.Fatalf("probe lifecycle created=%v removed=%v", created.Load(), removed.Load())
	}
}

func TestRemoveVolumeProbeContainerAcceptsResponseErrorWhenInspectConfirmsRemoval(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1.49")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodDelete && path == "/containers/probe":
			http.Error(w, `{"message":"response lost"}`, http.StatusInternalServerError)
		case r.Method == http.MethodGet && path == "/containers/probe/json":
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		default:
			http.Error(w, `{"message":"unexpected request"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()

	cli, err := mobyclient.New(mobyclient.WithHost(server.URL), mobyclient.WithAPIVersion("1.49"))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if err := removeVolumeProbeContainer(cli, "probe"); err != nil {
		t.Fatalf("removeVolumeProbeContainer() error = %v, want confirmed removal", err)
	}
}

func TestBuildVolumeReportClassifiesUnusedSuspectedAndUsedVolumes(t *testing.T) {
	report := buildVolumeReport(volume.ListResponse{
		Volumes: []volume.Volume{
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
		Volumes: []volume.Volume{
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

func TestRunVolumeReportUsesInspectMountsForVolumeRefs(t *testing.T) {
	fake := &fakeVolumeDockerService{
		volumes: volume.ListResponse{Volumes: []volume.Volume{
			{Name: "db_data", Driver: "local"},
		}},
		containers: []container.Summary{{
			ID:    "container-db",
			Names: []string{"/db"},
			Image: "postgres:16",
			State: "running",
		}},
		inspects: map[string]container.InspectResponse{
			"container-db": {
				ID:   "container-db",
				Name: "/db",
				State: &container.State{
					Status: container.StateRunning,
				},
				Config: &container.Config{Image: "postgres:16"},
				Mounts: []container.MountPoint{
					{Type: mount.TypeVolume, Name: "db_data", Source: "/var/lib/docker/volumes/db_data/_data", Destination: "/var/lib/postgresql/data", RW: true},
				},
			},
		},
	}
	restore := replaceVolumeServiceFactory(fake)
	defer restore()

	report, err := runVolumeReport(context.Background(), VolumeOptions{All: true, SizeMode: volumeSizeModeAPI})
	if err != nil {
		t.Fatalf("runVolumeReport() error = %v", err)
	}
	if len(report.Volumes) != 1 || report.Volumes[0].Status != "used" || report.Volumes[0].RefSource != "inspect" {
		t.Fatalf("volume = %#v, want used by inspect", report.Volumes)
	}
	refs := report.Volumes[0].Containers
	if len(refs) != 1 || refs[0].Image != "postgres:16" || refs[0].Destination != "/var/lib/postgresql/data" {
		t.Fatalf("refs = %#v, want inspect-derived db_data mount", refs)
	}
}

func TestRunVolumeReportFallsBackToSummaryMountsWhenInspectFails(t *testing.T) {
	fake := &fakeVolumeDockerService{
		volumes: volume.ListResponse{Volumes: []volume.Volume{
			{Name: "db_data", Driver: "local"},
		}},
		containers: []container.Summary{{
			ID:    "container-db",
			Names: []string{"/db"},
			State: "running",
			Mounts: []container.MountPoint{
				{Type: mount.TypeVolume, Name: "db_data", Destination: "/data", RW: true},
			},
		}},
		inspectErr: errors.New("inspect denied"),
	}
	restore := replaceVolumeServiceFactory(fake)
	defer restore()

	report, err := runVolumeReport(context.Background(), VolumeOptions{All: true, SizeMode: volumeSizeModeAPI})
	if err != nil {
		t.Fatalf("runVolumeReport() error = %v", err)
	}
	if len(report.Warnings) == 0 {
		t.Fatalf("Warnings = %#v, want inspect fallback warning", report.Warnings)
	}
	if len(report.Volumes) != 1 || len(report.Volumes[0].Containers) != 1 || report.Volumes[0].Containers[0].Destination != "/data" {
		t.Fatalf("volume = %#v, want summary fallback ref", report.Volumes)
	}
}

func TestRunVolumeReportDockerRunSizeModeMeasuresUnknownLocalVolumes(t *testing.T) {
	fake := &fakeVolumeDockerService{
		volumes: volume.ListResponse{Volumes: []volume.Volume{
			{Name: "unused", Driver: "local"},
		}},
		measuredSizes: map[string]int64{"unused": 4096},
	}
	restore := replaceVolumeServiceFactory(fake)
	defer restore()

	report, err := runVolumeReport(context.Background(), VolumeOptions{SizeMode: volumeSizeModeDockerRun, SizeImage: "busybox:latest"})
	if err != nil {
		t.Fatalf("runVolumeReport() error = %v", err)
	}
	if len(report.Volumes) != 1 || report.Volumes[0].Size != 4096 || report.Volumes[0].SizeSource != volumeSizeModeDockerRun {
		t.Fatalf("volume = %#v, want docker-run measured size", report.Volumes)
	}
	if report.Summary.UnknownSize != 0 || report.Summary.ReclaimableSize != 4096 {
		t.Fatalf("summary = %#v, want measured reclaimable size", report.Summary)
	}
}

func TestRunVolumeReportLocalGoSizeModeMeasuresUnknownLocalVolumes(t *testing.T) {
	fake := &fakeVolumeDockerService{
		volumes: volume.ListResponse{Volumes: []volume.Volume{
			{Name: "unused", Driver: "local", Mountpoint: "/var/lib/docker/volumes/unused/_data"},
		}},
	}
	restoreService := replaceVolumeServiceFactory(fake)
	defer restoreService()
	restoreLocal := replaceLocalVolumeSize(func(ctx context.Context, vol *VolumeRef) (int64, error) {
		if vol.Name != "unused" {
			t.Fatalf("local measure volume = %s, want unused", vol.Name)
		}
		return 8192, nil
	})
	defer restoreLocal()

	report, err := runVolumeReport(context.Background(), VolumeOptions{SizeMode: volumeSizeModeLocalGo})
	if err != nil {
		t.Fatalf("runVolumeReport() error = %v", err)
	}
	if len(report.Volumes) != 1 || report.Volumes[0].Size != 8192 || report.Volumes[0].SizeSource != volumeSizeModeLocalGo {
		t.Fatalf("volume = %#v, want local-go measured size", report.Volumes)
	}
	if report.Summary.UnknownSize != 0 || report.Summary.ReclaimableSize != 8192 {
		t.Fatalf("summary = %#v, want local-go reclaimable size", report.Summary)
	}
}

func TestRunVolumeReportAutoSizeModeFallsBackToDockerRun(t *testing.T) {
	fake := &fakeVolumeDockerService{
		volumes: volume.ListResponse{Volumes: []volume.Volume{
			{Name: "unused", Driver: "local", Mountpoint: "/var/lib/docker/volumes/unused/_data"},
		}},
		measuredSizes: map[string]int64{"unused": 16384},
	}
	restoreService := replaceVolumeServiceFactory(fake)
	defer restoreService()
	restoreLocal := replaceLocalVolumeSize(func(ctx context.Context, vol *VolumeRef) (int64, error) {
		return -1, errors.New("not local linux")
	})
	defer restoreLocal()

	report, err := runVolumeReport(context.Background(), VolumeOptions{SizeMode: volumeSizeModeAuto, SizeImage: "busybox:latest"})
	if err != nil {
		t.Fatalf("runVolumeReport() error = %v", err)
	}
	if len(report.Volumes) != 1 || report.Volumes[0].Size != 16384 || report.Volumes[0].SizeSource != volumeSizeModeDockerRun {
		t.Fatalf("volume = %#v, want docker-run fallback measured size", report.Volumes)
	}
	if report.Volumes[0].SizeError != "" || len(report.Warnings) != 0 {
		t.Fatalf("warnings=%#v sizeError=%q, want successful fallback without warning", report.Warnings, report.Volumes[0].SizeError)
	}
}

func TestNormalizeVolumeOptionsAcceptsNewSizeModes(t *testing.T) {
	for _, mode := range []string{volumeSizeModeAPI, volumeSizeModeLocalGo, volumeSizeModeDockerRun, volumeSizeModeAuto} {
		opts := VolumeOptions{SizeMode: mode}
		if err := normalizeVolumeOptions(&opts); err != nil {
			t.Fatalf("normalizeVolumeOptions(%q) error = %v", mode, err)
		}
	}
	opts := VolumeOptions{SizeMode: "bad"}
	if err := normalizeVolumeOptions(&opts); err == nil {
		t.Fatal("normalizeVolumeOptions(bad) error = nil, want error")
	}
}

func TestBuildVolumeReportFiltersVolumesByWildcard(t *testing.T) {
	report := buildVolumeReport(volume.ListResponse{
		Volumes: []volume.Volume{
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

func TestRunVolumeReportReturnsCanceledContext(t *testing.T) {
	fake := &fakeVolumeDockerService{
		volumes: volume.ListResponse{Volumes: []volume.Volume{{Name: "db_data", Driver: "local"}}},
		containers: []container.Summary{{
			ID:    "container-db",
			Names: []string{"/db"},
			State: "running",
		}},
	}
	restore := replaceVolumeServiceFactory(fake)
	defer restore()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := runVolumeReport(ctx, VolumeOptions{All: true, SizeMode: volumeSizeModeAPI})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runVolumeReport() error = %v, want context.Canceled", err)
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

func replaceLocalVolumeSize(fn func(context.Context, *VolumeRef) (int64, error)) func() {
	previous := measureLocalVolumeSize
	measureLocalVolumeSize = fn
	return func() {
		measureLocalVolumeSize = previous
	}
}
