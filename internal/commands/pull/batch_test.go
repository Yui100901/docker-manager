package pull

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadPullBatchImagesReadsFileAndDeduplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images.txt")
	content := "# comment\n\nbusybox:latest\nteam/api:v1\nbusybox:latest\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := loadPullBatchImages([]string{"nginx:latest", "team/api:v1"}, path)
	if err != nil {
		t.Fatalf("loadPullBatchImages() error = %v", err)
	}
	want := []string{"nginx:latest", "team/api:v1", "busybox:latest"}
	if len(got) != len(want) {
		t.Fatalf("images = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("images = %#v, want %#v", got, want)
		}
	}
}

func TestRunPullBatchRetriesAndWritesState(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")
	failOnce := errors.New("temporary failure")
	attempts := map[string]int{}
	pull := func(image string, opts PullOptions) error {
		attempts[image]++
		if opts.To != "registry.local/mirror" {
			t.Fatalf("PullOptions.To = %q, want registry.local/mirror", opts.To)
		}
		if image == "team/api:v1" && attempts[image] == 1 {
			return failOnce
		}
		return nil
	}
	exists := func(ctx context.Context, image, target string, opts PullOptions) (bool, error) {
		return false, nil
	}

	report, err := runPullBatchWithDeps(context.Background(), PullBatchOptions{
		Images:         []string{"team/api:v1", "busybox:latest"},
		To:             "registry.local/mirror",
		OutputDir:      dir,
		StateFile:      stateFile,
		Concurrency:    1,
		Retries:        1,
		ProgressOutput: io.Discard,
	}, pull, exists)
	if err != nil {
		t.Fatalf("runPullBatchWithDeps() error = %v", err)
	}
	if report.Succeeded != 2 || report.Failed != 0 || report.Skipped != 0 {
		t.Fatalf("summary = success:%d failed:%d skipped:%d", report.Succeeded, report.Failed, report.Skipped)
	}
	if attempts["team/api:v1"] != 2 {
		t.Fatalf("team/api attempts = %d, want 2", attempts["team/api:v1"])
	}
	state, err := readPullBatchState(stateFile)
	if err != nil {
		t.Fatalf("readPullBatchState() error = %v", err)
	}
	if state.Items["team/api:v1"].Status != pullBatchStatusSuccess {
		t.Fatalf("state = %#v, want team/api success", state.Items["team/api:v1"])
	}
}

func TestRunPullBatchAllowsPullWithoutTarget(t *testing.T) {
	dir := t.TempDir()
	var seen []string
	pull := func(image string, opts PullOptions) error {
		seen = append(seen, image)
		if opts.To != "" {
			t.Fatalf("PullOptions.To = %q, want empty", opts.To)
		}
		if opts.Load {
			t.Fatal("PullOptions.Load = true, want false")
		}
		return nil
	}
	exists := func(ctx context.Context, image, target string, opts PullOptions) (bool, error) {
		t.Fatal("exists should not be called without --to")
		return false, nil
	}

	report, err := runPullBatchWithDeps(context.Background(), PullBatchOptions{
		Images:      []string{"busybox:latest", "alpine:latest"},
		OutputDir:   dir,
		Concurrency: 1,
		Retries:     0,
	}, pull, exists)
	if err != nil {
		t.Fatalf("runPullBatchWithDeps() error = %v", err)
	}
	if strings.Join(seen, ",") != "busybox:latest,alpine:latest" {
		t.Fatalf("seen = %#v, want both images", seen)
	}
	if report.Succeeded != 2 || report.Failed != 0 {
		t.Fatalf("report = %#v, want 2 successes", report)
	}
}

func TestRunPullBatchRejectsSkipExistingWithoutTarget(t *testing.T) {
	_, err := runPullBatchWithDeps(context.Background(), PullBatchOptions{
		Images:       []string{"busybox:latest"},
		OutputDir:    t.TempDir(),
		Concurrency:  1,
		Retries:      0,
		SkipExisting: true,
	}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "--skip-existing") {
		t.Fatalf("error = %v, want --skip-existing requires --to", err)
	}
}

func TestRunPullBatchResumeSkipsSuccessfulStateItem(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")
	target, err := resolvePullBatchTarget("busybox:latest", "registry.local/mirror")
	if err != nil {
		t.Fatal(err)
	}
	if err := writePullBatchState(stateFile, pullBatchState{Items: map[string]pullBatchStateItem{
		"busybox:latest": {Image: "busybox:latest", Target: target, Status: pullBatchStatusSuccess},
	}}); err != nil {
		t.Fatal(err)
	}
	pullCalled := false
	pull := func(image string, opts PullOptions) error {
		pullCalled = true
		return nil
	}
	exists := func(ctx context.Context, image, target string, opts PullOptions) (bool, error) {
		return false, nil
	}

	report, err := runPullBatchWithDeps(context.Background(), PullBatchOptions{
		Images:      []string{"busybox:latest"},
		To:          "registry.local/mirror",
		OutputDir:   dir,
		StateFile:   stateFile,
		Concurrency: 1,
		Retries:     0,
		Resume:      true,
	}, pull, exists)
	if err != nil {
		t.Fatalf("runPullBatchWithDeps() error = %v", err)
	}
	if pullCalled {
		t.Fatal("pull was called for resumed successful item")
	}
	if report.Skipped != 1 || report.Items[0].Status != pullBatchStatusSkipped {
		t.Fatalf("report = %#v, want skipped", report)
	}
	state, err := readPullBatchState(stateFile)
	if err != nil {
		t.Fatalf("readPullBatchState() error = %v", err)
	}
	if state.Items["busybox:latest"].Status != pullBatchStatusSuccess {
		t.Fatalf("state item = %#v, want success preserved", state.Items["busybox:latest"])
	}
}

func TestRunPullBatchSkipExistingSkipsPull(t *testing.T) {
	dir := t.TempDir()
	pullCalled := false
	pull := func(image string, opts PullOptions) error {
		pullCalled = true
		return nil
	}
	exists := func(ctx context.Context, image, target string, opts PullOptions) (bool, error) {
		if target != "registry.local/mirror/busybox:latest" {
			t.Fatalf("target = %q, want registry.local/mirror/busybox:latest", target)
		}
		return true, nil
	}

	report, err := runPullBatchWithDeps(context.Background(), PullBatchOptions{
		Images:       []string{"busybox:latest"},
		To:           "registry.local/mirror",
		OutputDir:    dir,
		Concurrency:  1,
		Retries:      0,
		SkipExisting: true,
	}, pull, exists)
	if err != nil {
		t.Fatalf("runPullBatchWithDeps() error = %v", err)
	}
	if pullCalled {
		t.Fatal("pull was called when target already exists")
	}
	if report.Skipped != 1 || report.Items[0].Status != pullBatchStatusSkipped {
		t.Fatalf("report = %#v, want skipped", report)
	}
}
