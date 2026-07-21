package hub

import (
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestRebuildTaskRunSummariesStatusV3MigratesOldSchema(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE task_runs (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	// This is the pre-v3 table definition: only the status CHECK differs from
	// the current task_run_summaries schema.
	if _, err := db.Exec(`CREATE TABLE task_run_summaries (
		id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, run_id TEXT NOT NULL REFERENCES task_runs(id) ON DELETE CASCADE,
		initial_attempt_id TEXT NOT NULL DEFAULT '', current_attempt_id TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL CHECK(status IN ('running','clean_success','human_in_the_loop','warning_success','failed')),
		phase TEXT NOT NULL CHECK(phase IN ('claimed','queued','provisioning','agent_running','pr_opened','waiting_for_merge','terminal')),
		attempt_count INTEGER NOT NULL DEFAULT 0 CHECK(attempt_count >= 0), owner_type TEXT NOT NULL DEFAULT '', workspace_name TEXT NOT NULL DEFAULT '', workflow_name TEXT NOT NULL DEFAULT '', factory_name TEXT NOT NULL DEFAULT '', owner_id TEXT NOT NULL DEFAULT '', owner_display_name TEXT NOT NULL DEFAULT '', run_kind TEXT NOT NULL DEFAULT 'pr_task' CHECK(run_kind IN ('code_task','pr_task')), integration TEXT NOT NULL DEFAULT '', integration_workspace TEXT NOT NULL DEFAULT '', issue_id TEXT NOT NULL DEFAULT '', issue_title TEXT NOT NULL DEFAULT '', issue_created_at INTEGER NOT NULL DEFAULT 0, claw_id TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0, total_tokens INTEGER NOT NULL DEFAULT 0, estimated_cost_usd REAL NOT NULL DEFAULT 0, usage_updated_at INTEGER NOT NULL DEFAULT 0, llm_key TEXT NOT NULL DEFAULT '', repo TEXT NOT NULL DEFAULT '', primary_pr_url TEXT NOT NULL DEFAULT '', pr_count INTEGER NOT NULL DEFAULT 0 CHECK(pr_count >= 0), open_pr_count INTEGER NOT NULL DEFAULT 0 CHECK(open_pr_count >= 0), merged_pr_count INTEGER NOT NULL DEFAULT 0 CHECK(merged_pr_count >= 0), closed_pr_count INTEGER NOT NULL DEFAULT 0 CHECK(closed_pr_count >= 0), warning_types TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(warning_types) AND json_type(warning_types) = 'array'), failure_type TEXT NOT NULL DEFAULT '' CHECK(failure_type IN ('','creation_failed','provision_failed','bootstrap_failed','agent_stopped','manual_stop_before_delivery','done_without_pr','no_pr','pr_closed_unmerged','timeout','provider_lost','permission_or_auth_failed','unknown')), human_interaction_count INTEGER NOT NULL DEFAULT 0 CHECK(human_interaction_count >= 0), started_at INTEGER NOT NULL, queued_at INTEGER NOT NULL DEFAULT 0, provision_started_at INTEGER NOT NULL DEFAULT 0, agent_started_at INTEGER NOT NULL DEFAULT 0, pr_opened_at INTEGER NOT NULL DEFAULT 0, merged_at INTEGER NOT NULL DEFAULT 0, finished_at INTEGER NOT NULL DEFAULT 0, timeout_at INTEGER NOT NULL DEFAULT 0, last_event_at INTEGER NOT NULL, materialized_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, analytics_enabled INTEGER NOT NULL DEFAULT 1 CHECK(analytics_enabled IN (0,1)), requires_pr INTEGER NOT NULL DEFAULT 1 CHECK(requires_pr IN (0,1)), excluded_reason TEXT NOT NULL DEFAULT '', UNIQUE(run_id), UNIQUE(tenant_id, run_id)
	)`); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE INDEX idx_task_run_summaries_status ON task_run_summaries(tenant_id, status, started_at DESC)`,
		`CREATE INDEX idx_task_run_summaries_owner_started ON task_run_summaries(workspace_name, owner_type, owner_display_name, started_at DESC)`,
		`CREATE INDEX idx_task_run_summaries_run ON task_run_summaries(run_id)`,
		`CREATE INDEX idx_task_run_summaries_started_run ON task_run_summaries(tenant_id, started_at DESC, run_id DESC)`,
		`CREATE INDEX idx_task_run_summaries_workspace ON task_run_summaries(tenant_id, workspace_name, started_at DESC)`,
		`CREATE INDEX idx_task_run_summaries_workflow ON task_run_summaries(tenant_id, workspace_name, workflow_name, started_at DESC)`,
		`CREATE INDEX idx_task_run_summaries_factory ON task_run_summaries(tenant_id, factory_name, started_at DESC)`,
		`CREATE INDEX idx_task_run_summaries_model ON task_run_summaries(tenant_id, model, started_at DESC)`,
		`CREATE INDEX idx_task_run_summaries_repo ON task_run_summaries(tenant_id, repo, started_at DESC)`,
		`CREATE INDEX idx_task_run_summaries_timeout ON task_run_summaries(tenant_id, timeout_at)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}

	statuses := []struct{ old, want string }{{"running", "running"}, {"clean_success", "clean"}, {"human_in_the_loop", "human_in_the_loop"}, {"warning_success", "warning"}, {"failed", "failed"}}
	for i, tc := range statuses {
		runID := "run-" + tc.old
		if _, err := db.Exec(`INSERT INTO task_runs(id) VALUES(?)`, runID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO task_run_summaries(id, tenant_id, run_id, status, phase, input_tokens, output_tokens, total_tokens, estimated_cost_usd, usage_updated_at, started_at, last_event_at, materialized_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, "summary-"+tc.old, "tenant", runID, tc.old, "claimed", i+1, i+11, i+21, float64(i)+0.25, i+31, i+41, i+51, i+61, i+71); err != nil {
			t.Fatal(err)
		}
	}

	if err := rebuildTaskRunSummariesStatusV3(db); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	var schema string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='task_run_summaries'`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(schema, "'running','clean','human_in_the_loop','warning','failed'") || strings.Contains(schema, "clean_success") || strings.Contains(schema, "warning_success") {
		t.Fatalf("unexpected rebuilt schema: %s", schema)
	}
	for i, tc := range statuses {
		var status string
		var input, output, total, usageUpdated int
		var cost float64
		if err := db.QueryRow(`SELECT status, input_tokens, output_tokens, total_tokens, estimated_cost_usd, usage_updated_at FROM task_run_summaries WHERE run_id=?`, "run-"+tc.old).Scan(&status, &input, &output, &total, &cost, &usageUpdated); err != nil {
			t.Fatal(err)
		}
		if status != tc.want || input != i+1 || output != i+11 || total != i+21 || cost != float64(i)+0.25 || usageUpdated != i+31 {
			t.Fatalf("row %s changed: status=%q input=%d output=%d total=%d cost=%v usage_updated_at=%d", tc.old, status, input, output, total, cost, usageUpdated)
		}
	}
	for _, name := range []string{"idx_task_run_summaries_status", "idx_task_run_summaries_owner_started", "idx_task_run_summaries_run", "idx_task_run_summaries_started_run", "idx_task_run_summaries_workspace", "idx_task_run_summaries_workflow", "idx_task_run_summaries_factory", "idx_task_run_summaries_model", "idx_task_run_summaries_repo", "idx_task_run_summaries_timeout"} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND tbl_name='task_run_summaries' AND name=?`, name).Scan(&count); err != nil || count != 1 {
			t.Fatalf("index %s count=%d err=%v", name, count, err)
		}
	}
	if err := rebuildTaskRunSummariesStatusV3(db); err != nil {
		t.Fatalf("idempotent rebuild: %v", err)
	}
}
