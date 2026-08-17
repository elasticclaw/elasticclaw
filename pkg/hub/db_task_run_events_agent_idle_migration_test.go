package hub

import (
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// The pre-agent_idle task_run_events schema: identical to the current one
// except 'agent_idle' is absent from the event_type CHECK.
const oldTaskRunEventsSchema = `CREATE TABLE task_run_events (
	id                 TEXT PRIMARY KEY,
	tenant_id          TEXT NOT NULL,
	run_id             TEXT NOT NULL REFERENCES task_runs(id) ON DELETE CASCADE,
	attempt_id         TEXT NOT NULL DEFAULT '',
	event_key          TEXT NOT NULL,
	source             TEXT NOT NULL DEFAULT 'hub' CHECK(source IN ('github','linear','shortcut','elasticclaw','hub','provider','agent','unknown')),
	source_event_id    TEXT NOT NULL DEFAULT '',
	source_delivery_id TEXT NOT NULL DEFAULT '',
	event_type         TEXT NOT NULL CHECK(event_type IN (
		'task_start','task_completed','run_claimed','run_queued','provision_started','claw_created','agent_started',
		'creation_failed','provision_failed','bootstrap_failed','model_selected','agent_stopped',
		'manual_stop_before_delivery','provider_lost','done_without_pr','permission_or_auth_failed',
		'timeout','unknown_failure','pr_associated','pr_opened','pr_closed_unmerged','pr_merged',
		'approval_only_pr_review','human_requested_changes','human_review_comment','human_pr_comment',
		'human_manual_code_push','human_tracker_update','human_dashboard_message',
		'human_manual_stop_or_resume','human_settings_or_status_change',
		'unknown_human_interaction','pr_replaced','correction','retraction'
	)),
	event_time         INTEGER NOT NULL,
	observed_at        INTEGER NOT NULL,
	actor_type         TEXT NOT NULL DEFAULT 'unknown' CHECK(actor_type IN ('agent','human','bot','system','unknown')),
	actor_source       TEXT NOT NULL DEFAULT '',
	actor_id           TEXT NOT NULL DEFAULT '',
	actor_login        TEXT NOT NULL DEFAULT '',
	actor_display_name TEXT NOT NULL DEFAULT '',
	actor_classification_reason TEXT NOT NULL DEFAULT '',
	interaction_role   TEXT NOT NULL DEFAULT '' CHECK(interaction_role IN ('','allowed_start','allowed_approval','allowed_merge','warning','neutral','terminal')),
	target_type        TEXT NOT NULL DEFAULT '',
	target_id          TEXT NOT NULL DEFAULT '',
	target_url         TEXT NOT NULL DEFAULT '',
	target_label       TEXT NOT NULL DEFAULT '',
	warning_type       TEXT NOT NULL DEFAULT '',
	failure_type       TEXT NOT NULL DEFAULT '' CHECK(failure_type IN ('','creation_failed','provision_failed','bootstrap_failed','agent_stopped','manual_stop_before_delivery','done_without_pr','no_pr','pr_closed_unmerged','timeout','provider_lost','permission_or_auth_failed','unknown')),
	detail             TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(detail) AND json_type(detail) = 'object'),
	created_at         INTEGER NOT NULL
)`

// The post-agent_idle, pre-CI-events task_run_events schema: identical to
// the current one except ci_succeeded and ci_failed are absent from the
// event_type CHECK.
const agentIdleTaskRunEventsSchema = `CREATE TABLE task_run_events (
	id                 TEXT PRIMARY KEY,
	tenant_id          TEXT NOT NULL,
	run_id             TEXT NOT NULL REFERENCES task_runs(id) ON DELETE CASCADE,
	attempt_id         TEXT NOT NULL DEFAULT '',
	event_key          TEXT NOT NULL,
	source             TEXT NOT NULL DEFAULT 'hub' CHECK(source IN ('github','linear','shortcut','elasticclaw','hub','provider','agent','unknown')),
	source_event_id    TEXT NOT NULL DEFAULT '',
	source_delivery_id TEXT NOT NULL DEFAULT '',
	event_type         TEXT NOT NULL CHECK(event_type IN (
		'task_start','task_completed','run_claimed','run_queued','provision_started','claw_created','agent_started',
		'creation_failed','provision_failed','bootstrap_failed','model_selected','agent_stopped',
		'manual_stop_before_delivery','provider_lost','done_without_pr','permission_or_auth_failed',
		'timeout','unknown_failure','agent_idle','pr_associated','pr_opened','pr_closed_unmerged','pr_merged',
		'approval_only_pr_review','human_requested_changes','human_review_comment','human_pr_comment',
		'human_manual_code_push','human_tracker_update','human_dashboard_message',
		'human_manual_stop_or_resume','human_settings_or_status_change',
		'unknown_human_interaction','pr_replaced','correction','retraction'
	)),
	event_time         INTEGER NOT NULL,
	observed_at        INTEGER NOT NULL,
	actor_type         TEXT NOT NULL DEFAULT 'unknown' CHECK(actor_type IN ('agent','human','bot','system','unknown')),
	actor_source       TEXT NOT NULL DEFAULT '',
	actor_id           TEXT NOT NULL DEFAULT '',
	actor_login        TEXT NOT NULL DEFAULT '',
	actor_display_name TEXT NOT NULL DEFAULT '',
	actor_classification_reason TEXT NOT NULL DEFAULT '',
	interaction_role   TEXT NOT NULL DEFAULT '' CHECK(interaction_role IN ('','allowed_start','allowed_approval','allowed_merge','warning','neutral','terminal')),
	target_type        TEXT NOT NULL DEFAULT '',
	target_id          TEXT NOT NULL DEFAULT '',
	target_url         TEXT NOT NULL DEFAULT '',
	target_label       TEXT NOT NULL DEFAULT '',
	warning_type       TEXT NOT NULL DEFAULT '',
	failure_type       TEXT NOT NULL DEFAULT '' CHECK(failure_type IN ('','creation_failed','provision_failed','bootstrap_failed','agent_stopped','manual_stop_before_delivery','done_without_pr','no_pr','pr_closed_unmerged','timeout','provider_lost','permission_or_auth_failed','unknown')),
	detail             TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(detail) AND json_type(detail) = 'object'),
	created_at         INTEGER NOT NULL
)`

func insertOldSchemaEvent(t *testing.T, db *sql.DB, id, eventType string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO task_run_events(id, tenant_id, run_id, event_key, event_type, event_time, observed_at, created_at)
		VALUES(?,?,?,?,?,?,?,?)`, id, "tenant", "run-1", "key-"+id, eventType, 100, 100, 100); err != nil {
		t.Fatalf("insert event %s: %v", id, err)
	}
}

func TestRebuildTaskRunEventsAgentIdleV1MigratesPopulatedDatabase(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE task_runs (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO task_runs(id) VALUES('run-1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(oldTaskRunEventsSchema); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE UNIQUE INDEX idx_task_run_events_tenant_key ON task_run_events(tenant_id, run_id, event_key)`,
		`CREATE INDEX idx_task_run_events_run_time ON task_run_events(run_id, event_time, id)`,
		`CREATE INDEX idx_task_run_events_type_time ON task_run_events(event_type, event_time)`,
		`CREATE INDEX idx_task_run_events_tenant_run_time ON task_run_events(tenant_id, run_id, event_time, observed_at, event_key)`,
		`CREATE INDEX idx_task_run_events_source_event ON task_run_events(tenant_id, source, source_event_id)`,
		`CREATE INDEX idx_task_run_events_observed ON task_run_events(tenant_id, observed_at)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}

	// Existing rows, including a rowid gap: the lifecycle notifier's cursor
	// keys on rowid, so the rebuild must carry rowids across unchanged.
	insertOldSchemaEvent(t, db, "ev-1", "agent_started")
	insertOldSchemaEvent(t, db, "ev-2", "pr_opened")
	insertOldSchemaEvent(t, db, "ev-3", "agent_stopped")
	if _, err := db.Exec(`DELETE FROM task_run_events WHERE id='ev-2'`); err != nil {
		t.Fatal(err)
	}
	// Sanity: the old CHECK rejects the new type.
	if _, err := db.Exec(`
		INSERT INTO task_run_events(id, tenant_id, run_id, event_key, event_type, event_time, observed_at, created_at)
		VALUES('ev-idle-pre','tenant','run-1','key-pre','agent_idle',100,100,100)`); err == nil {
		t.Fatal("old schema accepted agent_idle; fixture is wrong")
	}

	if err := rebuildTaskRunEventsAgentIdleV1(db); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	// Idempotent on re-run (every later startup probes and skips).
	if err := rebuildTaskRunEventsAgentIdleV1(db); err != nil {
		t.Fatalf("second rebuild: %v", err)
	}

	var schema string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='task_run_events'`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(schema, "'agent_idle'") {
		t.Fatalf("rebuilt schema lacks agent_idle: %s", schema)
	}

	rows, err := db.Query(`SELECT rowid, id, event_type FROM task_run_events ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row struct {
		rowid     int64
		id, etype string
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.rowid, &r.id, &r.etype); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	want := []row{{1, "ev-1", "agent_started"}, {3, "ev-3", "agent_stopped"}}
	if len(got) != len(want) {
		t.Fatalf("rebuilt rows = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %+v, want %+v (rowids must survive the rebuild)", i, got[i], want[i])
		}
	}

	// The new CHECK admits agent_idle and still rejects garbage.
	if _, err := db.Exec(`
		INSERT INTO task_run_events(id, tenant_id, run_id, event_key, event_type, event_time, observed_at, created_at)
		VALUES('ev-idle','tenant','run-1','key-idle','agent_idle',200,200,200)`); err != nil {
		t.Fatalf("rebuilt schema rejects agent_idle: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO task_run_events(id, tenant_id, run_id, event_key, event_type, event_time, observed_at, created_at)
		VALUES('ev-bad','tenant','run-1','key-bad','not_a_type',200,200,200)`); err == nil {
		t.Fatal("rebuilt schema accepts unknown event types")
	}

	for _, name := range []string{
		"idx_task_run_events_tenant_key", "idx_task_run_events_run_time", "idx_task_run_events_type_time",
		"idx_task_run_events_tenant_run_time", "idx_task_run_events_source_event", "idx_task_run_events_observed",
	} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("index %s missing after rebuild", name)
		}
	}
}

// TestRebuildTaskRunEventsAgentIdleV1AddsCIEventsAfterAgentIdle verifies the
// rebuild does not mistake the intermediate agent_idle-only schema for the
// current one, while a subsequent call correctly skips the finished schema.
func TestRebuildTaskRunEventsAgentIdleV1AddsCIEventsAfterAgentIdle(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE task_runs (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO task_runs(id) VALUES('run-1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(agentIdleTaskRunEventsSchema); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE UNIQUE INDEX idx_task_run_events_tenant_key ON task_run_events(tenant_id, run_id, event_key)`,
		`CREATE INDEX idx_task_run_events_run_time ON task_run_events(run_id, event_time, id)`,
		`CREATE INDEX idx_task_run_events_type_time ON task_run_events(event_type, event_time)`,
		`CREATE INDEX idx_task_run_events_tenant_run_time ON task_run_events(tenant_id, run_id, event_time, observed_at, event_key)`,
		`CREATE INDEX idx_task_run_events_source_event ON task_run_events(tenant_id, source, source_event_id)`,
		`CREATE INDEX idx_task_run_events_observed ON task_run_events(tenant_id, observed_at)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	insertOldSchemaEvent(t, db, "ev-idle", "agent_idle")
	if _, err := db.Exec(`INSERT INTO task_run_events(id, tenant_id, run_id, event_key, event_type, event_time, observed_at, created_at) VALUES('ev-ci-before','tenant','run-1','key-ci-before','ci_succeeded',100,100,100)`); err == nil {
		t.Fatal("intermediate schema accepted ci_succeeded; fixture is wrong")
	}

	if err := rebuildTaskRunEventsAgentIdleV1(db); err != nil {
		t.Fatalf("rebuild intermediate schema: %v", err)
	}
	var schema string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='task_run_events'`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(schema, "'ci_succeeded'") || !strings.Contains(schema, "'ci_failed'") {
		t.Fatalf("rebuilt schema lacks CI event types: %s", schema)
	}
	var eventType string
	if err := db.QueryRow(`SELECT event_type FROM task_run_events WHERE id='ev-idle'`).Scan(&eventType); err != nil || eventType != "agent_idle" {
		t.Fatalf("pre-existing agent_idle row = %q, %v; want agent_idle", eventType, err)
	}
	for _, eventType := range []string{"ci_succeeded", "ci_failed"} {
		if _, err := db.Exec(`INSERT INTO task_run_events(id, tenant_id, run_id, event_key, event_type, event_time, observed_at, created_at) VALUES(?,?,?,?,?,?,?,?)`, "ev-"+eventType, "tenant", "run-1", "key-"+eventType, eventType, 200, 200, 200); err != nil {
			t.Fatalf("rebuilt schema rejects %s: %v", eventType, err)
		}
	}

	// A rebuild drops and recreates the table, so this index distinguishes the
	// guard's true skip branch from a second (otherwise successful) rebuild.
	if _, err := db.Exec(`CREATE INDEX idx_task_run_events_guard_sentinel ON task_run_events(id)`); err != nil {
		t.Fatal(err)
	}
	if err := rebuildTaskRunEventsAgentIdleV1(db); err != nil {
		t.Fatalf("second rebuild over current schema: %v", err)
	}
	var sentinelCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_task_run_events_guard_sentinel'`).Scan(&sentinelCount); err != nil {
		t.Fatal(err)
	}
	if sentinelCount != 1 {
		t.Fatal("second rebuild did not skip the current schema")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_run_events WHERE event_type IN ('agent_idle','ci_succeeded','ci_failed')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("rows after skipped second rebuild = %d, want 3", count)
	}
}

// A database that already carries old-schema rows goes through the FULL
// migrate() path — the exact hub-startup scenario — and comes out accepting
// agent_idle with its rows intact.
func TestMigrateAppliesAgentIdleCheckToExistingDatabase(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	// Build a current database, then regress task_run_events to the pre-
	// agent_idle schema with rows in it — the state an old hub leaves behind.
	if err := migrate(db); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE task_run_events`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(oldTaskRunEventsSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO task_runs(id, tenant_id, initial_attempt_id, run_kind, owner_type, created_at, updated_at)
		VALUES('run-1','tenant','attempt-1','pr_task','manual',1,1)`); err != nil {
		t.Fatal(err)
	}
	insertOldSchemaEvent(t, db, "ev-1", "agent_started")

	if err := migrate(db); err != nil {
		t.Fatalf("migrate over old schema: %v", err)
	}
	var etype string
	if err := db.QueryRow(`SELECT event_type FROM task_run_events WHERE id='ev-1'`).Scan(&etype); err != nil || etype != "agent_started" {
		t.Fatalf("pre-existing row lost: %q, %v", etype, err)
	}
	if _, err := db.Exec(`
		INSERT INTO task_run_events(id, tenant_id, run_id, event_key, event_type, event_time, observed_at, created_at)
		VALUES('ev-idle','tenant','run-1','key-idle','agent_idle',200,200,200)`); err != nil {
		t.Fatalf("migrated schema rejects agent_idle: %v", err)
	}
}
