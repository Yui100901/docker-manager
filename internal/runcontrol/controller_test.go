package runcontrol

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type controllerTestContextKey struct{}

func TestValidateLimits(t *testing.T) {
	tests := []struct {
		name   string
		limits Limits
		valid  bool
	}{
		{name: "zero values", limits: Limits{}, valid: true},
		{name: "maximum values", limits: Limits{Concurrency: MaxConcurrency, Timeout: MaxTimeout, Rate: MaxRate, MaxItems: MaxItems}, valid: true},
		{name: "negative concurrency", limits: Limits{Concurrency: -1}},
		{name: "large concurrency", limits: Limits{Concurrency: MaxConcurrency + 1}},
		{name: "negative timeout", limits: Limits{Timeout: -time.Second}},
		{name: "large timeout", limits: Limits{Timeout: MaxTimeout + time.Nanosecond}},
		{name: "negative rate", limits: Limits{Rate: -1}},
		{name: "large rate", limits: Limits{Rate: MaxRate + 1}},
		{name: "nan rate", limits: Limits{Rate: math.NaN()}},
		{name: "infinite rate", limits: Limits{Rate: math.Inf(1)}},
		{name: "negative items", limits: Limits{MaxItems: -1}},
		{name: "large items", limits: Limits{MaxItems: MaxItems + 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := Validate(test.limits)
			if test.valid && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("Validate() error = nil, want rejection")
			}
		})
	}
}

func TestControllerContextSharesControllerAndAppliesTimeout(t *testing.T) {
	controller, err := New(Limits{Timeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := controller.Context(context.Background())
	defer cancel()
	derived := context.WithValue(ctx, controllerTestContextKey{}, "value")
	got, ok := FromContext(derived)
	if !ok || got != controller {
		t.Fatalf("FromContext() = %p, %v; want %p, true", got, ok, controller)
	}
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("context error = %v", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("controller timeout did not expire")
	}
}

func TestControllerRunSharesConcurrencyAcrossCallers(t *testing.T) {
	controller, err := New(Limits{Concurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := controller.Context(context.Background())
	defer cancel()

	var active atomic.Int32
	var maximum atomic.Int32
	// Keep notifications non-blocking so a failed assertion never strands a
	// callback and prevents the test process from cleaning up.
	start := make(chan struct{}, 2)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := controller.Run(ctx, func(context.Context) error {
				now := active.Add(1)
				updateMaximum(&maximum, now)
				select {
				case start <- struct{}{}:
				default:
				}
				<-release
				active.Add(-1)
				return nil
			}); err != nil {
				t.Errorf("Run() error = %v", err)
			}
		}()
	}
	<-start
	<-start
	time.Sleep(20 * time.Millisecond)
	if got := maximum.Load(); got > 2 {
		t.Fatalf("maximum active callbacks = %d, want at most 2", got)
	}
	close(release)
	wg.Wait()
	if got := maximum.Load(); got != 2 {
		t.Fatalf("maximum active callbacks = %d, want 2", got)
	}
}

func TestControllerRunReleasesLeaseWhenCallbackPanics(t *testing.T) {
	controller, err := New(Limits{Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := controller.Context(context.Background())
	defer cancel()

	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("Run() did not propagate callback panic")
			}
		}()
		_ = controller.Run(ctx, func(context.Context) error {
			panic("callback panic")
		})
	}()

	probeCtx, probeCancel := context.WithTimeout(ctx, time.Second)
	defer probeCancel()
	_, release, err := controller.Acquire(probeCtx)
	if err != nil {
		t.Fatalf("Acquire() after recovered panic = %v, want success", err)
	}
	release()
}

func TestControllerRunRateLimitIsShared(t *testing.T) {
	controller, err := New(Limits{Rate: 25})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := controller.Context(context.Background())
	defer cancel()
	started := time.Now()
	for i := 0; i < 3; i++ {
		if err := controller.Run(ctx, func(context.Context) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(started); elapsed < 70*time.Millisecond {
		t.Fatalf("three rate-limited starts took %s, want at least 70ms", elapsed)
	}
}

func TestControllerCheckItemsIsCumulativeAndAtomic(t *testing.T) {
	controller, err := New(Limits{MaxItems: 100})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := controller.CheckItems("container", 10); err != nil {
				failures.Add(1)
			}
		}()
	}
	wg.Wait()
	if failures.Load() != 0 || controller.ItemsUsed() != 100 {
		t.Fatalf("failures=%d items=%d, want 0 and 100", failures.Load(), controller.ItemsUsed())
	}
	if err := controller.CheckItems("network", 1); err == nil {
		t.Fatal("CheckItems() error = nil after budget exhaustion")
	}
	if controller.ItemsUsed() != 100 {
		t.Fatalf("failed reservation changed usage to %d", controller.ItemsUsed())
	}
}

func updateMaximum(maximum *atomic.Int32, value int32) {
	for {
		previous := maximum.Load()
		if value <= previous || maximum.CompareAndSwap(previous, value) {
			return
		}
	}
}
