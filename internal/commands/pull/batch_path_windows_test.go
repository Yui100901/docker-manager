//go:build windows

package pull

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestRunPullBatchWindowsResumeDetectsUntrustedMarkerAcrossStatePathCase(t *testing.T) {
	dir := t.TempDir()
	upperStatePath := filepath.Join(dir, "State.json")
	lowerStatePath := filepath.Join(dir, "state.json")
	if upperMarker, lowerMarker := pullBatchStateUntrustedMarkerPath(upperStatePath), pullBatchStateUntrustedMarkerPath(lowerStatePath); upperMarker != lowerMarker {
		t.Fatalf("case-alias marker paths differ: %q != %q", upperMarker, lowerMarker)
	}
	opts := PullBatchOptions{
		Images:      []string{"busybox:latest"},
		OutputDir:   dir,
		StateFile:   upperStatePath,
		Concurrency: 1,
		platform:    targetPlatform{targetOS: "linux", targetArch: "amd64"},
	}
	syncErr := errors.New("injected state directory sync failure")
	_, err := runPullBatchWithDepsAndPersistence(context.Background(), opts, func(_ string, pullOpts PullOptions) error {
		return writePullBatchTestArtifact(pullOpts, "first archive")
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		return false, nil
	}, pullBatchPersistence{
		writeState: func(ctx context.Context, path string, state pullBatchState) error {
			return writePullBatchStateWhileLockedWithSync(ctx, path, state, func(*os.Root) error {
				return syncErr
			})
		},
	})
	if !errors.Is(err, syncErr) {
		t.Fatalf("first run error = %v, want injected sync failure", err)
	}

	opts.StateFile = lowerStatePath
	opts.Resume = true
	pullCalls := 0
	report, err := runPullBatchWithDeps(context.Background(), opts, func(_ string, pullOpts PullOptions) error {
		pullCalls++
		return writePullBatchTestArtifact(pullOpts, "second archive")
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		return false, nil
	})
	if err != nil {
		t.Fatalf("second run error = %v", err)
	}
	if pullCalls != 1 || report.Succeeded != 1 || report.Skipped != 0 {
		t.Fatalf("second run pullCalls/report = %d/%#v, want one recovery pull", pullCalls, report)
	}
}

func TestRunPullBatchRejectsWindowsUnsafeStatePathsBeforeCallbacks(t *testing.T) {
	tests := []struct {
		name      string
		stateName string
		want      string
	}{
		{name: "archive alias with trailing dot", stateName: "busybox_latest.tar.", want: "点或空格结尾"},
		{name: "trailing space", stateName: "state.json ", want: "点或空格结尾"},
		{name: "device name", stateName: "NUL.json", want: "设备名"},
		{name: "future leaf short alias", stateName: "BUSYBO~1.TAR", want: "8.3"},
		{name: "future non ASCII short alias", stateName: "测试测~1", want: "8.3"},
		{name: "future ancestor short alias", stateName: filepath.Join("METADA~1", "state.json"), want: "8.3"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			pullCalled := false
			existsCalled := false
			_, err := runPullBatchWithDeps(context.Background(), PullBatchOptions{
				Images:       []string{"busybox:latest"},
				OutputDir:    dir,
				StateFile:    filepath.Join(dir, test.stateName),
				To:           "registry.example/mirror",
				SkipExisting: true,
				Concurrency:  1,
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
				t.Fatalf("callbacks after unsafe Windows path: pull=%v exists=%v", pullCalled, existsCalled)
			}
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("files written after unsafe Windows path rejection: %#v", entries)
			}
		})
	}
}

func TestValidatePullBatchWindowsPathRejectsNamespacesAndUnsafeUNCVolumes(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "global root", path: `\\?\GLOBALROOT\Device\HarddiskVolumeShadowCopy1\state.json`, want: "命名空间"},
		{name: "extended drive", path: `\\?\C:\output\state.json`, want: "命名空间"},
		{name: "extended UNC", path: `\\?\UNC\server\share\state.json`, want: "命名空间"},
		{name: "UNC server trailing dot", path: `\\server.\share\state.json`, want: "点或空格结尾"},
		{name: "UNC share trailing space", path: `\\server\share \state.json`, want: "点或空格结尾"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validatePullBatchPlatformPath(test.path)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validatePullBatchPlatformPath(%q) error = %v, want %q", test.path, err, test.want)
			}
		})
	}
}

func TestPullBatchWindowsPathComparisonUsesOrdinalIgnoreCase(t *testing.T) {
	equivalent := [][2]string{
		{"A", "a"},
		{"\u03a3", "\u03c3"},
	}
	for _, pair := range equivalent {
		if !pullBatchPathsEqual(pair[0], pair[1]) {
			t.Fatalf("pullBatchPathsEqual(%q, %q) = false, want Windows ordinal equality", pair[0], pair[1])
		}
	}
	distinct := [][2]string{
		{"\u03c3", "\u03c2"},
		{"K", "\u212a"},
		{"S", "\u017f"},
		{"\u00c5", "\u212b"},
	}
	for _, pair := range distinct {
		if pullBatchPathsEqual(pair[0], pair[1]) {
			t.Fatalf("pullBatchPathsEqual(%q, %q) = true, want Windows ordinal distinction", pair[0], pair[1])
		}
	}

	dir := t.TempDir()
	if pullBatchPathContains(filepath.Join(dir, "K"), filepath.Join(dir, "\u212a", "state.json")) {
		t.Fatal("pullBatchPathContains() used Unicode folding beyond Windows ordinal rules")
	}
	if !pullBatchPathContains(filepath.Join(dir, "\u03a3"), filepath.Join(dir, "\u03c3", "state.json")) {
		t.Fatal("pullBatchPathContains() missed a Windows ordinal case-equivalent ancestor")
	}
}

func TestPullBatchWindowsOrdinalComparisonFailsClosedOnAPIError(t *testing.T) {
	defer func() {
		failure := recover()
		if failure == nil || !strings.Contains(fmt.Sprint(failure), "Windows ordinal 路径比较失败") {
			t.Fatalf("ordinal comparison panic = %v, want fail-closed API error", failure)
		}
	}()
	pullBatchWindowsOrdinalCompareWith("left", "right", func([]uint16, []uint16) (uintptr, error) {
		return 0, windows.ERROR_INVALID_PARAMETER
	})
}

func TestPullBatchWindowsDirectoryCaseSensitiveResult(t *testing.T) {
	tests := []struct {
		name      string
		flags     uint32
		queryErr  error
		want      bool
		wantError bool
	}{
		{name: "disabled"},
		{name: "enabled", flags: windows.FILE_CS_FLAG_CASE_SENSITIVE_DIR, want: true},
		{name: "enabled with unknown flags", flags: windows.FILE_CS_FLAG_CASE_SENSITIVE_DIR | 0x80000000, want: true},
		{name: "invalid parameter unsupported", queryErr: windows.ERROR_INVALID_PARAMETER},
		{name: "not supported wrapped", queryErr: fmt.Errorf("query: %w", windows.ERROR_NOT_SUPPORTED)},
		{name: "invalid function unsupported", queryErr: windows.ERROR_INVALID_FUNCTION},
		{name: "access denied", queryErr: windows.ERROR_ACCESS_DENIED, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := pullBatchWindowsDirectoryCaseSensitiveWith(0, func(_ windows.Handle, info *pullBatchWindowsCaseSensitiveInfo) error {
				info.Flags = test.flags
				return test.queryErr
			})
			if (err != nil) != test.wantError {
				t.Fatalf("case-sensitive query error = %v, wantError=%v", err, test.wantError)
			}
			if got != test.want {
				t.Fatalf("case-sensitive query result = %v, want %v", got, test.want)
			}
		})
	}
}

func TestRejectPullInternalOutputAliasRejectsReservedWindowsShortName(t *testing.T) {
	dir := t.TempDir()
	markerPath := filepath.Join(dir, pullBatchStateMarkerFileName)
	if err := os.WriteFile(markerPath, []byte("marker must survive"), 0600); err != nil {
		t.Fatal(err)
	}
	shortPath, err := pullBatchTestWindowsShortPath(markerPath)
	if err != nil {
		t.Skipf("Windows 8.3 short names unavailable: %v", err)
	}
	if strings.EqualFold(filepath.Base(shortPath), filepath.Base(markerPath)) {
		t.Skip("Windows did not assign an 8.3 short leaf name to the marker")
	}
	if err := rejectPullInternalOutputName(shortPath); err == nil || !strings.Contains(err.Error(), "保留命名空间") {
		t.Fatalf("rejectPullInternalOutputName(%q) error = %v, want reserved actual-name rejection", shortPath, err)
	}
	data, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "marker must survive" {
		t.Fatalf("marker content = %q, want preserved content", data)
	}
}

func TestRunPullBatchRejectsWindowsOrdinalCollisionBeforeCallbacks(t *testing.T) {
	dir := t.TempDir()
	pullCalled := false
	existsCalled := false
	_, err := runPullBatchWithDeps(context.Background(), PullBatchOptions{
		Images:       []string{"busybox:latest"},
		OutputDir:    dir,
		StateFile:    filepath.Join(dir, "metadata-\u03a3.json"),
		ReportFile:   filepath.Join(dir, "metadata-\u03c3.json"),
		To:           "registry.example/mirror",
		SkipExisting: true,
		Concurrency:  1,
	}, func(string, PullOptions) error {
		pullCalled = true
		return nil
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		existsCalled = true
		return false, nil
	})
	if err == nil || !strings.Contains(err.Error(), "输出路径冲突") {
		t.Fatalf("runPullBatchWithDeps() error = %v, want Windows ordinal collision", err)
	}
	if pullCalled || existsCalled {
		t.Fatalf("callbacks after Windows ordinal collision: pull=%v exists=%v", pullCalled, existsCalled)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("files written after Windows ordinal collision: %#v", entries)
	}
}

func TestPreparePullBatchPlanAllowsWindowsOrdinalDistinctUnicodePaths(t *testing.T) {
	dir := t.TempDir()
	opts := PullBatchOptions{
		OutputDir:  dir,
		StateFile:  filepath.Join(dir, "metadata-K.json"),
		ReportFile: filepath.Join(dir, "metadata-\u212a.json"),
	}
	if _, err := preparePullBatchPlan(&opts, []string{"busybox:latest"}); err != nil {
		t.Fatalf("preparePullBatchPlan() rejected Windows ordinal-distinct paths: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("preparePullBatchPlan() wrote files: %#v", entries)
	}
}

func TestWindowsPathInspectionRejectsReparseBeforeTargetAccess(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "probe.json"), []byte("unreachable"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("directory symlink creation unavailable: %v", err)
	}
	targetPointer, err := windows.UTF16PtrFromString(target)
	if err != nil {
		t.Fatal(err)
	}
	targetHandle, err := windows.CreateFile(
		targetPointer,
		windows.FILE_READ_ATTRIBUTES,
		0,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		t.Skipf("exclusive directory handle unavailable: %v", err)
	}
	defer windows.CloseHandle(targetHandle)

	probePath := filepath.Join(link, "probe.json")
	for name, check := range map[string]func(string) error{
		"canonical": func(path string) error {
			_, checkErr := canonicalPullBatchWindowsPath(path)
			return checkErr
		},
		"archive ancestor": rejectPullArchiveOutputLinks,
	} {
		t.Run(name, func(t *testing.T) {
			err := check(probePath)
			if err == nil || !strings.Contains(err.Error(), "重解析点") {
				t.Fatalf("path inspection error = %v, want reparse rejection before exclusive target access", err)
			}
		})
	}
}

func TestWindowsAtomicJSONLockCannotBeDeletedOrReplacedWhileLocked(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lockName := pullAtomicJSONLockName("state.json")
	lockPath := filepath.Join(dir, lockName)
	first, firstInfo, err := openPullAtomicJSONLock(root, lockName)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if err := lockPullAtomicJSONFile(first); err != nil {
		t.Fatal(err)
	}
	defer unlockPullAtomicJSONFile(first)

	if err := os.Remove(lockPath); err == nil {
		t.Fatal("JSON lock removal succeeded while no-delete handle was open")
	}
	replacementPath := filepath.Join(dir, "replacement.lock")
	if err := os.WriteFile(replacementPath, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacementPath, lockPath); err == nil {
		t.Fatal("JSON lock replacement succeeded while no-delete handle was open")
	}
	currentInfo, err := root.Lstat(lockName)
	if err != nil {
		t.Fatal(err)
	}
	if !safePullAtomicJSONLockIdentity(firstInfo, currentInfo) {
		t.Fatal("JSON lock identity changed despite no-delete handle")
	}

	second, secondInfo, err := openPullAtomicJSONLock(root, lockName)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if !safePullAtomicJSONLockIdentity(firstInfo, secondInfo) {
		t.Fatal("second writer opened a different JSON lock identity")
	}
	acquired, err := tryLockPullAtomicJSONFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if acquired {
		_ = unlockPullAtomicJSONFile(second)
		t.Fatal("second writer acquired JSON lock while first writer held it")
	}
}

func TestRunPullBatchRejectsWindowsShortNameAncestorAliasBeforeCallbacks(t *testing.T) {
	root := t.TempDir()
	longDir := filepath.Join(root, "pull batch long directory")
	if err := os.Mkdir(longDir, 0700); err != nil {
		t.Fatal(err)
	}
	shortDir, err := pullBatchTestWindowsShortPath(longDir)
	if err != nil {
		t.Skipf("Windows 8.3 short names unavailable: %v", err)
	}
	if strings.EqualFold(filepath.Clean(shortDir), filepath.Clean(longDir)) {
		t.Skip("Windows 8.3 short names are disabled on the test volume")
	}

	pullCalled := false
	existsCalled := false
	_, err = runPullBatchWithDeps(context.Background(), PullBatchOptions{
		Images:       []string{"busybox:latest"},
		OutputDir:    longDir,
		StateFile:    filepath.Join(shortDir, "busybox_latest.tar"),
		To:           "registry.example/mirror",
		SkipExisting: true,
		Concurrency:  1,
	}, func(string, PullOptions) error {
		pullCalled = true
		return nil
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		existsCalled = true
		return false, nil
	})
	if err == nil || !strings.Contains(err.Error(), "Windows 别名冲突") {
		t.Fatalf("runPullBatchWithDeps() error = %v, want Windows alias collision", err)
	}
	if pullCalled || existsCalled {
		t.Fatalf("callbacks after Windows short-name alias collision: pull=%v exists=%v", pullCalled, existsCalled)
	}
	entries, readErr := os.ReadDir(longDir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("files written after Windows short-name alias rejection: %#v", entries)
	}
}

func pullBatchTestWindowsShortPath(path string) (string, error) {
	longPath, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	buffer := make([]uint16, 256)
	for {
		length, err := windows.GetShortPathName(longPath, &buffer[0], uint32(len(buffer)))
		if err != nil {
			return "", err
		}
		if length < uint32(len(buffer)) {
			return windows.UTF16ToString(buffer[:length]), nil
		}
		buffer = make([]uint16, length+1)
	}
}
