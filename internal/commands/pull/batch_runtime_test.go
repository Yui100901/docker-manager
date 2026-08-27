package pull

import (
	"context"
	"strings"
	"testing"

	"docker-manager/internal/runcontrol"
)

func TestRunPullBatchRejectsMaxItemsBeforePulling(t *testing.T) {
	controller, err := runcontrol.New(runcontrol.Limits{MaxItems: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := controller.Context(context.Background())
	defer cancel()
	pullCalled := false
	_, err = runPullBatchWithDeps(ctx, PullBatchOptions{
		Images:      []string{"one:latest", "two:latest"},
		OutputDir:   t.TempDir(),
		Concurrency: 1,
	}, func(string, PullOptions) error {
		pullCalled = true
		return nil
	}, func(context.Context, string, string, PullOptions) (bool, error) {
		return false, nil
	})
	if err == nil || !strings.Contains(err.Error(), "item budget exceeded") {
		t.Fatalf("runPullBatchWithDeps() error = %v, want max-items rejection", err)
	}
	if pullCalled {
		t.Fatal("pull callback was called after max-items rejection")
	}
}
