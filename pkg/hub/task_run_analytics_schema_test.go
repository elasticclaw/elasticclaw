package hub

import (
	"context"
	"testing"
)

func TestWorkflowV2AnalyticsMigrationPreservesChildrenAndIndexes(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "migration-claw")
	runID, _ := startTaskRunForTest(t, s, "migration-claw", "migration")
	associatePRForTest(t, s, runID, "org/repo", 1, "open")
	counts := map[string]int{}
	for _, table := range []string{"task_runs", "task_run_attempts", "task_run_events", "task_run_prs", "task_run_summaries"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Fatalf("fixture missing %s", table)
		}
		counts[table] = n
	}
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct{ table, current, legacy string }{
		{"task_runs", "'code_task','pr_task','maintenance')", "'code_task','pr_task')"},
		{"task_run_summaries", "'code_task','pr_task','maintenance')", "'code_task','pr_task')"},
		{"task_run_summaries", "'running','clean','human_in_the_loop','warning','failed','cancelled')", "'running','clean','human_in_the_loop','warning','failed')"},
	} {
		var schema string
		if err := conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE name=?`, change.table).Scan(&schema); err != nil {
			t.Fatal(err)
		}
		if err := rebuildTaskRunCheck(conn, change.table, schema, change.current, change.legacy); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.ExecContext(ctx, `ALTER TABLE task_run_summaries DROP COLUMN outcome`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	var indexesBefore int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND tbl_name IN ('task_runs','task_run_summaries')`).Scan(&indexesBefore); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := migrateTaskRunAnalyticsOutcomes(db); err != nil {
			t.Fatal(err)
		}
		for table, want := range counts {
			var got int
			if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("%s: %d rows after migration, want %d", table, got, want)
			}
		}
	}
	var indexesAfter, foreignKeys, violations int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND tbl_name IN ('task_runs','task_run_summaries')`).Scan(&indexesAfter); err != nil {
		t.Fatal(err)
	}
	if indexesAfter != indexesBefore {
		t.Fatalf("indexes: %d -> %d", indexesBefore, indexesAfter)
	}
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("foreign keys=%d: %v", foreignKeys, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
		t.Fatalf("foreign key violations=%d: %v", violations, err)
	}
	assertExecSucceeds(t, db, `UPDATE task_runs SET run_kind='maintenance' WHERE id=?`, runID)
	assertExecSucceeds(t, db, `UPDATE task_run_summaries SET run_kind='maintenance', status='cancelled', outcome='cancelled' WHERE run_id=?`, runID)
	assertExecFails(t, db, `UPDATE task_run_summaries SET outcome='unknown' WHERE run_id=?`, runID)
}
