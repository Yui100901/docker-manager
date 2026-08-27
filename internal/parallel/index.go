package parallel

import (
	"context"
	"errors"
	"sync"

	"docker-manager/internal/runcontrol"
)

func ForEachIndex(ctx context.Context, total, limit int, fn func(context.Context, int)) {
	if ctx == nil {
		ctx = context.Background()
	}
	if total <= 0 {
		return
	}
	limit, controller := controlledLimit(ctx, total, limit)
	jobs := make(chan int)
	var wg sync.WaitGroup
	wg.Add(limit)
	for worker := 0; worker < limit; worker++ {
		go func() {
			defer wg.Done()
			for index := range jobs {
				if ctx.Err() != nil {
					continue
				}
				taskCtx := ctx
				release := func() {}
				if controller != nil {
					var err error
					taskCtx, release, err = controller.Acquire(ctx)
					if err != nil {
						continue
					}
				}
				func() {
					defer release()
					fn(taskCtx, index)
				}()
			}
		}()
	}
send:
	for index := 0; index < total; index++ {
		select {
		case <-ctx.Done():
			break send
		case jobs <- index:
		}
	}
	close(jobs)
	wg.Wait()
}

func ForEachIndexErr(ctx context.Context, total, limit int, fn func(context.Context, int) error) error {
	if total <= 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	limit, controller := controlledLimit(ctx, total, limit)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan int)
	errs := make(chan error, total)
	var wg sync.WaitGroup
	wg.Add(limit)
	for worker := 0; worker < limit; worker++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-runCtx.Done():
					return
				case index, ok := <-jobs:
					if !ok {
						return
					}
					taskCtx := runCtx
					release := func() {}
					if controller != nil {
						var err error
						taskCtx, release, err = controller.Acquire(runCtx)
						if err != nil {
							return
						}
					}
					var err error
					func() {
						defer release()
						err = fn(taskCtx, index)
					}()
					if err != nil {
						errs <- err
						cancel()
						return
					}
				}
			}
		}()
	}

	for index := 0; index < total; index++ {
		select {
		case <-runCtx.Done():
			index = total
		case jobs <- index:
		}
	}
	close(jobs)
	wg.Wait()
	close(errs)

	if err := ctx.Err(); err != nil {
		return err
	}
	var joined error
	for err := range errs {
		if err == nil {
			continue
		}
		joined = errors.Join(joined, err)
	}
	return joined
}

func controlledLimit(ctx context.Context, total, local int) (int, *runcontrol.Controller) {
	if local <= 0 || local > total {
		local = total
	}
	controller, ok := runcontrol.FromContext(ctx)
	if !ok {
		return local, nil
	}
	if global := controller.Concurrency(); global > 0 && global < local {
		local = global
	}
	if controller.Acquired(ctx) && local > 1 {
		local = 1
	}
	return local, controller
}
