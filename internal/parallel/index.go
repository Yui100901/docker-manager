package parallel

import (
	"context"
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
