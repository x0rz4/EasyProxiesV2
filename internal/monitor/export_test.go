package monitor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type exportNodeManager struct {
	reloadNodeManager
	nodes []ManagedNodeConfig
}

func (m *exportNodeManager) ListConfigNodes(context.Context, *int64) ([]ManagedNodeConfig, error) {
	return append([]ManagedNodeConfig(nil), m.nodes...), nil
}

// 导出范围必须覆盖库内全部节点（含禁用），并与列表页口径一致。
func TestExportIncludesEveryConfiguredNode(t *testing.T) {
	manager := &exportNodeManager{nodes: []ManagedNodeConfig{
		{ID: 3, Name: "zeta", URI: "trojan://pw@zeta.example:443"},
		{ID: 1, Name: "alpha", URI: "http://alpha.example:80"},
		{ID: 2, Name: "beta", URI: "http://beta.example:80", Disabled: true},
		{ID: 4, Name: "blank", URI: "   "},
	}}
	server := &Server{nodeMgr: manager}
	response := httptest.NewRecorder()
	server.handleExport(response, httptest.NewRequest(http.MethodGet, "/api/export", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("X-Export-Count"); got != "3" {
		t.Fatalf("X-Export-Count=%q", got)
	}
	if got := response.Header().Get("Content-Disposition"); got != "attachment; filename=nodes_export.txt" {
		t.Fatalf("Content-Disposition=%q", got)
	}

	body := response.Body.String()
	if body != "http://alpha.example:80\nhttp://beta.example:80\ntrojan://pw@zeta.example:443" {
		t.Fatalf("导出内容或顺序不符合预期: %q", body)
	}
	if !strings.Contains(body, "http://beta.example:80") {
		t.Fatalf("禁用节点被丢弃: %q", body)
	}
	if strings.Contains(body, "blank") {
		t.Fatalf("空 URI 不应出现在导出结果中: %q", body)
	}
}

func TestExportEmptyLibraryReportsZeroCount(t *testing.T) {
	server := &Server{nodeMgr: &exportNodeManager{}}
	response := httptest.NewRecorder()
	server.handleExport(response, httptest.NewRequest(http.MethodGet, "/api/export", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	if response.Body.Len() != 0 {
		t.Fatalf("body=%q", response.Body.String())
	}
	if got := response.Header().Get("X-Export-Count"); got != "0" {
		t.Fatalf("X-Export-Count=%q", got)
	}
}

func TestExportWithoutNodeManagerReturnsServiceUnavailable(t *testing.T) {
	server := &Server{}
	response := httptest.NewRecorder()
	server.handleExport(response, httptest.NewRequest(http.MethodGet, "/api/export", nil))

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
