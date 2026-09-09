package workflowv2_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	workflowv2 "github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
)

func TestFailCronRunRecordsReason(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	fixed := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return fixed })

	cronRunID, err := store.RecordCronRunStarted(context.Background(), "tenant-1", "ws", "wf", "cron", map[string]interface{}{
		"scheduled_at": fixed.Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("record cron run: %v", err)
	}

	reason := "activation failed: missing knowledge file"
	if err := store.FailCronRun(context.Background(), cronRunID, reason); err != nil {
		t.Fatalf("fail cron run: %v", err)
	}

	row, err := store.GetCronRun(context.Background(), "tenant-1", "ws", "wf", cronRunID)
	if err != nil {
		t.Fatalf("get cron run: %v", err)
	}
	if row == nil {
		t.Fatal("expected cron run row, got nil")
	}
	if row.Status != "failed" {
		t.Fatalf("status = %q, want failed", row.Status)
	}
	if row.Result != "failure" {
		t.Fatalf("result = %q, want failure", row.Result)
	}
	if row.FinishedAt == nil || !row.FinishedAt.Equal(fixed) {
		t.Fatalf("finished_at = %v, want %v", row.FinishedAt, fixed)
	}
	gotReason, ok := row.RunContext["failure_reason"].(string)
	if !ok || gotReason != reason {
		t.Fatalf("run_context.failure_reason = %q (%v), want %q", gotReason, row.RunContext["failure_reason"], reason)
	}
	if gotScheduled, ok := row.RunContext["scheduled_at"].(string); !ok || gotScheduled != fixed.Format(time.RFC3339) {
		t.Fatalf("run_context.scheduled_at lost or changed: %v", row.RunContext["scheduled_at"])
	}
}

func TestFailCronRunIdempotentWhenAlreadyTerminal(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)

	cronRunID, err := store.RecordCronRunSkipped(context.Background(), "tenant-1", "ws", "wf", "cron", map[string]interface{}{})
	if err != nil {
		t.Fatalf("record skipped cron run: %v", err)
	}

	if err := store.FailCronRun(context.Background(), cronRunID, "should not overwrite"); err != nil {
		t.Fatalf("fail cron run on skipped row: %v", err)
	}

	row, err := store.GetCronRun(context.Background(), "tenant-1", "ws", "wf", cronRunID)
	if err != nil {
		t.Fatalf("get cron run: %v", err)
	}
	if row == nil {
		t.Fatal("expected cron run row, got nil")
	}
	if row.Status != "skipped" {
		t.Fatalf("status changed from skipped to %q", row.Status)
	}
}

func TestRecordCronRunStartedStoresRunContext(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)

	ctx := map[string]interface{}{"trigger": "cron", "scheduled_at": "2026-08-08T12:00:00Z"}
	cronRunID, err := store.RecordCronRunStarted(context.Background(), "tenant-1", "ws", "wf", "cron", ctx)
	if err != nil {
		t.Fatalf("record cron run: %v", err)
	}

	row, err := store.GetCronRun(context.Background(), "tenant-1", "ws", "wf", cronRunID)
	if err != nil {
		t.Fatalf("get cron run: %v", err)
	}
	if row == nil {
		t.Fatal("expected cron run row, got nil")
	}
	expected, _ := json.Marshal(ctx)
	actual, _ := json.Marshal(row.RunContext)
	if string(expected) != string(actual) {
		t.Fatalf("run_context = %s, want %s", actual, expected)
	}
}
