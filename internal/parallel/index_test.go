package parallel

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestForEachIndexVisitsAllIndexes(t *testing.T) {
	seen := make([]bool, 5)
	ForEachIndex(context.Background(), len(seen), 2, func(ctx context.Context, i int) {
		seen[i] = true
	})
	for i, ok := range seen {
		if !ok {
			t.Fatalf("index %d was not visited", i)
		}
	}
}

func TestForEachIndexAcceptsNilContext(t *testing.T) {
	var calls atomic.Int32
	//lint:ignore SA1012 This verifies the helper's documented nil-context fallback.
	ForEachIndex(nil, 4, 2, func(context.Context, int) {
		calls.Add(1)
	})
	if got := calls.Load(); got != 4 {
		t.Fatalf("calls = %d, want 4", got)
	}
}

func TestForEachIndexRespectsLimit(t *testing.T) {
	var active int32
	var maxActive int32
	ForEachIndex(context.Background(), 8, 2, func(ctx context.Context, i int) {
		now := atomic.AddInt32(&active, 1)
		for {
			previous := atomic.LoadInt32(&maxActive)
			if now <= previous || atomic.CompareAndSwapInt32(&maxActive, previous, now) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		atomic.AddInt32(&active, -1)
	})
	if maxActive > 2 {
		t.Fatalf("max active workers = %d, want <= 2", maxActive)
	}
}

func TestForEachIndexSkipsWorkWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls int32
	ForEachIndex(ctx, 5, 2, func(ctx context.Context, i int) {
		atomic.AddInt32(&calls, 1)
	})
	if calls != 0 {
		t.Fatalf("calls = %d, want 0", calls)
	}
}

func TestForEachIndexCancellationDoesNotBlockProducer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ForEachIndex(ctx, 100, 1, func(taskCtx context.Context, _ int) {
			select {
			case <-started:
			default:
				close(started)
			}
			<-taskCtx.Done()
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("callback did not start")
	}
	// Give the producer time to block trying to hand off the next index while
	// the sole worker remains in the callback.
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ForEachIndex remained blocked after cancellation")
	}
}

func TestForEachIndexErrVisitsAllIndexes(t *testing.T) {
	seen := make([]bool, 5)
	if err := ForEachIndexErr(context.Background(), len(seen), 2, func(ctx context.Context, i int) error {
		seen[i] = true
		return nil
	}); err != nil {
		t.Fatalf("ForEachIndexErr() error = %v", err)
	}
	for i, ok := range seen {
		if !ok {
			t.Fatalf("index %d was not visited", i)
		}
	}
}

func TestForEachIndexErrReturnsAndStopsOnError(t *testing.T) {
	wantErr := errors.New("boom")
	var calls int32
	err := ForEachIndexErr(context.Background(), 10, 1, func(ctx context.Context, i int) error {
		atomic.AddInt32(&calls, 1)
		if i == 0 {
			return fmt.Errorf("index %d: %w", i, wantErr)
		}
		return nil
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("ForEachIndexErr() error = %v, want %v", err, wantErr)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestForEachIndexErrPrefersContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls int32
	err := ForEachIndexErr(ctx, 5, 2, func(ctx context.Context, i int) error {
		atomic.AddInt32(&calls, 1)
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ForEachIndexErr() error = %v, want context.Canceled", err)
	}
	if calls != 0 {
		t.Fatalf("calls = %d, want 0", calls)
	}
}
