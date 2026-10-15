package hub

import (
	"context"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

func TestWorkflowV2AgentWorkIntervals(t *testing.T) {
	for _, boundary := range []string{"completed", "failed", "disconnect", "reconnect"} {
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
			start := now().Add(-time.Second)
			envelope := typesv2.ControlEnvelope{ProtocolVersion: typesv2.ControlProtocolVersion, MessageID: "work-start", Kind: typesv2.MessageAgentTaskStarted, RunID: run.ID, AttemptID: attempt.ID, TaskID: "work-task", ExpectedStateVersion: &run.StateVersion, SentAt: start, Payload: []byte(`{"task":{}}`)}
			receipt, err := s.applyWorkflowV2ClawControl(ctx, store, envelope, "connection")
			if err != nil || receipt.Disposition != typesv2.DispositionAccepted {
				t.Fatalf("start receipt=%#v error=%v", receipt, err)
			}
			duplicate, err := s.applyWorkflowV2ClawControl(ctx, store, envelope, "connection")
			if err != nil || duplicate.Disposition != typesv2.DispositionDuplicate {
				t.Fatalf("duplicate=%#v error=%v", duplicate, err)
			}
			connectionID := "connection"
			if boundary == "reconnect" {
				s.closeWorkflowV2WorkTurns(run.ID, attempt.ID, connectionID)
				s.resumeWorkflowV2WorkTurns(run.ID, attempt.ID, "replacement")
				// Late cleanup from the old socket must not pause the new owner.
				s.closeWorkflowV2WorkTurns(run.ID, attempt.ID, connectionID)
				connectionID = "replacement"
			}
			if boundary == "disconnect" {
				s.closeWorkflowV2WorkTurns(run.ID, attempt.ID, "connection")
				s.closeWorkflowV2WorkTurns(run.ID, attempt.ID, "connection")
			} else {
				envelope.MessageID = "work-end"
				envelope.Kind = typesv2.MessageAgentTaskCompleted
				if boundary == "failed" {
					envelope.Kind = typesv2.MessageAgentTaskFailed
					envelope.Payload = []byte(`{"task":{"error":"failed"}}`)
				}
				envelope.ExpectedStateVersion = &receipt.StateVersion
				envelope.SentAt = start.Add(500 * time.Millisecond)
				if boundary == "reconnect" {
					envelope.SentAt = now().Add(time.Second)
				}
				receipt, err = s.applyWorkflowV2ClawControl(ctx, store, envelope, connectionID)
				if err != nil || receipt.Disposition != typesv2.DispositionAccepted {
					t.Fatalf("end receipt=%#v error=%v", receipt, err)
				}
				if _, err = s.applyWorkflowV2ClawControl(ctx, store, envelope, connectionID); err != nil {
					t.Fatal(err)
				}
			}
			got, err := s.readTaskRunWorkIntervals("test-tenant-id", []string{"parent-work"})
			if err != nil {
				t.Fatal(err)
			}
			intervals := got["parent-work"]
			wantCount := 1
			if boundary == "reconnect" {
				wantCount = 2
			}
			if len(intervals) != wantCount || intervals[0].Start != start.UnixMilli() || intervals[0].End <= intervals[0].Start {
				t.Fatalf("intervals=%#v", intervals)
			}
			if boundary != "disconnect" && boundary != "reconnect" && intervals[0].End-intervals[0].Start != 500 {
				t.Fatalf("duration=%#v", intervals)
			}
		})
	}
}
