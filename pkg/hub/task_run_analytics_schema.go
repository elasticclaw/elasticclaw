package hub

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

func migrateTaskRunAnalyticsOutcomes(db *sql.DB) error {
	if err := widenTaskRunAnalyticsChecks(db); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_workflow_v2_runs_task_run ON workflow_v2_runs(task_run_id)`); err != nil {
		return err
	}
	return addColumn(db, "task_run_summaries", "outcome", `TEXT NOT NULL DEFAULT '' CHECK(outcome IN ('','delivered','no_changes','closed','failed','cancelled'))`)
}

func widenTaskRunAnalyticsChecks(db *sql.DB) (err error) {
	// task_runs has ON DELETE CASCADE children. Rebuild on one connection with
	// foreign keys disabled so dropping the old parent cannot delete analytics.
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var foreignKeys int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	defer func() {
		if _, restoreErr := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA foreign_keys=%d", foreignKeys)); err == nil {
			err = restoreErr
		}
	}()

	for _, change := range []struct{ table, oldCheck, newCheck string }{
		{"task_runs", "'code_task','pr_task')", "'code_task','pr_task','maintenance')"},
		{"task_run_summaries", "'code_task','pr_task')", "'code_task','pr_task','maintenance')"},
		{"task_run_summaries", "'running','clean','human_in_the_loop','warning','failed')", "'running','clean','human_in_the_loop','warning','failed','cancelled')"},
	} {
		var schema string
		if err := conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, change.table).Scan(&schema); err != nil {
			return err
		}
		if strings.Contains(schema, change.newCheck) {
			continue
		}
		if !strings.Contains(schema, change.oldCheck) {
			return fmt.Errorf("unsupported %s analytics CHECK", change.table)
		}
		if err := rebuildTaskRunCheck(conn, change.table, schema, change.oldCheck, change.newCheck); err != nil {
			return err
		}
	}
	return nil
}
