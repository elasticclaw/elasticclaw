package workflowv2_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	workflowv2 "github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

func TestAttemptSnapshotAndControlOutbox(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	createRuntimeRun(t, store, "run-control")
	attempt, err := store.StartAttempt(context.Background(), "run-control", "claw-1")
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Number != 1 || attempt.Status != "active" {
		t.Fatalf("attempt = %#v", attempt)
	}
	if err := store.AuthorizeControlAttempt(context.Background(), "run-control", attempt.ID, "claw-1", "tenant-1"); err != nil {
		t.Fatalf("authorize active attempt: %v", err)
	}
	if err := store.AuthorizeControlAttempt(context.Background(), "run-control", attempt.ID, "other-claw", "tenant-1"); err == nil {
		t.Fatal("different claw authorized for attempt")
	}

	version := uint64(1)
	envelope := typesv2.ControlEnvelope{
		ProtocolVersion:      typesv2.ControlProtocolVersion,
		MessageID:            "assign-1",
		Kind:                 typesv2.MessageAgentTaskAssign,
		RunID:                "run-control",
		AttemptID:            attempt.ID,
		TaskID:               "task-1",
		ExpectedStateVersion: &version,
		Payload:              json.RawMessage(`{"instructions":"implement"}`),
	}
	if err := store.EnqueueControl(context.Background(), envelope); err != nil {
		t.Fatal(err)
	}
	ready, err := store.ReadyControl(context.Background(), "run-control", attempt.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 || ready[0].MessageID != envelope.MessageID {
		t.Fatalf("ready = %#v", ready)
	}
	if err := store.MarkControlSent(context.Background(), envelope.MessageID); err != nil {
		t.Fatal(err)
	}
	if err := store.AcknowledgeControl(context.Background(), "run-control", attempt.ID, typesv2.ControlReceipt{
		MessageID: envelope.MessageID, Disposition: typesv2.DispositionAccepted, StateVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	ready, err = store.ReadyControl(context.Background(), "run-control", attempt.ID, 10)
	if err != nil || len(ready) != 0 {
		t.Fatalf("ready after ack = %#v, %v", ready, err)
	}

	snapshot, err := store.Snapshot(context.Background(), "run-control", attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.RunID != "run-control" || snapshot.AttemptID != attempt.ID || snapshot.StateVersion != 1 || snapshot.State != "building" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestAgentControlIsTypedDeduplicatedAndAttemptBound(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	createRuntimeRun(t, store, "run-agent-control")
	attempt, err := store.StartAttempt(context.Background(), "run-agent-control", "claw-1")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := db.Exec(`INSERT INTO workflow_v2_agent_tasks(
		id,run_id,effect_id,attempt_id,state,state_version,status,instructions,heartbeat_deadline,deadline,created_at,updated_at)
		VALUES(?,?,?,?,?,?,'assigned',?,?,?,?,?)`, "task-1", "run-agent-control", "", attempt.ID, "building", 1,
		"implement", now.Add(2*time.Minute).UnixMilli(), now.Add(time.Hour).UnixMilli(), now.UnixMilli(), now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE workflow_v2_runs SET current_task_id=? WHERE id=?`, "task-1", "run-agent-control"); err != nil {
		t.Fatal(err)
	}
	version := uint64(1)
	envelope := typesv2.ControlEnvelope{
		ProtocolVersion:      typesv2.ControlProtocolVersion,
		MessageID:            "completed-1",
		Kind:                 typesv2.MessageAgentTaskCompleted,
		RunID:                "run-agent-control",
		AttemptID:            attempt.ID,
		TaskID:               "task-1",
		ExpectedStateVersion: &version,
		Payload:              json.RawMessage(`{"task":{"result":"success"}}`),
	}
	receipt, err := store.ApplyAgentControl(context.Background(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Disposition != typesv2.DispositionAccepted || receipt.StateVersion != 2 {
		t.Fatalf("receipt = %#v", receipt)
	}
	duplicate, err := store.ApplyAgentControl(context.Background(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.Disposition != typesv2.DispositionDuplicate || duplicate.StateVersion != 2 {
		t.Fatalf("duplicate = %#v", duplicate)
	}
	var taskStatus string
	if err := db.QueryRow(`SELECT status FROM workflow_v2_agent_tasks WHERE id='task-1'`).Scan(&taskStatus); err != nil {
		t.Fatal(err)
	}
	if taskStatus != string(typesv2.AgentTaskCompleted) {
		t.Fatalf("task status = %q", taskStatus)
	}
	var currentTaskID string
	if err := db.QueryRow(`SELECT current_task_id FROM workflow_v2_runs WHERE id='run-agent-control'`).Scan(&currentTaskID); err != nil {
		t.Fatal(err)
	}
	if currentTaskID != "" {
		t.Fatalf("terminal task remained current: %q", currentTaskID)
	}
}

func TestDuplicateHeartbeatDoesNotExtendTaskLiveness(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	current := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return current })
	createRuntimeRun(t, store, "run-heartbeat-replay")
	attempt, err := store.StartAttempt(context.Background(), "run-heartbeat-replay", "claw-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO workflow_v2_agent_tasks(
		id,run_id,effect_id,attempt_id,state,state_version,status,instructions,heartbeat_deadline,deadline,created_at,updated_at)
		VALUES(?,?,?,?,?,?,'running',?,?,?,?,?)`, "task-heartbeat", "run-heartbeat-replay", "", attempt.ID, "building", 1,
		"implement", current.Add(time.Minute).UnixMilli(), current.Add(time.Hour).UnixMilli(), current.UnixMilli(), current.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE workflow_v2_runs SET current_task_id=? WHERE id=?`, "task-heartbeat", "run-heartbeat-replay"); err != nil {
		t.Fatal(err)
	}
	envelope := typesv2.ControlEnvelope{ProtocolVersion: typesv2.ControlProtocolVersion, MessageID: "heartbeat-1",
		Kind: typesv2.MessageAgentTaskHeartbeat, RunID: "run-heartbeat-replay", AttemptID: attempt.ID,
		TaskID: "task-heartbeat", Payload: json.RawMessage(`{"task":{"heartbeat":true}}`)}
	first, err := store.ApplyAgentControl(context.Background(), envelope)
	if err != nil || first.Disposition != typesv2.DispositionAccepted {
		t.Fatalf("first heartbeat = %#v, %v", first, err)
	}
	var firstDeadline int64
	if err := db.QueryRow(`SELECT heartbeat_deadline FROM workflow_v2_agent_tasks WHERE id='task-heartbeat'`).Scan(&firstDeadline); err != nil {
		t.Fatal(err)
	}
	current = current.Add(30 * time.Second)
	duplicate, err := store.ApplyAgentControl(context.Background(), envelope)
	if err != nil || duplicate.Disposition != typesv2.DispositionDuplicate {
		t.Fatalf("duplicate heartbeat = %#v, %v", duplicate, err)
	}
	var replayedDeadline int64
	if err := db.QueryRow(`SELECT heartbeat_deadline FROM workflow_v2_agent_tasks WHERE id='task-heartbeat'`).Scan(&replayedDeadline); err != nil {
		t.Fatal(err)
	}
	if replayedDeadline != firstDeadline {
		t.Fatalf("duplicate heartbeat extended deadline from %d to %d", firstDeadline, replayedDeadline)
	}
}

func TestStartingNewAttemptInvalidatesOldControlIdentity(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	createRuntimeRun(t, store, "run-attempt-replace")
	first, err := store.StartAttempt(context.Background(), "run-attempt-replace", "claw-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.StartAttempt(context.Background(), "run-attempt-replace", "claw-2")
	if err != nil {
		t.Fatal(err)
	}
	if second.Number != 2 {
		t.Fatalf("second attempt = %#v", second)
	}
	if err := store.AuthorizeControlAttempt(context.Background(), "run-attempt-replace", first.ID, "claw-1", "tenant-1"); err == nil {
		t.Fatal("superseded attempt remained authorized")
	}
	if err := store.AuthorizeControlAttempt(context.Background(), "run-attempt-replace", second.ID, "claw-2", "tenant-1"); err != nil {
		t.Fatalf("new attempt not authorized: %v", err)
	}
	if _, err := store.ApplyAgentControl(context.Background(), typesv2.ControlEnvelope{
		ProtocolVersion: typesv2.ControlProtocolVersion, MessageID: "old-heartbeat",
		Kind: typesv2.MessageAgentTaskHeartbeat, RunID: "run-attempt-replace", AttemptID: first.ID,
		TaskID: "old-task", Payload: json.RawMessage(`{"task":{"heartbeat":true}}`),
	}); err == nil {
		t.Fatal("superseded attempt was allowed to submit a control envelope")
	}
}

func TestAttemptBoundTrustedEventRejectsRevokedAttempt(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	createRuntimeRun(t, store, "run-revoked-trusted-event")
	first, err := store.StartAttempt(context.Background(), "run-revoked-trusted-event", "claw-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartAttempt(context.Background(), "run-revoked-trusted-event", "claw-2"); err != nil {
		t.Fatal(err)
	}
	_, err = store.ApplyEvent(context.Background(), "run-revoked-trusted-event", workflowv2.EventInput{
		ID: "late-delivery-event", Kind: "delivery.verified", AttemptID: first.ID,
		Producer: workflowv2.ProducerSourceControl,
		Payload:  map[string]interface{}{"delivery": map[string]interface{}{"count": 1}},
		Provenance: typesv2.EvidenceProvenance{
			Producer: string(workflowv2.ProducerSourceControl), ObservedAt: time.Now().UTC(),
		},
	})
	if err == nil {
		t.Fatal("trusted event from revoked attempt was accepted")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE id='late-delivery-event'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("revoked event rows = %d", count)
	}
}

func TestStartingNewAttemptReassignsCurrentTask(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	current := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return current })
	createRuntimeRun(t, store, "run-task-reassign")
	first, err := store.StartAttempt(context.Background(), "run-task-reassign", "claw-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO workflow_v2_agent_tasks(
		id,run_id,effect_id,attempt_id,state,state_version,status,instructions,allowed_actions,required_artifacts,
		heartbeat_deadline,deadline,created_at,updated_at) VALUES(?,?,?,?,?,?,'running',?,?,?,?,?,?,?)`,
		"task-original", "run-task-reassign", "effect-1", first.ID, "building", 1, "implement", `["commit"]`, `["tests"]`,
		current.Add(time.Minute).UnixMilli(), current.Add(time.Hour).UnixMilli(), current.UnixMilli(), current.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE workflow_v2_runs SET current_task_id='task-original' WHERE id='run-task-reassign'`); err != nil {
		t.Fatal(err)
	}
	version := uint64(1)
	if err := store.EnqueueControl(context.Background(), typesv2.ControlEnvelope{
		ProtocolVersion: typesv2.ControlProtocolVersion, MessageID: "old-assignment", Kind: typesv2.MessageAgentTaskAssign,
		RunID: "run-task-reassign", AttemptID: first.ID, TaskID: "task-original", ExpectedStateVersion: &version,
		Payload: json.RawMessage(`{"instructions":"implement"}`),
	}); err != nil {
		t.Fatal(err)
	}
	second, err := store.StartAttempt(context.Background(), "run-task-reassign", "claw-2")
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.GetRun(context.Background(), "run-task-reassign")
	if err != nil {
		t.Fatal(err)
	}
	if run.CurrentAttemptID != second.ID || run.CurrentTaskID == "" || run.CurrentTaskID == "task-original" {
		t.Fatalf("run after reassignment = %#v", run)
	}
	var oldStatus, oldOutboxStatus, newAttempt, newStatus string
	if err := db.QueryRow(`SELECT status FROM workflow_v2_agent_tasks WHERE id='task-original'`).Scan(&oldStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT status FROM workflow_v2_control_outbox WHERE message_id='old-assignment'`).Scan(&oldOutboxStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT attempt_id,status FROM workflow_v2_agent_tasks WHERE id=?`, run.CurrentTaskID).Scan(
		&newAttempt, &newStatus); err != nil {
		t.Fatal(err)
	}
	if oldStatus != string(typesv2.AgentTaskCancelled) || oldOutboxStatus != "cancelled" ||
		newAttempt != second.ID || newStatus != string(typesv2.AgentTaskAssigned) {
		t.Fatalf("old/new task state = %s/%s/%s/%s", oldStatus, oldOutboxStatus, newAttempt, newStatus)
	}
	if ready, err := store.ReadyControl(context.Background(), "run-task-reassign", first.ID, 10); err != nil || len(ready) != 0 {
		t.Fatalf("old attempt ready control = %#v, %v", ready, err)
	}
	ready, err := store.ReadyControl(context.Background(), "run-task-reassign", second.ID, 10)
	if err != nil || len(ready) != 1 || ready[0].TaskID != run.CurrentTaskID {
		t.Fatalf("replacement assignment = %#v, %v", ready, err)
	}
	if _, err := store.Snapshot(context.Background(), "run-task-reassign", first.ID); err == nil {
		t.Fatal("superseded attempt received a snapshot")
	}
	snapshot, err := store.Snapshot(context.Background(), "run-task-reassign", second.ID)
	if err != nil || snapshot.CurrentTask == nil || snapshot.CurrentTask.ID != run.CurrentTaskID {
		t.Fatalf("replacement snapshot = %#v, %v", snapshot, err)
	}
}

func TestAgentControlMustNameCurrentTask(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	createRuntimeRun(t, store, "run-current-task")
	attempt, err := store.StartAttempt(context.Background(), "run-current-task", "claw-1")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := db.Exec(`INSERT INTO workflow_v2_agent_tasks(
		id,run_id,effect_id,attempt_id,state,state_version,status,instructions,heartbeat_deadline,deadline,created_at,updated_at)
		VALUES(?,?,?,?,?,?,'assigned',?,?,?,?,?)`, "task-current", "run-current-task", "", attempt.ID, "building", 1,
		"implement", now.Add(2*time.Minute).UnixMilli(), now.Add(time.Hour).UnixMilli(), now.UnixMilli(), now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE workflow_v2_runs SET current_task_id=? WHERE id=?`, "task-current", "run-current-task"); err != nil {
		t.Fatal(err)
	}
	version := uint64(1)
	_, err = store.ApplyAgentControl(context.Background(), typesv2.ControlEnvelope{
		ProtocolVersion: typesv2.ControlProtocolVersion, MessageID: "wrong-task-complete",
		Kind: typesv2.MessageAgentTaskCompleted, RunID: "run-current-task", AttemptID: attempt.ID,
		TaskID: "task-invented", ExpectedStateVersion: &version,
		Payload: json.RawMessage(`{"task":{"result":"success"}}`),
	})
	if err == nil {
		t.Fatal("control message for an invented task was accepted")
	}
	run, err := store.GetRun(context.Background(), "run-current-task")
	if err != nil {
		t.Fatal(err)
	}
	if run.StateVersion != 1 || run.State != "building" {
		t.Fatalf("run changed after rejected control: %#v", run)
	}
}

func TestAgentControlRollsBackEventWhenTaskUpdateFails(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	createRuntimeRun(t, store, "run-atomic-control")
	attempt, err := store.StartAttempt(context.Background(), "run-atomic-control", "claw-1")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := db.Exec(`INSERT INTO workflow_v2_agent_tasks(
		id,run_id,effect_id,attempt_id,state,state_version,status,instructions,heartbeat_deadline,deadline,created_at,updated_at)
		VALUES(?,?,?,?,?,?,'assigned',?,?,?,?,?)`, "task-atomic", "run-atomic-control", "", attempt.ID, "building", 1,
		"implement", now.Add(2*time.Minute).UnixMilli(), now.Add(time.Hour).UnixMilli(), now.UnixMilli(), now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE workflow_v2_runs SET current_task_id=? WHERE id=?`, "task-atomic", "run-atomic-control"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_task_completion BEFORE UPDATE OF status ON workflow_v2_agent_tasks
		WHEN NEW.id='task-atomic' AND NEW.status='completed' BEGIN SELECT RAISE(ABORT, 'simulated task write failure'); END`); err != nil {
		t.Fatal(err)
	}
	version := uint64(1)
	_, err = store.ApplyAgentControl(context.Background(), typesv2.ControlEnvelope{
		ProtocolVersion: typesv2.ControlProtocolVersion, MessageID: "atomic-complete",
		Kind: typesv2.MessageAgentTaskCompleted, RunID: "run-atomic-control", AttemptID: attempt.ID,
		TaskID: "task-atomic", ExpectedStateVersion: &version,
		Payload: json.RawMessage(`{"task":{"result":"success"}}`),
	})
	if err == nil {
		t.Fatal("simulated task write failure did not fail control application")
	}
	run, err := store.GetRun(context.Background(), "run-atomic-control")
	if err != nil {
		t.Fatal(err)
	}
	if run.StateVersion != 1 || run.State != "building" {
		t.Fatalf("run advanced despite failed task update: %#v", run)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE id='atomic-complete'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("event persisted despite failed task update: %d", count)
	}
}

func TestDeliveryControlRequiresTrustedVerificationPath(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	createRuntimeRun(t, store, "run-unverified-delivery")
	attempt, err := store.StartAttempt(context.Background(), "run-unverified-delivery", "claw-1")
	if err != nil {
		t.Fatal(err)
	}
	version := uint64(1)
	_, err = store.ApplyAgentControl(context.Background(), typesv2.ControlEnvelope{
		ProtocolVersion: typesv2.ControlProtocolVersion, MessageID: "unverified-delivery",
		Kind: typesv2.MessageDeliverySubmitted, RunID: "run-unverified-delivery", AttemptID: attempt.ID,
		ExpectedStateVersion: &version, Payload: json.RawMessage(`{"delivery":{"pull_requests":[]}}`),
	})
	if err == nil {
		t.Fatal("unverified delivery entered through the generic control path")
	}
	run, err := store.GetRun(context.Background(), "run-unverified-delivery")
	if err != nil {
		t.Fatal(err)
	}
	if run.StateVersion != 1 || run.State != "building" {
		t.Fatalf("run changed after rejected delivery: %#v", run)
	}
}

func TestControlOutboxRejectsSupersededAttempt(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	createRuntimeRun(t, store, "run-stale-assignment")
	first, err := store.StartAttempt(context.Background(), "run-stale-assignment", "claw-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartAttempt(context.Background(), "run-stale-assignment", "claw-2"); err != nil {
		t.Fatal(err)
	}
	version := uint64(1)
	err = store.EnqueueControl(context.Background(), typesv2.ControlEnvelope{
		ProtocolVersion: typesv2.ControlProtocolVersion, MessageID: "stale-assignment",
		Kind: typesv2.MessageAgentTaskAssign, RunID: "run-stale-assignment", AttemptID: first.ID,
		TaskID: "task-old", ExpectedStateVersion: &version, Payload: json.RawMessage(`{"instructions":"old"}`),
	})
	if err == nil {
		t.Fatal("control assignment for a superseded attempt was queued")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_control_outbox WHERE run_id='run-stale-assignment'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("stale outbox rows = %d", count)
	}
}

func TestMaterializeAgentTaskIsAtomicWithAssignmentAndEffect(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	current := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return current })
	createRuntimeRun(t, store, "run-materialize")
	attempt, err := store.StartAttempt(context.Background(), "run-materialize", "claw-1")
	if err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimEffect(context.Background(), "task-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("effect claim = %#v, %v", claim, err)
	}
	task, err := store.MaterializeAgentTask(context.Background(), claim.Effect.ID, claim.AttemptID,
		"task-worker", 90*time.Second, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if task.AttemptID != attempt.ID || task.State != "building" || task.StateVersion != 1 || task.Instructions != "Implement the change." {
		t.Fatalf("task = %#v", task)
	}
	var effectStatus, attemptStatus, currentTask string
	if err := db.QueryRow(`SELECT status FROM workflow_v2_effects WHERE id=?`, claim.Effect.ID).Scan(&effectStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT status FROM workflow_v2_effect_attempts WHERE id=?`, claim.AttemptID).Scan(&attemptStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT current_task_id FROM workflow_v2_runs WHERE id='run-materialize'`).Scan(&currentTask); err != nil {
		t.Fatal(err)
	}
	if effectStatus != "succeeded" || attemptStatus != "succeeded" || currentTask != task.ID {
		t.Fatalf("effect/attempt/current task = %s/%s/%s", effectStatus, attemptStatus, currentTask)
	}
	ready, err := store.ReadyControl(context.Background(), "run-materialize", attempt.ID, 10)
	if err != nil || len(ready) != 1 || ready[0].Kind != typesv2.MessageAgentTaskAssign || ready[0].TaskID != task.ID {
		t.Fatalf("assignment = %#v, %v", ready, err)
	}
	var assigned typesv2.AgentTask
	if err := json.Unmarshal(ready[0].Payload, &assigned); err != nil {
		t.Fatal(err)
	}
	if assigned.ID != task.ID || assigned.Instructions != task.Instructions {
		t.Fatalf("assigned task = %#v", assigned)
	}
}

func TestAgentTaskDeadlineSuspendsRunForExplicitRecovery(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	current := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return current })
	createRuntimeRun(t, store, "run-timeout")
	_, err := store.StartAttempt(context.Background(), "run-timeout", "claw-1")
	if err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimEffect(context.Background(), "task-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("effect claim = %#v, %v", claim, err)
	}
	task, err := store.MaterializeAgentTask(context.Background(), claim.Effect.ID, claim.AttemptID,
		"task-worker", time.Minute, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	current = current.Add(time.Minute + time.Millisecond)
	expired, err := store.ExpireAgentTasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0] != task.ID {
		t.Fatalf("expired = %#v", expired)
	}
	run, err := store.GetRun(context.Background(), "run-timeout")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != workflowv2.RunSuspended || run.WaitingReason == "" {
		t.Fatalf("run = %#v", run)
	}
}
