package boxmgr

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/controlplane"
	json "easy_proxies/internal/jsonx"
	"easy_proxies/internal/monitor"
	"easy_proxies/internal/store"
)

type experimentMemory struct {
	Heap         uint64                 `json:"heap_objects_bytes"`
	GoMemory     uint64                 `json:"go_memory_bytes"`
	RSS          uint64                 `json:"rss_bytes"`
	RSSAvailable bool                   `json:"rss_available"`
	Goroutines   int                    `json:"goroutines"`
	Traffic      monitor.TrafficSummary `json:"traffic"`
}

type experimentResult struct {
	Nodes         int                           `json:"nodes"`
	Groups        int                           `json:"groups"`
	MemberPercent int                           `json:"member_percent"`
	Connections   int                           `json:"connections"`
	StartMS       int64                         `json:"start_ms"`
	Resources     controlplane.RuntimeResources `json:"resources"`
	Idle          experimentMemory              `json:"idle"`
	Loaded        experimentMemory              `json:"loaded"`
	After         experimentMemory              `json:"after"`
	Bytes         int64                         `json:"echo_bytes"`
	Seconds       float64                       `json:"seconds"`
	MiBPerSecond  float64                       `json:"echo_mib_per_second"`
}

// TestPoolScaleExperiment is an opt-in experiment rather than a timing gate.
// The JSON output and profiles must be kept outside tracked configuration/data.
func TestPoolScaleExperiment(t *testing.T) {
	if os.Getenv("EASY_PROXIES_SCALE_EXPERIMENT") != "1" {
		t.Skip("opt-in local scale experiment")
	}
	duration := 3 * time.Second
	if value := os.Getenv("EASY_PROXIES_SCALE_SECONDS"); value != "" {
		seconds, err := strconv.Atoi(value)
		if err != nil || seconds < 1 || seconds > 30 {
			t.Fatal("EASY_PROXIES_SCALE_SECONDS must be 1..30")
		}
		duration = time.Duration(seconds) * time.Second
	}
	output := os.Getenv("EASY_PROXIES_SCALE_OUTPUT")
	if output == "" {
		t.Fatal("set EASY_PROXIES_SCALE_OUTPUT to an ignored experiment JSON file")
	}
	profileDir := os.Getenv("EASY_PROXIES_SCALE_PROFILE_DIR")
	if profileDir != "" {
		if err := os.MkdirAll(profileDir, 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("EASY_PROXIES_PROFILE_ENABLED", "1")
	}
	var results []experimentResult
	for _, scenario := range []struct{ nodes, groups, percent, connections int }{
		{100, 1, 100, 1}, {100, 1, 100, 16}, {100, 8, 100, 16}, {100, 32, 100, 16},
		{100, 8, 50, 16}, {1000, 8, 100, 16}, {1000, 8, 100, 64},
	} {
		name := fmt.Sprintf("n%d-g%d-p%d-c%d", scenario.nodes, scenario.groups, scenario.percent, scenario.connections)
		if filter := os.Getenv("EASY_PROXIES_SCALE_CASE"); filter != "" && name != filter {
			continue
		}
		t.Run(name, func(t *testing.T) {
			result := runPoolExperiment(t, scenario.nodes, scenario.groups, scenario.percent, scenario.connections, duration, profileDir, name)
			results = append(results, result)
			encoded, _ := json.Marshal(result)
			t.Logf("SCALE %s", encoded)
		})
	}
	if len(results) == 0 {
		t.Fatal("no selected experiment cases")
	}
	data, err := json.MarshalCanonical(results)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func runPoolExperiment(t *testing.T, nodes, groups, percent, connections int, duration time.Duration, profileDir, name string) experimentResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != echo.Addr().String() {
			http.Error(w, "unexpected target", 400)
			return
		}
		target, err := net.DialTimeout("tcp", r.Host, 5*time.Second)
		if err != nil {
			http.Error(w, "dial failed", 502)
			return
		}
		defer target.Close()
		client, reader, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		if _, err = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		done := make(chan struct{})
		go func() {
			_, _ = io.Copy(target, reader)
			if tcp, ok := target.(*net.TCPConn); ok {
				_ = tcp.CloseWrite()
			}
			close(done)
		}()
		_, _ = io.Copy(client, target)
		_ = client.Close()
		<-done
	}))
	defer upstream.Close()
	ports := reserveTCPPorts(t, groups+2)
	api := "http://127.0.0.1:" + fmt.Sprint(ports[groups+1])
	enabled := true
	cfg := &config.Config{
		Listener:   config.ListenerConfig{Enabled: true, Address: "127.0.0.1", Port: ports[0], Protocol: "http"},
		Pool:       config.PoolConfig{Mode: "balance_p2c", FailureThreshold: 3, BlacklistDuration: time.Hour},
		Management: config.ManagementConfig{Enabled: &enabled, Listen: "127.0.0.1:" + fmt.Sprint(ports[groups+1]), Password: "scale-synthetic-password"},
		LogLevel:   "error",
	}
	for i := 0; i < nodes; i++ {
		cfg.Nodes = append(cfg.Nodes, config.NodeConfig{ID: int64(i + 1), Name: fmt.Sprintf("fixture-%d", i), URI: fmt.Sprintf("http://fixture-%d:synthetic@%s", i, strings.TrimPrefix(upstream.URL, "http://")), Region: "hk"})
	}
	for g := 0; g < groups; g++ {
		group := config.GroupPoolConfig{ID: int64(g + 1), Name: fmt.Sprintf("fixture-group-%d", g), BindAddress: "127.0.0.1", BindPort: ports[g+1], Protocol: "http", DispatchMode: "random", Enabled: true, RegionRuleVersion: 2, HealthCheckInterval: time.Hour}
		for i := 0; i < nodes*percent/100; i++ {
			group.ExplicitNodeIDs = append(group.ExplicitNodeIDs, int64((i+g*nodes/4)%nodes+1))
		}
		cfg.Groups = append(cfg.Groups, group)
	}
	if err := cfg.NormalizeWithPortMap(nil); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "experiment.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seed := make([]*store.Node, nodes)
	for i, node := range cfg.Nodes {
		seed[i] = &store.Node{Name: node.Name, URI: node.URI, Region: node.Region, Country: "fixture", Source: store.NodeSourceManual, Enabled: true}
	}
	if _, err := db.CreateNodes(ctx, seed); err != nil {
		t.Fatal(err)
	}
	if err := db.WithTx(ctx, func(tx store.Store) error {
		for i, node := range seed {
			if node.ID != cfg.Nodes[i].ID {
				return fmt.Errorf("fixture node ID mismatch")
			}
			// A synthetic successful cached exit keeps startup entirely local.
			// It is diagnostic evidence, never a fabricated healthy route.
			if err := tx.UpsertNodeDetectionResult(ctx, &store.NodeDetectionResult{NodeID: node.ID, ExitIP: "127.0.0.1", ExitIPStatus: "success", ExitCountryCode: "hk", ExitCountry: "fixture", ExitIPCheckedAt: time.Now().UTC()}); err != nil {
				return err
			}
		}
		for _, group := range cfg.Groups {
			stored := storeGroupFromConfig(group)
			if err := tx.CreateGroupPool(ctx, stored); err != nil {
				return err
			}
			if stored.ID != group.ID {
				return fmt.Errorf("fixture group ID mismatch")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	manager := New(cfg, monitor.Config{Enabled: true, Listen: cfg.Management.Listen, Password: cfg.Management.Password}, WithStore(db))
	defer manager.Close()
	started := time.Now()
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	result := experimentResult{Nodes: nodes, Groups: groups, MemberPercent: percent, Connections: connections, StartMS: time.Since(started).Milliseconds()}
	result.Resources = manager.RuntimeResources()
	if result.Resources.GroupInstances != groups || result.Resources.GroupMembers != groups*nodes*percent/100 {
		t.Fatalf("missing runtime members: %+v", result.Resources)
	}
	jar, _ := cookiejar.New(nil)
	httpClient := &http.Client{Timeout: 20 * time.Second, Jar: jar}
	defer httpClient.CloseIdleConnections()
	login, err := httpClient.Post(api+"/api/auth", "application/json", strings.NewReader(`{"password":"scale-synthetic-password"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = login.Body.Close()
	if login.StatusCode != 200 {
		t.Fatalf("login status=%d", login.StatusCode)
	}
	readMetrics := func() experimentMemory {
		runtime.GC() // laboratory checkpoint, never part of the timed load loop
		response, err := httpClient.Get(api + "/api/operations/resources")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var sample experimentMemory
		if response.StatusCode != 200 {
			t.Fatalf("metrics status=%d", response.StatusCode)
		}
		if err := json.NewDecoder(response.Body).Decode(&sample); err != nil {
			t.Fatal(err)
		}
		return sample
	}
	result.Idle = readMetrics()
	var streams []net.Conn
	var readers []*bufio.Reader
	defer func() {
		for _, conn := range streams {
			_ = conn.Close()
		}
	}()
	for i := 0; i < connections; i++ {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", ports[i%(groups+1)]), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		streams = append(streams, conn)
		_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
		if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echo.Addr(), echo.Addr()); err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(conn)
		response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
		if err != nil || response.StatusCode != 200 {
			t.Fatalf("CONNECT failed: %v %+v", err, response)
		}
		if err := conn.SetDeadline(time.Time{}); err != nil {
			t.Fatal(err)
		}
		readers = append(readers, reader)
	}
	result.Loaded = readMetrics()
	if result.Loaded.Traffic.ActiveConnections != int64(connections) {
		t.Fatalf("active connections=%d want=%d", result.Loaded.Traffic.ActiveConnections, connections)
	}
	profileErrors := make(chan error, 1)
	if profileDir != "" {
		captureExperimentProfile(t, httpClient, api+"/api/operations/profile/heap", filepath.Join(profileDir, name+"-heap.pprof"))
		go func() {
			profileErrors <- saveExperimentProfile(httpClient, api+"/api/operations/profile/cpu?seconds=1", filepath.Join(profileDir, name+"-cpu.pprof"))
		}()
	}
	loadCtx, stop := context.WithTimeout(ctx, duration)
	defer stop()
	var total atomic.Int64
	var workers sync.WaitGroup
	loadErrors := make(chan error, connections)
	loadStart := time.Now()
	for i, conn := range streams {
		workers.Go(func() {
			buffer := bytes.Repeat([]byte{byte(i + 1)}, 32*1024)
			received := make([]byte, len(buffer))
			closeOnCancel := context.AfterFunc(loadCtx, func() { _ = conn.Close() })
			defer closeOnCancel()
			for loadCtx.Err() == nil {
				n, err := conn.Write(buffer)
				if err == nil && n != len(buffer) {
					err = io.ErrShortWrite
				}
				if err == nil {
					_, err = io.ReadFull(readers[i], received)
				}
				if err != nil {
					if loadCtx.Err() == nil {
						loadErrors <- err
					}
					return
				}
				if !bytes.Equal(buffer, received) {
					loadErrors <- fmt.Errorf("payload mismatch")
					return
				}
				total.Add(int64(len(buffer)))
			}
		})
	}
	workers.Wait()
	result.Seconds = time.Since(loadStart).Seconds()
	result.Bytes = total.Load()
	result.MiBPerSecond = float64(result.Bytes) / result.Seconds / (1024 * 1024)
	close(loadErrors)
	for err := range loadErrors {
		t.Error(err)
	}
	if result.Bytes == 0 {
		t.Fatal("no verified payload transferred")
	}
	if profileDir != "" {
		if err := <-profileErrors; err != nil {
			t.Error(err)
		}
	}
	for _, conn := range streams {
		_ = conn.Close()
	}
	deadline := time.Now().Add(10 * time.Second)
	for manager.MonitorManager().TrafficSummary(false).ActiveConnections != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	result.After = readMetrics()
	if result.After.Traffic.ActiveConnections != 0 || result.After.Traffic.TotalUpload < result.Bytes || result.After.Traffic.TotalDownload < result.Bytes {
		t.Fatalf("traffic accounting mismatch: %+v bytes=%d", result.After.Traffic, result.Bytes)
	}
	return result
}

func captureExperimentProfile(t *testing.T, client *http.Client, url, path string) {
	t.Helper()
	if err := saveExperimentProfile(client, url, path); err != nil {
		t.Fatal(err)
	}
}

func saveExperimentProfile(client *http.Client, url, path string) error {
	response, err := client.Get(url)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("profile status %d", response.StatusCode)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, response.Body)
	return errors.Join(copyErr, file.Close())
}
