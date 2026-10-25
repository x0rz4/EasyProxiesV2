package boxmgr

import (
	"testing"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/monitor"
)

func TestObfsPluginStartsAndReloads(t *testing.T) {
	nodes, err := config.ParseSubscriptionContent(`proxies:
  - name: obfs-node
    type: ss
    server: 127.0.0.1
    port: 8388
    cipher: aes-128-gcm
    password: test-password
    plugin: obfs
    plugin-opts: {mode: tls, host: cdn.example}
`)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("import: nodes=%d err=%v", len(nodes), err)
	}
	cfg := &config.Config{
		Pool:                config.PoolConfig{Mode: "sequential", FailureThreshold: 3, BlacklistDuration: time.Minute},
		Nodes:               nodes,
		SubscriptionRefresh: config.SubscriptionRefreshConfig{HealthCheckTimeout: time.Millisecond},
		LogLevel:            "error",
	}
	if err := cfg.NormalizeWithPortMap(nil); err != nil {
		t.Fatal(err)
	}
	manager := New(cfg, monitor.Config{})
	if err := manager.Start(t.Context()); err != nil {
		t.Fatalf("cold start with obfs: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	initialBox := manager.currentBox

	updated := cfg.Clone()
	updated.Nodes = append(updated.Nodes, config.NodeConfig{
		Name: "http-obfs", URI: "ss://aes-128-gcm:test-password@127.0.0.1:8389?plugin=obfs&plugin-opts=mode%3Dhttp%3Bhost%3Dcdn.example",
	})
	if err := manager.ReloadWithPortMap(updated, manager.CurrentPortMap()); err != nil {
		t.Fatalf("incremental reload with obfs: %v", err)
	}
	if manager.currentBox != initialBox || len(manager.monitorMgr.Snapshot()) != 2 {
		t.Fatal("incremental reload did not retain the box and publish both nodes")
	}

	rebuilt := updated.Clone()
	rebuilt.LogLevel = "warn" // A runtime-wide change forces full reconstruction.
	if err := manager.ReloadWithPortMap(rebuilt, manager.CurrentPortMap()); err != nil {
		t.Fatalf("full reload with obfs: %v", err)
	}
	if manager.currentBox == initialBox || len(manager.monitorMgr.Snapshot()) != 2 {
		t.Fatal("full reload did not replace the box and retain both nodes")
	}
}
