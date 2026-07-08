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
