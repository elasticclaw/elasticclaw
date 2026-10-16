package hub

import (
	"context"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

func TestWorkflowV2AgentWorkIntervals(t *testing.T) {
	for _, boundary := range []string{"completed", "failed", "reconnect", "hub-timeout-then-late-failed", "hub-cancel", "in-progress", "zero-sent-at"} {
		t.Run(boundary, func(t *testing.T) {
			s, db := newTaskRunAnalyticsAPITestServer(t)
			insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{OwnerType: taskRunOwnerWorkflow, RunID: "parent-work", AttemptID: "parent-attempt", ClawID: "v2-work-claw", TenantID: "test-tenant-id", StartedAt: 1000})
			if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,template,status,pipeline_stage,task_run_id,created_at) VALUES('v2-work-claw','test-tenant-id','work','elasticclaw','running','planning','parent-work',datetime('now'))`); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			store := workflowv2.NewStore(db)
			run, err := store.CreateRun(ctx, workflowv2.CreateRunRequest{ID: "v2-work-run", TenantID: "test-tenant-id", WorkspaceYAML: []byte(workflowV2APIWorkspace), WorkflowYAML: []byte(workflowV2APIWorkflow)})
			if err != nil {
				t.Fatal(err)
			}
			attempt, err := store.StartAttempt(ctx, run.ID, "v2-work-claw")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO workflow_v2_agent_tasks(id,run_id,effect_id,attempt_id,state,state_version,status,instructions,heartbeat_deadline,deadline,created_at,updated_at) VALUES('work-task',?,'',?,'planning',?,'assigned','plan',0,0,0,0)`, run.ID, attempt.ID, run.StateVersion); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE workflow_v2_runs SET current_task_id='work-task' WHERE id=?`, run.ID); err != nil {
				t.Fatal(err)
			}
			start := now().Add(-time.Minute)
			envelope := typesv2.ControlEnvelope{ProtocolVersion: typesv2.ControlProtocolVersion, MessageID: "work-start", Kind: typesv2.MessageAgentTaskStarted, RunID: run.ID, AttemptID: attempt.ID, TaskID: "work-task", ExpectedStateVersion: &run.StateVersion, SentAt: start, Payload: []byte(`{"task":{}}`)}
			if boundary == "zero-sent-at" {
				envelope.SentAt = time.Time{}
			}
			receipt, err := s.applyWorkflowV2ClawControl(ctx, store, envelope)
			if err != nil || receipt.Disposition != typesv2.DispositionAccepted {
				t.Fatalf("start receipt=%#v error=%v", receipt, err)
			}
			duplicate, err := s.applyWorkflowV2ClawControl(ctx, store, envelope)
			if err != nil || duplicate.Disposition != typesv2.DispositionDuplicate {
				t.Fatalf("duplicate=%#v error=%v", duplicate, err)
			}
			read := func(requestNow int64) []taskRunWorkInterval {
				got, err := s.readTaskRunWorkIntervals("test-tenant-id", []string{"parent-work"}, requestNow)
				if err != nil {
					t.Fatal(err)
				}
				return got["parent-work"]
			}
			requestNow := now().Add(time.Hour).UnixMilli()
			if boundary == "zero-sent-at" {
				// A missing bridge timestamp must not open a turn at year 1.
				if got := read(requestNow); len(got) != 1 || got[0].Start < now().Add(-time.Minute).UnixMilli() {
					t.Fatalf("intervals=%#v", got)
				}
				return
			}
			s.checkpointTaskRunWorkTurns()
			// Open turns are visible (and survive reconnects/restarts) until closed.
			if got := read(requestNow); len(got) != 1 || got[0].Start != start.UnixMilli() || got[0].End != requestNow {
				t.Fatalf("open intervals=%#v", got)
			}
			if boundary == "in-progress" {
				return
			}
			end := start.Add(500 * time.Millisecond)
			switch boundary {
			case "hub-cancel":
				if _, err := db.Exec(`UPDATE workflow_v2_agent_tasks SET status='cancelled',finished_at=? WHERE id='work-task'`, end.UnixMilli()); err != nil {
					t.Fatal(err)
				}
				s.checkpointTaskRunWorkTurns()
			case "hub-timeout-then-late-failed":
				if _, err := db.Exec(`UPDATE workflow_v2_agent_tasks SET status='timed_out',finished_at=? WHERE id='work-task'`, now().UnixMilli()); err != nil {
					t.Fatal(err)
				}
				envelope.Kind = typesv2.MessageAgentTaskFailed
			default:
				envelope.Kind = typesv2.MessageAgentTaskCompleted
				if boundary == "failed" {
					envelope.Kind = typesv2.MessageAgentTaskFailed
				}
			}
			if boundary != "hub-cancel" {
				if envelope.Kind == typesv2.MessageAgentTaskFailed {
					envelope.Payload = []byte(`{"task":{"error":"failed"}}`)
				}
				envelope.MessageID = "work-end"
				envelope.ExpectedStateVersion = &receipt.StateVersion
				envelope.SentAt = end
				receipt, err = s.applyWorkflowV2ClawControl(ctx, store, envelope)
				// The hub already ended a timed-out task, so the late event is rejected.
				if late := boundary == "hub-timeout-then-late-failed"; late != (err != nil) || (!late && receipt.Disposition != typesv2.DispositionAccepted) {
					t.Fatalf("end receipt=%#v error=%v", receipt, err)
				}
				_, _ = s.applyWorkflowV2ClawControl(ctx, store, envelope)
			}
			s.checkpointTaskRunWorkTurns()
			if got := read(requestNow); len(got) != 1 || got[0].Start != start.UnixMilli() || got[0].End != end.UnixMilli() {
				t.Fatalf("closed intervals=%#v", got)
			}
		})
	}
}
