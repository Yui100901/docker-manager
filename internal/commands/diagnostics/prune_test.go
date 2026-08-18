package diagnostics

import (
	"bytes"
	"context"
	"docker-manager/internal/commandflags"
	"docker-manager/internal/docker"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/volume"
	mobyclient "github.com/moby/moby/client"
)

type fakePruneDockerService struct {
	usage            pruneDiskUsage
	containers       []container.Summary
	inspects         map[string]container.InspectResponse
	imageInspects    map[string]image.InspectResponse
	volumeInspects   map[string]volume.Volume
	cacheReports     map[string]*build.CachePruneReport
	operationErrors  map[string]error
	calls            []string
	diskUsageOptions []mobyclient.DiskUsageOptions
	diskUsageResults []pruneDiskUsage
	diskUsageErrors  []error
	diskUsageIndex   int
	diskUsageHook    func()
	operationHook    func(string)
	mu               sync.Mutex
}

func (f *fakePruneDockerService) DiskUsage(ctx context.Context, opts mobyclient.DiskUsageOptions) (pruneDiskUsage, error) {
	f.recordCall("disk-usage")
	f.mu.Lock()
	f.diskUsageOptions = append(f.diskUsageOptions, opts)
	usage := f.usage
	var sequenceErr error
	if f.diskUsageIndex < len(f.diskUsageResults) {
		usage = f.diskUsageResults[f.diskUsageIndex]
	}
	if f.diskUsageIndex < len(f.diskUsageErrors) {
		sequenceErr = f.diskUsageErrors[f.diskUsageIndex]
	}
	f.diskUsageIndex++
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return pruneDiskUsage{}, err
	}
	if err := f.operationError("disk-usage"); err != nil {
		return pruneDiskUsage{}, err
	}
	if sequenceErr != nil {
		return pruneDiskUsage{}, sequenceErr
	}
	if f.diskUsageHook != nil {
		f.diskUsageHook()
	}
	return usage, nil
}

func (f *fakePruneDockerService) ListContainers(ctx context.Context, all bool) ([]container.Summary, error) {
	f.recordCall("list-containers")
	if err := f.operationError("list-containers"); err != nil {
		return nil, err
	}
	return f.containers, nil
}

func (f *fakePruneDockerService) InspectContainer(ctx context.Context, id string) (container.InspectResponse, error) {
	call := "inspect-container:" + id
	f.recordCall(call)
	if err := f.operationError(call); err != nil {
		return container.InspectResponse{}, err
	}
	if f.inspects == nil {
		return container.InspectResponse{}, nil
	}
	return f.inspects[id], nil
}

func (f *fakePruneDockerService) InspectImage(ctx context.Context, id string) (image.InspectResponse, error) {
	call := "inspect-image:" + id
	f.recordCall(call)
	if err := f.operationError(call); err != nil {
		return image.InspectResponse{}, err
	}
	if inspect, ok := f.imageInspects[id]; ok {
		return inspect, nil
	}
	for _, candidate := range f.usage.Images {
		if candidate != nil && samePruneImageID(candidate.ID, id) {
			return image.InspectResponse{ID: candidate.ID, RepoTags: append([]string(nil), candidate.RepoTags...)}, nil
		}
	}
	return image.InspectResponse{ID: id, RepoTags: []string{"<none>:<none>"}}, nil
}

func (f *fakePruneDockerService) InspectVolume(ctx context.Context, name string) (volume.Volume, error) {
	call := "inspect-volume:" + name
	f.recordCall(call)
	if err := f.operationError(call); err != nil {
		return volume.Volume{}, err
	}
	if inspect, ok := f.volumeInspects[name]; ok {
		return inspect, nil
	}
	for _, candidate := range f.usage.Volumes {
		if candidate != nil && candidate.Name == name {
			return *candidate, nil
		}
	}
	return volume.Volume{Name: name}, nil
}

func (f *fakePruneDockerService) RemoveContainer(ctx context.Context, id string) error {
	call := "remove-container:" + id
	f.recordCall(call)
	return f.operationError(call)
}

func (f *fakePruneDockerService) RemoveImage(ctx context.Context, id string) error {
	call := "remove-image:" + id
	f.recordCall(call)
	return f.operationError(call)
}

func (f *fakePruneDockerService) RemoveVolume(ctx context.Context, name string) error {
	call := "remove-volume:" + name
	f.recordCall(call)
	return f.operationError(call)
}

func (f *fakePruneDockerService) RemoveBuildCache(ctx context.Context, id string, _ time.Time, _ bool) (*build.CachePruneReport, error) {
	call := "remove-build-cache:" + id
	f.recordCall(call)
	if report, ok := f.cacheReports[id]; ok {
		return report, f.operationError(call)
	}
	return &build.CachePruneReport{CachesDeleted: []string{id}}, f.operationError(call)
}

func (f *fakePruneDockerService) recordCall(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	hook := f.operationHook
	f.mu.Unlock()
	if hook != nil {
		hook(call)
	}
}

func (f *fakePruneDockerService) operationError(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.operationErrors[call]
}

func (f *fakePruneDockerService) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func TestBuildPruneReportIncludesOnlyReclaimableResources(t *testing.T) {
	report := buildPruneReport(pruneDiskUsage{
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
	if len(report.DanglingImages) != 1 || report.DanglingImages[0].ID != "sha256:dangling-image" {
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

func TestBuildPruneReportAppliesUntilToBuildCacheLastUsedAt(t *testing.T) {
	cutoff := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	old := cutoff.Add(-time.Second)
	atCutoff := cutoff
	recent := cutoff.Add(time.Second)
	report := buildPruneReport(pruneDiskUsage{BuildCache: []*build.CacheRecord{
		{ID: "old", Size: 10, LastUsedAt: &old},
		{ID: "at-cutoff", Size: 20, LastUsedAt: &atCutoff},
		{ID: "recent", Size: 40, LastUsedAt: &recent},
		{ID: "never-used", Size: 80},
	}}, PruneScope{Until: cutoff.Format(time.RFC3339Nano)})

	var ids []string
	for _, candidate := range report.BuildCaches {
		ids = append(ids, candidate.ID)
	}
	if got, want := strings.Join(ids, ","), "at-cutoff,never-used,old"; got != want {
		t.Fatalf("BuildCaches = %q, want %q", got, want)
	}
	if report.EstimatedBytes != 110 {
		t.Fatalf("EstimatedBytes = %d, want 110", report.EstimatedBytes)
	}
	for _, candidate := range report.BuildCaches {
		if candidate.snapshot == nil || !candidate.snapshot.HasUntilCutoff || !candidate.snapshot.UntilCutoff.Equal(cutoff) {
			t.Fatalf("candidate %q snapshot = %#v, want fixed cutoff %s", candidate.ID, candidate.snapshot, cutoff)
		}
	}
}

func TestRunPruneReportRechecksBuildCacheEligibilityBeforeDelete(t *testing.T) {
	cutoff := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	old := cutoff.Add(-time.Hour)
	recent := cutoff.Add(time.Minute)
	tests := []struct {
		name    string
		current *build.CacheRecord
		want    string
	}{
		{name: "recently used", current: &build.CacheRecord{ID: "cache", LastUsedAt: &recent}, want: "used after the fixed until cutoff"},
		{name: "now in use", current: &build.CacheRecord{ID: "cache", LastUsedAt: &old, InUse: true}, want: "now in use"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			initial := &build.CacheRecord{ID: "cache", LastUsedAt: &old}
			fake := &fakePruneDockerService{usage: pruneDiskUsage{BuildCache: []*build.CacheRecord{initial}}}
			fake.diskUsageHook = func() {
				fake.mu.Lock()
				fake.usage.BuildCache = []*build.CacheRecord{tt.current}
				fake.mu.Unlock()
			}
			restoreFactory := replacePruneServiceFactory(fake)
			defer restoreFactory()

			report, err := runPruneReport(context.Background(), PruneReportOptions{
				Apply:       true,
				Confirm:     true,
				Only:        []string{"build-cache"},
				UntilValues: []string{cutoff.Format(time.RFC3339Nano)},
			})
			if err == nil {
				t.Fatal("runPruneReport() error = nil, want eligibility failure")
			}
			if report.ApplyResult == nil || len(report.ApplyResult.Failures) != 1 || !strings.Contains(report.ApplyResult.Failures[0].Error, tt.want) {
				t.Fatalf("ApplyResult = %#v, want %q failure", report.ApplyResult, tt.want)
			}
			if hasPruneCall(fake.callList(), "remove-build-cache:cache") {
				t.Fatalf("calls = %#v, changed cache eligibility must prevent deletion", fake.callList())
			}
			if got, want := strings.Join(fake.callList(), ","), "disk-usage,disk-usage"; got != want {
				t.Fatalf("calls = %q, want %q", got, want)
			}
		})
	}
}

func TestApplyPruneReportRejectsBuildCacheCandidateRoundTrippedThroughJSON(t *testing.T) {
	cache := &build.CacheRecord{ID: "cache"}
	report := buildPruneReport(pruneDiskUsage{BuildCache: []*build.CacheRecord{cache}}, PruneScope{})
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var roundTripped PruneReport
	if err := json.Unmarshal(encoded, &roundTripped); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	fake := &fakePruneDockerService{usage: pruneDiskUsage{BuildCache: []*build.CacheRecord{cache}}}

	result, err := applyPruneReport(context.Background(), fake, roundTripped)
	if err == nil {
		t.Fatal("applyPruneReport() error = nil, want missing trusted snapshot error")
	}
	if len(result.Failures) != 1 || !strings.Contains(result.Failures[0].Error, "trusted snapshot identity") {
		t.Fatalf("Failures = %#v, want trusted snapshot failure", result.Failures)
	}
	if len(fake.callList()) != 0 {
		t.Fatalf("calls = %#v, untrusted JSON candidate must not reach Docker", fake.callList())
	}
}

func TestBuildPruneReportDeduplicatesSnapshotCandidates(t *testing.T) {
	duplicateVolume := &volume.Volume{Name: "volume", Driver: "local", UsageData: &volume.UsageData{RefCount: 0, Size: 30}}
	report := buildPruneReport(pruneDiskUsage{
		Containers: []*container.Summary{
			{ID: "container", State: "exited", SizeRw: 10},
			{ID: "container", State: "exited", SizeRw: 999},
		},
		Images: []*image.Summary{
			{ID: "sha256:image", RepoTags: []string{"<none>:<none>"}, Size: 20},
			{ID: "image", RepoTags: []string{"<none>:<none>"}, Size: 999},
		},
		Volumes: []*volume.Volume{duplicateVolume, duplicateVolume},
		BuildCache: []*build.CacheRecord{
			{ID: "cache", Size: 40},
			{ID: "cache", Size: 999},
		},
	}, PruneScope{})

	if len(report.StoppedContainers) != 1 || len(report.DanglingImages) != 1 || len(report.UnusedVolumes) != 1 || len(report.BuildCaches) != 1 {
		t.Fatalf("report contains duplicate candidates: %#v", report)
	}
	if report.EstimatedBytes != 100 {
		t.Fatalf("EstimatedBytes = %d, want first unique candidates totaling 100", report.EstimatedBytes)
	}
}

func TestPruneCommandRejectsEmptyScopeAndConflictingUntilBeforeDocker(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "empty only", args: []string{"--only="}},
		{name: "empty only comma part", args: []string{"--only=container,,image"}},
		{name: "empty filter", args: []string{"--filter="}},
		{name: "empty filter comma part", args: []string{"--filter=label=keep,"}},
		{name: "empty protect label", args: []string{"--protect-label="}},
		{name: "empty protect label comma part", args: []string{"--protect-label=keep,"}},
		{name: "empty until", args: []string{"--until="}},
		{name: "zero duration", args: []string{"--until=0s"}},
		{name: "negative duration", args: []string{"--until=-1h"}},
		{name: "repeated conflicting until", args: []string{"--until=24h", "--until=48h"}},
		{name: "flag and filter conflict", args: []string{"--until=24h", "--filter=until=48h"}},
		{name: "filter conflict", args: []string{"--filter=until=24h", "--filter=until=48h"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			factoryCalls := 0
			previous := newPruneDockerService
			newPruneDockerService = func() (pruneDockerService, error) {
				factoryCalls++
				return &fakePruneDockerService{}, nil
			}
			defer func() { newPruneDockerService = previous }()

			cmd := NewPruneReportCommand()
			cmd.SilenceUsage = true
			cmd.SetArgs(tt.args)
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			if err := cmd.ExecuteContext(context.Background()); err == nil {
				t.Fatalf("ExecuteContext(%v) error = nil", tt.args)
			}
			if factoryCalls != 0 {
				t.Fatalf("factory calls = %d, want validation before Docker", factoryCalls)
			}
		})
	}
}

func TestPruneCommandAcceptsEquivalentRepeatedUntil(t *testing.T) {
	fake := &fakePruneDockerService{}
	restoreFactory := replacePruneServiceFactory(fake)
	defer restoreFactory()

	cmd := NewPruneReportCommand()
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--until=24h", "--until=1440m", "--filter=until=24h"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}
	if got := fake.callList(); len(got) != 1 || got[0] != "disk-usage" {
		t.Fatalf("calls = %#v, want one report snapshot", got)
	}
}

func TestRunPruneReportSkipsVolumeReferencedByInspect(t *testing.T) {
	fake := &fakePruneDockerService{
		usage: pruneDiskUsage{
			Volumes: []*volume.Volume{
				{Name: "data", Driver: "local", UsageData: &volume.UsageData{RefCount: 0, Size: 500}},
			},
		},
		containers: []container.Summary{{ID: "container-db", Names: []string{"/db"}, State: "running"}},
		inspects: map[string]container.InspectResponse{
			"container-db": {
				ID:   "container-db",
				Name: "/db",
				Mounts: []container.MountPoint{
					{Type: mount.TypeVolume, Name: "data", Destination: "/var/lib/postgresql/data", RW: true},
				},
			},
		},
	}
	restoreFactory := replacePruneServiceFactory(fake)
	defer restoreFactory()

	report, err := runPruneReport(context.Background(), PruneReportOptions{})
	if err != nil {
		t.Fatalf("runPruneReport() error = %v", err)
	}
	if len(report.UnusedVolumes) != 0 {
		t.Fatalf("UnusedVolumes = %#v, want referenced volume skipped", report.UnusedVolumes)
	}
	if len(report.Warnings) == 0 || !strings.Contains(report.Warnings[0], "仍被 1 个容器引用") {
		t.Fatalf("Warnings = %#v, want referenced volume warning", report.Warnings)
	}
	if report.EstimatedBytes != 0 {
		t.Fatalf("EstimatedBytes = %d, want 0", report.EstimatedBytes)
	}
}

func TestRunPruneReportUsesDefaultDiskUsageOptionsWithoutOnly(t *testing.T) {
	fake := &fakePruneDockerService{}
	restoreFactory := replacePruneServiceFactory(fake)
	defer restoreFactory()

	if _, err := runPruneReport(context.Background(), PruneReportOptions{}); err != nil {
		t.Fatalf("runPruneReport() error = %v", err)
	}
	if len(fake.diskUsageOptions) != 1 {
		t.Fatalf("diskUsageOptions = %#v, want one call", fake.diskUsageOptions)
	}
	if fake.diskUsageOptions[0] != (mobyclient.DiskUsageOptions{}) {
		t.Fatalf("DiskUsageOptions = %#v, want zero value for default full report", fake.diskUsageOptions[0])
	}
}

func TestRunPruneReportUsesOnlyDiskUsageOptions(t *testing.T) {
	fake := &fakePruneDockerService{
		usage: pruneDiskUsage{
			Volumes: []*volume.Volume{
				{Name: "data", Driver: "local", UsageData: &volume.UsageData{RefCount: 0, Size: 500}},
			},
			Images: []*image.Summary{
				{ID: "sha256:dangling-image", RepoTags: []string{"<none>:<none>"}, Size: 300},
			},
		},
	}
	restoreFactory := replacePruneServiceFactory(fake)
	defer restoreFactory()

	report, err := runPruneReport(context.Background(), PruneReportOptions{Only: []string{"volume"}})
	if err != nil {
		t.Fatalf("runPruneReport() error = %v", err)
	}
	if len(fake.diskUsageOptions) != 1 {
		t.Fatalf("diskUsageOptions = %#v, want one call", fake.diskUsageOptions)
	}
	want := mobyclient.DiskUsageOptions{Volumes: true}
	if fake.diskUsageOptions[0] != want {
		t.Fatalf("DiskUsageOptions = %#v, want %#v", fake.diskUsageOptions[0], want)
	}
	if len(report.UnusedVolumes) != 1 || len(report.DanglingImages) != 0 {
		t.Fatalf("report = %#v, want only volume candidates", report)
	}
	if !hasPruneCall(fake.callList(), "list-containers") {
		t.Fatalf("calls = %#v, want volume reference check", fake.callList())
	}
}

func hasPruneCall(calls []string, want string) bool {
	for _, call := range calls {
		if call == want {
			return true
		}
	}
	return false
}

func TestApplyPruneReportRejectsReferencedVolume(t *testing.T) {
	volumeSnapshot := &volume.Volume{Name: "data", CreatedAt: "2026-08-17T10:00:00Z", Driver: "local", Mountpoint: "/volumes/data", Scope: "local"}
	fake := &fakePruneDockerService{
		containers: []container.Summary{{ID: "container-db", Names: []string{"/db"}}},
		inspects: map[string]container.InspectResponse{
			"container-db": {
				ID:   "container-db",
				Name: "/db",
				Mounts: []container.MountPoint{
					{Type: mount.TypeVolume, Name: "data", Destination: "/data", RW: true},
				},
			},
		},
	}

	result, err := applyPruneReport(context.Background(), fake, PruneReport{
		UnusedVolumes: []PruneVolumeRef{{Name: "data", snapshot: newPruneVolumeSnapshot(volumeSnapshot)}},
	})
	if err == nil {
		t.Fatal("applyPruneReport() error = nil, want referenced-volume failure")
	}
	if len(result.Failures) != 1 || !strings.Contains(result.Failures[0].Error, "referenced") {
		t.Fatalf("Failures = %#v, want referenced-volume failure", result.Failures)
	}
	if hasPruneCall(fake.callList(), "remove-volume:data") {
		t.Fatalf("calls = %#v, referenced volume must not be removed", fake.callList())
	}
}

func TestRunPruneReportApplyRequiresConfirm(t *testing.T) {
	factoryCalls := 0
	previous := newPruneDockerService
	newPruneDockerService = func() (pruneDockerService, error) {
		factoryCalls++
		return &fakePruneDockerService{}, nil
	}
	defer func() { newPruneDockerService = previous }()

	_, err := runPruneReport(context.Background(), PruneReportOptions{Apply: true})
	if err == nil {
		t.Fatal("runPruneReport() error = nil, want confirm error")
	}
	if !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("runPruneReport() error = %q, want --confirm hint", err.Error())
	}
	if factoryCalls != 0 {
		t.Fatalf("factory calls = %d, want confirmation before client creation", factoryCalls)
	}
}

func TestRunPruneReportRejectsInvalidFormatBeforeDocker(t *testing.T) {
	factoryCalls := 0
	previous := newPruneDockerService
	newPruneDockerService = func() (pruneDockerService, error) {
		factoryCalls++
		return &fakePruneDockerService{usage: pruneDiskUsage{
			Containers: []*container.Summary{{ID: "candidate", State: "exited"}},
		}}, nil
	}
	defer func() { newPruneDockerService = previous }()

	_, err := runPruneReport(context.Background(), PruneReportOptions{
		Apply:   true,
		Confirm: true,
		FormatOptions: commandflags.FormatOptions{
			Format: "yaml",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "不支持的输出格式") {
		t.Fatalf("runPruneReport() error = %v, want invalid format", err)
	}
	if factoryCalls != 0 {
		t.Fatalf("factory calls = %d, want format validation before Docker", factoryCalls)
	}
}

func TestPruneCommandRemoteJSONKeepsTargetNoticeOffStdout(t *testing.T) {
	t.Cleanup(func() { docker.Configure(docker.Options{}) })
	docker.Configure(docker.Options{Host: "tcp://192.0.2.10:2375"})
	fake := &fakePruneDockerService{}
	restoreFactory := replacePruneServiceFactory(fake)
	defer restoreFactory()

	cmd := NewPruneReportCommand()
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--apply", "--confirm", "--format=json"})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}
	if !json.Valid(stdout.Bytes()) {
		t.Fatalf("stdout is not valid JSON: %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "Target Docker:") {
		t.Fatalf("stdout = %q, remote notice must not corrupt JSON", stdout.String())
	}
	if !strings.Contains(stderr.String(), "Target Docker: tcp://192.0.2.10:2375") {
		t.Fatalf("stderr = %q, want remote target notice", stderr.String())
	}
}

func TestRunPruneReportApplyRequiresConfirmMentionsRemoteDocker(t *testing.T) {
	t.Cleanup(func() { docker.Configure(docker.Options{}) })
	docker.Configure(docker.Options{Host: "tcp://docker.example:2375"})

	_, err := runPruneReport(context.Background(), PruneReportOptions{Apply: true})
	if err == nil {
		t.Fatal("runPruneReport() error = nil, want confirm error")
	}
	if !strings.Contains(err.Error(), "tcp://docker.example:2375") {
		t.Fatalf("runPruneReport() error = %q, want remote endpoint", err.Error())
	}
}

func TestRunPruneReportReturnsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := runPruneReport(ctx, PruneReportOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runPruneReport() error = %v, want context.Canceled", err)
	}
}

func TestRunPruneReportReturnsCanceledContextDuringVolumeRefs(t *testing.T) {
	fake := &fakePruneDockerService{
		usage: pruneDiskUsage{
			Volumes: []*volume.Volume{
				{Name: "data", Driver: "local", UsageData: &volume.UsageData{RefCount: 0, Size: 500}},
			},
		},
		containers: []container.Summary{{ID: "container-db", Names: []string{"/db"}}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	fake.diskUsageHook = cancel
	restoreFactory := replacePruneServiceFactory(fake)
	defer restoreFactory()

	_, err := runPruneReport(ctx, PruneReportOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runPruneReport() error = %v, want context.Canceled", err)
	}
}

func TestBuildPruneReportReturnsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := buildPruneReportWithContext(ctx, pruneDiskUsage{
		Containers: []*container.Summary{{ID: "old", State: "exited"}},
	}, PruneScope{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("buildPruneReportWithContext() error = %v, want context.Canceled", err)
	}
}

func TestRunPruneReportApplyRunsExactSnapshotCandidatesInSafeOrder(t *testing.T) {
	containerID := strings.Repeat("c", 64)
	imageID := "sha256:" + strings.Repeat("a", 64)
	cacheID := "cache-record-full-id-1234567890"
	fake := &fakePruneDockerService{
		usage: pruneDiskUsage{
			Containers: []*container.Summary{{ID: containerID, Names: []string{"/old"}, State: "exited", SizeRw: 100}},
			Images:     []*image.Summary{{ID: imageID, RepoTags: []string{"<none>:<none>"}, Size: 200}},
			Volumes: []*volume.Volume{{
				Name: "unused", CreatedAt: "2026-08-17T10:00:00Z", Driver: "local", Mountpoint: "/volumes/unused", Scope: "local",
				UsageData: &volume.UsageData{RefCount: 0, Size: 300},
			}},
			BuildCache: []*build.CacheRecord{{ID: cacheID, Size: 400, InUse: false}},
		},
		cacheReports: map[string]*build.CachePruneReport{cacheID: {
			CachesDeleted:  []string{cacheID},
			SpaceReclaimed: 400,
		}},
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
	if report.ApplyResult.SpaceReclaimed != 400 {
		t.Fatalf("SpaceReclaimed = %d, want Docker-confirmed 400", report.ApplyResult.SpaceReclaimed)
	}
	if report.ApplyResult.EstimatedBytesReclaimed != 1000 {
		t.Fatalf("EstimatedBytesReclaimed = %d, want 1000", report.ApplyResult.EstimatedBytesReclaimed)
	}
	if len(report.ApplyResult.Failures) != 0 {
		t.Fatalf("Failures = %#v, want none", report.ApplyResult.Failures)
	}
	if len(report.ApplyResult.ContainersDeleted) != 1 || report.ApplyResult.ContainersDeleted[0] != containerID {
		t.Fatalf("ContainersDeleted = %#v, want full snapshot ID", report.ApplyResult.ContainersDeleted)
	}
	wantCalls := []string{
		"disk-usage",
		"list-containers",
		"remove-container:" + containerID,
		"inspect-image:" + imageID,
		"remove-image:" + imageID,
		"list-containers",
		"inspect-volume:unused",
		"remove-volume:unused",
		"disk-usage",
		"remove-build-cache:" + cacheID,
	}
	if got := fake.callList(); strings.Join(got, ",") != strings.Join(wantCalls, ",") {
		t.Fatalf("calls = %#v, want %#v", got, wantCalls)
	}
}

func TestRunPruneReportApplyOnlyRunsSelectedOperations(t *testing.T) {
	fake := &fakePruneDockerService{usage: pruneDiskUsage{
		Containers: []*container.Summary{{ID: "old-container", State: "exited"}},
		Images:     []*image.Summary{{ID: "sha256:dangling", RepoTags: []string{"<none>:<none>"}}},
		Volumes: []*volume.Volume{{
			Name: "data", CreatedAt: "2026-08-17T10:00:00Z", Driver: "local", Mountpoint: "/volumes/data", Scope: "local",
			UsageData: &volume.UsageData{RefCount: 0},
		}},
		BuildCache: []*build.CacheRecord{{ID: "cache", InUse: false}},
	}}
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
	wantCalls := []string{
		"disk-usage",
		"list-containers",
		"remove-container:old-container",
		"list-containers",
		"inspect-volume:data",
		"remove-volume:data",
	}
	if got := fake.callList(); strings.Join(got, ",") != strings.Join(wantCalls, ",") {
		t.Fatalf("calls = %#v, want %#v", got, wantCalls)
	}
}

func TestRunPruneReportApplyWithLabelFilterSkipsBuildCache(t *testing.T) {
	fake := &fakePruneDockerService{usage: pruneDiskUsage{
		BuildCache: []*build.CacheRecord{{ID: "cache", InUse: false}},
	}}
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
	wantCalls := []string{"disk-usage"}
	if got := fake.callList(); strings.Join(got, ",") != strings.Join(wantCalls, ",") {
		t.Fatalf("calls = %#v, want %#v", got, wantCalls)
	}
}

func TestApplyPruneReportContinuesAfterCandidateFailure(t *testing.T) {
	imageID := "sha256:" + strings.Repeat("b", 64)
	fake := &fakePruneDockerService{operationErrors: map[string]error{
		"remove-container:first": errors.New("container became busy"),
	}}

	result, err := applyPruneReport(context.Background(), fake, PruneReport{
		StoppedContainers: []PruneContainerRef{{ID: "first", Size: 10}, {ID: "second", Size: 20}},
		DanglingImages:    []PruneImageRef{{ID: imageID, Size: 30}},
	})
	if err == nil {
		t.Fatal("applyPruneReport() error = nil, want partial-failure error")
	}
	if len(result.Failures) != 0 || len(result.UnknownOutcomes) != 1 || result.UnknownOutcomes[0].ID != "first" {
		t.Fatalf("Failures = %#v UnknownOutcomes = %#v, want first container outcome unknown", result.Failures, result.UnknownOutcomes)
	}
	if strings.Join(result.ContainersDeleted, ",") != "second" || strings.Join(result.ImagesDeleted, ",") != imageID {
		t.Fatalf("result = %#v, want later candidates deleted", result)
	}
	if result.EstimatedBytesReclaimed != 50 {
		t.Fatalf("EstimatedBytesReclaimed = %d, want 50", result.EstimatedBytesReclaimed)
	}
	wantCalls := []string{"remove-container:first", "inspect-container:first", "remove-container:second", "inspect-image:" + imageID, "remove-image:" + imageID}
	if got := fake.callList(); strings.Join(got, ",") != strings.Join(wantCalls, ",") {
		t.Fatalf("calls = %#v, want %#v", got, wantCalls)
	}
}

func TestApplyPruneReportStopsAfterCancellationBetweenCandidates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fake := &fakePruneDockerService{}
	fake.operationHook = func(call string) {
		if call == "remove-container:first" {
			cancel()
		}
	}

	result, err := applyPruneReport(ctx, fake, PruneReport{
		StoppedContainers: []PruneContainerRef{{ID: "first"}, {ID: "second"}},
		DanglingImages:    []PruneImageRef{{ID: "sha256:later"}},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("applyPruneReport() error = %v, want context.Canceled", err)
	}
	if strings.Join(result.ContainersDeleted, ",") != "first" {
		t.Fatalf("ContainersDeleted = %#v, want completed first candidate", result.ContainersDeleted)
	}
	if got := fake.callList(); len(got) != 1 || got[0] != "remove-container:first" {
		t.Fatalf("calls = %#v, want no API call after cancellation", got)
	}
}

func TestApplyPruneReportDoesNotDeleteAfterCanceledSafetyInspect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	const imageID = "sha256:full-image-id"
	fake := &fakePruneDockerService{}
	fake.operationHook = func(call string) {
		if call == "inspect-image:"+imageID {
			cancel()
		}
	}

	result, err := applyPruneReport(ctx, fake, PruneReport{
		DanglingImages: []PruneImageRef{{ID: imageID}},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("applyPruneReport() error = %v, want context.Canceled", err)
	}
	if len(result.ImagesDeleted) != 0 {
		t.Fatalf("ImagesDeleted = %#v, want none", result.ImagesDeleted)
	}
	if got := fake.callList(); len(got) != 1 || got[0] != "inspect-image:"+imageID {
		t.Fatalf("calls = %#v, want no delete after canceled safety inspect", got)
	}
}

func TestRunPruneReportPreservesPartialResultOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fake := &fakePruneDockerService{usage: pruneDiskUsage{
		Containers: []*container.Summary{{ID: "first", State: "exited"}, {ID: "second", State: "exited"}},
	}}
	fake.operationHook = func(call string) {
		if call == "remove-container:first" {
			cancel()
		}
	}
	restoreFactory := replacePruneServiceFactory(fake)
	defer restoreFactory()

	report, err := runPruneReport(ctx, PruneReportOptions{Apply: true, Confirm: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runPruneReport() error = %v, want context.Canceled", err)
	}
	if !report.Applied || report.ApplyResult == nil {
		t.Fatalf("Applied = %v ApplyResult = %#v, want partial result", report.Applied, report.ApplyResult)
	}
	if strings.Join(report.ApplyResult.ContainersDeleted, ",") != "first" {
		t.Fatalf("ContainersDeleted = %#v, want completed first candidate", report.ApplyResult.ContainersDeleted)
	}
	if got := fake.callList(); strings.Join(got, ",") != "disk-usage,remove-container:first" {
		t.Fatalf("calls = %#v, want no API call after cancellation", got)
	}
}

func TestRunPruneReportDoesNotTouchResourceAddedAfterSnapshot(t *testing.T) {
	const snapshotID = "snapshot-container"
	const addedID = "added-after-snapshot"
	fake := &fakePruneDockerService{usage: pruneDiskUsage{
		Containers: []*container.Summary{{ID: snapshotID, State: "exited"}},
	}}
	fake.diskUsageHook = func() {
		fake.usage.Containers = append(fake.usage.Containers, &container.Summary{ID: addedID, State: "exited"})
	}
	restoreFactory := replacePruneServiceFactory(fake)
	defer restoreFactory()

	report, err := runPruneReport(context.Background(), PruneReportOptions{Apply: true, Confirm: true})
	if err != nil {
		t.Fatalf("runPruneReport() error = %v", err)
	}
	if len(report.StoppedContainers) != 1 || report.StoppedContainers[0].ID != snapshotID {
		t.Fatalf("StoppedContainers = %#v, want original snapshot only", report.StoppedContainers)
	}
	if hasPruneCall(fake.callList(), "remove-container:"+addedID) {
		t.Fatalf("calls = %#v, resource added after snapshot was touched", fake.callList())
	}
}

func TestApplyPruneReportRejectsImageThatIsNoLongerDangling(t *testing.T) {
	const imageID = "sha256:full-image-id"
	fake := &fakePruneDockerService{imageInspects: map[string]image.InspectResponse{
		imageID: {ID: imageID, RepoTags: []string{"repo:new-tag"}},
	}}

	result, err := applyPruneReport(context.Background(), fake, PruneReport{
		DanglingImages: []PruneImageRef{{ID: imageID}},
	})
	if err == nil || len(result.Failures) != 1 || !strings.Contains(result.Failures[0].Error, "no longer dangling") {
		t.Fatalf("result = %#v error = %v, want state-change failure", result, err)
	}
	if hasPruneCall(fake.callList(), "remove-image:"+imageID) {
		t.Fatalf("calls = %#v, newly tagged image must not be removed", fake.callList())
	}
}

func TestApplyPruneReportRejectsRecreatedVolume(t *testing.T) {
	before := &volume.Volume{Name: "data", CreatedAt: "2026-08-17T10:00:00Z", Driver: "local", Mountpoint: "/volumes/data", Scope: "local"}
	after := *before
	after.CreatedAt = "2026-08-17T10:01:00Z"
	fake := &fakePruneDockerService{volumeInspects: map[string]volume.Volume{"data": after}}

	result, err := applyPruneReport(context.Background(), fake, PruneReport{
		UnusedVolumes: []PruneVolumeRef{{Name: "data", snapshot: newPruneVolumeSnapshot(before)}},
	})
	if err == nil || len(result.Failures) != 1 || !strings.Contains(result.Failures[0].Error, "identity changed") {
		t.Fatalf("result = %#v error = %v, want recreated-volume failure", result, err)
	}
	if hasPruneCall(fake.callList(), "remove-volume:data") {
		t.Fatalf("calls = %#v, recreated volume must not be removed", fake.callList())
	}
}

func TestPruneBuildCacheOptionsUseOneAnchoredEscapedID(t *testing.T) {
	cutoff := time.Now().Add(-2 * time.Hour)
	opts := pruneBuildCacheOptions("cache.id+[x]", cutoff, true)
	if !opts.All {
		t.Fatal("BuildCachePruneOptions.All = false, want true for exact non-dangling candidate support")
	}
	values := opts.Filters["id"]
	if len(values) != 1 || !values[`^cache\.id\+\[x\]$`] {
		t.Fatalf("id filters = %#v, want one anchored and escaped exact ID", values)
	}
	untilValues := opts.Filters["until"]
	if len(untilValues) != 1 {
		t.Fatalf("until filters = %#v, want one duration derived from the fixed cutoff", untilValues)
	}
	for value := range untilValues {
		duration, err := time.ParseDuration(value)
		if err != nil || duration < 2*time.Hour || duration > 2*time.Hour+time.Second {
			t.Fatalf("until filter = %q, want a Docker-compatible duration near 2h: %v", value, err)
		}
	}
	withoutUntil := pruneBuildCacheOptions("cache", time.Time{}, false)
	if _, exists := withoutUntil.Filters["until"]; exists {
		t.Fatalf("filters = %#v, want no until filter without a report cutoff", withoutUntil.Filters)
	}
}

func TestPruneBuildCacheUntilDurationKeepsFixedCutoff(t *testing.T) {
	now := time.Date(2026, 8, 18, 9, 30, 0, 0, time.UTC)
	if got := pruneBuildCacheUntilDuration(now.Add(-24*time.Hour), now); got != "24h0m0s" {
		t.Fatalf("pruneBuildCacheUntilDuration() = %q, want 24h0m0s", got)
	}
	if got := pruneBuildCacheUntilDuration(now.Add(time.Hour), now); got != "0s" {
		t.Fatalf("future cutoff duration = %q, want safe 0s", got)
	}
}

func TestApplyPruneReportRequiresBuildCacheDeletionConfirmationWhenRecordStillExists(t *testing.T) {
	const cacheID = "cache-full-id"
	cache := &build.CacheRecord{ID: cacheID}
	fake := &fakePruneDockerService{usage: pruneDiskUsage{BuildCache: []*build.CacheRecord{cache}}, cacheReports: map[string]*build.CachePruneReport{
		cacheID: {},
	}}

	result, err := applyPruneReport(context.Background(), fake, PruneReport{
		BuildCaches: []PruneBuildCacheRef{testPruneBuildCacheRef(cache, time.Time{}, false)},
	})
	if err == nil {
		t.Fatal("applyPruneReport() error = nil, want unconfirmed-deletion failure")
	}
	if len(result.Failures) != 1 || !strings.Contains(result.Failures[0].Error, "did not confirm") {
		t.Fatalf("Failures = %#v, want unconfirmed-deletion failure", result.Failures)
	}
	if len(result.BuildCachesDeleted) != 0 {
		t.Fatalf("BuildCachesDeleted = %#v, want none", result.BuildCachesDeleted)
	}
}

func TestApplyPruneReportConfirmsUnreportedBuildCacheDeletionByDiskUsage(t *testing.T) {
	const cacheID = "cache-full-id"
	cache := &build.CacheRecord{ID: cacheID, Size: 42}
	fake := &fakePruneDockerService{
		diskUsageResults: []pruneDiskUsage{{BuildCache: []*build.CacheRecord{cache}}, {}},
		cacheReports:     map[string]*build.CachePruneReport{cacheID: {}},
	}

	result, err := applyPruneReport(context.Background(), fake, PruneReport{
		BuildCaches: []PruneBuildCacheRef{testPruneBuildCacheRef(cache, time.Time{}, false)},
	})
	if err != nil {
		t.Fatalf("applyPruneReport() error = %v", err)
	}
	if strings.Join(result.BuildCachesDeleted, ",") != cacheID || result.EstimatedBytesReclaimed != 42 {
		t.Fatalf("result = %#v, want deletion confirmed by DiskUsage", result)
	}
	if len(result.Failures) != 0 || len(result.UnknownOutcomes) != 0 {
		t.Fatalf("Failures = %#v UnknownOutcomes = %#v, want neither", result.Failures, result.UnknownOutcomes)
	}
}

func TestApplyPruneReportRecordsUnknownWhenUnreportedBuildCacheCannotBeVerified(t *testing.T) {
	const cacheID = "cache-full-id"
	cache := &build.CacheRecord{ID: cacheID}
	fake := &fakePruneDockerService{
		diskUsageResults: []pruneDiskUsage{{BuildCache: []*build.CacheRecord{cache}}, {}},
		diskUsageErrors:  []error{nil, errors.New("verification unavailable")},
		cacheReports:     map[string]*build.CachePruneReport{cacheID: {}},
	}

	result, err := applyPruneReport(context.Background(), fake, PruneReport{
		BuildCaches: []PruneBuildCacheRef{testPruneBuildCacheRef(cache, time.Time{}, false)},
	})
	if err == nil {
		t.Fatal("applyPruneReport() error = nil, want unknown-outcome error")
	}
	if len(result.Failures) != 0 || len(result.UnknownOutcomes) != 1 || !strings.Contains(result.UnknownOutcomes[0].Reason, "verification unavailable") {
		t.Fatalf("Failures = %#v UnknownOutcomes = %#v, want one verification unknown", result.Failures, result.UnknownOutcomes)
	}
}

func TestApplyPruneReportReportsBuildCacheDeletionOutsideSnapshot(t *testing.T) {
	const candidateID = "cache-candidate"
	const unexpectedID = "cache-outside-snapshot"
	cache := &build.CacheRecord{ID: candidateID, Size: 50}
	fake := &fakePruneDockerService{usage: pruneDiskUsage{BuildCache: []*build.CacheRecord{cache}}, cacheReports: map[string]*build.CachePruneReport{
		candidateID: {
			CachesDeleted:  []string{candidateID, unexpectedID},
			SpaceReclaimed: 123,
		},
	}}

	result, err := applyPruneReport(context.Background(), fake, PruneReport{
		BuildCaches: []PruneBuildCacheRef{testPruneBuildCacheRef(cache, time.Time{}, false)},
	})
	if err == nil {
		t.Fatal("applyPruneReport() error = nil, want unexpected-deletion error")
	}
	if got, want := strings.Join(result.BuildCachesDeleted, ","), candidateID+","+unexpectedID; got != want {
		t.Fatalf("BuildCachesDeleted = %q, want %q", got, want)
	}
	if result.SpaceReclaimed != 123 || result.EstimatedBytesReclaimed != 50 {
		t.Fatalf("result sizes = confirmed:%d estimated:%d, want 123 and 50", result.SpaceReclaimed, result.EstimatedBytesReclaimed)
	}
	if len(result.Failures) != 1 || result.Failures[0].ID != unexpectedID || !strings.Contains(result.Failures[0].Error, "outside the fixed snapshot") {
		t.Fatalf("Failures = %#v, want unexpected deleted ID", result.Failures)
	}
}

func TestApplyPruneReportReportsAndDeduplicatesExtraBuildCacheFromSnapshot(t *testing.T) {
	const firstID = "cache-first"
	const secondID = "cache-second"
	first := &build.CacheRecord{ID: firstID, Size: 10}
	second := &build.CacheRecord{ID: secondID, Size: 20}
	fake := &fakePruneDockerService{usage: pruneDiskUsage{BuildCache: []*build.CacheRecord{first, second}}, cacheReports: map[string]*build.CachePruneReport{
		firstID: {
			CachesDeleted:  []string{firstID, secondID, secondID},
			SpaceReclaimed: 30,
		},
	}}

	result, err := applyPruneReport(context.Background(), fake, PruneReport{
		BuildCaches: []PruneBuildCacheRef{
			testPruneBuildCacheRef(first, time.Time{}, false),
			testPruneBuildCacheRef(second, time.Time{}, false),
			testPruneBuildCacheRef(second, time.Time{}, false),
		},
	})
	if err == nil {
		t.Fatal("applyPruneReport() error = nil, want exact-filter violation")
	}
	if got, want := strings.Join(result.BuildCachesDeleted, ","), firstID+","+secondID; got != want {
		t.Fatalf("BuildCachesDeleted = %q, want %q", got, want)
	}
	if result.SpaceReclaimed != 30 || result.EstimatedBytesReclaimed != 30 {
		t.Fatalf("result sizes = confirmed:%d estimated:%d, want 30 and 30", result.SpaceReclaimed, result.EstimatedBytesReclaimed)
	}
	if len(result.Failures) != 1 || result.Failures[0].ID != secondID || !strings.Contains(result.Failures[0].Error, "beyond the exact requested") {
		t.Fatalf("Failures = %#v, want exact-filter violation", result.Failures)
	}
	if got, want := strings.Join(fake.callList(), ","), "disk-usage,remove-build-cache:"+firstID; got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestApplyPruneReportDeduplicatesDirectCandidates(t *testing.T) {
	fake := &fakePruneDockerService{}
	result, err := applyPruneReport(context.Background(), fake, PruneReport{
		StoppedContainers: []PruneContainerRef{{ID: "container", Size: 10}, {ID: "container", Size: 999}},
	})
	if err != nil {
		t.Fatalf("applyPruneReport() error = %v", err)
	}
	if got := fake.callList(); len(got) != 1 || got[0] != "remove-container:container" {
		t.Fatalf("calls = %#v, want one removal", got)
	}
	if result.EstimatedBytesReclaimed != 10 {
		t.Fatalf("EstimatedBytesReclaimed = %d, want 10", result.EstimatedBytesReclaimed)
	}
}

func TestApplyPruneReportCancellationRechecksAndConfirmsRemoval(t *testing.T) {
	fake := &fakePruneDockerService{operationErrors: map[string]error{
		"remove-container:container":  context.Canceled,
		"inspect-container:container": cerrdefs.ErrNotFound,
	}}

	result, err := applyPruneReport(context.Background(), fake, PruneReport{
		StoppedContainers: []PruneContainerRef{{ID: "container", Size: 10}},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("applyPruneReport() error = %v, want context.Canceled", err)
	}
	if strings.Join(result.ContainersDeleted, ",") != "container" || result.EstimatedBytesReclaimed != 10 {
		t.Fatalf("result = %#v, want deletion confirmed by recheck", result)
	}
	if len(result.Failures) != 0 || len(result.UnknownOutcomes) != 0 {
		t.Fatalf("Failures = %#v UnknownOutcomes = %#v, want neither", result.Failures, result.UnknownOutcomes)
	}
}

func TestApplyPruneReportCancellationRechecksAndConfirmsResourceStillExists(t *testing.T) {
	fake := &fakePruneDockerService{operationErrors: map[string]error{
		"remove-container:container": context.DeadlineExceeded,
	}}

	result, err := applyPruneReport(context.Background(), fake, PruneReport{
		StoppedContainers: []PruneContainerRef{{ID: "container"}},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("applyPruneReport() error = %v, want context.DeadlineExceeded", err)
	}
	if len(result.ContainersDeleted) != 0 || len(result.Failures) != 0 || len(result.UnknownOutcomes) != 1 {
		t.Fatalf("result = %#v, want one unknown outcome because visibility does not prove final retention", result)
	}
}

func TestApplyPruneReportCancellationRecordsUnknownWhenRecheckFails(t *testing.T) {
	const imageID = "sha256:image"
	fake := &fakePruneDockerService{operationErrors: map[string]error{
		"remove-image:" + imageID: context.Canceled,
	}}
	fake.operationHook = func(call string) {
		if call == "remove-image:"+imageID {
			fake.mu.Lock()
			fake.operationErrors["inspect-image:"+imageID] = errors.New("recheck unavailable")
			fake.mu.Unlock()
		}
	}

	result, err := applyPruneReport(context.Background(), fake, PruneReport{
		DanglingImages: []PruneImageRef{{ID: imageID}},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("applyPruneReport() error = %v, want context.Canceled", err)
	}
	if len(result.Failures) != 0 || len(result.UnknownOutcomes) != 1 {
		t.Fatalf("Failures = %#v UnknownOutcomes = %#v, want one unknown only", result.Failures, result.UnknownOutcomes)
	}
	if result.UnknownOutcomes[0].ID != imageID || !strings.Contains(result.UnknownOutcomes[0].Reason, "recheck unavailable") {
		t.Fatalf("UnknownOutcomes = %#v, want recheck failure detail", result.UnknownOutcomes)
	}
}

func TestApplyPruneReportTransportErrorUsesVerifiedRemovalOutcome(t *testing.T) {
	fake := &fakePruneDockerService{operationErrors: map[string]error{
		"remove-container:container":  errors.New("connection reset by peer"),
		"inspect-container:container": cerrdefs.ErrNotFound,
	}}

	result, err := applyPruneReport(context.Background(), fake, PruneReport{
		StoppedContainers: []PruneContainerRef{{ID: "container", Size: 10}},
	})
	if err != nil {
		t.Fatalf("applyPruneReport() error = %v, want verified deletion to resolve transport error", err)
	}
	if strings.Join(result.ContainersDeleted, ",") != "container" || result.EstimatedBytesReclaimed != 10 {
		t.Fatalf("result = %#v, want confirmed deletion", result)
	}
	if len(result.Failures) != 0 || len(result.UnknownOutcomes) != 0 {
		t.Fatalf("Failures = %#v UnknownOutcomes = %#v, want neither", result.Failures, result.UnknownOutcomes)
	}
}

func TestApplyPruneReportTransportErrorRecordsUnknownWhenVerificationFails(t *testing.T) {
	fake := &fakePruneDockerService{operationErrors: map[string]error{
		"remove-container:container":  errors.New("connection reset by peer"),
		"inspect-container:container": errors.New("daemon unavailable"),
	}}

	result, err := applyPruneReport(context.Background(), fake, PruneReport{
		StoppedContainers: []PruneContainerRef{{ID: "container"}},
	})
	if err == nil {
		t.Fatal("applyPruneReport() error = nil, want unknown-outcome error")
	}
	if len(result.Failures) != 0 || len(result.UnknownOutcomes) != 1 {
		t.Fatalf("Failures = %#v UnknownOutcomes = %#v, want one unknown only", result.Failures, result.UnknownOutcomes)
	}
	if !strings.Contains(result.UnknownOutcomes[0].Reason, "daemon unavailable") {
		t.Fatalf("UnknownOutcomes = %#v, want verification failure", result.UnknownOutcomes)
	}
}

func TestApplyPruneReportBuildCacheCancellationRechecksDiskUsage(t *testing.T) {
	const cacheID = "cache"
	cache := &build.CacheRecord{ID: cacheID, Size: 10}
	fake := &fakePruneDockerService{
		diskUsageResults: []pruneDiskUsage{{BuildCache: []*build.CacheRecord{cache}}, {}},
		cacheReports:     map[string]*build.CachePruneReport{cacheID: nil},
		operationErrors: map[string]error{
			"remove-build-cache:" + cacheID: context.Canceled,
		},
	}

	result, err := applyPruneReport(context.Background(), fake, PruneReport{
		BuildCaches: []PruneBuildCacheRef{testPruneBuildCacheRef(cache, time.Time{}, false)},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("applyPruneReport() error = %v, want context.Canceled", err)
	}
	if strings.Join(result.BuildCachesDeleted, ",") != cacheID || result.EstimatedBytesReclaimed != 10 {
		t.Fatalf("result = %#v, want DiskUsage to confirm deletion", result)
	}
	if len(result.Failures) != 0 || len(result.UnknownOutcomes) != 0 {
		t.Fatalf("Failures = %#v UnknownOutcomes = %#v, want neither", result.Failures, result.UnknownOutcomes)
	}
	if got, want := strings.Join(fake.callList(), ","), "disk-usage,remove-build-cache:"+cacheID+",disk-usage"; got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestApplyPruneReportBuildCacheCancellationRecordsUnknownWhenDiskUsageFails(t *testing.T) {
	const cacheID = "cache"
	cache := &build.CacheRecord{ID: cacheID}
	fake := &fakePruneDockerService{
		diskUsageResults: []pruneDiskUsage{{BuildCache: []*build.CacheRecord{cache}}, {}},
		diskUsageErrors:  []error{nil, errors.New("disk usage unavailable")},
		cacheReports:     map[string]*build.CachePruneReport{cacheID: nil},
		operationErrors: map[string]error{
			"remove-build-cache:" + cacheID: context.DeadlineExceeded,
		},
	}

	result, err := applyPruneReport(context.Background(), fake, PruneReport{
		BuildCaches: []PruneBuildCacheRef{testPruneBuildCacheRef(cache, time.Time{}, false)},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("applyPruneReport() error = %v, want context.DeadlineExceeded", err)
	}
	if len(result.Failures) != 0 || len(result.UnknownOutcomes) != 1 {
		t.Fatalf("Failures = %#v UnknownOutcomes = %#v, want one unknown only", result.Failures, result.UnknownOutcomes)
	}
	if !strings.Contains(result.UnknownOutcomes[0].Reason, "disk usage unavailable") {
		t.Fatalf("UnknownOutcomes = %#v, want DiskUsage failure detail", result.UnknownOutcomes)
	}
}

func TestPruneCommandPrintsPartialResultBeforeReturningFailure(t *testing.T) {
	fake := &fakePruneDockerService{
		usage: pruneDiskUsage{Containers: []*container.Summary{{ID: "first", State: "exited"}, {ID: "second", State: "exited"}}},
		operationErrors: map[string]error{
			"remove-container:first": errors.New("became busy"),
		},
	}
	restoreFactory := replacePruneServiceFactory(fake)
	defer restoreFactory()

	cmd := NewPruneReportCommand()
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--apply", "--confirm"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("ExecuteContext() error = nil, want partial-failure error")
	}
	if !strings.Contains(err.Error(), "执行清理失败") || strings.Contains(err.Error(), "生成清理报告失败") {
		t.Fatalf("ExecuteContext() error = %q, want apply-specific wording", err)
	}
	for _, want := range []string{"执行结果:", "已删除容器: 1", "删除失败: 0", "结果未知: 1", "became busy"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output = %q, want %q", out.String(), want)
		}
	}
}

func TestPruneCommandPrintsPartialResultAndPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fake := &fakePruneDockerService{usage: pruneDiskUsage{
		Containers: []*container.Summary{{ID: "first", State: "exited"}, {ID: "second", State: "exited"}},
	}}
	fake.operationHook = func(call string) {
		if call == "remove-container:first" {
			cancel()
		}
	}
	restoreFactory := replacePruneServiceFactory(fake)
	defer restoreFactory()

	cmd := NewPruneReportCommand()
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--apply", "--confirm"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.ExecuteContext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ExecuteContext() error = %v, want context.Canceled", err)
	}
	for _, want := range []string{"执行结果:", "已删除容器: 1", "删除失败: 0"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output = %q, want %q", out.String(), want)
		}
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

	report := buildPruneReport(pruneDiskUsage{
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
	fullID := strings.Repeat("a", 64)
	printPruneReport(&out, PruneReport{
		GeneratedAt:       time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
		EstimatedBytes:    1024,
		StoppedContainers: []PruneContainerRef{{ID: fullID, Name: "old", Image: "busybox"}},
		ApplyResult: &PruneApplyResult{
			SpaceReclaimed:          2048,
			EstimatedBytesReclaimed: 1024,
			ContainersDeleted:       []string{fullID},
			Failures:                []PruneApplyFailure{{Kind: pruneKindContainer, ID: fullID, Error: "busy"}},
		},
		Applied: true,
	})

	got := out.String()
	for _, want := range []string{
		"已停止容器: 1",
		"aaaaaaaaaaaa old",
		"预计可回收空间: 1.0 KiB",
		"执行结果:",
		"Docker 已确认回收空间: 2.0 KiB",
		"成功项快照估算空间: 1.0 KiB",
		"删除失败: 1",
		"container aaaaaaaaaaaa: busy",
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

func testPruneBuildCacheRef(cache *build.CacheRecord, cutoff time.Time, hasCutoff bool) PruneBuildCacheRef {
	return PruneBuildCacheRef{
		ID:       cache.ID,
		Type:     cache.Type,
		Size:     cache.Size,
		snapshot: newPruneBuildCacheSnapshot(cache, cutoff, hasCutoff),
	}
}
