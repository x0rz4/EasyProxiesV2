package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSavedFilterRevisionPreventsLostUpdates(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "filters.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	filter := &SavedFilter{Name: "香港", SpecJSON: `{"version":2,"regions":["hk"]}`}
	if err := db.SaveFilter(ctx, filter); err != nil {
		t.Fatal(err)
	}
	stale := *filter
	filter.Name = "香港节点"
	if err := db.SaveFilter(ctx, filter); err != nil {
		t.Fatal(err)
	}
	stale.SpecJSON = `{"version":2,"regions":["jp"]}`
	if err := db.SaveFilter(ctx, &stale); err == nil {
		t.Fatal("stale writer overwrote saved filter")
	}
	actual, err := db.GetSavedFilter(ctx, filter.ID)
	if err != nil || actual == nil || actual.Revision != 2 || actual.Name != filter.Name {
		t.Fatalf("saved filter=%+v err=%v", actual, err)
	}
	if err := db.DeleteSavedFilter(ctx, filter.ID); err != nil {
		t.Fatal(err)
	}
	actual, err = db.GetSavedFilter(ctx, filter.ID)
	if err != nil || actual != nil {
		t.Fatalf("deleted filter=%+v err=%v", actual, err)
	}
}
