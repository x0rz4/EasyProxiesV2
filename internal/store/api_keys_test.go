package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAPIKeyLifecycle(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "api-keys.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	keys, err := db.ListAPIKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("fresh store listed %d keys, want 0", len(keys))
	}

	key := &APIKey{Name: "CI 导入", Token: "0123456789abcdef"}
	if err := db.CreateAPIKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	if key.ID == 0 {
		t.Fatal("created key has no ID")
	}
	if key.CreatedAt.IsZero() {
		t.Fatal("created key has no timestamp")
	}

	keys, err = db.ListAPIKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Token != key.Token || keys[0].Name != "CI 导入" {
		t.Fatalf("listed keys=%+v", keys)
	}
	if !keys[0].LastUsedAt.IsZero() {
		t.Fatalf("fresh key reported last use %v", keys[0].LastUsedAt)
	}

	found, err := db.GetAPIKeyByToken(ctx, key.Token)
	if err != nil {
		t.Fatal(err)
	}
	if found == nil || found.ID != key.ID {
		t.Fatalf("lookup by token=%+v", found)
	}
	missing, err := db.GetAPIKeyByToken(ctx, "not-a-real-token")
	if err != nil {
		t.Fatal(err)
	}
	if missing != nil {
		t.Fatalf("unknown token resolved to %+v", missing)
	}
	empty, err := db.GetAPIKeyByToken(ctx, "")
	if err != nil || empty != nil {
		t.Fatalf("empty token lookup=%+v err=%v", empty, err)
	}

	usedAt := time.Now().UTC().Truncate(time.Second)
	if err := db.TouchAPIKey(ctx, key.ID, usedAt); err != nil {
		t.Fatal(err)
	}
	touched, err := db.GetAPIKeyByToken(ctx, key.Token)
	if err != nil {
		t.Fatal(err)
	}
	if !touched.LastUsedAt.Equal(usedAt) {
		t.Fatalf("last used at=%v, want %v", touched.LastUsedAt, usedAt)
	}
	if err := db.TouchAPIKey(ctx, key.ID, usedAt.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if latest, err := db.GetAPIKeyByToken(ctx, key.Token); err != nil || !latest.LastUsedAt.Equal(usedAt) {
		t.Fatalf("late old observation overwrote last use: %+v %v", latest, err)
	}

	if err := db.DeleteAPIKey(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	keys, err = db.ListAPIKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("after revoke listed %d keys, want 0", len(keys))
	}
	if _, err := db.GetAPIKeyByToken(ctx, key.Token); err != nil {
		t.Fatal(err)
	}
}

func TestAPIKeyTokenIsUnique(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "api-keys-unique.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.CreateAPIKey(ctx, &APIKey{Token: "duplicate"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateAPIKey(ctx, &APIKey{Token: "duplicate"}); err == nil {
		t.Fatal("stored two credentials with the same token")
	}
}
