package boxmgr

import (
	"testing"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/group"
	"easy_proxies/internal/monitor"
)

func TestIdleWakeRecreatesUnchangedGroupListener(t *testing.T) {
	manager, cfg := newLifecycleAuditManager(t)
	empty := cfg.Clone()
	empty.Nodes = nil
	if err := manager.ReloadWithPortMap(empty, nil); err != nil {
		t.Fatal(err)
	}
	if !manager.idle || manager.currentBox != nil {
		t.Fatal("manager did not enter idle")
	}
	assertTCPNotListening(t, cfg.Groups[0].BindPort)
	if err := manager.ReloadWithPortMap(cfg.Clone(), nil); err != nil {
		t.Fatal(err)
	}
	if manager.idle || manager.currentBox == nil {
		t.Fatal("manager did not resume")
	}
	assertTCPListening(t, cfg.Listener.Port)
	assertTCPListening(t, cfg.Groups[0].BindPort)
	if manager.GroupRuntimeStatus(971).Status != "ready" {
		t.Fatal("unchanged group was not recreated")
	}
}

func TestFullReloadFailureRestoresServingBase(t *testing.T) {
	manager, cfg := newLifecycleAuditManager(t)
	broken := cfg.Clone()
	broken.LogLevel = "invalid-log-level"
	if err := manager.Reload(broken); err == nil {
		t.Fatal("invalid full reload succeeded")
	}
	if manager.currentBox == nil {
		t.Fatal("rollback left no running base")
	}
	assertTCPListening(t, cfg.Listener.Port)
	assertTCPListening(t, cfg.Groups[0].BindPort)
	if manager.cfg.LogLevel != cfg.LogLevel {
		t.Fatal("failed configuration was published")
	}
}

func newLifecycleAuditManager(t *testing.T) (*Manager, *config.Config) {
	t.Helper()
	group.Reset()
	t.Cleanup(group.Reset)
	ports := reserveTCPPorts(t, 2)
	cfg := &config.Config{
		Listener: config.ListenerConfig{Enabled: true, Address: "127.0.0.1", Port: ports[0], Protocol: "http"},
		Pool:     config.PoolConfig{Mode: "fixed", FailureThreshold: 3, BlacklistDuration: time.Minute},
		Nodes:    []config.NodeConfig{{ID: 1, Name: "node", URI: "http://127.0.0.1:65530", Region: "hk"}},
		Groups: []config.GroupPoolConfig{{ID: 971, Name: "audit", BindAddress: "127.0.0.1", BindPort: ports[1],
			Protocol: "mixed", DispatchMode: "fixed", Regions: []string{"hk"}, FailureWindow: time.Minute,
			FailureThreshold: 3, HealthCheckInterval: time.Minute, Enabled: true}},
		LogLevel: "error",
	}
	if err := cfg.NormalizeWithPortMap(nil); err != nil {
		t.Fatal(err)
	}
	manager := New(cfg, monitor.Config{})
	if err := manager.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager, cfg
}
