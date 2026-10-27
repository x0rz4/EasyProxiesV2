package boxmgr_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"easy_proxies/internal/boxmgr"
	"easy_proxies/internal/config"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/nodetag"
	"easy_proxies/internal/store"
	"easy_proxies/internal/subscription"
)

// TestFrontendBrowserFixture runs the actual embedded UI/API/runtime against a
// disposable database and a loopback-only proxy. It never loads real config.
func TestFrontendBrowserFixture(t *testing.T) {
	if os.Getenv("EASY_PROXIES_FRONTEND_FIXTURE") != "1" {
		t.Skip("opt-in browser fixture")
	}
	stopFile := os.Getenv("EASY_PROXIES_FRONTEND_STOP_FILE")
	if stopFile == "" {
		t.Fatal("set EASY_PROXIES_FRONTEND_STOP_FILE to a new ignored file")
	}
	if _, err := os.Stat(stopFile); !os.IsNotExist(err) {
		t.Fatal("stop file must not exist at startup")
	}
	nodes := 1000
	if value := os.Getenv("EASY_PROXIES_FRONTEND_NODES"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 10000 {
			t.Fatal("fixture node count must be 1..10000")
		}
		nodes = parsed
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			conn, reader, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
				return
			}
			for {
				request, err := http.ReadRequest(reader.Reader)
				if err != nil {
					return
				}
				_ = request.Body.Close()
				if _, err := io.WriteString(conn, "HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n"); err != nil {
					return
				}
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	ports := make([]uint16, 3)
	for i := range ports {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ports[i] = uint16(listener.Addr().(*net.TCPAddr).Port)
		_ = listener.Close()
	}
	enabled := true
	cfg := &config.Config{
		Listener: config.ListenerConfig{Enabled: true, Address: "127.0.0.1", Port: ports[0], Protocol: "http"},
		Pool:     config.PoolConfig{Mode: "balance_p2c"}, LogLevel: "error",
		Management: config.ManagementConfig{Enabled: &enabled, Listen: fmt.Sprintf("127.0.0.1:%d", ports[1]),
			Password: "frontend-synthetic-password", ProbeTarget: "http://fixture.invalid/generate_204", HealthCheckInterval: time.Hour},
	}
	directory := t.TempDir()
	cfg.SetFilePath(filepath.Join(directory, "fixture.yaml"))
	group := config.GroupPoolConfig{ID: 1, Name: "本地测试分组", Enabled: true, BindAddress: "127.0.0.1", BindPort: ports[2], Protocol: "http", DispatchMode: "random", RegionRuleVersion: 2, HealthCheckInterval: time.Hour}
	for i := 0; i < nodes; i++ {
		name := fmt.Sprintf("测试节点 %05d · 香港 🇭🇰", i+1)
		if i == 0 {
			name = `<img src=x onerror=alert(1)> · 字符转义测试`
		}
		cfg.Nodes = append(cfg.Nodes, config.NodeConfig{ID: int64(i + 1), Name: name, URI: fmt.Sprintf("http://fixture-%d:synthetic@%s", i, strings.TrimPrefix(upstream.URL, "http://")), Region: "hk"})
		if i < 50 {
			group.ExplicitNodeIDs = append(group.ExplicitNodeIDs, int64(i+1))
		}
	}
	cfg.Groups = []config.GroupPoolConfig{group}
	if err := cfg.NormalizeWithPortMap(nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.FilePath(), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveSettings(); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(directory, "fixture.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	seed := make([]*store.Node, nodes)
	for i, node := range cfg.Nodes {
		seed[i] = &store.Node{Name: node.Name, URI: node.URI, Region: "hk", Country: "香港", Source: store.NodeSourceManual, Enabled: true}
	}
	if _, err := db.CreateNodes(ctx, seed); err != nil {
		t.Fatal(err)
	}
	if err := db.WithTx(ctx, func(tx store.Store) error {
		for i, node := range seed {
			if node.ID != int64(i+1) {
				return fmt.Errorf("fixture ID mismatch")
			}
			if err := tx.UpsertNodeDetectionResult(ctx, &store.NodeDetectionResult{NodeID: node.ID, ExitIP: "127.0.0.1", ExitIPStatus: "success", ExitCountryCode: "hk", ExitCountry: "香港", ExitIPCheckedAt: time.Now().UTC()}); err != nil {
				return err
			}
		}
		return tx.CreateGroupPool(ctx, &store.GroupPool{ID: group.ID, Name: group.Name, Enabled: true, BindAddress: group.BindAddress, BindPort: group.BindPort,
			Protocol: group.Protocol, DispatchMode: group.DispatchMode, RegionRuleVersion: 2, ExplicitNodeIDs: group.ExplicitNodeIDs, HealthCheckSeconds: 3600})
	}); err != nil {
		t.Fatal(err)
	}
	manager := boxmgr.New(cfg, monitor.Config{Enabled: true, Listen: cfg.Management.Listen, Password: cfg.Management.Password, ProbeTarget: cfg.Management.ProbeTarget}, boxmgr.WithStore(db))
	defer manager.Close()
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	manager.MonitorServer().SetTagService(nodetag.NewService(db, nodetag.WithRegistry(monitor.NewTagFactRegistry()), nodetag.WithUnlockProviders(monitor.TagUnlockFactProviders())))
	manager.MonitorServer().SetSubscriptionRefresher(subscription.New(cfg, manager, subscription.WithStore(db)))
	t.Logf("FRONTEND_FIXTURE http://%s nodes=%d; synthetic password in test source; stop via named file", cfg.Management.Listen, nodes)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("fixture reached its 30 minute limit")
		case <-ticker.C:
			if _, err := os.Stat(stopFile); err == nil {
				return
			}
		}
	}
}
