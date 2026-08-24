package store

import (
	"database/sql"
	"fmt"
	"log"
	"time"
)

// Migration represents a single database schema migration.
type Migration struct {
	Version     int
	Description string
	Up          string
}

// allMigrations returns all migrations in order. New migrations should be
// appended to this list with incrementing version numbers.
func allMigrations() []Migration {
	return []Migration{
		{
			Version:     1,
			Description: "initial schema",
			Up: `
-- Nodes table: stores all proxy nodes
CREATE TABLE IF NOT EXISTS nodes (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    uri        TEXT    NOT NULL,
    name       TEXT    NOT NULL DEFAULT '',
    source     TEXT    NOT NULL DEFAULT 'manual',
    port       INTEGER NOT NULL DEFAULT 0,
    username   TEXT    NOT NULL DEFAULT '',
    password   TEXT    NOT NULL DEFAULT '',
    region     TEXT    NOT NULL DEFAULT '',
    country    TEXT    NOT NULL DEFAULT '',
    enabled    INTEGER NOT NULL DEFAULT 1,
    created_at TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT    NOT NULL DEFAULT (datetime('now')),
    UNIQUE(uri)
);

CREATE INDEX IF NOT EXISTS idx_nodes_source  ON nodes(source);
CREATE INDEX IF NOT EXISTS idx_nodes_region  ON nodes(region);
CREATE INDEX IF NOT EXISTS idx_nodes_enabled ON nodes(enabled);

-- Node runtime statistics
CREATE TABLE IF NOT EXISTS node_stats (
    node_id           INTEGER PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    failure_count     INTEGER NOT NULL DEFAULT 0,
    success_count     INTEGER NOT NULL DEFAULT 0,
    blacklisted       INTEGER NOT NULL DEFAULT 0,
    blacklisted_until TEXT    NOT NULL DEFAULT '',
    last_error        TEXT    NOT NULL DEFAULT '',
    last_failure_at   TEXT    NOT NULL DEFAULT '',
    last_success_at   TEXT    NOT NULL DEFAULT '',
    last_latency_ms   INTEGER NOT NULL DEFAULT -1,
    available         INTEGER NOT NULL DEFAULT 0,
    initial_check_done INTEGER NOT NULL DEFAULT 0,
    updated_at        TEXT    NOT NULL DEFAULT (datetime('now'))
);

-- Node event timeline for debug tracking
CREATE TABLE IF NOT EXISTS node_timeline (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id    INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    success    INTEGER NOT NULL DEFAULT 0,
    latency_ms INTEGER NOT NULL DEFAULT 0,
    error      TEXT    NOT NULL DEFAULT '',
    created_at TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_timeline_node_id ON node_timeline(node_id);

-- User authentication sessions
CREATE TABLE IF NOT EXISTS sessions (
    token      TEXT PRIMARY KEY,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    expires_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

-- Subscription refresh status (singleton row)
CREATE TABLE IF NOT EXISTS subscription_status (
    id             INTEGER PRIMARY KEY CHECK (id = 1),
    last_refresh   TEXT    NOT NULL DEFAULT '',
    next_refresh   TEXT    NOT NULL DEFAULT '',
    node_count     INTEGER NOT NULL DEFAULT 0,
    last_error     TEXT    NOT NULL DEFAULT '',
    refresh_count  INTEGER NOT NULL DEFAULT 0,
    is_refreshing  INTEGER NOT NULL DEFAULT 0,
    nodes_hash     TEXT    NOT NULL DEFAULT '',
    updated_at     TEXT    NOT NULL DEFAULT (datetime('now'))
);

-- Insert the singleton subscription status row
INSERT OR IGNORE INTO subscription_status (id) VALUES (1);
`,
		},
		{
			Version:     2,
			Description: "add traffic columns to node_stats",
			Up: `
ALTER TABLE node_stats ADD COLUMN total_upload_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE node_stats ADD COLUMN total_download_bytes INTEGER NOT NULL DEFAULT 0;
`,
		},
		{
			Version:     3,
			Description: "add multiple subscriptions",
			Up: `
CREATE TABLE subscriptions (
    id                       INTEGER PRIMARY KEY AUTOINCREMENT,
    name                     TEXT    NOT NULL DEFAULT '',
    url                      TEXT    NOT NULL UNIQUE,
    enabled                  INTEGER NOT NULL DEFAULT 1,
    refresh_interval_seconds INTEGER NOT NULL DEFAULT 3600,
    refresh_timeout_seconds  INTEGER NOT NULL DEFAULT 30,
    sort_order               INTEGER NOT NULL DEFAULT 0,
    last_attempt             TEXT    NOT NULL DEFAULT '',
    last_success             TEXT    NOT NULL DEFAULT '',
    last_error               TEXT    NOT NULL DEFAULT '',
    node_count               INTEGER NOT NULL DEFAULT 0,
    etag                     TEXT    NOT NULL DEFAULT '',
    last_modified            TEXT    NOT NULL DEFAULT '',
    created_at               TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at               TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE subscription_nodes (
    subscription_id INTEGER NOT NULL REFERENCES subscriptions(id) ON DELETE CASCADE,
    node_id         INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    position        INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (subscription_id, node_id)
);

CREATE INDEX idx_subscriptions_enabled_order ON subscriptions(enabled, sort_order, id);
CREATE INDEX idx_subscription_nodes_node ON subscription_nodes(node_id);
CREATE INDEX idx_subscription_nodes_subscription_position ON subscription_nodes(subscription_id, position);
`,
		},
	}
}

// Migrate applies all pending migrations to the database.
// Each migration runs in its own transaction. Already-applied migrations
// (tracked in schema_migrations) are skipped.
func Migrate(db *sql.DB) error {
	// Create the migrations tracking table
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     INTEGER PRIMARY KEY,
			applied_at  TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT ''
		);
	`); err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}

	// Get the current version
	var currentVersion int
	row := db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_migrations")
	if err := row.Scan(&currentVersion); err != nil {
		return fmt.Errorf("query current version: %w", err)
	}

	migrations := allMigrations()
	applied := 0

	for _, m := range migrations {
		if m.Version <= currentVersion {
			continue
		}

		log.Printf("[store] applying migration %d: %s", m.Version, m.Description)

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin tx for migration %d: %w", m.Version, err)
		}

		if _, err := tx.Exec(m.Up); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("execute migration %d (%s): %w", m.Version, m.Description, err)
		}

		if _, err := tx.Exec(
			"INSERT INTO schema_migrations (version, applied_at, description) VALUES (?, ?, ?)",
			m.Version, time.Now().UTC().Format(time.RFC3339), m.Description,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d: %w", m.Version, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.Version, err)
		}

		applied++
		log.Printf("[store] migration %d applied successfully", m.Version)
	}

	if applied > 0 {
		log.Printf("[store] %d migration(s) applied, current version: %d", applied, migrations[len(migrations)-1].Version)
	} else {
		log.Printf("[store] database schema is up to date (version %d)", currentVersion)
	}

	return nil
}

// CurrentVersion returns the current schema version.
func CurrentVersion(db *sql.DB) (int, error) {
	// Check if schema_migrations table exists
	var count int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master 
		WHERE type='table' AND name='schema_migrations'
	`).Scan(&count)
	if err != nil {
		return 0, err
	}
	if count == 0 {
		return 0, nil
	}

	var version int
	err = db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&version)
	return version, err
}
