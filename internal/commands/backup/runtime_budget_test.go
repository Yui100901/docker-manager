package backup

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"docker-manager/internal/runcontrol"

	"github.com/moby/moby/api/types/container"
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
