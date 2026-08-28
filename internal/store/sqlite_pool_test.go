package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func openSQLiteStoreForPoolTest(t *testing.T) *sqliteStore {
	t.Helper()
	opened, err := Open(filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	store := opened.(*sqliteStore)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestSQLiteConnectionPools(t *testing.T) {
	store := openSQLiteStoreForPoolTest(t)

	if got := store.writerDB.Stats().MaxOpenConnections; got != sqliteWriterConnections {
		t.Fatalf("writer max connections = %d, want %d", got, sqliteWriterConnections)
	}
	if got := store.readerDB.Stats().MaxOpenConnections; got != sqliteReaderConnections {
		t.Fatalf("reader max connections = %d, want %d", got, sqliteReaderConnections)
	}

	ctx := context.Background()
	connections := make([]interface{ Close() error }, 0, sqliteReaderConnections)
	for i := 0; i < sqliteReaderConnections; i++ {
		connection, err := store.readerDB.Conn(ctx)
		if err != nil {
			t.Fatalf("acquire reader connection %d: %v", i, err)
		}
		connections = append(connections, connection)

		var queryOnly, value int
		if err := connection.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil {
			t.Fatalf("query reader pragma on connection %d: %v", i, err)
		}
		if queryOnly != 1 {
			t.Fatalf("reader connection %d query_only = %d, want 1", i, queryOnly)
		}
		if err := connection.QueryRowContext(ctx, "SELECT 1").Scan(&value); err != nil || value != 1 {
			t.Fatalf("query reader connection %d: value=%d err=%v", i, value, err)
		}
	}
	for _, connection := range connections {
		if err := connection.Close(); err != nil {
			t.Fatal(err)
		}
	}

	if got := store.readerDB.Stats().OpenConnections; got < sqliteReaderConnections {
		t.Fatalf("reader opened connections = %d, want at least %d", got, sqliteReaderConnections)
	}
	if _, err := store.readerDB.ExecContext(ctx, "INSERT INTO sessions(token,created_at,expires_at) VALUES('reader-write','','')"); err == nil {
		t.Fatal("reader pool accepted a write while query_only is enabled")
	}
}

func TestSQLiteTransactionReadsUseWriterTransaction(t *testing.T) {
	store := openSQLiteStoreForPoolTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	node := &Node{
		URI:     "http://transaction-visible.example:80",
		Name:    "transaction-visible",
		Source:  NodeSourceManual,
		Port:    80,
		Enabled: true,
	}
	if err := store.WithTx(ctx, func(tx Store) error {
		if err := tx.CreateNode(ctx, node); err != nil {
			return err
		}
		inside, err := tx.GetNodeByURI(ctx, node.URI)
		if err != nil {
			return fmt.Errorf("read own uncommitted node: %w", err)
		}
		if inside == nil || inside.ID != node.ID {
			return fmt.Errorf("transaction read = %+v, want node %d", inside, node.ID)
		}

		outside, err := store.GetNodeByURI(ctx, node.URI)
		if err != nil {
			return fmt.Errorf("concurrent reader during write transaction: %w", err)
		}
		if outside != nil {
			return fmt.Errorf("reader observed uncommitted node: %+v", outside)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	committed, err := store.GetNodeByURI(ctx, node.URI)
	if err != nil || committed == nil || committed.ID != node.ID {
		t.Fatalf("committed node = %+v, err=%v", committed, err)
	}
}

func TestSQLiteNestedTransactionRollbackKeepsReadsInTransaction(t *testing.T) {
	store := openSQLiteStoreForPoolTest(t)
	ctx := context.Background()
	wantErr := errors.New("rollback nested transaction")
	node := &Node{
		URI:     "http://nested-rollback.example:80",
		Name:    "nested-rollback",
		Source:  NodeSourceManual,
		Port:    80,
		Enabled: true,
	}

	err := store.WithTx(ctx, func(tx Store) error {
		return tx.WithTx(ctx, func(nested Store) error {
			if err := nested.CreateNode(ctx, node); err != nil {
				return err
			}
			found, err := nested.GetNodeByURI(ctx, node.URI)
			if err != nil || found == nil {
				return fmt.Errorf("nested transaction read: node=%+v err=%v", found, err)
			}
			return wantErr
		})
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("transaction error = %v, want %v", err, wantErr)
	}
	if found, err := store.GetNodeByURI(ctx, node.URI); err != nil || found != nil {
		t.Fatalf("rolled back node = %+v, err=%v", found, err)
	}
}

func TestSQLiteCloseClosesBothPools(t *testing.T) {
	opened, err := Open(filepath.Join(t.TempDir(), "close.db"))
	if err != nil {
		t.Fatal(err)
	}
	store := opened.(*sqliteStore)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.readerDB.Ping(); err == nil {
		t.Fatal("reader pool remains open after Close")
	}
	if err := store.writerDB.Ping(); err == nil {
		t.Fatal("writer pool remains open after Close")
	}
}
