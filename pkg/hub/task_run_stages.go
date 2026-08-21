package hub

import (
	"database/sql"
	"fmt"
)

type taskRunStage struct {
	TenantID  string
	RunID     string
	Seq       int64
	StageID   string
	Label     string
	EnteredAt int64
	ExitedAt  *int64
	Source    string
}

// recordTaskRunStageEnteredTx records a stage entry and closes the preceding
// open stage, if any. Live duplicate transition delivery is intentionally a
// no-op so it does not inflate time spent in a stage.
func recordTaskRunStageEnteredTx(tx *sql.Tx, tenantID, runID, stageID, label string, at int64, source string) error {
	var previousSeq int64
	var previousStageID string
	err := tx.QueryRow(`
		SELECT seq, stage_id
		  FROM task_run_stages
		 WHERE tenant_id = ? AND run_id = ? AND exited_at IS NULL
		 ORDER BY seq DESC
		 LIMIT 1`, tenantID, runID).Scan(&previousSeq, &previousStageID)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("find open task run stage tenant=%q run=%q stage=%q: %w", tenantID, runID, stageID, err)
	}
	if err == nil && previousStageID == stageID && source == "live" {
		return nil
	}

	nextSeq := int64(1)
	if err == nil {
		if _, err := tx.Exec(`
			UPDATE task_run_stages
			   SET exited_at = ?
			 WHERE tenant_id = ? AND run_id = ? AND seq = ?`, at, tenantID, runID, previousSeq); err != nil {
			return fmt.Errorf("close task run stage tenant=%q run=%q stage=%q: %w", tenantID, runID, previousStageID, err)
		}
		nextSeq = previousSeq + 1
	} else {
		var lastSeq int64
		if err := tx.QueryRow(`
			SELECT COALESCE(MAX(seq), 0)
			  FROM task_run_stages
			 WHERE tenant_id = ? AND run_id = ?`, tenantID, runID).Scan(&lastSeq); err != nil {
			return fmt.Errorf("find latest task run stage tenant=%q run=%q stage=%q: %w", tenantID, runID, stageID, err)
		}
		nextSeq = lastSeq + 1
	}

	if _, err := tx.Exec(`
		INSERT INTO task_run_stages(tenant_id, run_id, seq, stage_id, label, entered_at, exited_at, source)
		VALUES(?, ?, ?, ?, ?, ?, NULL, ?)`, tenantID, runID, nextSeq, stageID, label, at, source); err != nil {
		return fmt.Errorf("insert task run stage tenant=%q run=%q stage=%q: %w", tenantID, runID, stageID, err)
	}
	return nil
}

func taskRunStagesForRun(db *sql.DB, tenantID, runID string) ([]taskRunStage, error) {
	rows, err := db.Query(`
		SELECT tenant_id, run_id, seq, stage_id, label, entered_at, exited_at, source
		  FROM task_run_stages
		 WHERE tenant_id = ? AND run_id = ?
		 ORDER BY seq ASC`, tenantID, runID)
	if err != nil {
		return nil, fmt.Errorf("list task run stages tenant=%q run=%q: %w", tenantID, runID, err)
	}
	defer rows.Close()

	var stages []taskRunStage
	for rows.Next() {
		var stage taskRunStage
		if err := rows.Scan(&stage.TenantID, &stage.RunID, &stage.Seq, &stage.StageID, &stage.Label, &stage.EnteredAt, &stage.ExitedAt, &stage.Source); err != nil {
			return nil, fmt.Errorf("scan task run stage tenant=%q run=%q: %w", tenantID, runID, err)
		}
		stages = append(stages, stage)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task run stages tenant=%q run=%q: %w", tenantID, runID, err)
	}
	return stages, nil
}

func (s *Server) recordTaskRunStageEntered(clawID, stageID, label string) error {
	var tenantID, runID string
	if err := s.db.QueryRow(`
		SELECT t.tenant_id, t.id
		  FROM claws c JOIN task_runs t ON t.id = c.task_run_id
		 WHERE c.id = ?`, clawID).Scan(&tenantID, &runID); err != nil {
		if err == sql.ErrNoRows {
			// Claws without a task run (claws.task_run_id='') are a documented
			// normal case; there is nothing to record for them.
			return nil
		}
		return fmt.Errorf("resolve task run for claw %q: %w", clawID, err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin task run stage transaction: %w", err)
	}
	defer tx.Rollback()
	if err := recordTaskRunStageEnteredTx(tx, tenantID, runID, stageID, label, now().UnixMilli(), "live"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit task run stage tenant=%q run=%q stage=%q: %w", tenantID, runID, stageID, err)
	}
	return nil
}
