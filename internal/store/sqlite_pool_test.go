package store

import (
	"context"
	"database/sql"
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
	t.Setenv("EASY_PROXIES_SQLITE_CACHE_KIB", "")
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

		var queryOnly, value, cacheSize int
		if err := connection.QueryRowContext(ctx, "PRAGMA cache_size").Scan(&cacheSize); err != nil || cacheSize != -defaultSQLiteCacheKiB {
			t.Fatalf("reader %d page cache=%d err=%v", i, cacheSize, err)
		}
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

func TestSQLiteCacheBudgetOverride(t *testing.T) {
	t.Setenv("EASY_PROXIES_SQLITE_CACHE_KIB", "2048")
	store := openSQLiteStoreForPoolTest(t)
	for name, db := range map[string]*sql.DB{"writer": store.writerDB, "reader": store.readerDB} {
		var size int
		if err := db.QueryRowContext(t.Context(), "PRAGMA cache_size").Scan(&size); err != nil || size != -2048 {
			t.Fatalf("%s cache=%d err=%v", name, size, err)
		}
	}
}

func TestSQLiteRejectsInvalidCacheBudget(t *testing.T) {
	for _, value := range []string{"0", "-1", "65537", "invalid"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("EASY_PROXIES_SQLITE_CACHE_KIB", value)
			if db, err := Open(filepath.Join(t.TempDir(), "invalid.db")); err == nil {
				db.Close()
				t.Fatal("invalid cache budget accepted")
			}
		})
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

func TestSQLiteReadTransactionSnapshotAllowsWriter(t *testing.T) {
	db := openSQLiteStoreForPoolTest(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	node := &Node{URI: "http://snapshot.example:80", Name: "before", Source: NodeSourceManual, Enabled: true}
	if err := db.CreateNode(ctx, node); err != nil {
		t.Fatal(err)
	}
	if err := db.WithReadTx(ctx, func(tx Store) error {
		before, err := tx.GetNode(ctx, node.ID)
		if err != nil {
			return err
		}
		node.Name = "after"
		// A query using the writer would deadlock here until the deadline.
		if err := db.UpdateNode(ctx, node); err != nil {
			return fmt.Errorf("writer during read snapshot: %w", err)
		}
		return tx.WithReadTx(ctx, func(nested Store) error {
			after, err := nested.GetNode(ctx, node.ID)
			if err != nil {
				return err
			}
			if after.Name != before.Name {
				return fmt.Errorf("snapshot changed: %s -> %s", before.Name, after.Name)
			}
			if err := nested.WithTx(ctx, func(write Store) error { return write.UpdateNode(ctx, node) }); err == nil {
				return errors.New("nested writer escaped read-only transaction")
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	found, err := db.GetNode(ctx, node.ID)
	if err != nil || found.Name != "after" {
		t.Fatalf("committed writer: %+v, %v", found, err)
	}
}
