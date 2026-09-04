package hub

import (
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// A database created before workspace_unresponsive existed must be widened in
// place: the new value becomes writable, every row survives, and the indexes
// come back. Without the widen, the hub would fail every write of a
// watchdog-stopped attempt with a CHECK constraint error.
func TestMigrateWidensFailureTypeCheck(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Pre-migration shape: the narrow CHECK, one row, one index.
	if _, err := db.Exec(`CREATE TABLE task_run_attempts (
		id             TEXT PRIMARY KEY,
		tenant_id      TEXT NOT NULL,
		run_id         TEXT NOT NULL,
		attempt_id     TEXT NOT NULL,
		attempt_number INTEGER NOT NULL CHECK(attempt_number > 0),
		trigger_id     TEXT NOT NULL DEFAULT '',
		claw_id        TEXT NOT NULL DEFAULT '',
		status         TEXT NOT NULL DEFAULT 'running' CHECK(status IN ('running','succeeded','failed')),
		failure_type   TEXT NOT NULL DEFAULT '' CHECK(failure_type IN ('','creation_failed','provision_failed','bootstrap_failed','agent_stopped','manual_stop_before_delivery','done_without_pr','no_pr','pr_closed_unmerged','timeout','provider_lost','permission_or_auth_failed','unknown')),
		started_at     INTEGER NOT NULL,
		finished_at    INTEGER NOT NULL DEFAULT 0,
		created_at     INTEGER NOT NULL,
		updated_at     INTEGER NOT NULL,
		UNIQUE(tenant_id, attempt_id)
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE INDEX idx_widen_probe ON task_run_attempts(run_id, attempt_number)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO task_run_attempts(id,tenant_id,run_id,attempt_id,attempt_number,status,failure_type,started_at,created_at,updated_at)
		VALUES('a1','t1','r1','at1',1,'failed','bootstrap_failed',1,1,1)`); err != nil {
		t.Fatal(err)
	}

	// The narrow CHECK rejects the new value before migration.
	if _, err := db.Exec(`INSERT INTO task_run_attempts(id,tenant_id,run_id,attempt_id,attempt_number,status,failure_type,started_at,created_at,updated_at)
		VALUES('a2','t1','r1','at2',2,'failed','workspace_unresponsive',1,1,1)`); err == nil {
		t.Fatal("expected the pre-migration CHECK to reject workspace_unresponsive")
	}

	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// The value is now writable.
	if _, err := db.Exec(`INSERT INTO task_run_attempts(id,tenant_id,run_id,attempt_id,attempt_number,status,failure_type,started_at,created_at,updated_at)
		VALUES('a2','t1','r1','at2',2,'failed','workspace_unresponsive',1,1,1)`); err != nil {
		t.Fatalf("workspace_unresponsive still rejected after migration: %v", err)
	}

	// The pre-existing row survived untouched.
	var failureType string
	if err := db.QueryRow(`SELECT failure_type FROM task_run_attempts WHERE id='a1'`).Scan(&failureType); err != nil {
		t.Fatalf("pre-existing row lost: %v", err)
	}
	if failureType != "bootstrap_failed" {
		t.Errorf("failure_type = %q, want bootstrap_failed preserved", failureType)
	}

	// The index came back -- a rebuild that drops indexes silently degrades
	// every analytics query that depended on them.
	if n := countRows(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_widen_probe'`); n != 1 {
		t.Error("idx_widen_probe was not recreated by the rebuild")
	}

	// The temporary table must not survive.
	if n := countRows(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE name LIKE '%_widen_tmp'`); n != 0 {
		t.Error("the rebuild left its temporary table behind")
	}

	// Re-running is a no-op.
	if err := migrate(db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM task_run_attempts`); n != 2 {
		t.Errorf("row count = %d after second migrate, want 2", n)
	}
}

// A fresh database gets the value from the base schema, so the widen must find
// nothing to do and the two paths must agree.
func TestFreshSchemaAcceptsWorkspaceUnresponsive(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for _, table := range []string{"task_run_attempts", "task_run_summaries", "task_run_events"} {
		var schema string
		if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&schema); err != nil {
			t.Fatalf("read %s schema: %v", table, err)
		}
		if !strings.Contains(schema, "workspace_unresponsive") {
			t.Errorf("%s does not accept workspace_unresponsive on a fresh database", table)
		}
	}
}

// The mapping is the point of the change: a watchdog-stopped workspace is not a
// bootstrap failure, and the other three kinds still are.
func TestWorkspaceReadinessIsNotBootstrapFailure(t *testing.T) {
	if got := taskRunFailureTypeForAgentFailure(agentFailureWorkspaceReadiness); got != taskRunFailureWorkspaceUnresponsive {
		t.Errorf("readiness maps to %q, want %q", got, taskRunFailureWorkspaceUnresponsive)
	}
	for _, kind := range []agentFailureKind{agentFailureBootstrap, agentFailureRestore, agentFailureWorkspaceFiles} {
		if got := taskRunFailureTypeForAgentFailure(kind); got != taskRunFailureBootstrapFailed {
			t.Errorf("kind %v maps to %q, want bootstrap_failed", kind, got)
		}
	}
}
