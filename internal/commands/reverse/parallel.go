package reverse

import "sync"

const reverseInspectConcurrency = 8

func runReverseParallel(total, limit int, fn func(int)) {
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
				fn(index)
			}
		}()
	}
	for index := 0; index < total; index++ {
		jobs <- index
	}
	close(jobs)
	wg.Wait()
}
