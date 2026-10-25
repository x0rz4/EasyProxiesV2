package httpapi

import (
	"context"

	"easy_proxies/internal/monitor"
	"easy_proxies/internal/probe"

	"golang.org/x/sync/semaphore"
)

// These ceilings cover every HTTP entry point and survive Server replacement.
// Per-task settings can reduce concurrency further; health probes are separate.
var processDiagnosticBudgets = map[probe.Kind]*semaphore.Weighted{
	probe.Latency: semaphore.NewWeighted(128),
	probe.Speed:   semaphore.NewWeighted(8),
	probe.Quality: semaphore.NewWeighted(10),
	probe.Unlock:  semaphore.NewWeighted(32),
}

func (s *Server) diagnosticSemaphore(kind probe.Kind) *semaphore.Weighted {
	if kind == probe.Unlock && s.probeSem != nil {
		return s.probeSem
	}
	return processDiagnosticBudgets[kind]
}

func (s *Server) acquireDiagnostic(ctx context.Context, kind probe.Kind) (func(), error) {
	budget := s.diagnosticSemaphore(kind)
	if err := budget.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	return func() { budget.Release(1) }, nil
}

func (s *Server) runDiagnosticBatch(ctx context.Context, kind probe.Kind, limit int, nodes []monitor.Snapshot, run func(monitor.Snapshot)) {
	monitor.RunLimited(limit, nodes, func(node monitor.Snapshot) {
		release, err := s.acquireDiagnostic(ctx, kind)
		if err != nil {
			return
		}
		defer release()
		run(node)
	})
}
