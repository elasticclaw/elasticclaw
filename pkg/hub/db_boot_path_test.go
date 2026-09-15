package hub

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// fatalBootObjects is every table and index fatalBootSchemaSQL creates. It is
// frozen on purpose: adding a statement to that script is adding a way for the
// hub not to boot, and it has happened four times for statements only the
// retention sweeper needs. A change to this list is a reviewable event, not
// something a diff in the middle of a 600-line SQL literal reveals.
var fatalBootObjects = []string{
	"claw_checkpoints", "claw_pr_feedback_deliveries", "claw_prs", "claw_turn_observations", "claws",
	"dependency_status_state", "factory_events", "factory_triggers", "hub_templates",
	"idx_claw_checkpoints_claw", "idx_claw_checkpoints_status", "idx_claw_turn_observations_claw",
	"idx_claws_stage_stalled", "idx_claws_tenant", "idx_factory_triggers_claw",
	"idx_factory_triggers_integration_status", "idx_factory_triggers_key", "idx_messages_claw",
	"idx_messages_pending", "idx_pipeline_gate_results_claw", "idx_pipeline_outputs_claw",
	"idx_pipeline_outputs_stage", "idx_pipeline_stage_history_claw", "idx_slack_deliveries_time",
	"idx_task_run_attempts_run_number", "idx_task_run_events_observed", "idx_task_run_events_run_time",
	"idx_task_run_events_source_event", "idx_task_run_events_tenant_key", "idx_task_run_events_tenant_run_time",
	"idx_task_run_events_type_time", "idx_task_run_prs_repo_pr", "idx_task_run_prs_run",
	"idx_task_run_prs_tenant_merged", "idx_task_run_prs_tenant_run", "idx_task_run_stages_run",
	"idx_task_run_summaries_factory", "idx_task_run_summaries_model", "idx_task_run_summaries_owner_started",
	"idx_task_run_summaries_repo", "idx_task_run_summaries_run", "idx_task_run_summaries_started_run",
	"idx_task_run_summaries_status", "idx_task_run_summaries_ticket_detail", "idx_task_run_summaries_ticket_page",
	"idx_task_run_summaries_timeout", "idx_task_run_summaries_workflow", "idx_task_run_summaries_workspace",
	"idx_task_runs_claw", "idx_task_runs_owner", "idx_task_runs_tenant_created", "idx_task_runs_trigger",
	"idx_workflow_runs_claw", "idx_workflow_runs_status", "idx_workflow_runs_tenant", "idx_workflow_runs_workflow",
	"infra_events", "infra_notification_deliveries", "integration_poll_state", "llm_usage_limits",
	"messages", "model_prices", "pipeline_gate_results", "pipeline_outputs", "pipeline_stage_history",
	"slack_notification_deliveries", "slack_notification_deliveries_v2", "slack_notifier_state",
	"slack_run_threads", "ssh_known_hosts", "task_run_attempts", "task_run_events", "task_run_prs",
	"task_run_stages", "task_run_summaries", "task_run_usage", "task_runs", "tenants", "usage_daily",
	"workflow_runs",
}

var fatalBootCreateRe = regexp.MustCompile(`(?i)\bCREATE\s+(?:UNIQUE\s+)?(?:TABLE|INDEX)\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+)`)

// The fatal boot script creates exactly the objects it did when this test was
// written. Anything the hub can serve without -- retention indexes, the blob
// reference tables -- belongs in a non-fatal helper, where SQLITE_FULL on a
// disk-full hub costs a log line instead of the boot.
func TestFatalBootPathCreatesOnlyTheKnownObjects(t *testing.T) {
	var got []string
	for _, m := range fatalBootCreateRe.FindAllStringSubmatch(fatalBootSchemaSQL, -1) {
		got = append(got, m[1])
	}
	sort.Strings(got)
	want := append([]string(nil), fatalBootObjects...)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("fatalBootSchemaSQL creates a different set of objects than fatalBootObjects lists.\n"+
			"If the new object is something the hub cannot serve without, add it to the list.\n"+
			"If it is not, create it from a non-fatal helper (see ensureRetentionIndexes and\n"+
			"ensureCheckpointBlobRefTables) so a full disk cannot stop the hub from booting.\n\ngot:\n%s\n\nwant:\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	onBootPath := map[string]bool{}
	for _, name := range got {
		onBootPath[name] = true
	}
	for _, table := range checkpointBlobRefTables {
		if onBootPath[table.name] {
			t.Errorf("%s is created on the fatal boot path; it belongs to ensureCheckpointBlobRefTables", table.name)
		}
	}
	for _, idx := range retentionIndexes {
		if onBootPath[idx.name] {
			t.Errorf("%s is created on the fatal boot path; it belongs to ensureRetentionIndexes", idx.name)
		}
	}
}

// The non-fatal tables still exist after a boot, and are rebuilt on the next
// boot when an earlier one could not create them.
func TestCheckpointBlobRefTablesAreBuiltOutsideTheBootCriticalPath(t *testing.T) {
	s := newRetentionTestServer(t)
	for _, table := range checkpointBlobRefTables {
		var ddl string
		if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, table.name).Scan(&ddl); err != nil {
			t.Fatalf("%s missing after migrate: %v", table.name, err)
		}
		if !strings.Contains(strings.ToUpper(ddl), "WITHOUT ROWID") {
			t.Fatalf("%s is not WITHOUT ROWID: %s", table.name, ddl)
		}
		if _, err := s.db.Exec(`DROP TABLE ` + table.name); err != nil {
			t.Fatal(err)
		}
	}
	ensureCheckpointBlobRefTables(s.db)
	for _, table := range checkpointBlobRefTables {
		if _, err := s.db.Exec(`SELECT COUNT(*) FROM ` + table.name); err != nil {
			t.Fatalf("%s was not rebuilt on the retry path: %v", table.name, err)
		}
	}
}

// A checkpoint_blob_refs table created by the per-file version of the model had
// a rowid; its rows are carried into the WITHOUT ROWID shape on the next boot.
func TestCheckpointBlobRefsRowidTableIsRebuiltWithoutRowid(t *testing.T) {
	s := newRetentionTestServer(t)
	if _, err := s.db.Exec(`DROP TABLE checkpoint_blob_refs`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TABLE checkpoint_blob_refs (
		checkpoint_id TEXT NOT NULL, sha256 TEXT NOT NULL, PRIMARY KEY (checkpoint_id, sha256))`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO checkpoint_blob_refs VALUES('cp', ?)`, validTestSHA); err != nil {
		t.Fatal(err)
	}
	ensureCheckpointBlobRefTables(s.db)
	var ddl string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='checkpoint_blob_refs'`).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToUpper(ddl), "WITHOUT ROWID") {
		t.Fatalf("table was not rebuilt: %s", ddl)
	}
	if got := checkpointBlobRefCount(t, s, "cp"); got != 1 {
		t.Fatalf("the rebuild lost rows: %d, want 1", got)
	}
}
