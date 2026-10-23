package boxmgr

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/controlplane"
	"easy_proxies/internal/geoip"
	"easy_proxies/internal/groupmember"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/store"
)

func TestCachedLocationPreservesRegionalMembershipWithClassifier(t *testing.T) {
	for _, status := range []string{"missing", "failed", "success-unclassified", "success"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			db, err := store.Open(filepath.Join(t.TempDir(), "location.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			node := &store.Node{URI: "http://127.0.0.1:8080", Enabled: true, Source: store.NodeSourceManual, Region: "hk", Country: "Hong Kong"}
			if err := db.CreateNode(ctx, node); err != nil {
				t.Fatal(err)
			}
			if status != "missing" {
				result := &store.NodeDetectionResult{NodeID: node.ID, ExitIPStatus: "success", ExitIP: "203.0.113.1"}
				if status == "failed" {
					result.ExitIPStatus = "failed"
					result.ExitCountryCode = "JP" // stale data must not be promoted
				}
				if err := db.UpsertNodeDetectionResult(ctx, result); err != nil {
					t.Fatal(err)
				}
			}
			cfg := &config.Config{Nodes: []config.NodeConfig{{ID: node.ID, URI: node.URI, Region: node.Region, Country: node.Country}}}
			manager := New(cfg, monitor.Config{}, WithStore(db))
			lookups := 0
			classify := func(string) geoip.RegionInfo {
				lookups++
				if status == "success" {
					return geoip.RegionInfo{ISOCode: "JP", Country: "Japan"}
				}
				return geoip.RegionInfo{Code: geoip.RegionOther}
			}
			manager.applyCachedLocations(ctx, cfg, classify)
			// A reload replay must produce the same location and membership.
			manager.applyCachedLocations(ctx, cfg, classify)
			wantRegion, wantCountry := "hk", "Hong Kong"
			if status == "success" {
				wantRegion, wantCountry = "jp", "Japan"
			}
			persisted, err := db.GetNode(ctx, node.ID)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.Region != wantRegion || persisted.Country != wantCountry || cfg.Nodes[0].Region != wantRegion || cfg.Nodes[0].Country != wantCountry {
				t.Fatalf("location mismatch: persisted=%+v config=%+v", persisted, cfg.Nodes[0])
			}
			members := groupmember.Nodes(cfg, config.GroupPoolConfig{Enabled: true, Regions: []string{wantRegion}})
			if len(members) != 1 || members[0].ID != node.ID {
				t.Fatalf("regional membership lost: %+v", members)
			}
			if (status == "missing" || status == "failed") && lookups != 0 {
				t.Fatalf("classified an unsuccessful observation %d times", lookups)
			}
		})
	}
}

type locationWriteFailureStore struct {
	store.Store
}

func (s locationWriteFailureStore) WithTx(context.Context, func(store.Store) error) error {
	return errors.New("location write failed")
}

func TestCachedLocationWriteFailurePreservesConfig(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "location.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	node := &store.Node{URI: "http://127.0.0.1:8080", Enabled: true, Source: store.NodeSourceManual, Region: "hk", Country: "Hong Kong"}
	if err := db.CreateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertNodeDetectionResult(ctx, &store.NodeDetectionResult{NodeID: node.ID, ExitIPStatus: "success", ExitIP: "203.0.113.1", ExitCountryCode: "JP", ExitCountry: "Japan"}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Nodes: []config.NodeConfig{{ID: node.ID, URI: node.URI, Region: node.Region, Country: node.Country}}}
	manager := New(cfg, monitor.Config{}, WithStore(locationWriteFailureStore{db}))
	manager.applyCachedLocations(ctx, cfg, nil)
	if cfg.Nodes[0].Region != "hk" || cfg.Nodes[0].Country != "Hong Kong" {
		t.Fatalf("uncommitted location leaked into config: %+v", cfg.Nodes[0])
	}
}

func TestApplyCachedNodeLocationsUsesClassifiedCacheAndPreservesLocationWithoutDatabase(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "landing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cached := &store.Node{URI: "http://127.0.0.1:8080", Name: "cached", Source: store.NodeSourceManual, Enabled: true, Region: "us", Country: "United States"}
	legacy := &store.Node{URI: "http://127.0.0.2:8080", Name: "legacy", Source: store.NodeSourceManual, Enabled: true, Region: "jp", Country: "Japan"}
	if err := db.CreateNode(ctx, cached); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateNode(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertNodeDetectionResult(ctx, &store.NodeDetectionResult{NodeID: cached.ID, LatencyStatus: "untested", SpeedStatus: "untested", ExitIPStatus: "success", ExitIP: "1.1.1.1", ExitCountry: "Australia", ExitCountryCode: "AU"}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Nodes: []config.NodeConfig{
		{ID: cached.ID, URI: cached.URI, Region: cached.Region, Country: cached.Country},
		{ID: legacy.ID, URI: legacy.URI, Region: legacy.Region, Country: legacy.Country},
	}}
	manager := New(cfg, monitor.Config{}, WithStore(db))
	manager.applyCachedNodeLocations(ctx, cfg)
	if cfg.Nodes[0].Region != "au" || cfg.Nodes[0].Country != "Australia" {
		t.Fatalf("cached landing location not applied: %+v", cfg.Nodes[0])
	}
	if cfg.Nodes[1].Region != "jp" || cfg.Nodes[1].Country != "Japan" {
		t.Fatalf("location was cleared while GeoIP was unavailable: %+v", cfg.Nodes[1])
	}
	nodes, err := db.ListNodes(ctx, store.NodeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[int64]store.Node)
	for _, node := range nodes {
		byID[node.ID] = node
	}
	if byID[cached.ID].Region != "au" || byID[legacy.ID].Region != "jp" {
		t.Fatalf("authoritative landing metadata not persisted: %+v", byID)
	}
}

func TestListConfigNodesReturnsPersistedLocationForDisabledNodes(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "managed-locations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	enabled := &store.Node{URI: "http://127.0.0.1:8080", Name: "enabled", Source: store.NodeSourceManual, Enabled: true, Region: "us", Country: "United States"}
	disabled := &store.Node{URI: "http://127.0.0.2:8080", Name: "disabled", Source: store.NodeSourceManual, Enabled: false, Region: "jp", Country: "Japan"}
	for _, node := range []*store.Node{enabled, disabled} {
		if err := db.CreateNode(ctx, node); err != nil {
			t.Fatal(err)
		}
	}

	manager := New(&config.Config{Nodes: []config.NodeConfig{{ID: enabled.ID, URI: enabled.URI}}}, monitor.Config{}, WithStore(db))
	nodes, err := manager.ListConfigNodes(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("managed nodes = %d, want 2", len(nodes))
	}
	byName := make(map[string]controlplane.ManagedNodeConfig, len(nodes))
	for _, node := range nodes {
		byName[node.Name] = node
	}
	if got := byName["enabled"]; got.Region != "us" || got.Country != "United States" || got.Disabled {
		t.Fatalf("enabled location missing from API model: %+v", got)
	}
	if got := byName["disabled"]; got.Region != "jp" || got.Country != "Japan" || !got.Disabled {
		t.Fatalf("disabled location missing from API model: %+v", got)
	}
}

func TestSuccessfulLandingIPIsCacheHit(t *testing.T) {
	if !hasSuccessfulLandingIP(&store.NodeDetectionResult{ExitIPStatus: "success", ExitIP: "2001:db8::1"}) {
		t.Fatal("successful landing IP should suppress startup redetection")
	}
	for _, result := range []*store.NodeDetectionResult{nil, {ExitIPStatus: "failed", ExitIP: "1.1.1.1"}, {ExitIPStatus: "success"}} {
		if hasSuccessfulLandingIP(result) {
			t.Fatalf("invalid cache hit: %+v", result)
		}
	}
}

func TestDetectMissingNodeLocationPausesWithoutGeoIPDatabase(t *testing.T) {
	ctx := context.Background()
	var requests atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("203.0.113.7"))
	}))
	defer endpoint.Close()
	db, err := store.Open(filepath.Join(t.TempDir(), "detect-once.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	node := &store.Node{URI: "http://127.0.0.1:8080", Name: "new", Source: store.NodeSourceManual, Enabled: true}
	if err := db.CreateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Management: config.ManagementConfig{NodeCheck: config.NodeCheckConfig{LandingIPURL: endpoint.URL, QualityTimeout: time.Second, QualityConcurrency: 1}}, Nodes: []config.NodeConfig{{ID: node.ID, Name: node.Name, URI: node.URI}}}
	runtimeMonitor, err := monitor.NewManager(monitor.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeMonitor.Stop()
	handle := runtimeMonitor.Register(monitor.NodeInfo{NodeID: node.ID, Tag: "new", Name: node.Name})
	dialer := &net.Dialer{}
	handle.SetDialer(dialer.DialContext)
	manager := New(cfg, monitor.Config{}, WithStore(db))
	manager.monitorMgr = runtimeMonitor

	manager.detectMissingNodeLocations(ctx, cfg)
	manager.detectMissingNodeLocations(ctx, cfg)
	if got := requests.Load(); got != 0 {
		t.Fatalf("landing endpoint requests=%d, want none while GeoIP is unavailable", got)
	}
	results, err := db.ListNodeDetectionResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if results[node.ID] != nil {
		t.Fatalf("landing result was written while classification was paused: %+v", results[node.ID])
	}
}

// Node CRUD splices cfg.Nodes in place while a landing detection is running.
// The detector used to capture slice indexes and re-index the shared slice
// inside its worker goroutines, which panics with "index out of range" as soon
// as a node is deleted mid-detection (index 375 vs length 369 in the field).
// These two helpers are what removed the shared mutable state: collection
// copies node identity, and results are resolved by stable ID.
func TestLandingNodesCopiesIdentityBeforeProbing(t *testing.T) {
	nodes := make([]config.NodeConfig, 0, 4)
	for i := 0; i < 4; i++ {
		nodes = append(nodes, config.NodeConfig{ID: int64(i + 1), Name: "n", URI: "http://127.0.0.1:80"})
	}
	cfg := &config.Config{Nodes: nodes}
	manager := New(cfg, monitor.Config{})

	captured := manager.landingNodes(cfg)
	if len(captured) != 4 {
		t.Fatalf("captured %d nodes, want 4", len(captured))
	}
	// Simulate SetNodeEnabled/DeleteNodeByID running while probes are in flight.
	manager.mu.Lock()
	cfg.Nodes = append(cfg.Nodes[:0], cfg.Nodes[1:]...)
	manager.mu.Unlock()

	if len(captured) != 4 || captured[3].ID != 4 {
		t.Fatalf("captured snapshot changed with the live config: %+v", captured)
	}
	if live := manager.landingNodes(cfg); len(live) != 3 {
		t.Fatalf("expected the live config to be shorter, got %d", len(live))
	}
}

// A probe that finishes after its node was deleted must be a no-op, not an
// out-of-range panic, and must not resurrect the location on another node.
func TestLandingLocationResolvesByIDAfterNodeRemoval(t *testing.T) {
	cfg := &config.Config{Nodes: []config.NodeConfig{
		{ID: 1, URI: "http://127.0.0.1:80"},
		{ID: 2, URI: "http://127.0.0.2:80"},
	}}
	manager := New(cfg, monitor.Config{})

	if !manager.setLandingNodeLocation(cfg, 2, "jp", "Japan") {
		t.Fatal("location for a live node was not applied")
	}
	if cfg.Nodes[1].Region != "jp" || cfg.Nodes[1].Country != "Japan" {
		t.Fatalf("location applied to the wrong node: %+v", cfg.Nodes)
	}
	// Re-applying the same location is not a change.
	if manager.setLandingNodeLocation(cfg, 2, "jp", "Japan") {
		t.Fatal("identical location reported as a change")
	}

	// Node 2 disappears while its probe is still in flight.
	manager.mu.Lock()
	cfg.Nodes = append(cfg.Nodes[:1], cfg.Nodes[2:]...)
	manager.mu.Unlock()
	if manager.setLandingNodeLocation(cfg, 2, "us", "United States") {
		t.Fatal("a deleted node's result was reported as a change")
	}
	if len(cfg.Nodes) != 1 || cfg.Nodes[0].ID != 1 || cfg.Nodes[0].Region != "" {
		t.Fatalf("deleted node's result leaked onto another node: %+v", cfg.Nodes)
	}
}

// This is the exact shape of the field crash: targets carry a slice index,
// and node CRUD shortens cfg.Nodes before the worker dereferences it. The test
// asserts the invariant the fix relies on — a captured index is unusable after
// a concurrent delete — so a future refactor that reintroduces index capture
// fails here instead of in production.
func TestCapturedNodeIndexIsUnsafeAcrossConcurrentDelete(t *testing.T) {
	nodes := make([]config.NodeConfig, 0, 400)
	for i := 0; i < 400; i++ {
		nodes = append(nodes, config.NodeConfig{ID: int64(i + 1), URI: "http://127.0.0.1:80"})
	}
	cfg := &config.Config{Nodes: nodes}
	const capturedIndex = 375
	const fieldLength = 369 // the length reported by the field panic

	// SetNodeEnabled / DeleteNodeByID splice the slice in place. Removing 31
	// nodes leaves the length at 369 while a collected target still holds 375.
	cfg.Nodes = append(cfg.Nodes[:10], cfg.Nodes[41:]...)

	if len(cfg.Nodes) != fieldLength || capturedIndex < len(cfg.Nodes) {
		t.Fatalf("fixture no longer reproduces the field slice: len=%d", len(cfg.Nodes))
	}
	// The detector must never reach this index; it identifies nodes by ID.
	if got := (&Manager{}).landingNodes(cfg); len(got) != len(cfg.Nodes) {
		t.Fatalf("snapshot length %d, want %d", len(got), len(cfg.Nodes))
	}
	defer func() {
		if recover() == nil {
			t.Fatal("expected the stale index to panic, proving index capture is unsafe here")
		}
	}()
	_ = cfg.Nodes[capturedIndex].URI
}
