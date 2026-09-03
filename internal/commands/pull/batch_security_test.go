package pull

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"docker-manager/internal/sensitive"
)

type pullBatchManualDeadlineContext struct {
	context.Context
	done chan struct{}
	once sync.Once
}

func newPullBatchManualDeadlineContext() *pullBatchManualDeadlineContext {
	return &pullBatchManualDeadlineContext{
		Context: context.Background(),
		done:    make(chan struct{}),
	}
}

func (ctx *pullBatchManualDeadlineContext) Done() <-chan struct{} {
	return ctx.done
}

func (ctx *pullBatchManualDeadlineContext) Err() error {
	select {
	case <-ctx.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (ctx *pullBatchManualDeadlineContext) expire() {
	ctx.once.Do(func() { close(ctx.done) })
}

func TestWriteAtomicJSONIgnoresPresetFixedStagingSymlink(t *testing.T) {
	dir := t.TempDir()
	outputPath := filepath.Join(dir, "state.json")
	victimPath := filepath.Join(dir, "victim.json")
	if err := os.WriteFile(victimPath, []byte("victim-must-survive"), 0600); err != nil {
		t.Fatal(err)
	}
	fixedStagingPath := outputPath + ".tmp"
	if err := os.Symlink(victimPath, fixedStagingPath); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}

	if err := writeAtomicJSON(outputPath, []byte(`{"status":"ok"}`)); err != nil {
		t.Fatalf("writeAtomicJSON() error = %v", err)
	}
	assertPullBatchFileContent(t, victimPath, "victim-must-survive")
	info, err := os.Lstat(fixedStagingPath)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("preset fixed staging changed: info=%v error=%v", info, err)
	}
	assertPullBatchJSON(t, outputPath)
	assertNoPullBatchJSONStaging(t, dir)
}

func TestWriteAtomicJSONRejectsLinksAndNonRegularDestinations(t *testing.T) {
	t.Run("destination symlink", func(t *testing.T) {
		dir := t.TempDir()
		victimPath := filepath.Join(dir, "victim.json")
		if err := os.WriteFile(victimPath, []byte("victim-must-survive"), 0600); err != nil {
			t.Fatal(err)
		}
		outputPath := filepath.Join(dir, "state.json")
		if err := os.Symlink(victimPath, outputPath); err != nil {
			t.Skipf("symlink creation unavailable: %v", err)
		}

		if err := writeAtomicJSON(outputPath, []byte(`{"status":"unsafe"}`)); err == nil {
			t.Fatal("writeAtomicJSON() error = nil, want symlink rejection")
		}
		assertPullBatchFileContent(t, victimPath, "victim-must-survive")
		info, err := os.Lstat(outputPath)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("destination symlink changed: info=%v error=%v", info, err)
		}
		assertNoPullBatchJSONStaging(t, dir)
	})

	t.Run("ancestor symlink", func(t *testing.T) {
		root := t.TempDir()
		realDir := filepath.Join(root, "real")
		if err := os.Mkdir(realDir, 0700); err != nil {
			t.Fatal(err)
		}
		linkedDir := filepath.Join(root, "linked")
		if err := os.Symlink(realDir, linkedDir); err != nil {
			t.Skipf("symlink creation unavailable: %v", err)
		}

		if err := writeAtomicJSON(filepath.Join(linkedDir, "state.json"), []byte(`{"status":"unsafe"}`)); err == nil {
			t.Fatal("writeAtomicJSON() error = nil, want ancestor symlink rejection")
		}
		if _, err := os.Lstat(filepath.Join(realDir, "state.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("linked destination stat error = %v, want not exist", err)
		}
		assertNoPullBatchJSONStaging(t, realDir)
	})

	t.Run("directory destination", func(t *testing.T) {
		dir := t.TempDir()
		outputPath := filepath.Join(dir, "state.json")
		if err := os.Mkdir(outputPath, 0700); err != nil {
			t.Fatal(err)
		}

		if err := writeAtomicJSON(outputPath, []byte(`{"status":"unsafe"}`)); err == nil {
			t.Fatal("writeAtomicJSON() error = nil, want non-regular destination rejection")
		}
		info, err := os.Lstat(outputPath)
		if err != nil || !info.IsDir() {
			t.Fatalf("directory destination changed: info=%v error=%v", info, err)
		}
		assertNoPullBatchJSONStaging(t, dir)
	})
}

func TestWriteAtomicJSONReplacesWithPrivateCompleteFile(t *testing.T) {
	const writers = 8
	dir := t.TempDir()
	outputPath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(outputPath, []byte(`{"writer":-1}`), 0644); err != nil {
		t.Fatal(err)
	}

	type preparedWrite struct {
		root          *os.Root
		outputName    string
		outputPath    string
		parentInfo    os.FileInfo
		initialInfo   os.FileInfo
		initialExists bool
	}
	prepared := make([]preparedWrite, writers)
	for index := range writers {
		root, outputName, absoluteOutput, parentInfo, err := openPullArchiveOutput(outputPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = root.Close() })
		initial, existed, err := snapshotPullJSONOutput(root, outputName)
		if err != nil || !existed {
			t.Fatalf("snapshot %d info=%v existed=%v error=%v", index, initial, existed, err)
		}
		prepared[index] = preparedWrite{root: root, outputName: outputName, outputPath: absoluteOutput, parentInfo: parentInfo, initialInfo: initial, initialExists: existed}
	}

	start := make(chan struct{})
	errs := make(chan error, writers)
	var group sync.WaitGroup
	for index := range writers {
		group.Add(1)
		go func(writer int, write preparedWrite) {
			defer group.Done()
			<-start
			payload := []byte(`{"writer":` + string(rune('0'+writer)) + `}`)
			errs <- writeAtomicJSONFromSnapshot(write.root, write.outputName, write.outputPath, write.parentInfo, write.initialInfo, write.initialExists, payload)
		}(index, prepared[index])
	}
	close(start)
	group.Wait()
	close(errs)
	succeeded := 0
	for err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		if !strings.Contains(err.Error(), "JSON 输出在发布前") {
			t.Fatalf("concurrent writeAtomicJSON() unexpected error = %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("same-version concurrent publishers succeeded = %d, want exactly 1", succeeded)
	}

	var value struct {
		Writer int `json:"writer"`
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &value); err != nil || value.Writer < 0 || value.Writer >= writers {
		t.Fatalf("published JSON = %q value=%#v error=%v", data, value, err)
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0600 {
		t.Fatalf("published permissions = %04o, want 0600", got)
	}
	assertNoPullBatchJSONStaging(t, dir)
	if err := writeAtomicJSON(outputPath, []byte(`{"writer":9}`)); err != nil {
		t.Fatalf("writeAtomicJSON() with stale unlocked lock file error = %v", err)
	}
}

func TestVerifyPullJSONOutputUnchangedDetectsReplacement(t *testing.T) {
	dir := t.TempDir()
	outputPath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(outputPath, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	root, outputName, absoluteOutput, _, err := openPullArchiveOutput(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	initial, existed, err := snapshotPullJSONOutput(root, outputName)
	if err != nil || !existed {
		t.Fatalf("snapshotPullJSONOutput() info=%v existed=%v error=%v", initial, existed, err)
	}
	if err := root.Remove(outputName); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(outputName, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := verifyPullJSONOutputUnchanged(root, outputName, absoluteOutput, initial, existed); err == nil {
		t.Fatal("verifyPullJSONOutputUnchanged() error = nil, want replacement detection")
	}
	assertPullBatchFileContent(t, outputPath, "replacement")
}

func TestWriteAtomicJSONCrossProcessSameVersionHasSingleWinner(t *testing.T) {
	if os.Getenv("DM_PULL_JSON_LOCK_CHILD") == "1" {
		runPullAtomicJSONLockChild(t)
		return
	}

	dir := t.TempDir()
	coordDir := filepath.Join(dir, "coord")
	if err := os.Mkdir(coordDir, 0700); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(outputPath, []byte(`{"writer":-1}`), 0600); err != nil {
		t.Fatal(err)
	}
	goPath := filepath.Join(coordDir, "go")
	type child struct {
		command *exec.Cmd
		ready   string
		result  string
	}
	children := make([]child, 2)
	for index := range children {
		readyPath := filepath.Join(coordDir, fmt.Sprintf("ready-%d", index))
		resultPath := filepath.Join(coordDir, fmt.Sprintf("result-%d", index))
		command := exec.Command(os.Args[0], "-test.run=^TestWriteAtomicJSONCrossProcessSameVersionHasSingleWinner$")
		command.Env = append(os.Environ(),
			"DM_PULL_JSON_LOCK_CHILD=1",
			"DM_PULL_JSON_OUTPUT="+outputPath,
			"DM_PULL_JSON_READY="+readyPath,
			"DM_PULL_JSON_GO="+goPath,
			"DM_PULL_JSON_RESULT="+resultPath,
			fmt.Sprintf("DM_PULL_JSON_WRITER=%d", index),
		)
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		children[index] = child{command: command, ready: readyPath, result: resultPath}
	}
	deadline := time.Now().Add(15 * time.Second)
	for _, child := range children {
		for {
			if _, err := os.Stat(child.ready); err == nil {
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for JSON lock child snapshots")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if err := os.WriteFile(goPath, []byte("go"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, child := range children {
		if err := child.command.Wait(); err != nil {
			t.Fatalf("JSON lock child failed: %v", err)
		}
	}

	succeeded := 0
	for _, child := range children {
		data, err := os.ReadFile(child.result)
		if err != nil {
			t.Fatal(err)
		}
		result := string(data)
		if result == "success" {
			succeeded++
		} else if !strings.Contains(result, "JSON 输出在发布前") {
			t.Fatalf("unexpected child result = %q", result)
		}
	}
	if succeeded != 1 {
		t.Fatalf("cross-process same-version publishers succeeded = %d, want exactly 1", succeeded)
	}
	assertPullBatchJSON(t, outputPath)
	assertNoPullBatchJSONStaging(t, dir)
	if err := writeAtomicJSON(outputPath, []byte(`{"writer":9}`)); err != nil {
		t.Fatalf("write after child process lock release error = %v", err)
	}
}

func runPullAtomicJSONLockChild(t *testing.T) {
	outputPath := os.Getenv("DM_PULL_JSON_OUTPUT")
	root, outputName, absoluteOutput, parentInfo, err := openPullArchiveOutput(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	initial, existed, err := snapshotPullJSONOutput(root, outputName)
	if err != nil || !existed {
		t.Fatalf("child snapshot info=%v existed=%v error=%v", initial, existed, err)
	}
	if err := os.WriteFile(os.Getenv("DM_PULL_JSON_READY"), []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(os.Getenv("DM_PULL_JSON_GO")); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("child timed out waiting for publication signal")
		}
		time.Sleep(10 * time.Millisecond)
	}
	payload := []byte(`{"writer":` + os.Getenv("DM_PULL_JSON_WRITER") + `}`)
	err = writeAtomicJSONFromSnapshot(root, outputName, absoluteOutput, parentInfo, initial, existed, payload)
	result := "success"
	if err != nil {
		result = "error: " + err.Error()
	}
	if err := os.WriteFile(os.Getenv("DM_PULL_JSON_RESULT"), []byte(result), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestWriteAtomicJSONRejectsUnsafeLockPath(t *testing.T) {
	dir := t.TempDir()
	outputPath := filepath.Join(dir, "state.json")
	lockPath := pullAtomicJSONLockPath(outputPath)
	victimPath := filepath.Join(dir, "victim")
	if err := os.WriteFile(victimPath, []byte("victim-must-survive"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victimPath, lockPath); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}

	if err := writeAtomicJSON(outputPath, []byte(`{"status":"unsafe"}`)); err == nil {
		t.Fatal("writeAtomicJSON() error = nil, want unsafe lock rejection")
	}
	assertPullBatchFileContent(t, victimPath, "victim-must-survive")
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output stat error = %v, want not exist", err)
	}
}

func TestRunPullBatchRejectsPlannedCollisionsBeforeCallbacksOrWrites(t *testing.T) {
	tests := []struct {
		name   string
		images []string
		to     string
		paths  func(dir string) (state, report string)
		want   string
	}{
		{
			name:   "state and report",
			images: []string{"busybox:latest"},
			paths: func(dir string) (string, string) {
				path := filepath.Join(dir, "metadata.json")
				return path, filepath.Join(dir, ".", "metadata.json")
			},
			want: "输出路径冲突",
		},
		{
			name:   "state and archive",
			images: []string{"x/team/app:v1"},
			paths: func(dir string) (string, string) {
				return filepath.Join(dir, "x_team_app_v1.tar"), filepath.Join(dir, "report.json")
			},
			want: "输出路径冲突",
		},
		{
			name:   "report and archive",
			images: []string{"x/team/app:v1"},
			paths: func(dir string) (string, string) {
				return filepath.Join(dir, "state.json"), filepath.Join(dir, "x_team_app_v1.tar")
			},
			want: "输出路径冲突",
		},
		{
			name:   "archive outputs",
			images: []string{"x/team/app:v1", "x_team/app:v1"},
			paths: func(dir string) (string, string) {
				return filepath.Join(dir, "state.json"), filepath.Join(dir, "report.json")
			},
			want: "输出路径冲突",
		},
		{
			name:   "archive outputs differing only by case",
			images: []string{"x/team/app:v1", "x/team/app:V1"},
			paths: func(dir string) (string, string) {
				return filepath.Join(dir, "state.json"), filepath.Join(dir, "report.json")
			},
			want: "输出路径冲突",
		},
		{
			name:   "push targets",
			images: []string{"one/app:v1", "two/app:v1"},
			to:     "registry.example/mirror",
			paths: func(dir string) (string, string) {
				return filepath.Join(dir, "state.json"), filepath.Join(dir, "report.json")
			},
			want: "--to 目标冲突",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			statePath, reportPath := test.paths(dir)
			pullCalled := false
			existsCalled := false
			_, err := runPullBatchWithDeps(context.Background(), PullBatchOptions{
				Images:      test.images,
				To:          test.to,
				OutputDir:   dir,
				StateFile:   statePath,
				ReportFile:  reportPath,
				Concurrency: 1,
			}, func(string, PullOptions) error {
				pullCalled = true
				return nil
			}, func(context.Context, string, string, PullOptions) (bool, error) {
				existsCalled = true
				return false, nil
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("runPullBatchWithDeps() error = %v, want %q", err, test.want)
			}
			if pullCalled || existsCalled {
				t.Fatalf("callbacks after collision: pull=%v exists=%v", pullCalled, existsCalled)
			}
			for _, path := range []string{statePath, reportPath} {
				if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("metadata path %q stat error = %v, want not exist", path, statErr)
				}
			}
			assertNoPullBatchJSONStaging(t, dir)
		})
	}
}

func TestRunPullBatchRejectsClaimAncestorCollisionsBeforeCallbacksOrWrites(t *testing.T) {
	tests := []struct {
		name       string
		paths      func(string) (outputDir, stateFile, reportFile string)
		checkPaths func(string) []string
	}{
		{
			name: "state is archive ancestor",
			paths: func(dir string) (string, string, string) {
				outputDir := filepath.Join(dir, "state.json")
				return outputDir, outputDir, filepath.Join(dir, "report.json")
			},
			checkPaths: func(dir string) []string { return []string{filepath.Join(dir, "state.json")} },
		},
		{
			name: "archive is state ancestor",
			paths: func(dir string) (string, string, string) {
				archivePath := filepath.Join(dir, "x_team_app_v1.tar")
				return dir, filepath.Join(archivePath, "state.json"), filepath.Join(dir, "report.json")
			},
			checkPaths: func(dir string) []string { return []string{filepath.Join(dir, "x_team_app_v1.tar")} },
		},
		{
			name: "lock is archive ancestor",
			paths: func(dir string) (string, string, string) {
				stateFile := filepath.Join(dir, "state.json")
				return pullAtomicJSONLockPath(stateFile), stateFile, filepath.Join(dir, "report.json")
			},
			checkPaths: func(dir string) []string { return []string{pullAtomicJSONLockPath(filepath.Join(dir, "state.json"))} },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			outputDir, stateFile, reportFile := test.paths(dir)
			pullCalled := false
			existsCalled := false
			_, err := runPullBatchWithDeps(context.Background(), PullBatchOptions{
				Images:      []string{"x/team/app:v1"},
				OutputDir:   outputDir,
				StateFile:   stateFile,
				ReportFile:  reportFile,
				Concurrency: 1,
			}, func(string, PullOptions) error {
				pullCalled = true
				return nil
			}, func(context.Context, string, string, PullOptions) (bool, error) {
				existsCalled = true
				return false, nil
			})
			if err == nil || !strings.Contains(err.Error(), "路径拓扑冲突") {
				t.Fatalf("runPullBatchWithDeps() error = %v, want topology collision", err)
			}
			if pullCalled || existsCalled {
				t.Fatalf("callbacks after topology collision: pull=%v exists=%v", pullCalled, existsCalled)
			}
			for _, path := range test.checkPaths(dir) {
				if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("collision path %q stat error = %v, want not exist", path, statErr)
				}
			}
		})
	}
}

func TestReadPullBatchStateRejectsReplacementAfterLifecycleLock(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(statePath, []byte(`{"items":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	opts := PullBatchOptions{OutputDir: dir, StateFile: statePath}
	if _, err := preparePullBatchPlan(&opts, []string{"busybox:latest"}); err != nil {
		t.Fatal(err)
	}
	locks, err := acquirePullBatchLifecycleLocks(context.Background(), opts.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	defer releasePullBatchLifecycleLocks(locks)

	victimPath := filepath.Join(dir, "victim.json")
	if err := os.WriteFile(victimPath, []byte(`{"items":{"secret":{"status":"success"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victimPath, statePath); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if _, err := readPullBatchState(statePath); err == nil {
		t.Fatal("readPullBatchState() error = nil, want post-lock symlink replacement rejection")
	}
	assertPullBatchFileContent(t, victimPath, `{"items":{"secret":{"status":"success"}}}`)
}

func TestReadPullBatchStateRejectsOversizedRegularFile(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	file, err := os.OpenFile(statePath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(pullBatchStateMaxBytes + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readPullBatchState(statePath); err == nil || !strings.Contains(err.Error(), "超过") {
		t.Fatalf("readPullBatchState() error = %v, want size-limit rejection", err)
	}
}

func TestRunPullBatchPublishesPartialReportOnDeadline(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "report.json")
	ctx := newPullBatchManualDeadlineContext()
	report, err := runPullBatchWithDeps(ctx, PullBatchOptions{
		Images:      []string{"busybox:latest"},
		OutputDir:   dir,
		StateFile:   filepath.Join(dir, "state.json"),
		ReportFile:  reportPath,
		Concurrency: 1,
	}, func(_ string, opts PullOptions) error {
		ctx.expire()
		<-opts.Context.Done()
		return opts.Context.Err()
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		return false, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runPullBatchWithDeps() error = %v, want deadline exceeded", err)
	}
	if report.Failed != 1 {
		t.Fatalf("report failed = %d, want 1", report.Failed)
	}
	assertPullBatchJSON(t, reportPath)
}

func TestRunPullBatchStateWriteFailureRemainsFailedAfterLaterPublish(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	call := 0
	report, err := runPullBatchWithDeps(context.Background(), PullBatchOptions{
		Images:      []string{"one:latest", "two:latest"},
		OutputDir:   filepath.Join(dir, "archives"),
		StateFile:   statePath,
		Concurrency: 1,
	}, func(image string, opts PullOptions) error {
		call++
		if call == 1 {
			if err := os.Mkdir(statePath, 0700); err != nil {
				return err
			}
			return writePullBatchTestArtifact(opts, image)
		}
		if err := os.Remove(statePath); err != nil {
			return err
		}
		return writePullBatchTestArtifact(opts, image)
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		return false, nil
	})
	if err == nil || report.Failed != 1 {
		t.Fatalf("runPullBatchWithDeps() error/report = %v/%#v, want one state-write failure", err, report)
	}
	state, readErr := readPullBatchState(statePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if report.Items[0].Status != pullBatchStatusFailed || state.Items["one:latest"].Status != pullBatchStatusFailed {
		t.Fatalf("first item report/state = %#v/%#v, want failed", report.Items[0], state.Items["one:latest"])
	}
	if report.Items[1].Status != pullBatchStatusSuccess || state.Items["two:latest"].Status != pullBatchStatusSuccess {
		t.Fatalf("second item report/state = %#v/%#v, want success", report.Items[1], state.Items["two:latest"])
	}
}

func TestRunPullBatchRejectsHardLinkedClaimsBeforeCallbacksOrWrites(t *testing.T) {
	tests := []struct {
		name   string
		images []string
		paths  func(dir string) (state, report, alias string)
	}{
		{
			name:   "state and report",
			images: []string{"busybox:latest"},
			paths: func(dir string) (string, string, string) {
				return filepath.Join(dir, "state.json"), filepath.Join(dir, "report.json"), ""
			},
		},
		{
			name:   "state and archive",
			images: []string{"x/team/app:v1"},
			paths: func(dir string) (string, string, string) {
				archivePath := filepath.Join(dir, "x_team_app_v1.tar")
				return filepath.Join(dir, "state.json"), "", archivePath
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			statePath, reportPath, aliasPath := test.paths(dir)
			if err := os.WriteFile(statePath, []byte("original-metadata"), 0600); err != nil {
				t.Fatal(err)
			}
			linkPath := reportPath
			if linkPath == "" {
				linkPath = aliasPath
			}
			if err := os.Link(statePath, linkPath); err != nil {
				t.Skipf("hard-link creation unavailable: %v", err)
			}
			before, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			pullCalled := false
			existsCalled := false
			_, err = runPullBatchWithDeps(context.Background(), PullBatchOptions{
				Images:      test.images,
				OutputDir:   dir,
				StateFile:   statePath,
				ReportFile:  reportPath,
				Concurrency: 1,
			}, func(string, PullOptions) error {
				pullCalled = true
				return nil
			}, func(context.Context, string, string, PullOptions) (bool, error) {
				existsCalled = true
				return false, nil
			})
			if err == nil || !strings.Contains(err.Error(), "文件身份冲突") {
				t.Fatalf("runPullBatchWithDeps() error = %v, want hard-link identity collision", err)
			}
			if pullCalled || existsCalled {
				t.Fatalf("callbacks after hard-link collision: pull=%v exists=%v", pullCalled, existsCalled)
			}
			assertPullBatchFileContent(t, statePath, "original-metadata")
			assertPullBatchFileContent(t, linkPath, "original-metadata")
			after, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(before) {
				t.Fatalf("directory entry count after rejected plan = %d, want %d", len(after), len(before))
			}
		})
	}
}

func TestRunPullBatchRejectsMetadataLockPathCollision(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	reportPath := pullAtomicJSONLockPath(statePath)
	pullCalled := false
	_, err := runPullBatchWithDeps(context.Background(), PullBatchOptions{
		Images:      []string{"busybox:latest"},
		OutputDir:   dir,
		StateFile:   statePath,
		ReportFile:  reportPath,
		Concurrency: 1,
	}, func(string, PullOptions) error {
		pullCalled = true
		return nil
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		return false, nil
	})
	if err == nil || !strings.Contains(err.Error(), "输出路径冲突") {
		t.Fatalf("runPullBatchWithDeps() error = %v, want lock helper path collision", err)
	}
	if pullCalled {
		t.Fatal("pull callback called after lock helper path collision")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("outputs after lock helper path collision = %#v, want none", entries)
	}
}

func TestRunPullBatchRejectsStateUntrustedMarkerPathCollisions(t *testing.T) {
	tests := []struct {
		name       string
		reportPath func(string) string
	}{
		{
			name: "marker",
			reportPath: func(statePath string) string {
				return pullBatchStateUntrustedMarkerPath(statePath)
			},
		},
		{
			name: "marker lock",
			reportPath: func(statePath string) string {
				return pullAtomicJSONLockPath(pullBatchStateUntrustedMarkerPath(statePath))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			statePath := filepath.Join(dir, "state.json")
			pullCalled := false
			_, err := runPullBatchWithDeps(context.Background(), PullBatchOptions{
				Images:      []string{"busybox:latest"},
				OutputDir:   dir,
				StateFile:   statePath,
				ReportFile:  test.reportPath(statePath),
				Concurrency: 1,
			}, func(string, PullOptions) error {
				pullCalled = true
				return nil
			}, func(context.Context, string, string, PullOptions) (bool, error) {
				return false, nil
			})
			if err == nil || !strings.Contains(err.Error(), "输出路径冲突") {
				t.Fatalf("runPullBatchWithDeps() error = %v, want marker collision", err)
			}
			if pullCalled {
				t.Fatal("pull callback called after marker collision")
			}
		})
	}
}

func TestRunPullBatchRejectsUnsafeStateUntrustedMarkerBeforeCallbacks(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		statePath := filepath.Join(dir, "state.json")
		markerPath := pullBatchStateUntrustedMarkerPath(statePath)
		if err := os.Mkdir(markerPath, 0700); err != nil {
			t.Fatal(err)
		}
		pullCalled := false
		_, err := runPullBatchWithDeps(context.Background(), PullBatchOptions{
			Images: []string{"busybox:latest"}, OutputDir: dir, StateFile: statePath, Concurrency: 1,
		}, func(string, PullOptions) error {
			pullCalled = true
			return nil
		}, func(context.Context, string, string, PullOptions) (bool, error) {
			return false, nil
		})
		if err == nil || !strings.Contains(err.Error(), "非普通") {
			t.Fatalf("runPullBatchWithDeps() error = %v, want non-regular marker rejection", err)
		}
		if pullCalled {
			t.Fatal("pull callback called after non-regular marker rejection")
		}
	})

	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		statePath := filepath.Join(dir, "state.json")
		markerPath := pullBatchStateUntrustedMarkerPath(statePath)
		victimPath := filepath.Join(dir, "victim")
		if err := os.WriteFile(victimPath, []byte("victim-must-survive"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victimPath, markerPath); err != nil {
			t.Skipf("symlink creation unavailable: %v", err)
		}
		pullCalled := false
		_, err := runPullBatchWithDeps(context.Background(), PullBatchOptions{
			Images: []string{"busybox:latest"}, OutputDir: dir, StateFile: statePath, Concurrency: 1,
		}, func(string, PullOptions) error {
			pullCalled = true
			return nil
		}, func(context.Context, string, string, PullOptions) (bool, error) {
			return false, nil
		})
		if err == nil {
			t.Fatal("runPullBatchWithDeps() error = nil, want marker symlink rejection")
		}
		if pullCalled {
			t.Fatal("pull callback called after marker symlink rejection")
		}
		assertPullBatchFileContent(t, victimPath, "victim-must-survive")
	})
}

func TestRunPullBatchRejectsUntrustedMarkerOwnedByAnotherStateBeforeCallbacks(t *testing.T) {
	tests := []struct {
		name         string
		ownerName    string
		currentName  string
		createStates bool
	}{
		{name: "both state files missing", ownerName: "owner.json", currentName: "current.json"},
		{name: "different existing state files", ownerName: "owner.json", currentName: "current.json", createStates: true},
		{name: "ordinal-distinct kelvin", ownerName: "state-K.json", currentName: "state-\u212a.json", createStates: true},
		{name: "ordinal-distinct supplementary", ownerName: "state-\U00010400.json", currentName: "state-\U00010428.json", createStates: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			ownerPath := filepath.Join(dir, test.ownerName)
			statePath := filepath.Join(dir, test.currentName)
			if test.createStates {
				if err := os.WriteFile(ownerPath, []byte(`{"items":{}}`), 0600); err != nil {
					t.Skipf("owner state name unavailable: %v", err)
				}
				if err := os.WriteFile(statePath, []byte(`{"items":{}}`), 0600); err != nil {
					t.Skipf("current state name unavailable: %v", err)
				}
				ownerInfo, ownerErr := os.Lstat(ownerPath)
				currentInfo, currentErr := os.Lstat(statePath)
				if ownerErr != nil || currentErr != nil {
					t.Fatal(errors.Join(ownerErr, currentErr))
				}
				if os.SameFile(ownerInfo, currentInfo) {
					t.Skip("test directory treats the two state names as one file")
				}
			}
			marker, err := newPullBatchStateUntrustedMarker(ownerPath)
			if err != nil {
				t.Fatal(err)
			}
			markerData, err := json.Marshal(marker)
			if err != nil {
				t.Fatal(err)
			}
			markerPath := pullBatchStateUntrustedMarkerPath(statePath)
			if err := writeAtomicJSON(markerPath, markerData); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(markerPath)
			if err != nil {
				t.Fatal(err)
			}
			pullCalled := false
			existsCalled := false
			_, err = runPullBatchWithDeps(context.Background(), PullBatchOptions{
				Images: []string{"busybox:latest"}, OutputDir: filepath.Join(dir, "archives"), StateFile: statePath, Concurrency: 1,
			}, func(string, PullOptions) error {
				pullCalled = true
				return nil
			}, func(context.Context, string, string, PullOptions) (bool, error) {
				existsCalled = true
				return false, nil
			})
			if err == nil || !strings.Contains(err.Error(), "属于另一状态文件") {
				t.Fatalf("runPullBatchWithDeps() error = %v, want marker ownership rejection", err)
			}
			if pullCalled || existsCalled {
				t.Fatalf("callbacks after marker ownership rejection: pull=%v exists=%v", pullCalled, existsCalled)
			}
			after, readErr := os.ReadFile(markerPath)
			if readErr != nil || string(after) != string(before) {
				t.Fatalf("marker after rejection = %q/%v, want unchanged %q", after, readErr, before)
			}
		})
	}
}

func TestRunPullBatchRejectsMalformedOrOversizedStateMarkerBeforeCallbacks(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{name: "malformed", data: []byte(`{"version":`), want: "解析"},
		{name: "oversized", data: []byte(strings.Repeat("x", int(pullBatchStateMarkerMaxBytes+1))), want: "超过"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			statePath := filepath.Join(dir, "state.json")
			markerPath := pullBatchStateUntrustedMarkerPath(statePath)
			if err := os.WriteFile(markerPath, test.data, 0600); err != nil {
				t.Fatal(err)
			}
			pullCalled := false
			_, err := runPullBatchWithDeps(context.Background(), PullBatchOptions{
				Images: []string{"busybox:latest"}, OutputDir: dir, StateFile: statePath, Concurrency: 1,
			}, func(string, PullOptions) error {
				pullCalled = true
				return nil
			}, func(context.Context, string, string, PullOptions) (bool, error) {
				return false, nil
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("runPullBatchWithDeps() error = %v, want %q", err, test.want)
			}
			if pullCalled {
				t.Fatal("pull callback called after invalid marker")
			}
		})
	}
}

func TestRunPullBatchDoesNotClearReplacedStateMarker(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	markerPath := pullBatchStateUntrustedMarkerPath(statePath)
	replacement := []byte("replacement-marker-must-survive")
	report, err := runPullBatchWithDepsAndPersistence(context.Background(), PullBatchOptions{
		Images: []string{"busybox:latest"}, OutputDir: dir, StateFile: statePath, Concurrency: 1,
	}, func(_ string, opts PullOptions) error {
		return writePullBatchTestArtifact(opts, "archive")
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		return false, nil
	}, pullBatchPersistence{
		writeState: func(ctx context.Context, path string, state pullBatchState) error {
			if err := writePullBatchStateWhileLocked(ctx, path, state); err != nil {
				return err
			}
			if err := os.Remove(markerPath); err != nil {
				return err
			}
			return os.WriteFile(markerPath, replacement, 0600)
		},
	})
	if err == nil || report.Failed != 1 || !strings.Contains(err.Error(), "被替换") {
		t.Fatalf("run error/report = %v/%#v, want replaced marker failure", err, report)
	}
	assertPullBatchFileContent(t, markerPath, string(replacement))
}

func TestPullArchiveRejectsInternalOutputNamespaces(t *testing.T) {
	tests := []string{
		".DM-PULL-state-untrusted.marker",
		".Docker-Manager-Pull-forged.tmp",
	}
	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			sourceDir := filepath.Join(dir, "source")
			if err := os.Mkdir(sourceDir, 0700); err != nil {
				t.Fatal(err)
			}
			outputPath := filepath.Join(dir, name)
			if err := os.WriteFile(outputPath, []byte("reserved-output-must-survive"), 0600); err != nil {
				t.Fatal(err)
			}
			err := createTarArchiveWithContext(context.Background(), sourceDir, outputPath)
			if err == nil || !strings.Contains(err.Error(), "保留命名空间") {
				t.Fatalf("createTarArchiveWithContext() error = %v, want reserved namespace rejection", err)
			}
			assertPullBatchFileContent(t, outputPath, "reserved-output-must-survive")
		})
	}
}

func TestRunPullBatchRejectsCrossDirectoryReservedReportBeforeCallbacks(t *testing.T) {
	root := t.TempDir()
	metadataDir := filepath.Join(root, "metadata")
	if err := os.Mkdir(metadataDir, 0700); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(metadataDir, ".DM-PULL-state-untrusted.marker")
	if err := os.WriteFile(reportPath, []byte("reserved-report-must-survive"), 0600); err != nil {
		t.Fatal(err)
	}
	pullCalled := false
	existsCalled := false
	_, err := runPullBatchWithDeps(context.Background(), PullBatchOptions{
		Images:      []string{"busybox:latest"},
		OutputDir:   filepath.Join(root, "archives"),
		StateFile:   filepath.Join(root, "state", "state.json"),
		ReportFile:  reportPath,
		Concurrency: 1,
	}, func(string, PullOptions) error {
		pullCalled = true
		return nil
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		existsCalled = true
		return false, nil
	})
	if err == nil || !strings.Contains(err.Error(), "保留命名空间") {
		t.Fatalf("runPullBatchWithDeps() error = %v, want reserved report rejection", err)
	}
	if pullCalled || existsCalled {
		t.Fatalf("callbacks after reserved report rejection: pull=%v exists=%v", pullCalled, existsCalled)
	}
	assertPullBatchFileContent(t, reportPath, "reserved-report-must-survive")
}

func TestRunPullBatchLifecycleLockRejectsSecondBatchBeforeCallbacks(t *testing.T) {
	dir := t.TempDir()
	opts := PullBatchOptions{
		Images:      []string{"busybox:latest"},
		OutputDir:   dir,
		StateFile:   filepath.Join(dir, "state.json"),
		ReportFile:  filepath.Join(dir, "report.json"),
		Concurrency: 1,
	}
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, err := runPullBatchWithDeps(context.Background(), opts, func(image string, pullOpts PullOptions) error {
			close(firstStarted)
			<-releaseFirst
			return writePullBatchTestArtifact(pullOpts, image)
		}, func(context.Context, string, string, PullOptions) (bool, error) {
			return false, nil
		})
		firstDone <- err
	}()
	select {
	case <-firstStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for first batch callback")
	}

	secondPullCalled := false
	secondExistsCalled := false
	_, secondErr := runPullBatchWithDeps(context.Background(), opts, func(string, PullOptions) error {
		secondPullCalled = true
		return nil
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		secondExistsCalled = true
		return false, nil
	})
	if secondErr == nil || !strings.Contains(secondErr.Error(), "正在被另一进程使用") {
		t.Fatalf("second runPullBatchWithDeps() error = %v, want lifecycle lock rejection", secondErr)
	}
	if secondPullCalled || secondExistsCalled {
		t.Fatalf("second callbacks after lifecycle lock rejection: pull=%v exists=%v", secondPullCalled, secondExistsCalled)
	}
	if _, err := os.Lstat(opts.StateFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state file before first release stat error = %v, want not exist", err)
	}
	if _, err := os.Lstat(opts.ReportFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("report file before first release stat error = %v, want not exist", err)
	}

	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first runPullBatchWithDeps() error = %v", err)
	}
	assertPullBatchJSON(t, opts.StateFile)
	assertPullBatchJSON(t, opts.ReportFile)
}

func TestPullBatchLifecycleLockCannotBeBypassedByMarkerReplacement(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	locks, err := acquirePullBatchLifecycleLocks(context.Background(), statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer releasePullBatchLifecycleLocks(locks)
	markerPath := pullBatchLifecycleLockPath(dir)
	removeErr := os.Remove(markerPath)
	if runtime.GOOS == "windows" {
		if removeErr == nil {
			t.Fatal("Windows lifecycle marker removal succeeded while no-delete handle was open")
		}
	} else if removeErr != nil {
		t.Fatalf("remove lifecycle marker error = %v", removeErr)
	}

	second, secondErr := acquirePullBatchLifecycleLocks(context.Background(), statePath)
	if secondErr == nil {
		releasePullBatchLifecycleLocks(second)
		t.Fatal("second lifecycle lock acquired after marker replacement")
	}
	if runtime.GOOS != "windows" {
		if err := verifyPullBatchLifecycleLock(&locks[0]); err == nil {
			t.Fatal("first lifecycle lock did not detect removed marker")
		}
	}
}

func TestRunPullBatchArchiveScopeLockRejectsDifferentMetadataBatch(t *testing.T) {
	dir := t.TempDir()
	firstOpts := PullBatchOptions{
		Images:      []string{"busybox:latest"},
		OutputDir:   dir,
		StateFile:   filepath.Join(dir, "first-state.json"),
		ReportFile:  filepath.Join(dir, "first-report.json"),
		Concurrency: 1,
	}
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, err := runPullBatchWithDeps(context.Background(), firstOpts, func(image string, pullOpts PullOptions) error {
			close(firstStarted)
			<-releaseFirst
			return writePullBatchTestArtifact(pullOpts, image)
		}, func(context.Context, string, string, PullOptions) (bool, error) {
			return false, nil
		})
		firstDone <- err
	}()
	select {
	case <-firstStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for first batch callback")
	}

	secondPullCalled := false
	secondOpts := firstOpts
	secondOpts.StateFile = filepath.Join(dir, "second-state.json")
	secondOpts.ReportFile = filepath.Join(dir, "second-report.json")
	_, secondErr := runPullBatchWithDeps(context.Background(), secondOpts, func(string, PullOptions) error {
		secondPullCalled = true
		return nil
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		return false, nil
	})
	if secondErr == nil || !strings.Contains(secondErr.Error(), "正在被另一进程使用") {
		t.Fatalf("second runPullBatchWithDeps() error = %v, want archive-scope lock rejection", secondErr)
	}
	if secondPullCalled {
		t.Fatal("second pull callback called despite archive-scope lock")
	}
	for _, path := range []string{secondOpts.StateFile, secondOpts.ReportFile} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("second metadata path %q stat error = %v, want not exist", path, err)
		}
	}

	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first runPullBatchWithDeps() error = %v", err)
	}
}

func TestRunPullBatchArchiveScopeRejectsStandaloneOverwrite(t *testing.T) {
	dir := t.TempDir()
	batchSource := writePullArchiveSource(t, "batch")
	standaloneSource := writePullArchiveSource(t, "standalone")
	archivePath := pullBatchTestArchivePath(t, dir, "busybox:latest")
	archivePublished := make(chan struct{})
	releaseBatch := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := runPullBatchWithDeps(context.Background(), PullBatchOptions{
			Images:      []string{"busybox:latest"},
			OutputDir:   dir,
			StateFile:   filepath.Join(dir, "state.json"),
			ReportFile:  filepath.Join(dir, "report.json"),
			Concurrency: 1,
		}, func(_ string, opts PullOptions) error {
			if err := createTarArchiveWithContext(opts.Context, batchSource, opts.Output); err != nil {
				return err
			}
			close(archivePublished)
			<-releaseBatch
			return nil
		}, func(context.Context, string, string, PullOptions) (bool, error) {
			return false, nil
		})
		done <- err
	}()
	select {
	case <-archivePublished:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for batch archive publication")
	}
	standaloneErr := createTarArchiveWithContext(context.Background(), standaloneSource, archivePath)
	if standaloneErr == nil || !strings.Contains(standaloneErr.Error(), "正在被另一进程使用") {
		close(releaseBatch)
		t.Fatalf("standalone overwrite error = %v, want archive scope rejection", standaloneErr)
	}
	close(releaseBatch)
	if err := <-done; err != nil {
		t.Fatalf("runPullBatchWithDeps() error = %v", err)
	}
	if marker := readPullArchiveMarker(t, archivePath); marker != "batch" {
		t.Fatalf("archive marker = %q, want batch", marker)
	}
}

func TestRunPullBatchDeadlinePreservesContextErrorWhenReportPublishFails(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "report.json")
	ctx := newPullBatchManualDeadlineContext()
	_, err := runPullBatchWithDeps(ctx, PullBatchOptions{
		Images:      []string{"busybox:latest"},
		OutputDir:   filepath.Join(dir, "archives"),
		StateFile:   filepath.Join(dir, "state.json"),
		ReportFile:  reportPath,
		Concurrency: 1,
	}, func(_ string, opts PullOptions) error {
		ctx.expire()
		<-opts.Context.Done()
		if mkdirErr := os.Mkdir(reportPath, 0700); mkdirErr != nil {
			return mkdirErr
		}
		return opts.Context.Err()
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		return false, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "写入批量报告失败") {
		t.Fatalf("runPullBatchWithDeps() error = %v, want joined deadline/report error", err)
	}
}

func TestRunPullBatchDeadlineReportsEveryPlannedItem(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "report.json")
	images := []string{"one/app:v1", "two/worker:v2", "three/job:v3"}
	ctx := newPullBatchManualDeadlineContext()
	pullCalls := 0
	report, err := runPullBatchWithDeps(ctx, PullBatchOptions{
		Images:      images,
		OutputDir:   filepath.Join(dir, "archives"),
		StateFile:   filepath.Join(dir, "state.json"),
		ReportFile:  reportPath,
		Concurrency: 1,
	}, func(_ string, opts PullOptions) error {
		pullCalls++
		ctx.expire()
		<-opts.Context.Done()
		return opts.Context.Err()
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		return false, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runPullBatchWithDeps() error = %v, want context deadline", err)
	}
	if pullCalls != 1 {
		t.Fatalf("pull calls = %d, want only the first scheduled item", pullCalls)
	}
	assertCompleteReport := func(label string, got PullBatchReport) {
		t.Helper()
		if got.Total != len(images) || got.Failed != len(images) || got.Succeeded != 0 || got.Skipped != 0 {
			t.Fatalf("%s counts = total:%d success:%d skipped:%d failed:%d, want %d/0/0/%d", label, got.Total, got.Succeeded, got.Skipped, got.Failed, len(images), len(images))
		}
		if len(got.Items) != len(images) {
			t.Fatalf("%s item count = %d, want %d", label, len(got.Items), len(images))
		}
		for index, item := range got.Items {
			if item.Image != images[index] || item.Status != pullBatchStatusFailed || item.Message == "" {
				t.Fatalf("%s item %d = %#v, want named failed item with message", label, index, item)
			}
			if index > 0 && (item.StartedAt != "" || !strings.Contains(item.Message, context.DeadlineExceeded.Error())) {
				t.Fatalf("%s unstarted item %d = %#v, want unscheduled deadline failure", label, index, item)
			}
		}
	}
	assertCompleteReport("returned report", report)

	data, readErr := os.ReadFile(reportPath)
	if readErr != nil {
		t.Fatalf("read persisted report: %v", readErr)
	}
	var persisted PullBatchReport
	if unmarshalErr := json.Unmarshal(data, &persisted); unmarshalErr != nil {
		t.Fatalf("decode persisted report: %v", unmarshalErr)
	}
	assertCompleteReport("persisted report", persisted)
}

func TestRunPullBatchRechecksContextAfterReportPersistence(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reportCalled := false
	_, err := runPullBatchWithDepsAndPersistence(ctx, PullBatchOptions{
		Images:      []string{"busybox:latest"},
		OutputDir:   dir,
		StateFile:   filepath.Join(dir, "state.json"),
		ReportFile:  filepath.Join(dir, "report.json"),
		Concurrency: 1,
	}, func(image string, opts PullOptions) error {
		return writePullBatchTestArtifact(opts, image)
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		return false, nil
	}, pullBatchPersistence{
		writeState: writePullBatchStateWhileLocked,
		writeReport: func(context.Context, string, PullBatchReport) error {
			reportCalled = true
			cancel()
			return nil
		},
	})
	if !reportCalled || !errors.Is(err, context.Canceled) {
		t.Fatalf("reportCalled/error = %v/%v, want post-report context.Canceled", reportCalled, err)
	}
}

func TestRunPullBatchJoinsStatePersistenceAndContextErrors(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stateErr := errors.New("state persistence unavailable")
	reportCalled := false
	_, err := runPullBatchWithDepsAndPersistence(ctx, PullBatchOptions{
		Images:      []string{"busybox:latest"},
		OutputDir:   dir,
		StateFile:   filepath.Join(dir, "state.json"),
		ReportFile:  filepath.Join(dir, "report.json"),
		Concurrency: 1,
	}, func(image string, opts PullOptions) error {
		return writePullBatchTestArtifact(opts, image)
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		return false, nil
	}, pullBatchPersistence{
		writeState: func(context.Context, string, pullBatchState) error {
			cancel()
			return stateErr
		},
		writeReport: func(context.Context, string, PullBatchReport) error {
			reportCalled = true
			return nil
		},
	})
	if !errors.Is(err, context.Canceled) || !errors.Is(err, stateErr) {
		t.Fatalf("runPullBatchWithDepsAndPersistence() error = %v, want joined cancel/state errors", err)
	}
	if reportCalled {
		t.Fatal("report persistence ran after context cancellation")
	}
}

func TestRunPullBatchUsesPrecomputedAbsoluteArchivePaths(t *testing.T) {
	dir := t.TempDir()
	var outputs []string
	_, err := runPullBatchWithDeps(context.Background(), PullBatchOptions{
		Images:      []string{"one/app:v1", "two/worker:v2"},
		OutputDir:   dir,
		Concurrency: 1,
	}, func(image string, opts PullOptions) error {
		outputs = append(outputs, opts.Output)
		return writePullBatchTestArtifact(opts, image)
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		return false, nil
	})
	if err != nil {
		t.Fatalf("runPullBatchWithDeps() error = %v", err)
	}
	want := []string{
		filepath.Join(dir, "one_app_v1.tar"),
		filepath.Join(dir, "two_worker_v2.tar"),
	}
	if len(outputs) != len(want) {
		t.Fatalf("archive output count = %d, want %d: %#v", len(outputs), len(want), outputs)
	}
	for index := range want {
		if outputs[index] != want[index] || !filepath.IsAbs(outputs[index]) {
			t.Fatalf("archive output %d = %q, want absolute %q", index, outputs[index], want[index])
		}
	}
}

func TestPullBatchPersistentMessagesHonorRedactionProfile(t *testing.T) {
	previous := sensitive.DefaultProfile()
	t.Cleanup(func() { sensitive.SetDefaultProfile(previous) })

	tests := []struct {
		profile sensitive.Profile
		leaks   bool
	}{
		{profile: sensitive.ProfileNone, leaks: true},
		{profile: sensitive.ProfileBasic},
		{profile: sensitive.ProfileStrict},
	}
	for _, test := range tests {
		t.Run(string(test.profile), func(t *testing.T) {
			sensitive.SetDefaultProfile(test.profile)
			dir := t.TempDir()
			statePath := filepath.Join(dir, "state.json")
			reportPath := filepath.Join(dir, "report.json")
			const message = "registry failed: password=state-secret token=report-secret"
			state := pullBatchState{Items: map[string]pullBatchStateItem{
				"app:v1": {Image: "app:v1", Status: pullBatchStatusFailed, Message: message},
			}}
			report := PullBatchReport{Items: []PullBatchResult{{Image: "app:v1", Status: pullBatchStatusFailed, Message: message}}}
			if err := writePullBatchState(statePath, state); err != nil {
				t.Fatal(err)
			}
			if err := writePullBatchReport(reportPath, report); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{statePath, reportPath} {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				containsSecrets := strings.Contains(string(data), "state-secret") || strings.Contains(string(data), "report-secret")
				if containsSecrets != test.leaks {
					t.Fatalf("profile %q persisted data = %s, secret presence=%v want %v", test.profile, data, containsSecrets, test.leaks)
				}
				if !test.leaks && !strings.Contains(string(data), "redacted") {
					t.Fatalf("profile %q persisted data lacks redaction marker: %s", test.profile, data)
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0600 {
					t.Fatalf("profile %q permissions = %04o, want 0600", test.profile, got)
				}
			}
			if state.Items["app:v1"].Message != message || report.Items[0].Message != message {
				t.Fatal("persistence redaction mutated in-memory state or report")
			}
		})
	}
}

func assertPullBatchJSON(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("invalid JSON %q: %v", data, err)
	}
}

func assertPullBatchFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("file content = %q, want %q", data, want)
	}
}

func assertNoPullBatchJSONStaging(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), pullArchiveStagingPrefix) {
			t.Fatalf("private JSON staging was not cleaned: %s", entry.Name())
		}
	}
}
