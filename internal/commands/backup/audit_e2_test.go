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
	"time"

	"docker-manager/internal/audit"

	"github.com/moby/moby/api/types/container"
)

type backupAuditSink struct {
	mu     sync.Mutex
	events []audit.Event
	err    error
}

func (sink *backupAuditSink) Append(_ context.Context, event audit.Event) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.err != nil {
		return sink.err
	}
	sink.events = append(sink.events, event)
	return nil
}

func (sink *backupAuditSink) snapshot() []audit.Event {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]audit.Event(nil), sink.events...)
}

func newBackupAuditSession(t *testing.T, sink audit.Sink, policy audit.FailurePolicy) *audit.Session {
	t.Helper()
	session, err := audit.NewSession(audit.SessionOptions{
		Sink:          sink,
		Detail:        audit.DetailFull,
		FailurePolicy: policy,
		Operation:     "backup",
		Command:       "dm backup",
		Endpoint:      "unix:///var/run/docker.sock",
		IdentifierKey: bytes.Repeat([]byte{0x71}, 32),
		Random:        bytes.NewReader(bytes.Repeat([]byte{0x72}, 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func newBackupAuditFake() *fakeBackupDockerService {
	return &fakeBackupDockerService{
		inspect: container.InspectResponse{
			Name:       "/demo",
			Config:     &container.Config{Image: "busybox:latest"},
			HostConfig: &container.HostConfig{},
		},
		containers: []container.Summary{{ID: "demo-id", Names: []string{"/demo"}}},
	}
}

func TestBackupContainersAuditsBeforeFileAndImageWrites(t *testing.T) {
	fake := newBackupAuditFake()
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()
	sink := &backupAuditSink{}
	session := newBackupAuditSession(t, sink, audit.FailureRequired)
	outputDir := filepath.Join(t.TempDir(), "backup")
	result, err := backupContainers(audit.WithSession(context.Background(), session), []string{"demo"}, BackupOptions{
		OutputDir:    outputDir,
		IncludeImage: true,
	})
	if err != nil {
		t.Fatalf("backupContainers() error = %v", err)
	}
	if len(result.Paths) != 1 {
		t.Fatalf("result paths = %#v, want one path", result.Paths)
	}
	if !hasCall(fake.calls, "save-image:busybox:latest") {
		t.Fatalf("calls = %#v, want image save", fake.calls)
	}
	if _, err := os.Stat(filepath.Join(outputDir, backupManifestName)); err != nil {
		t.Fatalf("backup manifest missing: %v", err)
	}
	events := sink.snapshot()
	if len(events) != 3 || events[1].Type != audit.EventMutationCandidates || events[2].Type != audit.EventMutationAuthorized {
		t.Fatalf("audit events = %#v, want start/candidates/authorized", backupAuditEventTypes(events))
	}
	if events[2].Mutation == nil || events[2].Mutation.Scope != audit.MutationFilesystem {
		t.Fatalf("authorized mutation = %#v, want filesystem", events[2].Mutation)
	}
	if len(events[1].Candidates) != 2 || events[1].Candidates[0].Kind != "backup-directory" || events[1].Candidates[0].Display != outputDir {
		t.Fatalf("candidates = %#v, want actual backup output directory", events[1].Candidates)
	}
	imageDir := filepath.Join(outputDir, "images")
	if events[1].Candidates[1].Kind != "image-archive" || events[1].Candidates[1].Display != imageDir {
		t.Fatalf("candidates = %#v, want actual image archive directory %q", events[1].Candidates, imageDir)
	}
}

func TestBackupMutationCandidatesIncludeResolvedBundleAndSplitPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "backup")
	passphrase := filepath.Join(t.TempDir(), "passphrase.txt")
	if err := os.WriteFile(passphrase, []byte("correct horse battery staple\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts, err := resolveBackupOutputOptions([]string{"demo"}, BackupOptions{
		OutputDir:      root,
		IncludeImage:   true,
		Bundle:         true,
		BundleOutput:   filepath.Join(t.TempDir(), "portable.tar.gz"),
		Encrypt:        true,
		PassphraseFile: passphrase,
		SplitSize:      "1M",
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := backupMutationCandidates([]string{"demo"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 3 {
		t.Fatalf("candidates = %#v, want directory, image archive, and bundle", candidates)
	}
	archiveOpts, archivePath, err := resolveBackupBundleOptions(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	wantPart := backupPublishedArchivePath(archivePath, archiveOpts)
	if candidates[2].Kind != "backup-archive" || candidates[2].Identifier != wantPart || candidates[2].Display != wantPart {
		t.Fatalf("bundle candidate = %#v, want first published split path %q", candidates[2], wantPart)
	}
}

func TestBackupContainersAuditFailurePreventsAnyOutputOrImageSave(t *testing.T) {
	fake := newBackupAuditFake()
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()
	sink := &backupAuditSink{err: errors.New("audit unavailable")}
	session := newBackupAuditSession(t, sink, audit.FailureDenyMutation)
	outputDir := filepath.Join(t.TempDir(), "backup")
	err := func() error {
		_, callErr := backupContainers(audit.WithSession(context.Background(), session), []string{"demo"}, BackupOptions{
			OutputDir:    outputDir,
			IncludeImage: true,
		})
		return callErr
	}()
	if err == nil {
		t.Fatal("backupContainers() error = nil, want audit authorization failure")
	}
	if hasCallPrefix(fake.calls, "inspect-container:") || hasCallPrefix(fake.calls, "save-image:") {
		t.Fatalf("calls = %#v, Docker/file work started before denied audit", fake.calls)
	}
	if _, statErr := os.Stat(outputDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("output directory stat = %v, want no output directory", statErr)
	}
	if !strings.Contains(err.Error(), "审计授权失败") {
		t.Fatalf("error = %q, want audit context", err.Error())
	}
}

func TestBackupContainersDryRunSkipsMutationAudit(t *testing.T) {
	fake := newBackupAuditFake()
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()
	sink := &backupAuditSink{}
	session := newBackupAuditSession(t, sink, audit.FailureRequired)
	outputDir := filepath.Join(t.TempDir(), "backup")
	if _, err := backupContainers(audit.WithSession(context.Background(), session), []string{"demo"}, BackupOptions{
		OutputDir:    outputDir,
		IncludeImage: true,
		DryRun:       true,
	}); err != nil {
		t.Fatalf("backupContainers() dry-run error = %v", err)
	}
	if len(sink.snapshot()) != 0 {
		t.Fatal("dry-run emitted mutation audit events")
	}
	if hasCallPrefix(fake.calls, "save-image:") {
		t.Fatalf("calls = %#v, dry-run must not save image", fake.calls)
	}
	if _, err := os.Stat(outputDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry-run output directory stat = %v, want absent", err)
	}
}

func TestRestoreMutationCandidatesCoverImageNetworkVolumeAndContainer(t *testing.T) {
	dir := t.TempDir()
	prepared := &preparedRestoreBackup{
		dir:             dir,
		targets:         []string{"demo"},
		existingTargets: map[string]string{"demo": "old-id"},
		manifest: BackupManifest{Containers: []BackupContainerManifest{{
			ContainerName: "demo",
			ImageArchive:  "images/demo.tar",
			Networks:      []BackupResourceRef{{Name: "demo-net"}},
			Volumes:       []BackupResourceRef{{Name: "demo-data"}},
		}}},
	}
	candidates, err := restoreMutationCandidates([]*preparedRestoreBackup{prepared})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 4 {
		t.Fatalf("candidates = %#v, want image, network, volume, and container", candidates)
	}
	want := []struct {
		kind   string
		action string
		id     string
	}{
		{kind: "image", action: "load", id: filepath.Join(dir, "images", "demo.tar")},
		{kind: "network", action: "create", id: "demo-net"},
		{kind: "volume", action: "create", id: "demo-data"},
		{kind: "container", action: "replace", id: "demo"},
	}
	for index, expected := range want {
		candidate := candidates[index]
		if candidate.Kind != expected.kind || candidate.Action != expected.action || candidate.Identifier != expected.id {
			t.Fatalf("candidate %d = %#v, want %#v", index, candidate, expected)
		}
	}
}

func TestRestoreCommandAuditFailurePreventsDockerMutation(t *testing.T) {
	dir := writeRestoreFixture(t, container.InspectResponse{
		Name:       "/demo",
		Config:     &container.Config{Image: "busybox:latest"},
		HostConfig: &container.HostConfig{},
	})
	fake := &fakeBackupDockerService{}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	sink := &backupAuditSink{err: errors.New("audit unavailable")}
	session := newBackupAuditSession(t, sink, audit.FailureDenyMutation)
	cmd := NewRestoreCommand()
	cmd.SetContext(audit.WithSession(context.Background(), session))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{dir, "--confirm", "--skip-checksum", "--no-start"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "审计授权失败") {
		t.Fatalf("Execute() error = %v, want audit authorization failure", err)
	}
	for _, prefix := range []string{
		"load-image:", "create-network:", "create-volume:", "create-container:",
		"stop-container:", "rename-container:", "remove-container:", "start-container:",
	} {
		if hasCallPrefix(fake.calls, prefix) {
			t.Fatalf("calls = %#v, mutation %q started after denied audit", fake.calls, prefix)
		}
	}
}

func backupAuditEventTypes(events []audit.Event) []audit.EventType {
	result := make([]audit.EventType, len(events))
	for index, event := range events {
		result[index] = event.Type
	}
	return result
}
