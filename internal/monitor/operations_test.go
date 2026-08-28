package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"easy_proxies/internal/config"

	"golang.org/x/sync/semaphore"
)

func newOperationsTestServer(t *testing.T) (*Server, *Manager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("management:\n  probe_target: http://example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadForReload(path)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := NewManager(Config{
		ProbeTarget:          cfg.Management.ProbeTarget,
		ProbeConcurrency:     cfg.Management.ProbeConcurrency,
		StartupProbeTimeout:  cfg.Management.StartupProbeTimeout,
		RoutineProbeTimeout:  cfg.Management.RoutineProbeTimeout,
		ProbeDialTimeout:     cfg.Management.ProbeDialTimeout,
		ProbeResponseTimeout: cfg.Management.ProbeResponseTimeout,
		RoutineProbeRetries:  cfg.RoutineProbeRetryCount(),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{cfgSrc: cfg, cfg: Config{ProbeTarget: cfg.Management.ProbeTarget}, mgr: mgr}
	t.Cleanup(mgr.Stop)
	return server, mgr, path
}

func TestProbeSettingsAPIHotAppliesAndPersists(t *testing.T) {
	server, mgr, path := newOperationsTestServer(t)
	body := `{
        "probe_target":"https://cp.cloudflare.com/generate_204",
        "health_check_interval":"30m",
        "probe_concurrency":64,
        "startup_probe_timeout":"6s",
        "routine_probe_timeout":"18s",
        "probe_dial_timeout":"3s",
        "probe_response_timeout":"3s",
        "routine_probe_retries":2
    }`
	response := httptest.NewRecorder()
	server.handleProbeSettings(response, httptest.NewRequest(http.MethodPut, "/api/operations/probe-settings", strings.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", response.Code, response.Body.String())
	}
	policy := mgr.ProbePolicy()
	if policy.Concurrency != 64 || policy.StartupTimeout != 6*time.Second || policy.RoutineTimeout != 18*time.Second || policy.RoutineRetries != 2 {
		t.Fatalf("runtime policy = %+v", policy)
	}
	target, ready := mgr.TargetForProbe()
	if !ready || target.Scheme != "https" || target.RequestURI != "/generate_204" {
		t.Fatalf("runtime target = %+v ready=%v", target, ready)
	}
	reloaded, err := config.LoadForReload(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Management.ProbeConcurrency != 64 || reloaded.Management.HealthCheckInterval != 30*time.Minute || reloaded.RoutineProbeRetryCount() != 2 {
		t.Fatalf("persisted settings = %+v", reloaded.Management)
	}

	getResponse := httptest.NewRecorder()
	server.handleProbeSettings(getResponse, httptest.NewRequest(http.MethodGet, "/api/operations/probe-settings", nil))
	if getResponse.Code != http.StatusOK || !strings.Contains(getResponse.Body.String(), `"probe_concurrency":64`) {
		t.Fatalf("GET status=%d body=%s", getResponse.Code, getResponse.Body.String())
	}
}

func TestProbeSettingsAPIRejectsInvalidPolicyWithoutSaving(t *testing.T) {
	server, _, path := newOperationsTestServer(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	invalid := `{"probe_target":"http://example.com","health_check_interval":"1h","probe_concurrency":513,"startup_probe_timeout":"5s","routine_probe_timeout":"10s","probe_dial_timeout":"3s","probe_response_timeout":"2s","routine_probe_retries":1}`
	response := httptest.NewRecorder()
	server.handleProbeSettings(response, httptest.NewRequest(http.MethodPut, "/api/operations/probe-settings", strings.NewReader(invalid)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("invalid request modified config.yaml")
	}
}

func TestProbeStatusReportsAutoConcurrencyAndEstimates(t *testing.T) {
	server, mgr, _ := newOperationsTestServer(t)
	for index := range 1000 {
		mgr.Register(NodeInfo{Tag: fmt.Sprintf("node-%d", index)})
	}
	response := httptest.NewRecorder()
	server.handleProbeStatus(response, httptest.NewRequest(http.MethodGet, "/api/operations/probe-status", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var status probeStatusResponse
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.NodeCount != 1000 || status.EffectiveConcurrency != 100 || status.EstimatedStartupWorstSeconds != 50 {
		t.Fatalf("probe status = %+v", status)
	}
}

func TestManualProbeReturnsConflictWhileBatchRoundRuns(t *testing.T) {
	server, mgr, _ := newOperationsTestServer(t)
	started := make(chan struct{})
	mgr.Register(NodeInfo{Tag: "blocked"}).SetProbe(func(ctx context.Context) (time.Duration, error) {
		close(started)
		<-ctx.Done()
		return 0, ctx.Err()
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = mgr.RunProbeBatch(ctx, ProbeRoundPeriodic, false, nil, nil)
	}()
	<-started
	response := httptest.NewRecorder()
	server.handleProbeAll(response, httptest.NewRequest(http.MethodPost, "/api/nodes/probe-all", nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	cancel()
	<-done
}

func TestManualProbeKeepsSSEEventShape(t *testing.T) {
	server, mgr, _ := newOperationsTestServer(t)
	mgr.Register(NodeInfo{Tag: "node-a", Name: "Node A"}).SetProbe(func(context.Context) (time.Duration, error) {
		return 15 * time.Millisecond, nil
	})
	response := httptest.NewRecorder()
	server.handleProbeAll(response, httptest.NewRequest(http.MethodPost, "/api/nodes/probe-all", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, eventType := range []string{`"type":"start"`, `"type":"progress"`, `"type":"complete"`} {
		if !strings.Contains(body, eventType) {
			t.Fatalf("SSE body missing %s: %s", eventType, body)
		}
	}
}

func TestUnlockBatchesShareGlobalConcurrencyLimit(t *testing.T) {
	global := semaphore.NewWeighted(2)
	snapshots := make([]Snapshot, 6)
	for index := range snapshots {
		snapshots[index] = Snapshot{NodeInfo: NodeInfo{Tag: fmt.Sprintf("unlock-%d", index)}}
	}
	var active, maximum atomic.Int64
	run := func(context.Context, Snapshot) unlockBatchResult {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		return unlockBatchResult{err: "expected test result"}
	}

	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			count := 0
			for range collectUnlockBatch(t.Context(), len(snapshots), global, snapshots, run) {
				count++
			}
			if count != len(snapshots) {
				t.Errorf("batch results = %d, want %d", count, len(snapshots))
			}
		}()
	}
	wait.Wait()
	if got := maximum.Load(); got != 2 {
		t.Fatalf("maximum concurrency across batches = %d, want 2", got)
	}
}

func TestUnlockAllKeepsSSEEventShapeWhenNodesFail(t *testing.T) {
	server, mgr, _ := newOperationsTestServer(t)
	server.probeSem = semaphore.NewWeighted(2)
	for index := range 3 {
		mgr.Register(NodeInfo{Tag: fmt.Sprintf("unlock-error-%d", index), Name: fmt.Sprintf("Node %d", index)})
	}

	response := httptest.NewRecorder()
	server.handleUnlockAll(response, httptest.NewRequest(http.MethodPost, "/api/nodes/unlock-all", nil))
	body := response.Body.String()
	if strings.Count(body, `"type":"start"`) != 1 || strings.Count(body, `"type":"progress"`) != 3 || strings.Count(body, `"type":"complete"`) != 1 {
		t.Fatalf("unexpected unlock SSE sequence: %s", body)
	}
	if !strings.Contains(body, `"total":3,"success":0,"failed":3`) {
		t.Fatalf("unexpected unlock completion summary: %s", body)
	}
}
