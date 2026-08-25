package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"easy_proxies/internal/config"
)

type importNodeManager struct {
	reloadNodeManager
	existing []ManagedNodeConfig
	created  []config.NodeConfig
}

func (m *importNodeManager) ListConfigNodes(context.Context, *int64) ([]ManagedNodeConfig, error) {
	return append([]ManagedNodeConfig(nil), m.existing...), nil
}

func (m *importNodeManager) CreateNode(_ context.Context, node config.NodeConfig) (config.NodeConfig, error) {
	m.created = append(m.created, node)
	m.existing = append(m.existing, ManagedNodeConfig{Name: node.Name, URI: node.URI})
	return node, nil
}

func TestImportHTTPProxyGeneratesUniqueNames(t *testing.T) {
	manager := &importNodeManager{existing: []ManagedNodeConfig{{Name: "imported-1", URI: "http://existing.example:80"}}}
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
	if len(manager.created) != 2 || manager.created[0].Name != "imported-2" || manager.created[1].Name != "imported-3" {
		t.Fatalf("created nodes=%+v", manager.created)
	}
	if manager.created[0].URI != "http://mscaei8ikxp4:rr62tghfcit1twh@104.207.47.150:3129" {
		t.Fatalf("HTTP proxy URI=%q", manager.created[0].URI)
	}
}

func TestImportSuffixesExplicitDuplicateNameAndRejectsDuplicateURI(t *testing.T) {
	manager := &importNodeManager{existing: []ManagedNodeConfig{{Name: "proxy", URI: "http://existing.example:80"}}}
	server := &Server{nodeMgr: manager}
	content := "http://new.example:80#proxy\nhttp://existing.example:80"
	body, _ := json.Marshal(map[string]string{"content": content})
	response := httptest.NewRecorder()
	server.handleImport(response, httptest.NewRequest(http.MethodPost, "/api/import", bytes.NewReader(body)))
	var payload struct {
		Imported int      `json:"imported"`
		Errors   []string `json:"errors"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Imported != 1 || len(payload.Errors) != 1 || len(manager.created) != 1 || manager.created[0].Name != "proxy-2" {
		t.Fatalf("payload=%+v created=%+v", payload, manager.created)
	}
}
