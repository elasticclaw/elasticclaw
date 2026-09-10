package hub

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// An existing hub database predates the checkpoint telemetry columns. These
// three values used to live only inside the manifest JSON, so pruning manifests
// destroyed the record; as columns they survive any retention policy. The
// migration must be additive and leave existing rows readable.
func TestMigrateAddsCheckpointTelemetryToExistingClawCheckpoints(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Pre-migration shape of claw_checkpoints.
	if _, err := db.Exec(`CREATE TABLE claw_checkpoints (
		id                    TEXT PRIMARY KEY,
		tenant_id             TEXT NOT NULL,
		claw_id               TEXT NOT NULL,
		status                TEXT NOT NULL DEFAULT 'creating',
		reason                TEXT NOT NULL DEFAULT '',
		created_by            TEXT NOT NULL DEFAULT 'hub',
		provider              TEXT NOT NULL DEFAULT '',
		provider_id_at_create TEXT NOT NULL DEFAULT '',
		manifest_sha256       TEXT NOT NULL DEFAULT '',
		manifest_path         TEXT NOT NULL DEFAULT '',
		root_tree_sha256      TEXT NOT NULL DEFAULT '',
		message_tree_sha256   TEXT NOT NULL DEFAULT '',
		workspace_tree_sha256 TEXT NOT NULL DEFAULT '',
		message_count         INTEGER NOT NULL DEFAULT 0,
		pr_count              INTEGER NOT NULL DEFAULT 0,
		repo_count            INTEGER NOT NULL DEFAULT 0,
		error                 TEXT NOT NULL DEFAULT '',
		created_at            DATETIME NOT NULL,
		completed_at          DATETIME
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO claw_checkpoints(id,tenant_id,claw_id,status,reason,message_count,created_at)
		VALUES('cp1','t1','claw1','ready','idle-timer',42,datetime('now'))`); err != nil {
		t.Fatal(err)
	}

	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var stage, version string
	var filesCount, filesBytes, messageCount int64
	if err := db.QueryRow(`SELECT pipeline_stage, hub_version, files_count, files_bytes, message_count
		FROM claw_checkpoints WHERE id='cp1'`).Scan(&stage, &version, &filesCount, &filesBytes, &messageCount); err != nil {
		t.Fatalf("select after migration: %v", err)
	}
	if stage != "" || version != "" {
		t.Fatalf("expected empty defaults for pre-existing row, got stage=%q version=%q", stage, version)
	}
	if filesCount != 0 || filesBytes != 0 {
		t.Fatalf("expected zero file aggregates, got count=%d bytes=%d", filesCount, filesBytes)
	}
	if messageCount != 42 {
		t.Fatalf("migration must preserve existing data: message_count=%d, want 42", messageCount)
	}

	// Re-running must be a no-op rather than an error.
	if err := migrate(db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

// A fresh database must get the columns from the base schema, not the ALTER
// phase -- the two claw_checkpoints DDLs in db.go have to stay in sync.
func TestFreshSchemaHasCheckpointTelemetryColumns(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for _, col := range []string{"pipeline_stage", "hub_version", "files_count", "files_bytes"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('claw_checkpoints') WHERE name = ?`, col).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("fresh schema is missing claw_checkpoints.%s", col)
		}
	}
}
