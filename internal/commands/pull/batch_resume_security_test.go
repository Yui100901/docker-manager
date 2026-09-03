package pull

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRunPullBatchResumeRepullsAfterUncertainStateCommit(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	opts := PullBatchOptions{
		Images:      []string{"busybox:latest"},
		OutputDir:   dir,
		StateFile:   statePath,
		Concurrency: 1,
		platform:    targetPlatform{targetOS: "linux", targetArch: "amd64"},
	}
	syncErr := errors.New("injected state directory sync failure")
	syncCalls := 0
	firstReport, err := runPullBatchWithDepsAndPersistence(context.Background(), opts, func(_ string, pullOpts PullOptions) error {
		return writePullBatchTestArtifact(pullOpts, "first archive")
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		return false, nil
	}, pullBatchPersistence{
		writeState: func(ctx context.Context, path string, state pullBatchState) error {
			return writePullBatchStateWhileLockedWithSync(ctx, path, state, func(*os.Root) error {
				syncCalls++
				return syncErr
			})
		},
	})
	if !errors.Is(err, syncErr) || firstReport.Failed != 1 || syncCalls != 1 {
		t.Fatalf("first run error/report/syncCalls = %v/%#v/%d, want joined sync failure and one failed item", err, firstReport, syncCalls)
	}

	data, readErr := os.ReadFile(statePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var diskState pullBatchState
	if err := json.Unmarshal(data, &diskState); err != nil {
		t.Fatal(err)
	}
	diskItem := diskState.Items["busybox:latest"]
	if diskItem.Status != pullBatchStatusSuccess || diskItem.Fingerprint == nil {
		t.Fatalf("raw state item = %#v, want post-rename success with fingerprint", diskItem)
	}
	markerPath := pullBatchStateUntrustedMarkerPath(statePath)
	markerInfo, markerErr := os.Lstat(markerPath)
	if markerErr != nil || !markerInfo.Mode().IsRegular() {
		t.Fatalf("untrusted marker info/error = %v/%v, want regular marker", markerInfo, markerErr)
	}
	guardedState, readErr := readPullBatchState(statePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	guardedItem := guardedState.Items["busybox:latest"]
	if guardedItem.Status != pullBatchStatusFailed || guardedItem.Fingerprint != nil {
		t.Fatalf("guarded state item = %#v, want invalidated success", guardedItem)
	}

	opts.Resume = true
	pullCalls := 0
	secondReport, err := runPullBatchWithDeps(context.Background(), opts, func(_ string, pullOpts PullOptions) error {
		pullCalls++
		return writePullBatchTestArtifact(pullOpts, "second archive")
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		return false, nil
	})
	if err != nil {
		t.Fatalf("second run error = %v", err)
	}
	if pullCalls != 1 || secondReport.Succeeded != 1 || secondReport.Skipped != 0 {
		t.Fatalf("second run pullCalls/report = %d/%#v, want one recovery pull", pullCalls, secondReport)
	}
	if _, err := os.Lstat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("untrusted marker after confirmed state write error = %v, want not exist", err)
	}
	recoveredState, readErr := readPullBatchState(statePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	recoveredItem := recoveredState.Items["busybox:latest"]
	if recoveredItem.Status != pullBatchStatusSuccess || recoveredItem.Fingerprint == nil {
		t.Fatalf("recovered state item = %#v, want trusted success", recoveredItem)
	}
}

func TestRunPullBatchResumeRepullsOlderCommitProtocols(t *testing.T) {
	for _, protocol := range []int{0, 1} {
		t.Run(fmt.Sprintf("protocol-%d", protocol), func(t *testing.T) {
			dir := t.TempDir()
			statePath := filepath.Join(dir, "state.json")
			archivePath := pullBatchTestArchivePath(t, dir, "busybox:latest")
			if err := os.WriteFile(archivePath, []byte("legacy archive"), 0600); err != nil {
				t.Fatal(err)
			}
			opts := PullBatchOptions{
				Images:      []string{"busybox:latest"},
				OutputDir:   dir,
				StateFile:   statePath,
				Concurrency: 1,
				Resume:      true,
				platform:    targetPlatform{targetOS: "linux", targetArch: "amd64"},
			}
			fingerprint, err := buildPullBatchResumeFingerprint(context.Background(), pullBatchPlanItem{
				Image: "busybox:latest", ArchivePath: archivePath,
			}, opts)
			if err != nil {
				t.Fatal(err)
			}
			legacyData, err := json.Marshal(pullBatchState{
				CommitProtocol: protocol,
				Items: map[string]pullBatchStateItem{
					"busybox:latest": {Image: "busybox:latest", Status: pullBatchStatusSuccess, Fingerprint: fingerprint},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(statePath, legacyData, 0600); err != nil {
				t.Fatal(err)
			}

			pullCalls := 0
			report, err := runPullBatchWithDeps(context.Background(), opts, func(_ string, pullOpts PullOptions) error {
				pullCalls++
				return writePullBatchTestArtifact(pullOpts, "migrated archive")
			}, func(context.Context, string, string, PullOptions) (bool, error) {
				return false, nil
			})
			if err != nil {
				t.Fatalf("runPullBatchWithDeps() error = %v", err)
			}
			if pullCalls != 1 || report.Succeeded != 1 || report.Skipped != 0 {
				t.Fatalf("pullCalls/report = %d/%#v, want one migration pull", pullCalls, report)
			}
			state, err := readPullBatchState(statePath)
			if err != nil {
				t.Fatal(err)
			}
			if state.CommitProtocol != pullBatchStateCommitProtocolVersion || state.Items["busybox:latest"].Fingerprint == nil {
				t.Fatalf("migrated state = %#v, want current commit protocol and fingerprint", state)
			}
		})
	}
}

func TestRunPullBatchStateMarkerCreateSyncFailurePreventsStateWrite(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	opts := PullBatchOptions{
		Images: []string{"busybox:latest"}, OutputDir: dir, StateFile: statePath, Concurrency: 1,
		platform: targetPlatform{targetOS: "linux", targetArch: "amd64"},
	}
	syncErr := errors.New("injected marker directory sync failure")
	stateWriteCalled := false
	report, err := runPullBatchWithDepsAndPersistence(context.Background(), opts, func(_ string, pullOpts PullOptions) error {
		return writePullBatchTestArtifact(pullOpts, "first archive")
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		return false, nil
	}, pullBatchPersistence{
		writeState: func(context.Context, string, pullBatchState) error {
			stateWriteCalled = true
			return nil
		},
		writeStateMarker: func(ctx context.Context, path string, marker pullBatchStateUntrustedMarker) (pullBatchStateMarkerCommit, error) {
			return writePullBatchStateUntrustedMarkerWithSync(ctx, path, marker, func(*os.Root) error {
				return syncErr
			})
		},
	})
	if !errors.Is(err, syncErr) || report.Failed != 1 {
		t.Fatalf("run error/report = %v/%#v, want marker sync failure", err, report)
	}
	if stateWriteCalled {
		t.Fatal("state writer called after marker durability failure")
	}
	if _, err := os.Lstat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state file error = %v, want not exist", err)
	}
	if info, err := os.Lstat(pullBatchStateUntrustedMarkerPath(statePath)); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("marker info/error = %v/%v, want published marker", info, err)
	}

	opts.Resume = true
	pullCalls := 0
	recovery, err := runPullBatchWithDeps(context.Background(), opts, func(_ string, pullOpts PullOptions) error {
		pullCalls++
		return writePullBatchTestArtifact(pullOpts, "recovered archive")
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		return false, nil
	})
	if err != nil || pullCalls != 1 || recovery.Succeeded != 1 {
		t.Fatalf("recovery error/pullCalls/report = %v/%d/%#v", err, pullCalls, recovery)
	}
}

func TestRunPullBatchStateMarkerClearFailureRemainsUntrusted(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	opts := PullBatchOptions{
		Images: []string{"busybox:latest"}, OutputDir: dir, StateFile: statePath, Concurrency: 1,
		platform: targetPlatform{targetOS: "linux", targetArch: "amd64"},
	}
	clearErr := errors.New("injected marker removal failure")
	report, err := runPullBatchWithDepsAndPersistence(context.Background(), opts, func(_ string, pullOpts PullOptions) error {
		return writePullBatchTestArtifact(pullOpts, "first archive")
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		return false, nil
	}, pullBatchPersistence{
		clearStateMarker: func(context.Context, string, pullBatchStateMarkerCommit) error {
			return clearErr
		},
	})
	if !errors.Is(err, clearErr) || report.Failed != 1 {
		t.Fatalf("run error/report = %v/%#v, want marker clear failure", err, report)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var diskState pullBatchState
	if err := json.Unmarshal(data, &diskState); err != nil {
		t.Fatal(err)
	}
	if diskState.Items["busybox:latest"].Status != pullBatchStatusSuccess {
		t.Fatalf("raw disk state = %#v, want published success", diskState)
	}
	if _, err := os.Lstat(pullBatchStateUntrustedMarkerPath(statePath)); err != nil {
		t.Fatalf("marker missing after clear failure: %v", err)
	}

	opts.Resume = true
	pullCalls := 0
	recovery, err := runPullBatchWithDeps(context.Background(), opts, func(_ string, pullOpts PullOptions) error {
		pullCalls++
		return writePullBatchTestArtifact(pullOpts, "recovered archive")
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		return false, nil
	})
	if err != nil || pullCalls != 1 || recovery.Succeeded != 1 || recovery.Skipped != 0 {
		t.Fatalf("recovery error/pullCalls/report = %v/%d/%#v", err, pullCalls, recovery)
	}
}

func TestRunPullBatchResumeRepullsWhenFingerprintOrArchiveChanges(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, opts *PullBatchOptions, fingerprint *pullBatchResumeFingerprint, archivePath string)
	}{
		{
			name: "archive missing",
			mutate: func(t *testing.T, _ *PullBatchOptions, _ *pullBatchResumeFingerprint, archivePath string) {
				t.Helper()
				if err := os.Remove(archivePath); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "archive tampered",
			mutate: func(t *testing.T, _ *PullBatchOptions, _ *pullBatchResumeFingerprint, archivePath string) {
				t.Helper()
				if err := os.WriteFile(archivePath, []byte("tampered"), 0600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "invalid digest",
			mutate: func(_ *testing.T, _ *PullBatchOptions, fingerprint *pullBatchResumeFingerprint, _ string) {
				fingerprint.ArchiveDigest = "sha256:not-a-digest"
			},
		},
		{
			name: "output directory changed",
			mutate: func(t *testing.T, opts *PullBatchOptions, _ *pullBatchResumeFingerprint, _ string) {
				opts.OutputDir = filepath.Join(t.TempDir(), "new-output")
			},
		},
		{
			name: "target os changed",
			mutate: func(_ *testing.T, opts *PullBatchOptions, _ *pullBatchResumeFingerprint, _ string) {
				opts.platform.targetOS = "windows"
			},
		},
		{
			name: "target architecture changed",
			mutate: func(_ *testing.T, opts *PullBatchOptions, _ *pullBatchResumeFingerprint, _ string) {
				opts.platform.targetArch = "arm64"
			},
		},
		{
			name: "docker load changed",
			mutate: func(_ *testing.T, opts *PullBatchOptions, _ *pullBatchResumeFingerprint, _ string) {
				opts.Load = true
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			statePath := filepath.Join(dir, "state.json")
			archivePath := pullBatchTestArchivePath(t, dir, "busybox:latest")
			if err := os.WriteFile(archivePath, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			opts := PullBatchOptions{
				Images:      []string{"busybox:latest"},
				OutputDir:   dir,
				StateFile:   statePath,
				Concurrency: 1,
				Resume:      true,
				platform:    targetPlatform{targetOS: "linux", targetArch: "amd64"},
			}
			fingerprint, err := buildPullBatchResumeFingerprint(context.Background(), pullBatchPlanItem{
				Image: "busybox:latest", ArchivePath: archivePath,
			}, opts)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, &opts, fingerprint, archivePath)
			if err := writePullBatchState(statePath, pullBatchState{Items: map[string]pullBatchStateItem{
				"busybox:latest": {Image: "busybox:latest", Status: pullBatchStatusSuccess, Fingerprint: fingerprint},
			}}); err != nil {
				t.Fatal(err)
			}
			pullCalls := 0
			report, err := runPullBatchWithDeps(context.Background(), opts, func(_ string, pullOpts PullOptions) error {
				pullCalls++
				return writePullBatchTestArtifact(pullOpts, "replacement")
			}, func(context.Context, string, string, PullOptions) (bool, error) {
				return false, nil
			})
			if err != nil {
				t.Fatalf("runPullBatchWithDeps() error = %v", err)
			}
			if pullCalls != 1 || report.Succeeded != 1 || report.Skipped != 0 {
				t.Fatalf("pullCalls/report = %d/%#v, want one replacement pull", pullCalls, report)
			}
			state, err := readPullBatchState(statePath)
			if err != nil {
				t.Fatal(err)
			}
			if state.Items["busybox:latest"].Fingerprint == nil || state.Items["busybox:latest"].Fingerprint.Version != pullBatchResumeFingerprintVersion {
				t.Fatalf("migrated fingerprint = %#v", state.Items["busybox:latest"].Fingerprint)
			}
		})
	}
}

func TestRunPullBatchSuccessRequiresVerifiedArchive(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	report, err := runPullBatchWithDeps(context.Background(), PullBatchOptions{
		Images:      []string{"busybox:latest"},
		OutputDir:   dir,
		StateFile:   statePath,
		Concurrency: 1,
	}, func(string, PullOptions) error {
		return nil
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		return false, nil
	})
	if err == nil || report.Failed != 1 {
		t.Fatalf("runPullBatchWithDeps() error/report = %v/%#v, want missing archive failure", err, report)
	}
	state, readErr := readPullBatchState(statePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	item := state.Items["busybox:latest"]
	if item.Status != pullBatchStatusFailed || item.Fingerprint != nil {
		t.Fatalf("state item = %#v, want failed without fingerprint", item)
	}
}

func TestFingerprintPullBatchArchiveHonorsCanceledContext(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "image.tar")
	if err := os.WriteFile(archivePath, []byte("archive"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := fingerprintPullBatchArchive(ctx, archivePath)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("fingerprintPullBatchArchive() error = %v, want context.Canceled", err)
	}
}
