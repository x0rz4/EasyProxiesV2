package monitor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"easy_proxies/internal/unlock"
)

func TestHandleUnlockMetaListsModularProviders(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/nodes/unlock-meta", nil)
	response := httptest.NewRecorder()
	new(Server).handleUnlockMeta(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Providers []unlock.ProviderMeta `json:"providers"`
		Statuses  []unlock.StatusMeta   `json:"statuses"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	wantProviders := map[string]bool{"gemini": false, "claude": false, "bahamut": false, "youtube": false}
	for _, provider := range payload.Providers {
		if _, wanted := wantProviders[provider.Value]; wanted {
			wantProviders[provider.Value] = provider.Label != "" && provider.Description != "" && provider.Category != ""
		}
	}
	for provider, valid := range wantProviders {
		if !valid {
			t.Fatalf("provider %q metadata missing or incomplete: %+v", provider, payload.Providers)
		}
	}
	if len(payload.Statuses) == 0 {
		t.Fatal("status metadata is empty")
	}
}

func TestHandleUnlockMetaRejectsNonGET(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/nodes/unlock-meta", nil)
	response := httptest.NewRecorder()
	new(Server).handleUnlockMeta(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d", response.Code)
	}
}
