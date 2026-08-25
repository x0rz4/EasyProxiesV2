package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestWithTxCommitAndRollback(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "transactions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	committed := &Node{URI: "http://committed.example:80", Name: "committed", Source: NodeSourceManual, Port: 80, Enabled: true}
	if err := db.WithTx(ctx, func(tx Store) error {
		return tx.CreateNode(ctx, committed)
	}); err != nil {
		t.Fatalf("commit transaction: %v", err)
	}
	if got, err := db.GetNodeByURI(ctx, committed.URI); err != nil || got == nil {
		t.Fatalf("committed node: got=%+v err=%v", got, err)
	}

	rollbackErr := errors.New("force rollback")
	rolledBack := &Node{URI: "http://rolled-back.example:80", Name: "rolled back", Source: NodeSourceManual, Port: 80, Enabled: true}
	err = db.WithTx(ctx, func(tx Store) error {
		if err := tx.CreateNode(ctx, rolledBack); err != nil {
			return err
		}
		return rollbackErr
	})
	if !errors.Is(err, rollbackErr) {
		t.Fatalf("rollback transaction error = %v, want %v", err, rollbackErr)
	}
	if got, err := db.GetNodeByURI(ctx, rolledBack.URI); err != nil || got != nil {
		t.Fatalf("rolled-back node: got=%+v err=%v", got, err)
	}
}
