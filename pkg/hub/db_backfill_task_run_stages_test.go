package hub

import (
	"testing"
	"time"
)

func TestBackfillTaskRunStagesV1(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

	seedRun := func(runID, clawID string, finishedAt int64) {
		t.Helper()
		insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
			RunID: runID, AttemptID: "attempt-" + runID, ClawID: clawID, TenantID: "test-tenant-id",
			Status: taskRunStatusRunning, Phase: taskRunPhaseAgentRunning, OwnerType: taskRunOwnerFactory,
			StartedAt: base.UnixMilli(), FinishedAt: finishedAt,
		})
		if _, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, created_at, task_run_id) VALUES(?,?,?,?,?)`, clawID, "test-tenant-id", clawID, base, runID); err != nil {
			t.Fatalf("insert claw %s: %v", clawID, err)
		}
	}
	seedMessage := func(id, clawID, content string, at time.Time) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO messages(id, claw_id, tenant_id, role, content, created_at) VALUES(?,?,?,?,?,?)`, id, clawID, "test-tenant-id", "hub", content, at); err != nil {
			t.Fatalf("insert message %s: %v", id, err)
		}
	}
	seedHistory := func(clawID, stageID string, at time.Time) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO pipeline_stage_history(claw_id, stage_id, created_at) VALUES(?,?,?)`, clawID, stageID, at); err != nil {
			t.Fatalf("insert history %s: %v", stageID, err)
		}
	}

	finishedAt := base.Add(4 * time.Minute).UnixMilli()
	seedRun("run-markers", "claw-markers", finishedAt)
	seedMessage("marker-1", "claw-markers", "[hub] ▶ Stage: Working", base.Add(1*time.Second))
	seedMessage("marker-2", "claw-markers", "[hub] ▶ Stage: Pre-commit", base.Add(2*time.Second))
	seedMessage("marker-3", "claw-markers", "[hub] ▶ Stage: Working", base.Add(3*time.Second))
	seedHistory("claw-markers", "working", base.Add(1100*time.Millisecond))
	seedHistory("claw-markers", "pre_commit", base.Add(2100*time.Millisecond))

	seedRun("run-history", "claw-history", 0)
	seedHistory("claw-history", "plan", base.Add(10*time.Second))
	seedHistory("claw-history", "implement", base.Add(20*time.Second))

	seedRun("run-empty", "claw-empty", 0)
	seedRun("run-slug", "claw-slug", 0)
	seedMessage("marker-slug", "claw-slug", "[hub] ▶ Stage: PR Opened & Ready!", base.Add(30*time.Second))

	// The startup fixture runs migrations before its data is seeded.
	if _, err := db.Exec(`DELETE FROM hub_migrations WHERE name='task_run_stages_v1'`); err != nil {
		t.Fatalf("reset task stage backfill sentinel: %v", err)
	}
	if err := backfillTaskRunStagesV1(s.db); err != nil {
		t.Fatalf("backfill task run stages: %v", err)
	}

	stages, err := taskRunStagesForRun(db, "test-tenant-id", "run-markers")
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 3 {
		t.Fatalf("marker stages = %d, want 3", len(stages))
	}
	for i, want := range []string{"working", "pre_commit", "working"} {
		if stages[i].Seq != int64(i+1) || stages[i].StageID != want || stages[i].Source != "backfill_messages" {
			t.Fatalf("marker stage %d = %+v, want seq=%d stage=%q source=backfill_messages", i, stages[i], i+1, want)
		}
		if i < len(stages)-1 && (stages[i].ExitedAt == nil || *stages[i].ExitedAt != stages[i+1].EnteredAt) {
			t.Fatalf("marker stage %d exited_at = %v, want next entered_at %d", i, stages[i].ExitedAt, stages[i+1].EnteredAt)
		}
	}
	if stages[2].ExitedAt == nil || *stages[2].ExitedAt != finishedAt {
		t.Fatalf("last marker exited_at = %v, want %d", stages[2].ExitedAt, finishedAt)
	}

	historyStages, err := taskRunStagesForRun(db, "test-tenant-id", "run-history")
	if err != nil {
		t.Fatal(err)
	}
	if len(historyStages) != 2 {
		t.Fatalf("history stages = %d, want 2", len(historyStages))
	}
	for _, stage := range historyStages {
		if stage.Source != "backfill_history" || stage.Label != stage.StageID {
			t.Fatalf("history stage = %+v, want history source and stage ID label", stage)
		}
	}
	if historyStages[1].ExitedAt != nil {
		t.Fatalf("open history stage exited_at = %v, want NULL", *historyStages[1].ExitedAt)
	}

	var emptyCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_run_stages WHERE run_id='run-empty'`).Scan(&emptyCount); err != nil {
		t.Fatal(err)
	}
	if emptyCount != 0 {
		t.Fatalf("no-source run stages = %d, want 0", emptyCount)
	}
	var slugStage string
	if err := db.QueryRow(`SELECT stage_id FROM task_run_stages WHERE run_id='run-slug'`).Scan(&slugStage); err != nil {
		t.Fatal(err)
	}
	if slugStage != "pr_opened_ready" {
		t.Fatalf("slug stage ID = %q, want pr_opened_ready", slugStage)
	}

	var before, appliedAt int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_run_stages`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT applied_at FROM hub_migrations WHERE name='task_run_stages_v1'`).Scan(&appliedAt); err != nil {
		t.Fatal(err)
	}
	if appliedAt <= 0 {
		t.Fatalf("sentinel applied_at = %d, want positive", appliedAt)
	}
	if err := backfillTaskRunStagesV1(s.db); err != nil {
		t.Fatalf("re-run task stage backfill: %v", err)
	}
	var after int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_run_stages`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("stage count after re-run = %d, want %d", after, before)
	}
}

func TestSlugTaskRunStageLabel(t *testing.T) {
	if got := slugTaskRunStageLabel("PR Opened & Ready!"); got != "pr_opened_ready" {
		t.Fatalf("slug = %q, want pr_opened_ready", got)
	}
	if got := slugTaskRunStageLabel(" !!! "); got != "stage" {
		t.Fatalf("empty slug fallback = %q, want stage", got)
	}
}
