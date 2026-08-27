package runcontrol

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"
)

type controllerContextKey struct{}
type leaseContextKey struct{}

type controllerLease struct {
	controller *Controller
}

// Controller is safe to share between sibling and nested command tasks. Its
// semaphore, pacer, and item counter are process-local and scoped to one
// command execution.
type Controller struct {
	limits Limits
	sem    *semaphore.Weighted
	pacer  *pacer
	items  atomic.Int64
}

func New(limits Limits) (*Controller, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	controller := &Controller{limits: limits}
	if limits.Concurrency > 0 {
		controller.sem = semaphore.NewWeighted(int64(limits.Concurrency))
	}
	if limits.Rate > 0 {
		controller.pacer = newPacer(limits.Rate)
	}
	return controller, nil
}

func (c *Controller) Limits() Limits {
	if c == nil {
		return Limits{}
	}
	return c.limits
}

func (c *Controller) Concurrency() int {
	if c == nil {
		return 0
	}
	return c.limits.Concurrency
}

// Context attaches c to parent and applies the configured outer timeout once.
// Derived contexts automatically retain the same controller.
func (c *Controller) Context(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if c == nil {
		return parent, func() {}
	}
	if c.limits.Timeout > 0 {
		timed, cancel := context.WithTimeout(parent, c.limits.Timeout)
		return context.WithValue(timed, controllerContextKey{}, c), cancel
	}
	return context.WithValue(parent, controllerContextKey{}, c), func() {}
}

// WithController attaches an existing controller without starting another
// timeout. It is intended for sibling tasks that already share an outer
// command context.
func WithController(ctx context.Context, controller *Controller) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if controller == nil {
		return ctx
	}
	return context.WithValue(ctx, controllerContextKey{}, controller)
}

func FromContext(ctx context.Context) (*Controller, bool) {
	if ctx == nil {
		return nil, false
	}
	controller, ok := ctx.Value(controllerContextKey{}).(*Controller)
	return controller, ok && controller != nil
}

// Acquired reports whether ctx already owns one semaphore slot from c. Nested
// parallel loops use this to execute serially inside that slot and avoid a
// reentrant semaphore deadlock.
func (c *Controller) Acquired(ctx context.Context) bool {
	if c == nil || ctx == nil {
		return false
	}
	lease, ok := ctx.Value(leaseContextKey{}).(*controllerLease)
	return ok && lease != nil && lease.controller == c
}

// Acquire reserves one shared concurrency slot and one rate-limit start. The
// returned context must be passed to nested work, and release must be called.
func (c *Controller) Acquire(ctx context.Context) (context.Context, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c == nil {
		return ctx, func() {}, nil
	}
	if err := ctx.Err(); err != nil {
		return ctx, func() {}, err
	}

	reentrant := c.Acquired(ctx)
	if !reentrant && c.sem != nil {
		if err := c.sem.Acquire(ctx, 1); err != nil {
			return ctx, func() {}, err
		}
	}
	if c.pacer != nil {
		if err := c.pacer.Wait(ctx); err != nil {
			if !reentrant && c.sem != nil {
				c.sem.Release(1)
			}
			return ctx, func() {}, err
		}
	}
	if reentrant || c.sem == nil {
		return ctx, func() {}, nil
	}

	taskCtx := context.WithValue(ctx, leaseContextKey{}, &controllerLease{controller: c})
	var once sync.Once
	return taskCtx, func() {
		once.Do(func() { c.sem.Release(1) })
	}, nil
}

func (c *Controller) Run(ctx context.Context, fn func(context.Context) error) error {
	if fn == nil {
		return errors.New("runcontrol callback is nil")
	}
	taskCtx, release, err := c.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	return fn(taskCtx)
}

// CheckItems atomically reserves count items from the command-wide cumulative
// budget. A failed reservation leaves the previous usage unchanged.
func (c *Controller) CheckItems(kind string, count int) error {
	if count < 0 {
		return fmt.Errorf("%s item count cannot be negative", itemKind(kind))
	}
	if c == nil || count == 0 {
		return nil
	}
	addition := int64(count)
	for {
		used := c.items.Load()
		if addition > math.MaxInt64-used {
			return fmt.Errorf("%s item budget overflow", itemKind(kind))
		}
		next := used + addition
		if c.limits.MaxItems > 0 && next > int64(c.limits.MaxItems) {
			return fmt.Errorf("%s item budget exceeded: used=%d requested=%d limit=%d", itemKind(kind), used, count, c.limits.MaxItems)
		}
		if c.items.CompareAndSwap(used, next) {
			return nil
		}
	}
}

func (c *Controller) ItemsUsed() int64 {
	if c == nil {
		return 0
	}
	return c.items.Load()
}

func itemKind(kind string) string {
	if kind == "" {
		return "operation"
	}
	return kind
}

type pacer struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func newPacer(rate float64) *pacer {
	intervalValue := float64(time.Second) / rate
	if intervalValue > float64(math.MaxInt64) {
		intervalValue = float64(math.MaxInt64)
	}
	interval := time.Duration(intervalValue)
	if interval < time.Nanosecond {
		interval = time.Nanosecond
	}
	return &pacer{interval: interval}
}

func (p *pacer) Wait(ctx context.Context) error {
	if p == nil {
		return nil
	}
	now := time.Now()
	p.mu.Lock()
	start := now
	if p.next.After(start) {
		start = p.next
	}
	p.next = start.Add(p.interval)
	p.mu.Unlock()

	delay := time.Until(start)
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
