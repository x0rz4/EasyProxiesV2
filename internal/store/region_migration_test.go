package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestRegionMigrationPreservesOldRules(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY,applied_at TEXT NOT NULL,description TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range allMigrations() {
		if migration.Version > 15 {
			break
		}
		if _, err = db.Exec(migration.Up); err != nil {
			t.Fatalf("migration %d: %v", migration.Version, err)
		}
		if _, err = db.Exec(`INSERT INTO schema_migrations(version,applied_at) VALUES(?,'2026-10-15')`, migration.Version); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(`INSERT INTO group_pools(name,bind_port,regions_json) VALUES('legacy',25000,'["other"]')`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO tags(name,rule_json) VALUES('legacy','{"field":"node.region","op":"eq","value":"other"}'),('broken','{')`); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(db); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	var version, includeUnknown int
	var regions, exclude string
	if err = db.QueryRow(`SELECT region_rule_version,include_unknown_region,regions_json,exclude_regions_json FROM group_pools WHERE name='legacy'`).Scan(&version, &includeUnknown, &regions, &exclude); err != nil {
		t.Fatal(err)
	}
	if version != 1 || includeUnknown != 0 || regions != `["other"]` || exclude != `[]` {
		t.Fatalf("legacy changed: %d %d %s %s", version, includeUnknown, regions, exclude)
	}
	if err = db.QueryRow(`SELECT json_extract(rule_json,'$.region_version') FROM tags WHERE name='legacy'`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("legacy tag version=%d err=%v", version, err)
	}
}
