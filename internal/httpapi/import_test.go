package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"easy_proxies/internal/config"
	"easy_proxies/internal/controlplane"
)

type importNodeManager struct {
	reloadNodeManager
	existing []controlplane.ManagedNodeConfig
	created  []config.NodeConfig
}

func (m *importNodeManager) ListConfigNodes(context.Context, *int64) ([]controlplane.ManagedNodeConfig, error) {
	return append([]controlplane.ManagedNodeConfig(nil), m.existing...), nil
}

func (m *importNodeManager) CreateNode(_ context.Context, node config.NodeConfig) (config.NodeConfig, error) {
	m.created = append(m.created, node)
	m.existing = append(m.existing, controlplane.ManagedNodeConfig{Name: node.Name, URI: node.URI})
	return node, nil
}

func TestImportHTTPProxyGeneratesUniqueNames(t *testing.T) {
	manager := &importNodeManager{existing: []controlplane.ManagedNodeConfig{{Name: "imported-1", URI: "http://existing.example:80"}}}
	server := &Server{nodeMgr: manager}
	content := "[proxy](http://mscaei8ikxp4:rr62tghfcit1twh@104.207.47.150:3129)\nhttp://other:secret@198.51.100.9:8080"
	body, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.handleImport(response, httptest.NewRequest(http.MethodPost, "/api/import", bytes.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Imported int      `json:"imported"`
		Errors   []string `json:"errors"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Imported != 2 || len(payload.Errors) != 0 {
		t.Fatalf("payload=%+v", payload)
	}
	if manager.reloads.Load() != 1 {
		t.Fatalf("batch import reloads=%d, want one", manager.reloads.Load())
	}
	if len(manager.created) != 2 || manager.created[0].Name != "imported-2" || manager.created[1].Name != "imported-3" {
		t.Fatalf("created nodes=%+v", manager.created)
	}
	if manager.created[0].URI != "http://mscaei8ikxp4:rr62tghfcit1twh@104.207.47.150:3129" {
		t.Fatalf("HTTP proxy URI=%q", manager.created[0].URI)
	}
}

func TestImportSuffixesExplicitDuplicateNameAndSkipsDuplicateIdentity(t *testing.T) {
	manager := &importNodeManager{existing: []controlplane.ManagedNodeConfig{{Name: "proxy", URI: "http://existing.example:80"}}}
	server := &Server{nodeMgr: manager}
	content := "http://new.example:80#proxy\nhttp://EXISTING.example#different-name"
	body, _ := json.Marshal(map[string]string{"content": content})
	response := httptest.NewRecorder()
	server.handleImport(response, httptest.NewRequest(http.MethodPost, "/api/import", bytes.NewReader(body)))
	var payload struct {
		Imported          int      `json:"imported"`
		DuplicatesSkipped int      `json:"duplicates_skipped"`
		Errors            []string `json:"errors"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Imported != 1 || payload.DuplicatesSkipped != 1 || len(payload.Errors) != 0 || len(manager.created) != 1 || manager.created[0].Name != "proxy-2" {
		t.Fatalf("payload=%+v created=%+v", payload, manager.created)
	}
}

func TestNodeDuplicateErrorReturnsConflictWithExistingID(t *testing.T) {
	server := &Server{}
	response := httptest.NewRecorder()
	server.respondNodeError(response, &controlplane.NodeDuplicateError{ExistingID: 42, ExistingName: "existing"})
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d", response.Code)
	}
	var payload struct {
		ExistingID int64 `json:"existing_node_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ExistingID != 42 {
		t.Fatalf("payload=%s", response.Body.String())
	}
}

func TestImportRejectsUnknownRequestFields(t *testing.T) {
	manager := &importNodeManager{}
	server := &Server{nodeMgr: manager}
	body := []byte(`{"content":"http://user:pass@example.com:80","unknown":true}`)
	response := httptest.NewRecorder()
	server.handleImport(response, httptest.NewRequest(http.MethodPost, "/api/import", bytes.NewReader(body)))

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if len(manager.created) != 0 {
		t.Fatalf("未知字段不应触发写入: %+v", manager.created)
	}
}

// 结构化 issues 必须带上行号，且被丢弃的行不再静默。
func TestImportReportsLineLevelIssues(t *testing.T) {
	manager := &importNodeManager{}
	server := &Server{nodeMgr: manager}
	content := "http://user:pass@example.com:8080#ok\nssr://example"
	body, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.handleImport(response, httptest.NewRequest(http.MethodPost, "/api/import", bytes.NewReader(body)))

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Imported int `json:"imported"`
		Invalid  int `json:"invalid"`
		Issues   []struct {
			Line    int    `json:"line"`
			Kind    string `json:"kind"`
			Message string `json:"message"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Imported != 1 || payload.Invalid != 1 || len(payload.Issues) != 1 {
		t.Fatalf("payload=%+v", payload)
	}
	issue := payload.Issues[0]
	if issue.Line != 2 || issue.Kind != "unsupported_protocol" || !strings.Contains(issue.Message, "不支持的协议") {
		t.Fatalf("issue=%+v", issue)
	}
}

// 预览不能写库、不能触发重载，但必须给出"将新增多少"。
func TestImportDryRunDoesNotCreateOrReload(t *testing.T) {
	manager := &importNodeManager{}
	server := &Server{nodeMgr: manager}
	body, _ := json.Marshal(map[string]any{
		"content": "http://user:pass@a.example:8080\nhttp://user:pass@b.example:8080",
		"dry_run": true,
	})
	response := httptest.NewRecorder()
	server.handleImport(response, httptest.NewRequest(http.MethodPost, "/api/import", bytes.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		WouldCreate int  `json:"would_create"`
		Created     int  `json:"created"`
		Imported    int  `json:"imported"`
		DryRun      bool `json:"dry_run"`
		Reloaded    bool `json:"reloaded"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.DryRun || payload.WouldCreate != 2 || payload.Created != 0 || payload.Imported != 0 {
		t.Fatalf("payload=%+v", payload)
	}
	if payload.Reloaded {
		t.Fatal("dry run reported a reload")
	}
	if len(manager.created) != 0 {
		t.Fatalf("dry run created %d nodes", len(manager.created))
	}
	if manager.reloads.Load() != 0 {
		t.Fatalf("dry run reloaded %d times", manager.reloads.Load())
	}
}

// 报告里的 invalid 只统计解析问题；写库失败单独计入 failed。
func TestImportSeparatesParseIssuesFromCreateFailures(t *testing.T) {
	manager := &failingCreateNodeManager{}
	server := &Server{nodeMgr: manager}
	body, _ := json.Marshal(map[string]string{
		"content": "http://user:pass@a.example:8080\nssr://unsupported",
	})
	response := httptest.NewRecorder()
	server.handleImport(response, httptest.NewRequest(http.MethodPost, "/api/import", bytes.NewReader(body)))
	var payload struct {
		Invalid int      `json:"invalid"`
		Failed  int      `json:"failed"`
		Errors  []string `json:"errors"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Invalid != 1 || payload.Failed != 1 {
		t.Fatalf("payload=%+v", payload)
	}
}

type failingCreateNodeManager struct {
	reloadNodeManager
}

func (m *failingCreateNodeManager) CreateNode(_ context.Context, node config.NodeConfig) (config.NodeConfig, error) {
	return config.NodeConfig{}, errors.New("build failed")
}

// 不支持批量能力的 runtime 继续逐条创建，行为不变。
func TestBatchCreateFallsBackWithoutBatchCapability(t *testing.T) {
	manager := &importNodeManager{}
	server := &Server{nodeMgr: manager}
	body, _ := json.Marshal(map[string]any{
		"nodes": []map[string]string{{"name": "one", "uri": "http://user:pass@one.example:8080"}},
	})
	response := httptest.NewRecorder()
	server.handleConfigNodesBatchCreate(response, httptest.NewRequest(http.MethodPost, "/api/nodes/config/batch", bytes.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if len(manager.created) != 1 || manager.created[0].Name != "one" {
		t.Fatalf("created=%+v", manager.created)
	}
}

// 单节点路由改用 decodeJSON 后，未知字段必须被拒绝。
func TestNodeRoutesRejectUnknownFields(t *testing.T) {
	manager := &reloadNodeManager{}
	server := &Server{nodeMgr: manager, logger: log.Default()}
	request := httptest.NewRequest(http.MethodPost, "/api/nodes/config",
		strings.NewReader(`{"name":"a","uri":"socks5://127.0.0.1:1080","surprise":true}`))
	response := httptest.NewRecorder()
	server.routes().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

// A fallback runtime has no build dry run, so a dry-run batch must still
// write nothing and must not report a reload.
func TestBatchCreateDryRunWithoutBatchCapabilityWritesNothing(t *testing.T) {
	manager := &importNodeManager{}
	server := &Server{nodeMgr: manager}
	body, _ := json.Marshal(map[string]any{
		"nodes":   []map[string]string{{"name": "one", "uri": "http://user:pass@one.example:8080"}},
		"dry_run": true,
	})
	response := httptest.NewRecorder()
	server.handleConfigNodesBatchCreate(response, httptest.NewRequest(http.MethodPost, "/api/nodes/config/batch", bytes.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if len(manager.created) != 0 {
		t.Fatalf("dry run created %+v", manager.created)
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["dry_run"] != true || payload["imported"].(float64) != 0 || payload["reloaded"] != false {
		t.Fatalf("payload=%v", payload)
	}
}
