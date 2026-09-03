package backup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"docker-manager/internal/runcontrol"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
)

func TestBackupContainersRejectsMaxItemsBeforeInspectOrOutput(t *testing.T) {
	fake := &fakeBackupDockerService{containers: []container.Summary{
		{ID: "one-id", Names: []string{"/one"}},
		{ID: "two-id", Names: []string{"/two"}},
	}}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	controller, err := runcontrol.New(runcontrol.Limits{MaxItems: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx := runcontrol.WithController(context.Background(), controller)
	_, err = backupContainers(ctx, []string{"one", "two"}, BackupOptions{OutputDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "item budget exceeded") {
		t.Fatalf("backupContainers() error = %v, want max-items rejection", err)
	}
	if hasCallPrefix(fake.calls, "inspect-container:") || hasCallPrefix(fake.calls, "save-image:") {
		t.Fatalf("calls = %#v, backup work started after max-items rejection", fake.calls)
	}
}

func TestBackupRejectsRelatedResourceBudgetBeforeOutput(t *testing.T) {
	inspect := budgetContainerInspect("one")
	fake := &fakeBackupDockerService{
		containers: []container.Summary{{ID: "one-id", Names: []string{"/one"}}},
		inspects:   map[string]container.InspectResponse{"one": inspect},
	}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	controller, err := runcontrol.New(runcontrol.Limits{MaxItems: 2})
	if err != nil {
		t.Fatal(err)
	}
	ctx := runcontrol.WithController(context.Background(), controller)
	outputDir := filepath.Join(t.TempDir(), "backup-output")
	_, err = backupContainers(ctx, []string{"one"}, BackupOptions{OutputDir: outputDir})
	if err == nil || !strings.Contains(err.Error(), "backup-volume item budget exceeded") {
		t.Fatalf("backupContainers() error = %v, want related-resource max-items rejection", err)
	}
	if _, statErr := os.Stat(outputDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("backup output exists after max-items rejection: %v", statErr)
	}
	if hasCallPrefix(fake.calls, "save-image:") || hasCallPrefix(fake.calls, "inspect-network:") || hasCallPrefix(fake.calls, "inspect-volume:") {
		t.Fatalf("calls = %#v, backup output work started after max-items rejection", fake.calls)
	}
}

func TestBackupItemBudgetDeduplicatesSharedResources(t *testing.T) {
	fake := &fakeBackupDockerService{
		containers: []container.Summary{
			{ID: "one-id", Names: []string{"/one"}},
			{ID: "two-id", Names: []string{"/two"}},
		},
		inspects: map[string]container.InspectResponse{
			"one": budgetContainerInspect("one"),
			"two": budgetContainerInspect("two"),
		},
	}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	controller, err := runcontrol.New(runcontrol.Limits{MaxItems: 4})
	if err != nil {
		t.Fatal(err)
	}
	ctx := runcontrol.WithController(context.Background(), controller)
	if _, err := backupContainers(ctx, []string{"one", "two"}, BackupOptions{
		OutputDir: filepath.Join(t.TempDir(), "backup-output"),
		DryRun:    true,
		Output:    &bytes.Buffer{},
	}); err != nil {
		t.Fatalf("backupContainers() error = %v, want shared network and volume to count once", err)
	}
	if got := controller.ItemsUsed(); got != 4 {
		t.Fatalf("ItemsUsed() = %d, want 2 containers + 1 network + 1 volume", got)
	}
}

func TestBackupRejectsReservedInspectDriftBeforeOutput(t *testing.T) {
	tests := []struct {
		name    string
		initial container.InspectResponse
		current container.InspectResponse
		want    string
	}{
		{
			name:    "container identity",
			initial: budgetContainerInspectWithID("one", "one-id"),
			current: budgetContainerInspectWithID("one", "replacement-id"),
			want:    "identity changed",
		},
		{
			name:    "related resources",
			initial: budgetContainerInspectWithID("one", "one-id"),
			current: budgetContainerInspectWithResources("one", "one-id", "other-net", "other-volume"),
			want:    "network or volume set changed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := &fakeBackupDockerService{
				containers: []container.Summary{{ID: "one-id", Names: []string{"/one"}}},
			}
			fake := &driftingBackupDockerService{
				fakeBackupDockerService: base,
				inspects:                []container.InspectResponse{test.initial, test.current},
			}
			previousFactory := newBackupDockerService
			newBackupDockerService = func() (backupDockerService, error) { return fake, nil }
			defer func() { newBackupDockerService = previousFactory }()

			controller, err := runcontrol.New(runcontrol.Limits{MaxItems: 3})
			if err != nil {
				t.Fatal(err)
			}
			ctx := runcontrol.WithController(context.Background(), controller)
			outputDir := filepath.Join(t.TempDir(), "backup-output")
			_, err = backupContainers(ctx, []string{"one"}, BackupOptions{OutputDir: outputDir})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("backupContainers() error = %v, want %q", err, test.want)
			}
			if _, statErr := os.Stat(outputDir); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("backup output exists after inspect drift: %v", statErr)
			}
			if hasCallPrefix(base.calls, "save-image:") || hasCallPrefix(base.calls, "inspect-network:") || hasCallPrefix(base.calls, "inspect-volume:") {
				t.Fatalf("calls = %#v, output work started after inspect drift", base.calls)
			}
		})
	}
}

func TestRestoreRejectsMaxItemsBeforeDockerAccess(t *testing.T) {
	dir := t.TempDir()
	entries := make([]BackupContainerManifest, 0, 2)
	for _, name := range []string{"one", "two"} {
		entries = append(entries, BackupContainerManifest{
			ContainerName: name,
			SourceName:    name,
			Path:          name,
			Image:         "busybox:latest",
			InspectFile:   backupInspectName,
		})
		writeTestJSON(t, filepath.Join(dir, name, backupInspectName), basicRestoreInspect(name))
	}
	writeTestJSON(t, filepath.Join(dir, backupManifestName), BackupManifest{Version: 1, Containers: entries})

	for _, test := range []struct {
		name string
		run  func(context.Context) error
	}{
		{name: "text plan", run: func(ctx context.Context) error {
			return restoreBackup(ctx, dir, RestoreOptions{DryRun: true, SkipChecksum: true})
		}},
		{name: "structured plan", run: func(ctx context.Context) error {
			_, err := buildRestorePlanReport(ctx, dir, RestoreOptions{DryRun: true, SkipChecksum: true})
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeBackupDockerService{}
			restoreFactory := replaceBackupServiceFactory(fake)
			defer restoreFactory()
			controller, err := runcontrol.New(runcontrol.Limits{MaxItems: 1})
			if err != nil {
				t.Fatal(err)
			}
			ctx := runcontrol.WithController(context.Background(), controller)
			err = test.run(ctx)
			if err == nil || !strings.Contains(err.Error(), "item budget exceeded") {
				t.Fatalf("restore error = %v, want max-items rejection", err)
			}
			if len(fake.calls) != 0 {
				t.Fatalf("calls = %#v, Docker access started after max-items rejection", fake.calls)
			}
		})
	}
}

func TestRestorePathsRejectRelatedResourceBudgetBeforeDockerAccess(t *testing.T) {
	dir := writeBudgetRestoreFixture(t, "one")

	tests := []struct {
		name string
		run  func(context.Context, *bytes.Buffer) error
	}{
		{name: "restore preflight", run: func(ctx context.Context, output *bytes.Buffer) error {
			return restoreBackup(ctx, dir, RestoreOptions{Confirm: true, NoStart: true, SkipChecksum: true, Output: output})
		}},
		{name: "restore directory", run: func(ctx context.Context, output *bytes.Buffer) error {
			return restoreBackupDir(ctx, dir, RestoreOptions{Confirm: true, NoStart: true, SkipChecksum: true, Output: output})
		}},
		{name: "structured plan", run: func(ctx context.Context, _ *bytes.Buffer) error {
			_, err := buildRestorePlanReport(ctx, dir, RestoreOptions{DryRun: true, SkipChecksum: true})
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeBackupDockerService{}
			restoreFactory := replaceBackupServiceFactory(fake)
			defer restoreFactory()
			controller, err := runcontrol.New(runcontrol.Limits{MaxItems: 2})
			if err != nil {
				t.Fatal(err)
			}
			ctx := runcontrol.WithController(context.Background(), controller)
			var output bytes.Buffer
			err = test.run(ctx, &output)
			if err == nil || !strings.Contains(err.Error(), "restore-volume item budget exceeded") {
				t.Fatalf("restore error = %v, want related-resource max-items rejection", err)
			}
			if len(fake.calls) != 0 {
				t.Fatalf("calls = %#v, Docker access started after max-items rejection", fake.calls)
			}
			if output.Len() != 0 {
				t.Fatalf("output = %q, restore wrote output before max-items rejection", output.String())
			}
		})
	}
}

func TestPreparedRestoreBudgetDeduplicatesResourcesAndIsReservedOnce(t *testing.T) {
	dir := writeBudgetRestoreFixture(t, "one", "two")
	fake := &fakeBackupDockerService{}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()
	controller, err := runcontrol.New(runcontrol.Limits{MaxItems: 4})
	if err != nil {
		t.Fatal(err)
	}
	ctx := runcontrol.WithController(context.Background(), controller)
	opts := RestoreOptions{DryRun: true, SkipChecksum: true, Output: &bytes.Buffer{}}
	prepared, err := prepareRestoreBackup(ctx, dir, opts)
	if err != nil {
		t.Fatalf("prepareRestoreBackup() error = %v", err)
	}
	defer prepared.Close()
	if got := controller.ItemsUsed(); got != 4 {
		t.Fatalf("ItemsUsed() after preflight = %d, want 2 containers + 1 network + 1 volume", got)
	}
	if err := executePreparedRestore(ctx, prepared, opts); err != nil {
		t.Fatalf("executePreparedRestore() error = %v", err)
	}
	if got := controller.ItemsUsed(); got != 4 {
		t.Fatalf("ItemsUsed() after execute = %d, want prepared resources reserved only once", got)
	}
}

func TestStructuredRestorePlanDeduplicatesSharedResources(t *testing.T) {
	dir := writeBudgetRestoreFixture(t, "one", "two")
	fake := &fakeBackupDockerService{
		network: network.Inspect{Network: network.Network{Name: "shared-net"}},
		volume:  volume.Volume{Name: "shared-volume"},
	}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()
	controller, err := runcontrol.New(runcontrol.Limits{MaxItems: 4})
	if err != nil {
		t.Fatal(err)
	}
	ctx := runcontrol.WithController(context.Background(), controller)
	if _, err := buildRestorePlanReport(ctx, dir, RestoreOptions{DryRun: true, SkipChecksum: true}); err != nil {
		t.Fatalf("buildRestorePlanReport() error = %v", err)
	}
	if got := controller.ItemsUsed(); got != 4 {
		t.Fatalf("ItemsUsed() = %d, want 2 containers + 1 network + 1 volume", got)
	}
}

func TestPreparedStructuredRestorePlanUsesBudgetedManifestSnapshot(t *testing.T) {
	dir := writeBudgetRestoreFixture(t, "one")
	fake := &fakeBackupDockerService{}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()
	controller, err := runcontrol.New(runcontrol.Limits{MaxItems: 3})
	if err != nil {
		t.Fatal(err)
	}
	ctx := runcontrol.WithController(context.Background(), controller)
	opts := RestoreOptions{DryRun: true, SkipChecksum: true, Output: &bytes.Buffer{}}
	prepared, err := prepareRestoreBackup(ctx, dir, opts)
	if err != nil {
		t.Fatalf("prepareRestoreBackup() error = %v", err)
	}
	defer prepared.Close()

	mutatedManifest := prepared.manifest
	extra := mutatedManifest.Containers[0]
	extra.ContainerName = "two"
	extra.SourceName = "two"
	mutatedManifest.Containers = append(mutatedManifest.Containers, extra)
	writeTestJSON(t, filepath.Join(dir, backupManifestName), mutatedManifest)

	report, err := buildRestorePlanReportFromPrepared(ctx, prepared, "skipped", opts)
	if err != nil {
		t.Fatalf("buildRestorePlanReportFromPrepared() error = %v", err)
	}
	if report.ContainerCount != 1 || len(report.Containers) != 1 {
		t.Fatalf("report containers = %d/%d, want prepared snapshot with one container", report.ContainerCount, len(report.Containers))
	}
	if got := controller.ItemsUsed(); got != 3 {
		t.Fatalf("ItemsUsed() = %d, want one prepared container + one network + one volume", got)
	}
}

func TestRestoreCommandDeduplicatesSharedResourcesAcrossInputs(t *testing.T) {
	first := writeBudgetRestoreFixture(t, "one")
	second := writeBudgetRestoreFixture(t, "two")
	fake := &fakeBackupDockerService{}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()
	controller, err := runcontrol.New(runcontrol.Limits{MaxItems: 4})
	if err != nil {
		t.Fatal(err)
	}
	ctx := runcontrol.WithController(context.Background(), controller)
	cmd := NewRestoreCommand()
	cmd.SetContext(ctx)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{first, second, "--skip-checksum"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("restore command error = %v, want shared resources counted once across inputs", err)
	}
	if got := controller.ItemsUsed(); got != 4 {
		t.Fatalf("ItemsUsed() = %d, want 2 containers + 1 network + 1 volume", got)
	}
}

func TestRestoreCommandRejectsLaterInputBudgetBeforeDryRunOutputOrDocker(t *testing.T) {
	first := writeBudgetRestoreFixture(t, "one")
	second := writeBudgetRestoreFixture(t, "two")
	for _, test := range []struct {
		name      string
		extraArgs []string
	}{
		{name: "text"},
		{name: "structured", extraArgs: []string{"--format", "json"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeBackupDockerService{}
			restoreFactory := replaceBackupServiceFactory(fake)
			defer restoreFactory()
			controller, err := runcontrol.New(runcontrol.Limits{MaxItems: 3})
			if err != nil {
				t.Fatal(err)
			}
			ctx := runcontrol.WithController(context.Background(), controller)
			cmd := NewRestoreCommand()
			var output bytes.Buffer
			cmd.SilenceUsage = true
			cmd.SetContext(ctx)
			cmd.SetOut(&output)
			cmd.SetErr(&bytes.Buffer{})
			args := []string{first, second, "--skip-checksum"}
			args = append(args, test.extraArgs...)
			cmd.SetArgs(args)
			err = cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), "item budget exceeded") {
				t.Fatalf("restore command error = %v, want second-input max-items rejection", err)
			}
			if output.Len() != 0 {
				t.Fatalf("stdout = %q, want no dry-run output before all inputs reserve budget", output.String())
			}
			if len(fake.calls) != 0 {
				t.Fatalf("Docker calls = %#v, want none before all inputs reserve budget", fake.calls)
			}
		})
	}
}

func budgetContainerInspect(name string) container.InspectResponse {
	return budgetContainerInspectWithResources(name, "", "shared-net", "shared-volume")
}

func budgetContainerInspectWithID(name, id string) container.InspectResponse {
	return budgetContainerInspectWithResources(name, id, "shared-net", "shared-volume")
}

func budgetContainerInspectWithResources(name, id, networkName, volumeName string) container.InspectResponse {
	return container.InspectResponse{
		ID:         id,
		Name:       "/" + name,
		Config:     &container.Config{Image: "busybox:latest"},
		HostConfig: &container.HostConfig{},
		Mounts: []container.MountPoint{
			{Type: mount.TypeVolume, Name: volumeName, Destination: "/data"},
		},
		NetworkSettings: &container.NetworkSettings{
			Networks: map[string]*network.EndpointSettings{networkName: {}},
		},
	}
}

type driftingBackupDockerService struct {
	*fakeBackupDockerService
	mu       sync.Mutex
	inspects []container.InspectResponse
	next     int
}

func (fake *driftingBackupDockerService) InspectContainer(_ context.Context, name string) (container.InspectResponse, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.fakeBackupDockerService.mu.Lock()
	fake.fakeBackupDockerService.calls = append(fake.fakeBackupDockerService.calls, "inspect-container:"+name)
	fake.fakeBackupDockerService.mu.Unlock()
	if len(fake.inspects) == 0 {
		return container.InspectResponse{}, errors.New("missing drifting inspect response")
	}
	index := fake.next
	if index >= len(fake.inspects) {
		index = len(fake.inspects) - 1
	}
	fake.next++
	return fake.inspects[index], nil
}

func writeBudgetRestoreFixture(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	manifest := BackupManifest{Version: 1}
	for _, name := range names {
		entry := BackupContainerManifest{
			ContainerName: name,
			SourceName:    name,
			Path:          name,
			Image:         "busybox:latest",
			InspectFile:   backupInspectName,
			Networks: []BackupResourceRef{
				{Name: "shared-net", File: filepath.ToSlash(filepath.Join("networks", "shared-net.json"))},
			},
			Volumes: []BackupResourceRef{
				{Name: "shared-volume", File: filepath.ToSlash(filepath.Join("volumes", "shared-volume.json"))},
			},
		}
		manifest.Containers = append(manifest.Containers, entry)
		entryDir := filepath.Join(dir, name)
		writeTestJSON(t, filepath.Join(entryDir, backupInspectName), budgetContainerInspect(name))
		writeTestJSON(t, filepath.Join(entryDir, "networks", "shared-net.json"), network.Inspect{Network: network.Network{Name: "shared-net"}})
		writeTestJSON(t, filepath.Join(entryDir, "volumes", "shared-volume.json"), volume.Volume{Name: "shared-volume"})
	}
	writeTestJSON(t, filepath.Join(dir, backupManifestName), manifest)
	return dir
}
