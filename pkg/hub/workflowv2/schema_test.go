package workflowv2_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	workflowv2 "github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	_ "modernc.org/sqlite"
)

// TestMigrateAddsTimeoutAtIndex verifies that upgrading an older
// workflow_v2_runs table (created without timeout_at) succeeds and adds both
// the column and the reaper index that references it.
func TestMigrateAddsTimeoutAtIndex(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "migrate.db") + "?_txlock=immediate&_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	// Simulate a database from before timeout_at existed.
	if _, err := db.Exec(`
		CREATE TABLE workflow_v2_runs (
			id                 TEXT PRIMARY KEY,
			tenant_id          TEXT NOT NULL,
			workspace_name     TEXT NOT NULL,
			workflow_name      TEXT NOT NULL,
			workspace_revision TEXT NOT NULL,
			workflow_revision  TEXT NOT NULL,
			workspace_yaml     TEXT NOT NULL,
			workflow_yaml      TEXT NOT NULL,
			state              TEXT NOT NULL,
			display_phase      TEXT NOT NULL,
			state_version      INTEGER NOT NULL,
			status             TEXT NOT NULL,
			waiting_reason     TEXT NOT NULL DEFAULT '',
			current_attempt_id TEXT NOT NULL DEFAULT '',
			current_task_id    TEXT NOT NULL DEFAULT '',
			context_bundle_id  TEXT NOT NULL DEFAULT '',
			trigger_type       TEXT NOT NULL DEFAULT 'manual',
			task_run_id        TEXT NOT NULL DEFAULT '',
			created_at         INTEGER NOT NULL,
			updated_at         INTEGER NOT NULL,
			finished_at        INTEGER NOT NULL DEFAULT 0
		);
	`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}

	if err := workflowv2.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var colCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('workflow_v2_runs') WHERE name = 'timeout_at'`).Scan(&colCount); err != nil {
		t.Fatal(err)
	}
	if colCount != 1 {
		t.Fatalf("timeout_at column missing after migrate, count=%d", colCount)
	}

	var idxCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_index_list('workflow_v2_runs') WHERE name = 'idx_workflow_v2_runs_timeout'`).Scan(&idxCount); err != nil {
		t.Fatal(err)
	}
	if idxCount != 1 {
		t.Fatalf("idx_workflow_v2_runs_timeout missing after migrate, count=%d", idxCount)
	}
}
