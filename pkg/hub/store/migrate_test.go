package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_time_format=sqlite&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func schemaVersions(t *testing.T, db *sql.DB) []int {
	t.Helper()
	rows, err := db.Query(`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	defer rows.Close()
	var versions []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan version: %v", err)
		}
		versions = append(versions, v)
	}
	return versions
}

func columnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, table, column).Scan(&n); err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	return n > 0
}

func TestMigrateFreshInstall(t *testing.T) {
	db := openRaw(t, filepath.Join(t.TempDir(), "hub.db"))

	if err := Migrate(db); err != nil {
		t.Fatalf("migrate fresh db: %v", err)
	}

	versions := schemaVersions(t, db)
	if len(versions) == 0 || versions[0] != 1 {
		t.Fatalf("expected schema_migrations to start at version 1, got %v", versions)
	}
	for _, table := range []string{"tenants", "claws", "messages", "task_runs", "factory_triggers"} {
		ok, err := tableExists(db, table)
		if err != nil {
			t.Fatalf("tableExists(%s): %v", table, err)
		}
		if !ok {
			t.Fatalf("expected table %s to exist after fresh migrate", table)
		}
	}

	// A second run must be a no-op.
	if err := Migrate(db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if again := schemaVersions(t, db); len(again) != len(versions) {
		t.Fatalf("second migrate changed versions: %v -> %v", versions, again)
	}
}

// TestMigrateLegacyFixtureUpgrade upgrades a committed hub.db created by a
// pre-versioned-migrations release (see testdata/gen_legacy_fixture.go): the
// legacy tables get their missing columns, data is preserved, the data
// migrations run, and the database is baselined at version 1.
func TestMigrateLegacyFixtureUpgrade(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "hub-legacy.db"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "hub.db")
	if err := os.WriteFile(path, fixture, 0o644); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	db := openRaw(t, path)

	if err := Migrate(db); err != nil {
		t.Fatalf("migrate legacy db: %v", err)
	}

	// Baselined at version 1, not re-created from scratch.
	if versions := schemaVersions(t, db); len(versions) == 0 || versions[0] != 1 {
		t.Fatalf("expected baseline version 1, got %v", versions)
	}

	// Columns added after the fixture's vintage now exist.
	for _, c := range []struct{ table, column string }{
		{"claws", "trigger_actor_json"},
		{"claws", "task_run_id"},
		{"claws", "shortcut_story_id"},
		{"messages", "format"},
	} {
		if !columnExists(t, db, c.table, c.column) {
			t.Fatalf("expected column %s.%s after upgrade", c.table, c.column)
		}
	}

	// Tables the fixture predates were created.
	for _, table := range []string{"task_runs", "factory_triggers", "hub_templates"} {
		ok, err := tableExists(db, table)
		if err != nil {
			t.Fatalf("tableExists(%s): %v", table, err)
		}
		if !ok {
			t.Fatalf("expected table %s to exist after upgrade", table)
		}
	}

	// Existing data survived.
	var content string
	if err := db.QueryRow(`SELECT content FROM messages WHERE id='msg-1'`).Scan(&content); err != nil {
		t.Fatalf("read fixture message: %v", err)
	}
	if content != "hello from the old schema" {
		t.Fatalf("fixture message content changed: %q", content)
	}

	// The legacy data migrations ran: the 'sc-' Linear ID moved to
	// shortcut_story_id and a shortcut factory trigger was backfilled.
	var shortcutID string
	if err := db.QueryRow(`SELECT shortcut_story_id FROM claws WHERE id='claw-1'`).Scan(&shortcutID); err != nil {
		t.Fatalf("read shortcut_story_id: %v", err)
	}
	if shortcutID != "sc-123" {
		t.Fatalf("expected shortcut_story_id 'sc-123', got %q", shortcutID)
	}
	var triggers int
	if err := db.QueryRow(`SELECT COUNT(*) FROM factory_triggers WHERE integration='shortcut' AND claw_id='claw-1'`).Scan(&triggers); err != nil {
		t.Fatalf("count factory_triggers: %v", err)
	}
	if triggers != 1 {
		t.Fatalf("expected 1 backfilled shortcut trigger, got %d", triggers)
	}

	// A second boot is a plain no-op (no legacy path, no new migrations).
	if err := Migrate(db); err != nil {
		t.Fatalf("second migrate on upgraded db: %v", err)
	}
}

func TestMigrateRejectsNewerDatabase(t *testing.T) {
	db := openRaw(t, filepath.Join(t.TempDir(), "hub.db"))
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate fresh db: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(version, name, applied_at) VALUES(9999, 'from_the_future.sql', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("insert future version: %v", err)
	}

	err := Migrate(db)
	if err == nil {
		t.Fatal("expected error for database newer than the binary, got nil")
	}
	if !strings.Contains(err.Error(), "downgrades are not supported") {
		t.Fatalf("expected downgrade error, got: %v", err)
	}
}

func TestLoadMigrationsAreWellFormed(t *testing.T) {
	ms, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("no embedded migrations found")
	}
	if ms[0].version != 1 {
		t.Fatalf("first migration must be version 1, got %d (%s)", ms[0].version, ms[0].name)
	}
	for i := 1; i < len(ms); i++ {
		if ms[i].version != ms[i-1].version+1 {
			t.Fatalf("migration versions must be contiguous: %s then %s", ms[i-1].name, ms[i].name)
		}
	}
}
