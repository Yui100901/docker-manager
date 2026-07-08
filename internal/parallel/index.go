package parallel

import (
	"context"
	"errors"
	"sync"
)

func ForEachIndex(ctx context.Context, total, limit int, fn func(context.Context, int)) {
	if total <= 0 {
		return
	}
	if limit <= 0 || limit > total {
		limit = total
	}
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
				fn(ctx, index)
			}
		}()
	}
	for index := 0; index < total; index++ {
		if ctx.Err() != nil {
			break
		}
		jobs <- index
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
	if limit <= 0 || limit > total {
		limit = total
	}

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
					if err := fn(runCtx, index); err != nil {
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
