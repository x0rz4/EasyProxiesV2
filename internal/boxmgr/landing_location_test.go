package boxmgr

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/store"
)

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
	byName := make(map[string]monitor.ManagedNodeConfig, len(nodes))
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
