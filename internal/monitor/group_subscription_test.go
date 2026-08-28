package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"easy_proxies/internal/config"
	"easy_proxies/internal/group"
	"easy_proxies/internal/store"
)

func TestGroupSubscriptionEntryAndToken(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "sub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	groupPool := &store.GroupPool{Name: "HK VIP", BindAddress: "0.0.0.0", BindPort: 10002, Protocol: "mixed",
		DispatchMode: "fixed", FailureWindowSeconds: 300, FailureThreshold: 3, HealthCheckSeconds: 60,
		Enabled: true, SubscriptionEnabled: true, SubscriptionToken: "secret-token", SubscriptionMode: "entry"}
	if err := db.CreateGroupPool(ctx, groupPool); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: db, cfgSrc: &config.Config{ExternalIP: "203.0.113.10"}}

	unauthorized := httptest.NewRecorder()
	server.routes().ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/sub/"+strconv.FormatInt(groupPool.ID, 10)+"?format=clash", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", unauthorized.Code)
	}

	request := httptest.NewRequest(http.MethodGet, "/sub/"+strconv.FormatInt(groupPool.ID, 10)+"?token=secret-token&format=clash&mode=entry", nil)
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "server: 203.0.113.10") || !strings.Contains(response.Body.String(), "port: 10002") {
		t.Fatalf("unexpected body:\n%s", response.Body.String())
	}
	if response.Header().Get("Profile-Update-Interval") != "12" {
		t.Fatal("missing subscription headers")
	}

	reset := httptest.NewRecorder()
	server.routes().ServeHTTP(reset, httptest.NewRequest(http.MethodPost,
		"/api/groups/"+strconv.FormatInt(groupPool.ID, 10)+"/subscription/reset-token", nil))
	if reset.Code != http.StatusOK {
		t.Fatalf("reset status=%d body=%s", reset.Code, reset.Body.String())
	}
	var resetBody struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(reset.Body.Bytes(), &resetBody); err != nil || resetBody.Token == "" || resetBody.Token == "secret-token" {
		t.Fatalf("reset token=%q err=%v", resetBody.Token, err)
	}
	oldToken := httptest.NewRecorder()
	server.routes().ServeHTTP(oldToken, httptest.NewRequest(http.MethodGet,
		"/sub/"+strconv.FormatInt(groupPool.ID, 10)+"?token=secret-token&format=clash", nil))
	if oldToken.Code != http.StatusUnauthorized {
		t.Fatalf("old token remained valid: %d", oldToken.Code)
	}
	newToken := httptest.NewRecorder()
	server.routes().ServeHTTP(newToken, httptest.NewRequest(http.MethodGet,
		"/sub/"+strconv.FormatInt(groupPool.ID, 10)+"?token="+resetBody.Token+"&format=clash", nil))
	if newToken.Code != http.StatusOK {
		t.Fatalf("new token status=%d body=%s", newToken.Code, newToken.Body.String())
	}
}

func TestGroupMembersSubscriptionFiltersEvicted(t *testing.T) {
	group.Reset()
	defer group.Reset()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "members.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	node := &store.Node{URI: "vless://uuid@example.com:443?security=tls&sni=example.com", Name: "node", Source: store.NodeSourceManual, Enabled: true}
	if err := db.CreateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	groupPool := &store.GroupPool{Name: "members", BindAddress: "127.0.0.1", BindPort: 10003, Protocol: "mixed",
		DispatchMode: "fixed", FailureWindowSeconds: 300, FailureThreshold: 3, HealthCheckSeconds: 60,
		Enabled: true, SubscriptionEnabled: true, SubscriptionToken: "members-token", SubscriptionMode: "members"}
	if err := db.CreateGroupPool(ctx, groupPool); err != nil {
		t.Fatal(err)
	}
	group.Register(groupPool.ID, 5*time.Minute, 3, "node-tag", map[string]group.GroupInitialState{"node-tag": {NodeID: node.ID}})
	server := &Server{store: db}
	path := "/sub/" + strconv.FormatInt(groupPool.ID, 10) + "?token=members-token&format=uri&mode=members"
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), node.URI) {
		t.Fatalf("unexpected response: %d %s", response.Code, response.Body.String())
	}

	now := time.Now()
	for i := 0; i < 3; i++ {
		group.RecordFailure(groupPool.ID, "node-tag", errors.New("down"), now.Add(time.Duration(i)*time.Second))
	}
	response = httptest.NewRecorder()
	server.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("evicted node leaked, status=%d body=%s", response.Code, response.Body.String())
	}
}
