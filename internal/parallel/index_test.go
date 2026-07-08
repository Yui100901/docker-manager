package parallel

import (
	"context"
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
