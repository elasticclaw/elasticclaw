package hub

import (
	"database/sql"
	"testing"
)

func TestRecordTaskRunStageEnteredTxRecordsEntry(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	insertValidRun(t, db, "run-stages-entry", 100)

	recordStage(t, db, "tenant", "run-stages-entry", "intake", "Intake", 1000, "live")
	stages, err := taskRunStagesForRun(db, "tenant", "run-stages-entry")
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}
	if len(stages) != 1 {
		t.Fatalf("stage count = %d, want 1", len(stages))
	}
	stage := stages[0]
	if stage.Seq != 1 || stage.StageID != "intake" || stage.Label != "Intake" || stage.EnteredAt != 1000 || stage.ExitedAt != nil || stage.Source != "live" {
		t.Fatalf("unexpected stage: %+v", stage)
	}
}

func TestRecordTaskRunStageEnteredTxClosesPreviousStage(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	insertValidRun(t, db, "run-stages-transition", 100)

	recordStage(t, db, "tenant", "run-stages-transition", "intake", "Intake", 1000, "live")
	recordStage(t, db, "tenant", "run-stages-transition", "implementation", "Implementation", 2000, "live")
	stages, err := taskRunStagesForRun(db, "tenant", "run-stages-transition")
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}
	if len(stages) != 2 {
		t.Fatalf("stage count = %d, want 2", len(stages))
	}
	if stages[0].Seq != 1 || stages[0].ExitedAt == nil || *stages[0].ExitedAt != 2000 {
		t.Fatalf("unexpected first stage: %+v", stages[0])
	}
	if stages[1].Seq != 2 || stages[1].StageID != "implementation" || stages[1].ExitedAt != nil {
		t.Fatalf("unexpected second stage: %+v", stages[1])
	}
}

func TestRecordTaskRunStageEnteredTxIgnoresLiveDuplicate(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	insertValidRun(t, db, "run-stages-duplicate", 100)

	recordStage(t, db, "tenant", "run-stages-duplicate", "intake", "Intake", 1000, "live")
	recordStage(t, db, "tenant", "run-stages-duplicate", "intake", "Intake again", 2000, "live")
	stages, err := taskRunStagesForRun(db, "tenant", "run-stages-duplicate")
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}
	if len(stages) != 1 {
		t.Fatalf("stage count = %d, want 1", len(stages))
	}
	if stages[0].Seq != 1 || stages[0].ExitedAt != nil || stages[0].EnteredAt != 1000 {
		t.Fatalf("unexpected stage after duplicate: %+v", stages[0])
	}
}

func recordStage(t *testing.T, db *sql.DB, tenantID, runID, stageID, label string, at int64, source string) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer tx.Rollback()
	if err := recordTaskRunStageEnteredTx(tx, tenantID, runID, stageID, label, at, source); err != nil {
		t.Fatalf("record stage: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit transaction: %v", err)
	}
}
