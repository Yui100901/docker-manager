package backup

import (
	"bytes"
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/volume"
)

func TestVerifyBackupChecksumsRequiresExactCanonicalSet(t *testing.T) {
	const zeroSum = "0000000000000000000000000000000000000000000000000000000000000000"
	tests := []struct {
		name  string
		setup func(t *testing.T, dir, sum string)
		want  string
	}{
		{
			name: "empty",
			setup: func(t *testing.T, dir, sum string) {
				writeTestFile(t, filepath.Join(dir, backupChecksumName), "\n")
			},
			want: "does not cover any files",
		},
		{
			name: "duplicate",
			setup: func(t *testing.T, dir, sum string) {
				line := sum + "  payload.txt\n"
				writeTestFile(t, filepath.Join(dir, backupChecksumName), line+line)
			},
			want: "duplicate checksum path",
		},
		{
			name: "path alias",
			setup: func(t *testing.T, dir, sum string) {
				writeTestFile(t, filepath.Join(dir, backupChecksumName), sum+"  .\\payload.txt\n")
			},
			want: "not canonical",
		},
		{
			name: "self reference",
			setup: func(t *testing.T, dir, sum string) {
				writeTestFile(t, filepath.Join(dir, backupChecksumName), zeroSum+"  "+backupChecksumName+"\n")
			},
			want: "must not contain itself",
		},
		{
			name: "unlisted extra file",
			setup: func(t *testing.T, dir, sum string) {
				writeTestFile(t, filepath.Join(dir, backupChecksumName), sum+"  payload.txt\n")
				writeTestFile(t, filepath.Join(dir, "extra.txt"), "extra")
			},
			want: "missing from checksums.txt",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			payload := filepath.Join(dir, "payload.txt")
			writeTestFile(t, payload, "payload")
			sum, err := fileSHA256(payload)
			if err != nil {
				t.Fatal(err)
			}
			tt.setup(t, dir, sum)
			_, err = verifyBackupChecksums(dir)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("verifyBackupChecksums() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestConfirmedRestoreRejectsMissingChecksumUnlessExplicitlySkipped(t *testing.T) {
	dir := writeRestoreFixture(t, container.InspectResponse{
		Name:       "/demo",
		Config:     &container.Config{Image: "busybox:latest"},
		HostConfig: &container.HostConfig{},
	})
	fake := &fakeBackupDockerService{}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	err := restoreBackup(context.Background(), dir, RestoreOptions{Confirm: true, NoStart: true})
	if err == nil || !strings.Contains(err.Error(), "checksums.txt is required") {
		t.Fatalf("restoreBackup() error = %v, want missing checksum rejection", err)
	}
	assertNoRestoreMutations(t, fake.calls)

	fake.calls = nil
	if err := restoreBackup(context.Background(), dir, RestoreOptions{Confirm: true, NoStart: true, SkipChecksum: true}); err != nil {
		t.Fatalf("restoreBackup(skip checksum) error = %v", err)
	}
	assertRestoreCandidateCommitted(t, fake.calls, "demo")
}

func TestPrepareRestoreBackupCleansSnapshotAfterPreflightFailure(t *testing.T) {
	source := t.TempDir()
	tempRoot := t.TempDir()
	t.Setenv("TMPDIR", tempRoot)
	t.Setenv("TMP", tempRoot)
	t.Setenv("TEMP", tempRoot)
	if got := filepath.Clean(os.TempDir()); got != filepath.Clean(tempRoot) {
		t.Fatalf("os.TempDir() = %q, want isolated test directory %q", got, tempRoot)
	}

	prepared, err := prepareRestoreBackup(context.Background(), source, RestoreOptions{Confirm: true})
	if err == nil || !strings.Contains(err.Error(), "checksums.txt is required") {
		t.Fatalf("prepareRestoreBackup() error = %v, want missing checksum rejection", err)
	}
	if prepared != nil {
		t.Fatalf("prepareRestoreBackup() prepared = %#v, want nil after preflight failure", prepared)
	}
	entries, err := os.ReadDir(tempRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary restore entries after preflight failure = %v, want none", entries)
	}
}

func TestRestoreCommandRejectsConfirmWithDryRun(t *testing.T) {
	cmd := NewRestoreCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"unused", "--confirm", "--dry-run"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("Execute() error = %v, want conflicting flags", err)
	}
}

func TestRestoreRejectsMissingReferencedArtifactsBeforeDocker(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{
		{
			name: "manifest",
			setup: func(t *testing.T, dir string) {
				writeTestJSON(t, filepath.Join(dir, backupInspectName), basicRestoreInspect("demo"))
			},
		},
		{
			name: "inspect",
			setup: func(t *testing.T, dir string) {
				writeTestJSON(t, filepath.Join(dir, backupManifestName), basicRestoreManifest("demo"))
			},
		},
		{
			name: "image archive",
			setup: func(t *testing.T, dir string) {
				manifest := basicRestoreManifest("demo")
				manifest.ImageArchive = "images/missing.tar"
				writeTestJSON(t, filepath.Join(dir, backupManifestName), manifest)
				writeTestJSON(t, filepath.Join(dir, backupInspectName), basicRestoreInspect("demo"))
			},
		},
		{
			name: "network metadata",
			setup: func(t *testing.T, dir string) {
				manifest := basicRestoreManifest("demo")
				manifest.Networks = []BackupResourceRef{{Name: "demo_net", File: "networks/missing.json"}}
				writeTestJSON(t, filepath.Join(dir, backupManifestName), manifest)
				inspect := basicRestoreInspect("demo")
				inspect.NetworkSettings = &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{"demo_net": {}}}
				writeTestJSON(t, filepath.Join(dir, backupInspectName), inspect)
			},
		},
		{
			name: "volume metadata",
			setup: func(t *testing.T, dir string) {
				manifest := basicRestoreManifest("demo")
				manifest.Volumes = []BackupResourceRef{{Name: "demo_data", File: "volumes/missing.json"}}
				writeTestJSON(t, filepath.Join(dir, backupManifestName), manifest)
				writeTestJSON(t, filepath.Join(dir, backupInspectName), basicRestoreInspect("demo"))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.setup(t, dir)
			if err := writeChecksums(dir); err != nil {
				t.Fatal(err)
			}
			fake := &fakeBackupDockerService{}
			restoreFactory := replaceBackupServiceFactory(fake)
			defer restoreFactory()
			if err := restoreBackup(context.Background(), dir, RestoreOptions{Confirm: true, NoStart: true}); err == nil {
				t.Fatal("restoreBackup() error = nil, want missing artifact rejection")
			}
			if len(fake.calls) != 0 {
				t.Fatalf("calls = %#v, missing artifacts must fail before Docker access", fake.calls)
			}
		})
	}
}

func TestRestoreBatchPreflightRejectsSecondEntryBeforeMutations(t *testing.T) {
	for _, testName := range []string{"dangerous", "corrupt"} {
		t.Run(testName, func(t *testing.T) {
			dir := t.TempDir()
			entries := []BackupContainerManifest{
				{ContainerName: "first", Path: "containers/first", InspectFile: backupInspectName},
				{ContainerName: "second", Path: "containers/second", InspectFile: backupInspectName},
			}
			writeTestJSON(t, filepath.Join(dir, backupManifestName), BackupManifest{Version: 1, Containers: entries})
			writeTestJSON(t, filepath.Join(dir, "containers", "first", backupInspectName), basicRestoreInspect("first"))
			secondPath := filepath.Join(dir, "containers", "second", backupInspectName)
			if testName == "dangerous" {
				inspect := basicRestoreInspect("second")
				inspect.HostConfig.Privileged = true
				writeTestJSON(t, secondPath, inspect)
			} else {
				writeTestFile(t, secondPath, "{broken")
			}
			fake := &fakeBackupDockerService{}
			restoreFactory := replaceBackupServiceFactory(fake)
			defer restoreFactory()
			err := restoreBackup(context.Background(), dir, RestoreOptions{Confirm: true, NoStart: true, SkipChecksum: true})
			if err == nil {
				t.Fatal("restoreBackup() error = nil, want second entry preflight failure")
			}
			assertNoRestoreMutations(t, fake.calls)
		})
	}
}

func TestRestoreMultipleSourcesSignatureFailurePreventsFirstExecution(t *testing.T) {
	keyDir := t.TempDir()
	trustedPublic, trustedPrivate := writeTestEd25519KeyPair(t, keyDir, "trusted")
	_, wrongPrivate := writeTestEd25519KeyPair(t, keyDir, "wrong")
	first := writeRestoreFixture(t, basicRestoreInspect("first"))
	second := writeRestoreFixture(t, basicRestoreInspect("second"))
	for _, dir := range []string{first, second} {
		if err := writeChecksums(dir); err != nil {
			t.Fatal(err)
		}
	}
	if err := signBackupChecksumsWithContext(context.Background(), first, trustedPrivate); err != nil {
		t.Fatal(err)
	}
	if err := signBackupChecksumsWithContext(context.Background(), second, wrongPrivate); err != nil {
		t.Fatal(err)
	}
	fake := &fakeBackupDockerService{}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	cmd := NewRestoreCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{first, second, "--confirm", "--no-start", "--trusted-public-key", trustedPublic})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("Execute() error = %v, want second signature failure", err)
	}
	assertNoRestoreMutations(t, fake.calls)
}

func TestRestoreRejectsDangerousLocalVolumeOptions(t *testing.T) {
	dir := t.TempDir()
	ref := BackupResourceRef{Name: "demo_data", File: "volumes/demo_data.json"}
	manifest := basicRestoreManifest("demo")
	manifest.Volumes = []BackupResourceRef{ref}
	inspect := basicRestoreInspect("demo")
	inspect.HostConfig.Binds = []string{"demo_data:/data"}
	metadata := volume.Volume{Name: "demo_data", Driver: "local", Options: map[string]string{"type": "none", "o": "bind", "device": "/etc"}}
	writeTestJSON(t, filepath.Join(dir, backupManifestName), manifest)
	writeTestJSON(t, filepath.Join(dir, backupInspectName), inspect)
	writeTestJSON(t, filepath.Join(dir, filepath.FromSlash(ref.File)), metadata)
	fake := &fakeBackupDockerService{volume: metadata}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	err := restoreBackup(context.Background(), dir, RestoreOptions{Confirm: true, NoStart: true, SkipChecksum: true})
	if err == nil || !strings.Contains(err.Error(), "host mount/driver options") {
		t.Fatalf("restoreBackup() error = %v, want dangerous local volume rejection", err)
	}
	assertNoRestoreMutations(t, fake.calls)

	fake.calls = nil
	if err := restoreBackup(context.Background(), dir, RestoreOptions{Confirm: true, NoStart: true, SkipChecksum: true, AllowUnsafeHostConfig: true}); err != nil {
		t.Fatalf("explicitly allowed restore error = %v", err)
	}
	if !hasCall(fake.calls, "create-volume:demo_data") {
		t.Fatalf("calls = %#v, want volume restore", fake.calls)
	}
}

func TestRestoreRejectsUnsafeSystemPathOverridesBeforeDocker(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*container.HostConfig)
		wantReason string
	}{
		{
			name: "empty masked paths",
			configure: func(host *container.HostConfig) {
				host.MaskedPaths = []string{}
			},
			wantReason: "masked system paths missing default protections",
		},
		{
			name: "empty read-only paths",
			configure: func(host *container.HostConfig) {
				host.ReadonlyPaths = []string{}
			},
			wantReason: "read-only system paths missing default protections",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inspect := basicRestoreInspect("demo")
			tt.configure(inspect.HostConfig)
			dir := writeRestoreFixture(t, inspect)
			fake := &fakeBackupDockerService{}
			restoreFactory := replaceBackupServiceFactory(fake)
			defer restoreFactory()

			err := restoreBackup(context.Background(), dir, RestoreOptions{Confirm: true, NoStart: true, SkipChecksum: true})
			if err == nil || !strings.Contains(err.Error(), tt.wantReason) {
				t.Fatalf("restoreBackup() error = %v, want %q", err, tt.wantReason)
			}
			assertNoRestoreMutations(t, fake.calls)
		})
	}
}

func TestUnsafeRestoreSystemPathOverrides(t *testing.T) {
	t.Run("nil uses daemon defaults", func(t *testing.T) {
		if risks := unsafeRestoreHostConfig(basicRestoreInspect("demo")); len(risks) != 0 {
			t.Fatalf("unsafeRestoreHostConfig() = %#v, want no risks", risks)
		}
	})

	t.Run("defaults and stricter superset are safe", func(t *testing.T) {
		inspect := basicRestoreInspect("demo")
		inspect.HostConfig.MaskedPaths = append([]string(nil), restoreDefaultMaskedPaths...)
		inspect.HostConfig.MaskedPaths = append(inspect.HostConfig.MaskedPaths, restoreDefaultReadonlyPaths[0], "/extra/masked")
		inspect.HostConfig.ReadonlyPaths = append([]string(nil), restoreDefaultReadonlyPaths[1:]...)
		inspect.HostConfig.ReadonlyPaths = append(inspect.HostConfig.ReadonlyPaths, "/extra/read-only")
		if risks := unsafeRestoreHostConfig(inspect); len(risks) != 0 {
			t.Fatalf("unsafeRestoreHostConfig() = %#v, want no risks", risks)
		}
	})

	t.Run("masked path cannot be downgraded", func(t *testing.T) {
		inspect := basicRestoreInspect("demo")
		inspect.HostConfig.MaskedPaths = append([]string(nil), restoreDefaultMaskedPaths[1:]...)
		inspect.HostConfig.ReadonlyPaths = append([]string(nil), restoreDefaultReadonlyPaths...)
		inspect.HostConfig.ReadonlyPaths = append(inspect.HostConfig.ReadonlyPaths, restoreDefaultMaskedPaths[0])
		risks := strings.Join(unsafeRestoreHostConfig(inspect), "\n")
		if !strings.Contains(risks, "downgraded to read-only") {
			t.Fatalf("unsafeRestoreHostConfig() = %q, want downgrade risk", risks)
		}
	})
}

func TestRestorePreflightDetectsWildcardPortConflict(t *testing.T) {
	inspect := basicRestoreInspect("demo")
	inspect.HostConfig.PortBindings = network.PortMap{
		network.MustParsePort("80/tcp"): {{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: "8080"}},
	}
	dir := writeRestoreFixture(t, inspect)
	fake := &fakeBackupDockerService{
		containers: []container.Summary{{ID: "other-id", Names: []string{"/other"}, State: "running"}},
		inspects: map[string]container.InspectResponse{
			"other-id": {
				ID:    "other-id",
				Name:  "/other",
				State: &container.State{Running: true},
				HostConfig: &container.HostConfig{PortBindings: network.PortMap{
					network.MustParsePort("80/tcp"): {{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: "8080"}},
				}},
			},
		},
	}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()
	err := restoreBackup(context.Background(), dir, RestoreOptions{Confirm: true, NoStart: true, SkipChecksum: true})
	if err == nil || !strings.Contains(err.Error(), "host port") {
		t.Fatalf("restoreBackup() error = %v, want wildcard port conflict", err)
	}
	assertNoRestoreMutations(t, fake.calls)
}

func TestRestoreManifestInspectImageMismatchFailsBeforeDocker(t *testing.T) {
	dir := t.TempDir()
	manifest := basicRestoreManifest("demo")
	manifest.Image = "alpine:latest"
	writeTestJSON(t, filepath.Join(dir, backupManifestName), manifest)
	writeTestJSON(t, filepath.Join(dir, backupInspectName), basicRestoreInspect("demo"))
	fake := &fakeBackupDockerService{}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()
	err := restoreBackup(context.Background(), dir, RestoreOptions{Confirm: true, NoStart: true, SkipChecksum: true})
	if err == nil || !strings.Contains(err.Error(), "image metadata mismatch") {
		t.Fatalf("restoreBackup() error = %v, want image mismatch", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("calls = %#v, image mismatch must fail before Docker", fake.calls)
	}
}

func TestRestoreReplaceRejectsAutoRemoveBeforeMutations(t *testing.T) {
	dir := writeRestoreFixture(t, basicRestoreInspect("demo"))
	fake := &fakeBackupDockerService{
		containerExists: true,
		inspects: map[string]container.InspectResponse{
			"demo": {ID: "old-id", Name: "/demo", HostConfig: &container.HostConfig{AutoRemove: true}, State: &container.State{Running: true}},
		},
	}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()
	err := restoreBackup(context.Background(), dir, RestoreOptions{Confirm: true, Replace: true, SkipChecksum: true})
	if err == nil || !strings.Contains(err.Error(), "auto-remove") {
		t.Fatalf("restoreBackup() error = %v, want auto-remove rejection", err)
	}
	assertNoRestoreMutations(t, fake.calls)
}

func TestRestoreStartCancellationRollsBackReplacement(t *testing.T) {
	dir := writeRestoreFixture(t, basicRestoreInspect("demo"))
	ctx, cancel := context.WithCancel(context.Background())
	fake := runningExistingRestoreService("demo", "old-id")
	fake.cancelAfterStart = cancel
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()
	err := restoreBackup(ctx, dir, RestoreOptions{Confirm: true, Replace: true, SkipChecksum: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("restoreBackup() error = %v, want context.Canceled", err)
	}
	requireCallOrder(t, fake.calls, "start-container:restored-id", "remove-container:restored-id", "rename-container:old-id:demo", "start-container:old-id")
}

func TestRestoreRollbackStillRestartsOldContainerAfterOtherFailures(t *testing.T) {
	dir := writeRestoreFixture(t, basicRestoreInspect("demo"))
	fake := runningExistingRestoreService("demo", "old-id")
	fake.startErrors = map[string]error{"restored-id": errors.New("start failed")}
	fake.removeErrors = map[string]error{"restored-id": errors.New("remove failed")}
	fake.renameErrors = map[string]error{"demo": errors.New("rename failed")}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()
	if err := restoreBackup(context.Background(), dir, RestoreOptions{Confirm: true, Replace: true, SkipChecksum: true}); err == nil {
		t.Fatal("restoreBackup() error = nil, want rollback failure")
	}
	requireCallOrder(t, fake.calls, "remove-container:restored-id", "rename-container:old-id:demo", "start-container:old-id")
}

func TestRestoreRetainsNewContainerWhenOldRemovalErrorReportsNotFound(t *testing.T) {
	dir := writeRestoreFixture(t, basicRestoreInspect("demo"))
	fake := runningExistingRestoreService("demo", "old-id")
	fake.removeErrors = map[string]error{"old-id": errors.New("response lost")}
	fake.inspectErrors = map[string]error{"old-id": cerrdefs.ErrNotFound}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()
	if err := restoreBackup(context.Background(), dir, RestoreOptions{Confirm: true, Replace: true, SkipChecksum: true}); err != nil {
		t.Fatalf("restoreBackup() error = %v", err)
	}
	if hasCall(fake.calls, "remove-container:restored-id") {
		t.Fatalf("calls = %#v, committed new container must be retained", fake.calls)
	}
}

func TestRestoreDoesNotWaitForOrdinaryOneShotContainer(t *testing.T) {
	dir := writeRestoreFixture(t, basicRestoreInspect("job"))
	fake := &fakeBackupDockerService{}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()
	if err := restoreBackup(context.Background(), dir, RestoreOptions{Confirm: true, SkipChecksum: true}); err != nil {
		t.Fatalf("restoreBackup() error = %v", err)
	}
	if !hasCall(fake.calls, "start-container:restored-id") || hasCallPrefix(fake.calls, "wait-container:") {
		t.Fatalf("calls = %#v, ordinary restore should start without replacement readiness gating", fake.calls)
	}
}

func TestRestoreReconcilesCreateErrorAfterCandidateWasCreated(t *testing.T) {
	dir := writeRestoreFixture(t, basicRestoreInspect("demo"))
	fake := runningExistingRestoreService("demo", "old-id")
	fake.createErr = errors.New("response lost")
	fake.createCommitError = true
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()
	err := restoreBackup(context.Background(), dir, RestoreOptions{Confirm: true, Replace: true, SkipChecksum: true})
	if err == nil || !strings.Contains(err.Error(), "response lost") {
		t.Fatalf("restoreBackup() error = %v, want create error", err)
	}
	requireCallOrder(t, fake.calls, "create-container:demo-dm-restore-candidate-", "inspect-container:demo-dm-restore-candidate-", "remove-container:restored-id", "rename-container:old-id:demo", "start-container:old-id")
}

func TestRestoreReconcilesCandidateRenameErrorAfterCommit(t *testing.T) {
	dir := writeRestoreFixture(t, basicRestoreInspect("demo"))
	fake := runningExistingRestoreService("demo", "old-id")
	fake.renameErrors = map[string]error{"demo": errors.New("response lost")}
	fake.renameCommitError = map[string]bool{"demo": true}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()
	if err := restoreBackup(context.Background(), dir, RestoreOptions{Confirm: true, Replace: true, SkipChecksum: true}); err != nil {
		t.Fatalf("restoreBackup() error = %v", err)
	}
	if hasCall(fake.calls, "remove-container:restored-id") {
		t.Fatalf("calls = %#v, committed candidate rename must not roll back new container", fake.calls)
	}
}

func TestRestoreTargetNamesAreNormalizedAndValidatedBeforeDocker(t *testing.T) {
	t.Run("normalized duplicate", func(t *testing.T) {
		dir := t.TempDir()
		entries := []BackupContainerManifest{
			{ContainerName: "demo", Path: "containers/one", InspectFile: backupInspectName},
			{ContainerName: "/demo", Path: "containers/two", InspectFile: backupInspectName},
		}
		writeTestJSON(t, filepath.Join(dir, backupManifestName), BackupManifest{Version: 1, Containers: entries})
		writeTestJSON(t, filepath.Join(dir, "containers", "one", backupInspectName), basicRestoreInspect("demo"))
		writeTestJSON(t, filepath.Join(dir, "containers", "two", backupInspectName), basicRestoreInspect("demo"))
		fake := &fakeBackupDockerService{}
		restoreFactory := replaceBackupServiceFactory(fake)
		defer restoreFactory()
		err := restoreBackup(context.Background(), dir, RestoreOptions{Confirm: true, NoStart: true, SkipChecksum: true})
		if err == nil || !strings.Contains(err.Error(), "duplicate target") {
			t.Fatalf("restoreBackup() error = %v, want normalized duplicate", err)
		}
		if len(fake.calls) != 0 {
			t.Fatalf("calls = %#v, duplicate target must fail before Docker", fake.calls)
		}
	})
	t.Run("single character", func(t *testing.T) {
		dir := writeRestoreFixture(t, basicRestoreInspect("a"))
		fake := &fakeBackupDockerService{}
		restoreFactory := replaceBackupServiceFactory(fake)
		defer restoreFactory()
		err := restoreBackup(context.Background(), dir, RestoreOptions{Confirm: true, NoStart: true, SkipChecksum: true})
		if err == nil || !strings.Contains(err.Error(), "invalid restore container name") {
			t.Fatalf("restoreBackup() error = %v, want invalid name", err)
		}
		if len(fake.calls) != 0 {
			t.Fatalf("calls = %#v, invalid name must fail before Docker", fake.calls)
		}
	})
}

func TestCopyDockerLoadStreamReturnsDockerJSONError(t *testing.T) {
	input := "{\"stream\":\"Loading layer\"}\n{\"errorDetail\":{\"message\":\"invalid tar\"},\"error\":\"invalid tar\"}\n"
	var output bytes.Buffer
	err := copyDockerLoadStream(context.Background(), &output, strings.NewReader(input))
	if err == nil || !strings.Contains(err.Error(), "invalid tar") {
		t.Fatalf("copyDockerLoadStream() error = %v, want daemon error", err)
	}
	if output.String() != input {
		t.Fatalf("output = %q, want original stream %q", output.String(), input)
	}
}

func TestEncryptedSplitSignedRestoreVerifiesExtractedPayloadOnly(t *testing.T) {
	source := writeRestoreFixture(t, basicRestoreInspect("signed-job"))
	if err := writeChecksums(source); err != nil {
		t.Fatal(err)
	}
	keyDir := t.TempDir()
	publicKey, privateKey := writeTestEd25519KeyPair(t, keyDir, "bundle")
	if err := signBackupChecksumsWithContext(context.Background(), source, privateKey); err != nil {
		t.Fatal(err)
	}
	passphrase := filepath.Join(t.TempDir(), "backup.pass")
	writeTestFile(t, passphrase, "correct horse battery staple\n")
	archivePath := filepath.Join(t.TempDir(), "bundle.tar.gz.enc")
	if err := createBackupArchiveWithOptions(context.Background(), source, archivePath, backupArchiveOptions{Encrypt: true, PassphraseFile: passphrase, SplitSize: 128}); err != nil {
		t.Fatal(err)
	}
	firstPart := archivePath + ".part-001"
	fake := &fakeBackupDockerService{}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()
	if err := restoreBackup(context.Background(), firstPart, RestoreOptions{Confirm: true, NoStart: true, PassphraseFile: passphrase, TrustedPublicKey: publicKey, Output: io.Discard}); err != nil {
		t.Fatalf("restoreBackup() error = %v", err)
	}
	assertRestoreCandidateCommitted(t, fake.calls, "signed-job")
}

func TestBackupSensitiveFilesAndCollidingOutputsAreNotOverwritten(t *testing.T) {
	fake := &fakeBackupDockerService{inspect: basicRestoreInspect("demo")}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()

	t.Run("signing key inside generated inspect path", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "backup")
		if err := os.MkdirAll(root, 0755); err != nil {
			t.Fatal(err)
		}
		_, privateKey := writeTestEd25519KeyPair(t, root, "temporary")
		keyData, err := os.ReadFile(privateKey)
		if err != nil {
			t.Fatal(err)
		}
		inspectPath := filepath.Join(root, backupInspectName)
		if err := os.Rename(privateKey, inspectPath); err != nil {
			t.Fatal(err)
		}
		_, err = backupContainer(context.Background(), "demo", BackupOptions{OutputDir: root, Bundle: true, SigningKey: inspectPath, BundleOutput: filepath.Join(t.TempDir(), "bundle.tar.gz")})
		if err == nil {
			t.Fatal("backupContainer() error = nil, want in-root key rejection")
		}
		assertFileContent(t, inspectPath, keyData)
	})

	t.Run("passphrase inside generated manifest path", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "backup")
		if err := os.MkdirAll(root, 0755); err != nil {
			t.Fatal(err)
		}
		passphrase := filepath.Join(root, backupManifestName)
		original := []byte("do-not-overwrite")
		if err := os.WriteFile(passphrase, original, 0600); err != nil {
			t.Fatal(err)
		}
		_, err := backupContainer(context.Background(), "demo", BackupOptions{OutputDir: root, Bundle: true, Encrypt: true, PassphraseFile: passphrase, BundleOutput: filepath.Join(t.TempDir(), "bundle.tar.gz.enc")})
		if err == nil {
			t.Fatal("backupContainer() error = nil, want in-root passphrase rejection")
		}
		assertFileContent(t, passphrase, original)
	})

	t.Run("bundle output equals signing key", func(t *testing.T) {
		keyDir := t.TempDir()
		_, privateKey := writeTestEd25519KeyPair(t, keyDir, "signing")
		original, err := os.ReadFile(privateKey)
		if err != nil {
			t.Fatal(err)
		}
		_, err = backupContainer(context.Background(), "demo", BackupOptions{OutputDir: filepath.Join(t.TempDir(), "backup"), Bundle: true, SigningKey: privateKey, BundleOutput: privateKey})
		if err == nil {
			t.Fatal("backupContainer() error = nil, want exclusive output collision")
		}
		assertFileContent(t, privateKey, original)
	})

	t.Run("encrypted output equals passphrase", func(t *testing.T) {
		passphrase := filepath.Join(t.TempDir(), "secret.enc")
		original := []byte("keep-this-secret\n")
		if err := os.WriteFile(passphrase, original, 0600); err != nil {
			t.Fatal(err)
		}
		_, err := backupContainer(context.Background(), "demo", BackupOptions{OutputDir: filepath.Join(t.TempDir(), "backup"), Bundle: true, Encrypt: true, PassphraseFile: passphrase, BundleOutput: passphrase})
		if err == nil {
			t.Fatal("backupContainer() error = nil, want exclusive output collision")
		}
		assertFileContent(t, passphrase, original)
	})
}

func TestBackupRejectsHardlinkedPassphraseInsideRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "backup")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	passphrase := filepath.Join(t.TempDir(), "secret.pass")
	writeTestFile(t, passphrase, "secret\n")
	if err := os.Link(passphrase, filepath.Join(root, "linked-secret")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	fake := &fakeBackupDockerService{inspect: basicRestoreInspect("demo")}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()
	_, err := backupContainer(context.Background(), "demo", BackupOptions{OutputDir: root, Bundle: true, Encrypt: true, PassphraseFile: passphrase, BundleOutput: filepath.Join(t.TempDir(), "bundle.tar.gz.enc")})
	if err == nil || !strings.Contains(err.Error(), "linked into") {
		t.Fatalf("backupContainer() error = %v, want hardlink rejection", err)
	}
}

func TestRestoreRequiresNamedVolumesInManifest(t *testing.T) {
	inspect := basicRestoreInspect("demo")
	inspect.HostConfig.Binds = []string{"missing_data:/data"}
	dir := writeRestoreFixture(t, inspect)
	fake := &fakeBackupDockerService{}
	restoreFactory := replaceBackupServiceFactory(fake)
	defer restoreFactory()
	err := restoreBackup(context.Background(), dir, RestoreOptions{Confirm: true, NoStart: true, SkipChecksum: true})
	if err == nil || !strings.Contains(err.Error(), "missing from the restore manifest") {
		t.Fatalf("restoreBackup() error = %v, want named volume rejection", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("calls = %#v, missing volume manifest must fail before Docker", fake.calls)
	}
}

func TestUnsafeRestoreConfigIncludesEndpointAndHostControlledOptions(t *testing.T) {
	inspect := basicRestoreInspect("demo")
	inspect.HostConfig.PublishAllPorts = true
	inspect.HostConfig.CgroupParent = "custom.slice"
	inspect.HostConfig.StorageOpt = map[string]string{"size": "10G"}
	inspect.HostConfig.MaskedPaths = []string{}
	inspect.HostConfig.ReadonlyPaths = []string{}
	inspect.HostConfig.Mounts = []mount.Mount{{Type: mount.Type("cluster"), Source: "plugin", Target: "/data"}}
	inspect.NetworkSettings = &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{
		"demo_net": {DriverOpts: map[string]string{"com.example.host": "value"}, Links: []string{"db:db"}},
	}}
	risks := strings.Join(unsafeRestoreHostConfig(inspect), "\n")
	for _, want := range []string{"publish-all-ports", "custom cgroup parent", "storage driver options", "masked system paths missing", "read-only system paths missing", "unsupported mount type", "driver options", "legacy links"} {
		if !strings.Contains(risks, want) {
			t.Fatalf("risks = %q, want %q", risks, want)
		}
	}
}

func basicRestoreInspect(name string) container.InspectResponse {
	return container.InspectResponse{
		Name:       "/" + name,
		Config:     &container.Config{Image: "busybox:latest"},
		HostConfig: &container.HostConfig{},
	}
}

func basicRestoreManifest(name string) BackupManifest {
	return BackupManifest{Version: 1, ContainerName: name, Image: "busybox:latest", InspectFile: backupInspectName}
}

func writeTestEd25519KeyPair(t *testing.T, dir, prefix string) (string, string) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	privatePath := filepath.Join(dir, prefix+"-private.pem")
	publicPath := filepath.Join(dir, prefix+"-public.pem")
	if err := os.WriteFile(privatePath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publicPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}), 0644); err != nil {
		t.Fatal(err)
	}
	return publicPath, privatePath
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func assertFileContent(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("file %s changed: got %d bytes want %d", path, len(got), len(want))
	}
}

func assertNoRestoreMutations(t *testing.T, calls []string) {
	t.Helper()
	for _, prefix := range []string{"load-image:", "create-network:", "create-volume:", "stop-container:", "rename-container:", "remove-container:", "create-container:", "start-container:"} {
		if hasCallPrefix(calls, prefix) {
			t.Fatalf("calls = %#v, unexpected restore mutation %s", calls, prefix)
		}
	}
}
