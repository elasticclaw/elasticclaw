package hub

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
)

// The rule on migrateAfterSchema: a step may be non-fatal only if the running
// binary tolerates its absence. These tests pin the dependency side of it.
//
// TestBootSurvivesAFullDiskFromTheShippedSchema cannot: under PRAGMA
// max_page_count an ADD COLUMN that rewrites an existing sqlite_master page
// succeeds, so "column absent, hub serving" is a state that test never
// reaches -- which is how four columns the checkpoint and workflow hot paths
// name were made non-fatal under a comment calling them telemetry.

// nonFatalMigrationObjects is what every non-fatal step creates, as far as a
// test can remove it after the fact. The serving paths below must work with
// all of it gone.
var nonFatalMigrationObjects = []string{
	`DROP INDEX idx_messages_created_at`,
	`DROP INDEX idx_task_run_events_event_time`,
	`DROP INDEX idx_workflow_v2_runs_timeout`,
	`DROP INDEX idx_task_run_summaries_ticket_page`,
	`DROP TABLE workflow_v2_cron_runs`,
	`DELETE FROM hub_migrations`,
	`DELETE FROM model_prices`,
}

// The serving paths work with everything a non-fatal step creates removed --
// or, for the two first-step objects (the blob reference tables and the cron
// history table), fail at their first step with an error and no side effect.
// A serving path that starts to depend on one of these objects fails here,
// and the step it depends on must then become fatal or the path must learn to
// tolerate the absence.
func TestServingPathsTolerateTheAbsenceOfNonFatalMigrations(t *testing.T) {
	s := newRetentionTestServer(t)
	for _, stmt := range nonFatalMigrationObjects {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	reference := time.Now()
	insertRetentionClaw(t, s, "claw", reference)

	// Checkpoint plan and publish.
	rootSHA, _, plan := planTreeFixture(t, 2)
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "creating", createdAt: reference, noBlobRefs: true})
	if err := s.recordCheckpointBlobRefs("cp", rootSHA, plan); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := s.finalizeCheckpoint("cp", "tenant", "claw", rootSHA); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	// Workflow v2 run creation, the reaper's timeout read and the cron slot
	// release, which name timeout_at and cron_slot_released.
	store := workflowv2.NewStore(s.db)
	run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID: "run", TenantID: "tenant", TriggerType: "cron",
		WorkspaceYAML: []byte(workflowV2APIWorkspace), WorkflowYAML: []byte(workflowV2APIWorkflow),
	})
	if err != nil {
		t.Fatalf("create v2 run: %v", err)
	}
	if _, err := s.db.Exec(`SELECT r.id FROM workflow_v2_runs r WHERE r.status='active' AND r.timeout_at > 0 AND r.timeout_at < ?`, reference.UnixMilli()); err != nil {
		t.Fatalf("reaper timeout query: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE workflow_v2_runs SET cron_slot_released=1 WHERE id=? AND cron_slot_released=0`, run.ID); err != nil {
		t.Fatalf("cron slot release: %v", err)
	}

	// The cron history table is the first thing the v2 cron scheduler
	// touches, before it creates a claw: its absence is a first-step failure.
	if _, err := store.RecordCronRunStarted(context.Background(), "tenant", "ws", "wf", "cron", nil); err == nil {
		t.Fatal("RecordCronRunStarted succeeded without workflow_v2_cron_runs; the scheduler would go on to create a claw with no history row")
	}

	// The blob reference tables: the plan is the first step of a checkpoint
	// and must fail before anything is uploaded, leaving the row untouched.
	for _, table := range checkpointBlobRefTables {
		if _, err := s.db.Exec(`DROP TABLE ` + table.name); err != nil {
			t.Fatal(err)
		}
	}
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp-2", clawID: "claw", status: "creating", createdAt: reference, noBlobRefs: true})
	if err := s.recordCheckpointBlobRefs("cp-2", rootSHA, plan); err == nil {
		t.Fatal("a plan succeeded without the reference tables; the answer would tell the claw to skip uploads nothing protects")
	}
	if status, manifestPath, _ := retentionCheckpointRow(t, s, "cp-2"); status != "creating" || manifestPath != "" {
		t.Fatalf("the failed plan had a side effect: status=%q manifest=%q", status, manifestPath)
	}
}

// The fatal column steps are fatal because a serving path names what they
// create. Each drop below makes that path fail; if a change removes the
// dependency, this test fails and the step may be reconsidered.
func TestFatalMigrationColumnsAreNamedByServingPaths(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Now()
	insertRetentionClaw(t, s, "claw", reference)
	store := workflowv2.NewStore(s.db)
	createRun := func(id string) error {
		_, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
			ID: id, TenantID: "tenant", TriggerType: "cron",
			WorkspaceYAML: []byte(workflowV2APIWorkspace), WorkflowYAML: []byte(workflowV2APIWorkflow),
		})
		return err
	}
	if err := createRun("run"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.db.Exec(`ALTER TABLE workflow_v2_runs DROP COLUMN cron_slot_released`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE workflow_v2_runs SET cron_slot_released=1 WHERE id=? AND cron_slot_released=0`, "run"); err == nil || !strings.Contains(err.Error(), "cron_slot_released") {
		t.Fatalf("releaseWorkflowV2CronSlot's UPDATE without the column: err=%v, want 'no such column: cron_slot_released'", err)
	}

	if _, err := s.db.Exec(`DROP INDEX idx_workflow_v2_runs_timeout; ALTER TABLE workflow_v2_runs DROP COLUMN timeout_at`); err != nil {
		t.Fatal(err)
	}
	if err := createRun("run-2"); err == nil || !strings.Contains(err.Error(), "timeout_at") {
		t.Fatalf("CreateRun without timeout_at: err=%v, want 'no such column: timeout_at'", err)
	}

	for _, col := range []string{"pipeline_stage", "hub_version", "files_count", "files_bytes"} {
		if _, err := s.db.Exec(`ALTER TABLE claw_checkpoints DROP COLUMN ` + col); err != nil {
			t.Fatal(err)
		}
	}
	rootSHA, _, plan := planTreeFixture(t, 1)
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "creating", createdAt: reference, noBlobRefs: true})
	if err := s.recordCheckpointBlobRefs("cp", rootSHA, plan); err != nil {
		t.Fatalf("plan: %v", err)
	}
	err := s.finalizeCheckpoint("cp", "tenant", "claw", rootSHA)
	if err == nil || !strings.Contains(err.Error(), "no such column") {
		t.Fatalf("finalize without the telemetry columns: err=%v, want 'no such column'", err)
	}
	if err := s.completeMetadataOnlyCheckpoint("cp", "claw", "kill", "bridge unreachable"); err == nil || !strings.Contains(err.Error(), "no such column") {
		t.Fatalf("metadata-only capture without the telemetry columns: err=%v, want 'no such column'", err)
	}
}

// A boot that cannot add a column a serving path names refuses to boot, rather
// than serving and failing every checkpoint publish (or every v2 run creation)
// after the work.
//
// The ADD COLUMN is made to fail for a real reason: the table is created in
// its pre-column shape padded to SQLITE_MAX_COLUMN columns, so SQLite rejects
// the addition with "too many columns" while every other statement in
// migrate() works as it does on a shipped database.
func TestMigrateRefusesToBootWithoutAColumnAServingPathNames(t *testing.T) {
	cases := []struct{ name, table, columns, want string }{
		{"claw_checkpoints.pipeline_stage", "claw_checkpoints", legacyClawCheckpointsColumns, "claw_checkpoints.pipeline_stage"},
		{"workflow_v2_runs.timeout_at", "workflow_v2_runs", legacyWorkflowV2RunsColumns, "workflow_v2_runs.timeout_at"},
		{"workflow_v2_runs.cron_slot_released", "workflow_v2_runs",
			legacyWorkflowV2RunsColumns + ",\n\t\ttimeout_at INTEGER NOT NULL DEFAULT 0", "workflow_v2_runs.cron_slot_released"},
		{"workflow_v2_cron_runs.updated_at", "workflow_v2_cron_runs", legacyWorkflowV2CronRunsColumns, "workflow_v2_cron_runs.updated_at"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			db := openUnmigratedDB(t)
			if _, err := db.Exec(paddedCreateTable(tc.table, tc.columns)); err != nil {
				t.Fatal(err)
			}
			err := migrate(db)
			if err == nil {
				t.Fatalf("migrate() booted with %s absent; every path that names it would fail after the boot said it was fine", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("migrate() refused for another reason than %s: %v", tc.want, err)
			}
		})
	}
}

// paddedCreateTable builds a CREATE TABLE with the given columns padded to
// SQLite's default column limit, so that any further ADD COLUMN fails.
func paddedCreateTable(table, columns string) string {
	const sqliteMaxColumn = 2000
	n := strings.Count(columns, ",") + 1
	var b strings.Builder
	b.WriteString("CREATE TABLE " + table + " (" + columns)
	for i := n; i < sqliteMaxColumn; i++ {
		fmt.Fprintf(&b, ", pad_%d INTEGER", i)
	}
	b.WriteString(")")
	return b.String()
}

const legacyClawCheckpointsColumns = `
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
		completed_at          DATETIME`

const legacyWorkflowV2RunsColumns = `
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
		finished_at        INTEGER NOT NULL DEFAULT 0`

const legacyWorkflowV2CronRunsColumns = `
		id             TEXT PRIMARY KEY,
		tenant_id      TEXT NOT NULL,
		workspace_name TEXT NOT NULL,
		workflow_name  TEXT NOT NULL,
		trigger_type   TEXT NOT NULL DEFAULT 'cron',
		status         TEXT NOT NULL DEFAULT 'pending',
		result         TEXT NOT NULL DEFAULT '',
		claw_id        TEXT NOT NULL DEFAULT '',
		v2_run_id      TEXT NOT NULL DEFAULT '',
		run_context    TEXT NOT NULL DEFAULT '{}',
		created_at     INTEGER NOT NULL,
		finished_at    INTEGER NOT NULL DEFAULT 0`
