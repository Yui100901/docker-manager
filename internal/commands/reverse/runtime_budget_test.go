package reverse

import (
	"context"
	"strings"
	"testing"

	"docker-manager/internal/runcontrol"
)

func reverseRuntimeBudgetContext(t *testing.T, maxItems int) context.Context {
	t.Helper()
	controller, err := runcontrol.New(runcontrol.Limits{MaxItems: maxItems})
	if err != nil {
		t.Fatal(err)
	}
	return runcontrol.WithController(context.Background(), controller)
}

func TestReverseRejectsMaxItemsBeforeContainerManagerInitialization(t *testing.T) {
	ctx := reverseRuntimeBudgetContext(t, 1)
	_, err := reverseWithOptions(ctx, []string{"one", "two"}, ReverseOptions{})
	if err == nil || !strings.Contains(err.Error(), "item budget exceeded") {
		t.Fatalf("reverseWithOptions() error = %v, want max-items rejection", err)
	}
}

func TestReverseMetadataUsesCumulativeResourceBudget(t *testing.T) {
	ctx := reverseRuntimeBudgetContext(t, 2)
	if err := runcontrol.CheckItems(ctx, "container", 1); err != nil {
		t.Fatal(err)
	}
	_, err := inspectReverseVolumeMetadata(ctx, []string{"one", "two"})
	if err == nil || !strings.Contains(err.Error(), "item budget exceeded") {
		t.Fatalf("inspectReverseVolumeMetadata() error = %v, want cumulative max-items rejection", err)
	}
}

func TestRerunRejectsMaxItemsBeforeInspectOrBackup(t *testing.T) {
	ctx := reverseRuntimeBudgetContext(t, 1)
	err := rerunContainers(ctx, []string{"one", "two"}, rerunOptions{})
	if err == nil || !strings.Contains(err.Error(), "item budget exceeded") {
		t.Fatalf("rerunContainers() error = %v, want max-items rejection", err)
	}
}
