package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"easy_proxies/internal/nodeloc"
)

func TestLocationPersistenceRevisionAndRollback(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "location.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	node := &Node{URI: "http://127.0.0.1:8080", Source: NodeSourceManual, Enabled: true}
	if err := db.CreateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	state, err := db.GetNodeLocation(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	obs := nodeloc.Observation{Token: state.Token, ProbeID: "first", StartedAt: base, CheckedAt: base, IP: "203.0.113.1", ISO: "hk", Country: "Hong Kong", Succeeded: true, Source: nodeloc.SourceGeoIPExit}
	if c, err := db.ApplyNodeLocation(ctx, node.ID, obs); err != nil || !c.Changed {
		t.Fatalf("first: %+v %v", c, err)
	}
	obs.ProbeID, obs.ISO, obs.StartedAt, obs.CheckedAt = "second", "jp", base.Add(time.Second), base.Add(time.Second)
	if _, err := db.ApplyNodeLocation(ctx, node.ID, obs); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	state, err = db.GetNodeLocation(ctx, node.ID)
	if err != nil || state.Confirmations != 1 || state.Current.ISO != "hk" {
		t.Fatalf("pending lost on restart: %+v %v", state, err)
	}
	if c, err := db.ApplyNodeLocation(ctx, node.ID, obs); err != nil || c.Applied {
		t.Fatalf("replay: %+v %v", c, err)
	}
	obs.ProbeID, obs.StartedAt, obs.CheckedAt = "third", base.Add(2*time.Second), base.Add(2*time.Second)
	sentinel := errors.New("abort transaction")
	err = db.WithTx(ctx, func(tx Store) error {
		if _, err := tx.ApplyNodeLocation(ctx, node.ID, obs); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	state, err = db.GetNodeLocation(ctx, node.ID)
	if err != nil || state.Current.ISO != "hk" || state.Confirmations != 1 {
		t.Fatalf("rollback leaked: %+v %v", state, err)
	}
	if c, err := db.ApplyNodeLocation(ctx, node.ID, obs); err != nil || c.Location.ISO != "jp" {
		t.Fatalf("confirm: %+v %v", c, err)
	}
	node.URI = "http://127.0.0.2:8080"
	if err := db.UpdateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	node.URI = "http://127.0.0.1:8080"
	if err := db.UpdateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	if c, err := db.ApplyNodeLocation(ctx, node.ID, obs); err != nil || c.Applied || c.Reason != "identity_changed" {
		t.Fatalf("ABA result accepted: %+v %v", c, err)
	}
	fresh, err := db.GetNode(ctx, node.ID)
	if err != nil || fresh.Region != "" {
		t.Fatalf("old identity location retained: %+v %v", fresh, err)
	}
}

func TestLocationProjectionSurvivesUnrelatedUpdates(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "location.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	node := &Node{URI: "http://127.0.0.1:8080", Source: NodeSourceManual, Enabled: true}
	if err := db.CreateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateNodeLocation(ctx, node.ID, "hk", "Hong Kong"); err != nil {
		t.Fatal(err)
	}
	node.Name = "renamed"
	if err := db.UpdateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	if err := db.BulkUpsertNodes(ctx, []Node{*node}); err != nil {
		t.Fatal(err)
	}
	fresh, err := db.GetNode(ctx, node.ID)
	if err != nil || fresh.Region != "hk" {
		t.Fatalf("stale metadata erased projection: %+v %v", fresh, err)
	}
}
