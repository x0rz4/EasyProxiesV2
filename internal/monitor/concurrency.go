package monitor

import "golang.org/x/sync/errgroup"

// collectLimited runs one function per value with bounded concurrency and
// returns results as tasks finish. Submission happens on a coordinator
// goroutine so callers can consume streaming results while SetLimit applies
// backpressure to the remaining work.
func collectLimited[T, R any](limit int, values []T, run func(T) R) <-chan R {
	results := make(chan R, len(values))
	if len(values) == 0 {
		close(results)
		return results
	}
	if limit < 1 {
		limit = 1
	}
	if limit > len(values) {
		limit = len(values)
	}

	go func() {
		var group errgroup.Group
		group.SetLimit(limit)
		for _, value := range values {
			value := value
			group.Go(func() error {
				results <- run(value)
				return nil
			})
		}
		_ = group.Wait()
		close(results)
	}()
	return results
}

// runLimited is the side-effect-only form of collectLimited.
func runLimited[T any](limit int, values []T, run func(T)) {
	if len(values) == 0 {
		return
	}
	if limit < 1 {
		limit = 1
	}
	if limit > len(values) {
		limit = len(values)
	}

	var group errgroup.Group
	group.SetLimit(limit)
	for _, value := range values {
		value := value
		group.Go(func() error {
			run(value)
			return nil
		})
	}
	_ = group.Wait()
}
