package hub

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var updateShippedSchema = flag.Bool("update-shipped-schema", false,
	"rewrite testdata/shipped_schema.sql from the current migrate(); only once production runs this schema")

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
//
// This is the cheap guard over the one SQL constant. It cannot see a fatal step
// written in Go; TestBootSurvivesAFullDiskFromTheShippedSchema can.
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

// ---------------------------------------------------------------------------
// The boot path as a whole, on a full disk
// ---------------------------------------------------------------------------

const shippedSchemaPath = "testdata/shipped_schema.sql"

// openUnmigratedDB opens a file-backed database with the production settings
// and does NOT run migrate() on it.
func openUnmigratedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "hub.db")))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// forbidDatabaseGrowth caps the database at its current size, so every
// statement that needs a new page -- a table, an index, a copy of a table --
// fails with SQLITE_FULL while in-place writes still succeed. That is the
// disk-full hub: WAL frames are reused after a checkpoint, so small updates keep
// working long after the file cannot grow.
func forbidDatabaseGrowth(t *testing.T, db *sql.DB) {
	t.Helper()
	var pages int
	if err := db.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA max_page_count = %d`, pages)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE boot_path_growth_probe (x)`); err == nil {
		t.Fatal("the growth cap is not in effect; the test would prove nothing")
	}
}

func allowDatabaseGrowth(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`PRAGMA max_page_count = 1073741823`); err != nil {
		t.Fatal(err)
	}
}

// schemaObjects lists every table with its columns and every index with its
// name, so two databases can be compared for "migrate() converged to the same
// shape". Columns are compared by name and sorted: an upgraded database adds
// them by ALTER TABLE and never has the DDL text of a fresh one.
func schemaObjects(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`SELECT type, name FROM sqlite_master WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	type object struct{ typ, name string }
	var objects []object
	for rows.Next() {
		var o object
		if err := rows.Scan(&o.typ, &o.name); err != nil {
			t.Fatal(err)
		}
		if o.name != "boot_path_growth_probe" {
			objects = append(objects, o)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, o := range objects {
		if o.typ != "table" {
			fmt.Fprintf(&b, "%s %s\n", o.typ, o.name)
			continue
		}
		cols, err := db.Query(`SELECT name FROM pragma_table_info(?)`, o.name)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for cols.Next() {
			var name string
			if err := cols.Scan(&name); err != nil {
				t.Fatal(err)
			}
			names = append(names, name)
		}
		cols.Close()
		sort.Strings(names)
		fmt.Fprintf(&b, "table %s(%s)\n", o.name, strings.Join(names, ","))
	}
	return b.String()
}

// shippedSchemaDDL is the DDL of every table and index, which is what the
// fixture stores.
func shippedSchemaDDL(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`SELECT sql FROM sqlite_master WHERE sql IS NOT NULL ORDER BY CASE type WHEN 'table' THEN 0 ELSE 1 END, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var ddl string
		if err := rows.Scan(&ddl); err != nil {
			t.Fatal(err)
		}
		b.WriteString(ddl)
		b.WriteString(";\n")
	}
	return b.String()
}

func dumpShippedSchema(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(shippedSchemaDDL(t, db))
	rows, err := db.Query(`SELECT name FROM hub_migrations ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "INSERT INTO hub_migrations(name, applied_at) VALUES('%s', 0);\n", name)
	}
	return b.String()
}

// The upgrade that matters: the current binary boots against the database
// production is running, on a disk that cannot take one more page. migrate()
// must return nil -- every step that needs a page is one the hub can serve
// without, and logs instead. Then, once the disk has room, the next boot must
// converge to exactly the schema a fresh database gets.
//
// A regex over fatalBootSchemaSQL cannot see this: the fifth boot-fatal step
// was a table copy written in Go. Running the whole of migrate() under the cap
// covers the boot path as it is, not as one constant describes it.
func TestBootSurvivesAFullDiskFromTheShippedSchema(t *testing.T) {
	if *updateShippedSchema {
		db := openUnmigratedDB(t)
		if err := migrate(db); err != nil {
			t.Fatal(err)
		}
		header, err := os.ReadFile(shippedSchemaPath)
		if err != nil {
			t.Fatal(err)
		}
		var kept []string
		for _, line := range strings.Split(string(header), "\n") {
			if !strings.HasPrefix(line, "--") {
				break
			}
			kept = append(kept, line)
		}
		out := strings.Join(kept, "\n") + "\n" + dumpShippedSchema(t, db)
		if err := os.WriteFile(shippedSchemaPath, []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("rewrote %s", shippedSchemaPath)
		return
	}
	shipped, err := os.ReadFile(shippedSchemaPath)
	if err != nil {
		t.Fatal(err)
	}
	db := openUnmigratedDB(t)
	if _, err := db.Exec(string(shipped)); err != nil {
		t.Fatalf("load shipped schema: %v", err)
	}
	forbidDatabaseGrowth(t, db)

	if err := migrate(db); err != nil {
		t.Fatalf("migrate() refused to boot on a full disk from the shipped schema: %v\n\n"+
			"A migration step that needs a new page failed and was treated as fatal. If the hub\n"+
			"can serve without what that step creates -- and it could before the step existed --\n"+
			"run it through runOptionalMigration (or the equivalent non-fatal helper) so a full\n"+
			"disk costs a log line instead of the boot. See the rule on migrateAfterSchema.", err)
	}

	// The next boot, with room on the disk, must finish the job.
	allowDatabaseGrowth(t, db)
	if err := migrate(db); err != nil {
		t.Fatalf("migrate() after the disk was freed: %v", err)
	}
	fresh := openUnmigratedDB(t)
	if err := migrate(fresh); err != nil {
		t.Fatal(err)
	}
	if got, want := schemaObjects(t, db), schemaObjects(t, fresh); got != want {
		t.Fatalf("the retried boot did not converge to the fresh schema.\n\nupgraded:\n%s\n\nfresh:\n%s", got, want)
	}
}

// A hub that is already at the current schema reboots on a full disk. Nothing
// on the boot path may need a page then: every step is a no-op or an in-place
// write.
func TestBootSurvivesAFullDiskOnAMigratedDatabase(t *testing.T) {
	db := openUnmigratedDB(t)
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	forbidDatabaseGrowth(t, db)
	if err := migrate(db); err != nil {
		t.Fatalf("a reboot on a full disk refused to boot: %v", err)
	}
}
