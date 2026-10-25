package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"easy_proxies/internal/monitor"
	"easy_proxies/internal/probe"

	"golang.org/x/sync/semaphore"
)

func TestDiagnosticBudgetsSurviveServerReplacement(t *testing.T) {
	a, b := &Server{}, &Server{}
	for _, kind := range []probe.Kind{probe.Latency, probe.Speed, probe.Quality, probe.Unlock} {
		if a.diagnosticSemaphore(kind) != b.diagnosticSemaphore(kind) {
			t.Fatalf("%s budget was not shared", kind)
		}
	}
}

func TestDiagnosticBudgetWaitHonorsCancellation(t *testing.T) {
	s := &Server{probeSem: semaphore.NewWeighted(1)}
	release, err := s.acquireDiagnostic(t.Context(), probe.Unlock)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.acquireDiagnostic(ctx, probe.Unlock); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting acquisition=%v", err)
	}
}

func TestSingleUnlockUsesTheBatchBudget(t *testing.T) {
	s, mgr, _ := newOperationsTestServer(t)
	mgr.Register(monitor.NodeInfo{Tag: "budget-test"})
	s.probeSem = semaphore.NewWeighted(1)
	release, err := s.acquireDiagnostic(t.Context(), probe.Unlock)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/nodes/budget-test/unlock", nil)
	r.SetPathValue("tag", "budget-test")
	w := httptest.NewRecorder()
	s.handleNodeUnlock(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
