package parallel

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"docker-manager/internal/runcontrol"
)

func TestForEachIndexSharesControllerAcrossSiblingLoops(t *testing.T) {
	controller, err := runcontrol.New(runcontrol.Limits{Concurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := controller.Context(context.Background())
	defer cancel()

	var active atomic.Int32
	var maximum atomic.Int32
	var wg sync.WaitGroup
	for loop := 0; loop < 2; loop++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ForEachIndex(ctx, 8, 8, func(context.Context, int) {
				now := active.Add(1)
				updateParallelMaximum(&maximum, now)
				time.Sleep(2 * time.Millisecond)
				active.Add(-1)
			})
		}()
	}
	wg.Wait()
	if got := maximum.Load(); got > 2 {
		t.Fatalf("maximum active callbacks = %d, want <= 2", got)
	}
}

func TestForEachIndexControllerAvoidsNestedDeadlock(t *testing.T) {
	controller, err := runcontrol.New(runcontrol.Limits{Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := controller.Context(context.Background())
	defer cancel()
	done := make(chan struct{})
	var calls atomic.Int32
	go func() {
		defer close(done)
		ForEachIndex(ctx, 1, 1, func(ctx context.Context, _ int) {
			ForEachIndex(ctx, 3, 3, func(context.Context, int) {
				calls.Add(1)
			})
		})
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("nested controller loop deadlocked")
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("nested calls = %d, want 3", got)
	}
}

func TestForEachIndexErrControllerCancellation(t *testing.T) {
	controller, err := runcontrol.New(runcontrol.Limits{Rate: 1})
	if err != nil {
		t.Fatal(err)
	}
	parent, cancelParent := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelParent()
	ctx, cancel := controller.Context(parent)
	defer cancel()
	err = ForEachIndexErr(ctx, 3, 3, func(context.Context, int) error { return nil })
	if err == nil || err != context.DeadlineExceeded {
		t.Fatalf("ForEachIndexErr() error = %v, want context.DeadlineExceeded", err)
	}
}

func updateParallelMaximum(maximum *atomic.Int32, value int32) {
	for {
		previous := maximum.Load()
		if value <= previous || maximum.CompareAndSwap(previous, value) {
			return
		}
	}
}
