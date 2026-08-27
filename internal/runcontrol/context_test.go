package runcontrol

import (
	"context"
	"strings"
	"testing"
)

func TestCheckItemsWithoutControllerIsNoOp(t *testing.T) {
	if err := CheckItems(context.Background(), "container", 3); err != nil {
		t.Fatalf("CheckItems() error = %v, want nil without controller", err)
	}
}

func TestCheckItemsUsesAttachedController(t *testing.T) {
	controller, err := New(Limits{MaxItems: 2})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithController(context.Background(), controller)
	if err := CheckItems(ctx, "container", 2); err != nil {
		t.Fatalf("first CheckItems() error = %v", err)
	}
	err = CheckItems(ctx, "image", 1)
	if err == nil || !strings.Contains(err.Error(), "item budget exceeded") {
		t.Fatalf("second CheckItems() error = %v, want budget rejection", err)
	}
}

func TestCheckItemsPrefersContextCancellation(t *testing.T) {
	controller, err := New(Limits{MaxItems: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(WithController(context.Background(), controller))
	cancel()
	if err := CheckItems(ctx, "container", 2); err != context.Canceled {
		t.Fatalf("CheckItems() error = %v, want context.Canceled", err)
	}
	if got := controller.ItemsUsed(); got != 0 {
		t.Fatalf("ItemsUsed() = %d after canceled check, want 0", got)
	}
}
