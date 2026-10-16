package store

import (
	"testing"
	"time"

	json "easy_proxies/internal/jsonx"
	"easy_proxies/internal/noderesult"
)

// The store embeds noderesult rather than mirroring it. Embedding keeps the
// detectors' payloads in one place, but it also means the struct's JSON field
// order is now an implicit contract: probe events are hashed canonically to
// derive receipt IDs, and the WebUI reads these payloads back. Any reordering
// of the embedded fields would change the canonical bytes of every stored
// event and break reconciliation of pending receipts. These tests pin the
// exact wire shape so such a change fails loudly here instead of silently in
// production.

func TestUnlockResultJSONShapeIsStable(t *testing.T) {
	checked := time.Date(2026, 10, 17, 1, 2, 3, 0, time.UTC)
	stored := UnlockResult{
		NodeID: 7,
		Unlock: noderesult.Unlock{
			Tag:  "tag@v1",
			Name: "tokyo",
			Services: []noderesult.Service{
				{Name: "netflix", DisplayName: "Netflix", Status: "unlocked", Region: "HK", Detail: "原生解锁"},
			},
			IP:       noderesult.IPInfo{IP: "203.0.113.7", ISOCode: "JP", Pure: true},
			Error:    "e",
			Duration: 1234,
		},
		CheckedAt:  checked,
		ResultJSON: `{}`,
		UpdatedAt:  checked,
	}

	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"node_id":7,"tag":"tag@v1","name":"tokyo",` +
		`"services":[{"name":"netflix","display_name":"Netflix","status":"unlocked","region":"HK","detail":"原生解锁"}],` +
		`"ip":{"ip":"203.0.113.7","iso_code":"JP","pure":true},` +
		`"error":"e","duration_ms":1234,` +
		`"checked_at":"2026-10-17T01:02:03Z","result_json":"{}","updated_at":"2026-10-17T01:02:03Z"}`
	if string(raw) != want {
		t.Fatalf("unlock JSON shape changed:\n got: %s\nwant: %s", raw, want)
	}
}

func TestNodeIPQualityResultJSONShapeIsStable(t *testing.T) {
	checked := time.Date(2026, 10, 17, 1, 2, 3, 0, time.UTC)
	yes, no, score := true, false, 30
	stored := NodeIPQualityResult{
		NodeID: 7,
		TaskID: "task",
		Quality: noderesult.Quality{
			Provider:      "ippure",
			Status:        "success",
			IP:            "203.0.113.7",
			Family:        "ipv4",
			Country:       "JP",
			CountryCode:   "JP",
			ASN:           "AS1",
			Org:           "org",
			ISP:           "isp",
			IsBroadcast:   &yes,
			IsResidential: &no,
			FraudScore:    &score,
			Proxy:         &no,
			Hosting:       &no,
			Mobile:        &yes,
			Reason:        "r",
			CheckedAt:     checked,
		},
		UpdatedAt: checked,
	}

	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"node_id":7,"task_id":"task","provider":"ippure","status":"success",` +
		`"ip":"203.0.113.7","family":"ipv4","country":"JP","country_code":"JP",` +
		`"asn":"AS1","org":"org","isp":"isp","is_broadcast":true,"is_residential":false,` +
		`"fraud_score":30,"proxy":false,"hosting":false,"mobile":true,"reason":"r",` +
		`"checked_at":"2026-10-17T01:02:03Z","updated_at":"2026-10-17T01:02:03Z"}`
	if string(raw) != want {
		t.Fatalf("quality JSON shape changed:\n got: %s\nwant: %s", raw, want)
	}
}

// Promoted fields are the reason the conversion code could be deleted: metadata
// and payload are written in one literal and read back through the same names.
func TestEmbeddedPayloadFieldsArePromoted(t *testing.T) {
	quality := NodeIPQualityResult{NodeID: 1, Provider: "ippure", Status: "success"}
	if quality.NodeID != 1 || quality.Provider != "ippure" || quality.Status != "success" {
		t.Fatalf("promoted fields not reachable: %+v", quality)
	}
	unlock := UnlockResult{NodeID: 2, Unlock: noderesult.Unlock{Tag: "t", Name: "n"}}
	if unlock.NodeID != 2 || unlock.Tag != "t" || unlock.Name != "n" {
		t.Fatalf("promoted fields not reachable: %+v", unlock)
	}
}

// A row round-trips through SQLite without losing the embedded payload, which is
// the behavior the removed field-by-field copy used to guarantee.
func TestUnlockResultRoundTripPreservesPayload(t *testing.T) {
	checked := time.Date(2026, 10, 17, 1, 2, 3, 0, time.UTC)
	db, err := Open(t.TempDir() + "/unlock-roundtrip.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := t.Context()
	node := &Node{URI: "ss://node@127.0.0.1:1080", Name: "tokyo", Source: NodeSourceManual, Enabled: true}
	if err := db.CreateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(&noderesult.Unlock{
		Tag: "tag@v1", Name: "tokyo",
		Services: []noderesult.Service{{Name: "netflix", DisplayName: "Netflix", Status: "unlocked", Region: "HK", Detail: "原生解锁"}},
		IP:       noderesult.IPInfo{IP: "203.0.113.7", ISOCode: "JP", Pure: true, RiskLevel: "Low"},
		Duration: 1234,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertUnlockResult(ctx, &UnlockResult{
		NodeID: node.ID, CheckedAt: checked, ResultJSON: string(payload),
		Unlock: noderesult.Unlock{Tag: "tag@v1", Name: "tokyo", Duration: 1234,
			Services: []noderesult.Service{{Name: "netflix", DisplayName: "Netflix", Status: "unlocked", Region: "HK", Detail: "原生解锁"}},
			IP:       noderesult.IPInfo{IP: "203.0.113.7", ISOCode: "JP", Pure: true, RiskLevel: "Low"}},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetUnlockResult(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("stored unlock result went missing")
	}
	if got.NodeID != node.ID || got.Tag != "tag@v1" || got.Name != "tokyo" || got.Duration != 1234 {
		t.Fatalf("metadata lost: %+v", got)
	}
	if got.IP.IP != "203.0.113.7" || got.IP.ISOCode != "JP" || !got.IP.Pure {
		t.Fatalf("embedded IP payload lost: %+v", got.IP)
	}
	// Services are reconstructed from result_json when present, so the detail and
	// region that the indexed columns cannot hold must survive.
	if len(got.Services) != 1 || got.Services[0].Detail != "原生解锁" || got.Services[0].Region != "HK" {
		t.Fatalf("embedded service payload lost: %+v", got.Services)
	}
}

func TestNodeIPQualityResultRoundTripPreservesPayload(t *testing.T) {
	db, err := Open(t.TempDir() + "/quality-roundtrip.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := t.Context()
	node := &Node{URI: "ss://quality@127.0.0.1:1080", Name: "osaka", Source: NodeSourceManual, Enabled: true}
	if err := db.CreateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	residential, score := true, 42
	if err := db.UpsertNodeIPQualityResult(ctx, &NodeIPQualityResult{
		NodeID: node.ID, TaskID: "task",
		Quality: noderesult.Quality{Provider: "ippure", Status: "success", IP: "203.0.113.9",
			CountryCode: "JP", IsResidential: &residential, FraudScore: &score, Reason: "r"},
	}); err != nil {
		t.Fatal(err)
	}

	all, err := db.ListNodeIPQualityResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rows := all[node.ID]
	if len(rows) != 1 {
		t.Fatalf("want 1 stored row, got %+v", rows)
	}
	row := rows[0]
	if row.NodeID != node.ID || row.TaskID != "task" || row.Provider != "ippure" || row.CountryCode != "JP" {
		t.Fatalf("metadata or embedded payload lost: %+v", row)
	}
	// Nullable payload fields must survive as pointers so the UI can still tell
	// "measured false" from "never measured".
	if row.IsResidential == nil || !*row.IsResidential || row.FraudScore == nil || *row.FraudScore != 42 {
		t.Fatalf("nullable payload fields lost: %+v", row)
	}
}
