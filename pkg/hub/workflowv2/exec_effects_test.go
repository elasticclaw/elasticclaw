package workflowv2_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	workflowv2 "github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

const execWorkspaceYAML = `
schema_version: 2
name: exec-workspace
repositories:
  primary:
    provider: github
    repository: org/repo
execution:
  provider: daytona
`

const execRunWorkflowYAML = `
schema_version: 2
name: exec-run-workflow
enabled: true
initial_state: run
states:
  run:
    phase: build
    on_enter:
      effects:
        - exec.run:
            command: "echo hello"
            timeout: "1m"
  done:
    phase: done
    terminal: true
transitions:
  finish:
    from: run
    on: exec.run.completed
    when:
      exec:
        last_run:
          succeeded:
            equals: true
    to: done
`

const dependencyUpdateWorkflowYAML = `
schema_version: 2
name: dependency-update-workflow
enabled: true
initial_state: run
states:
  run:
    phase: build
    on_enter:
      effects:
        - dependency.update:
            ecosystems: [go]
            timeout: "1m"
  done:
    phase: done
    terminal: true
transitions:
  finish:
    from: run
    on: dependency.update.completed
    when:
      exec:
        dependency_update:
          succeeded:
            equals: true
    to: done
`

func TestMaterializeExecRunEffect(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)

	run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID: "run-exec", TenantID: "tenant-1", InitialClawID: "claw-exec",
		WorkspaceYAML: []byte(execWorkspaceYAML),
		WorkflowYAML:  []byte(execRunWorkflowYAML),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if run.CurrentAttemptID == "" {
		t.Fatal("expected bound attempt")
	}

	claim, err := store.ClaimEffect(context.Background(), "exec-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim = %#v, %v", claim, err)
	}
	if claim.Effect.Kind != typesv2.EffectExecRun {
		t.Fatalf("effect kind = %q", claim.Effect.Kind)
	}

	envelope, err := store.MaterializeCommandTask(context.Background(), claim.Effect.ID, claim.AttemptID, "exec-worker")
	if err != nil {
		t.Fatalf("materialize exec.run: %v", err)
	}
	if envelope.Kind != typesv2.MessageExecRunAssign {
		t.Fatalf("envelope kind = %q", envelope.Kind)
	}
	if envelope.TaskID == "" {
		t.Fatal("expected task id")
	}

	var outboxCount, effectStatus string
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_control_outbox WHERE run_id=? AND kind=?`,
		run.ID, string(typesv2.MessageExecRunAssign)).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != "1" {
		t.Fatalf("outbox count = %s, want 1", outboxCount)
	}
	if err := db.QueryRow(`SELECT status FROM workflow_v2_effects WHERE id=?`, claim.Effect.ID).Scan(&effectStatus); err != nil {
		t.Fatal(err)
	}
	if effectStatus != "succeeded" {
		t.Fatalf("effect status = %q", effectStatus)
	}
}

func TestMaterializeDependencyUpdateEffect(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)

	run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID: "run-dep", TenantID: "tenant-1", InitialClawID: "claw-dep",
		WorkspaceYAML: []byte(execWorkspaceYAML),
		WorkflowYAML:  []byte(dependencyUpdateWorkflowYAML),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if run.CurrentAttemptID == "" {
		t.Fatal("expected bound attempt")
	}

	claim, err := store.ClaimEffect(context.Background(), "dep-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim = %#v, %v", claim, err)
	}
	if claim.Effect.Kind != typesv2.EffectDependencyUpdate {
		t.Fatalf("effect kind = %q", claim.Effect.Kind)
	}

	envelope, err := store.MaterializeCommandTask(context.Background(), claim.Effect.ID, claim.AttemptID, "dep-worker")
	if err != nil {
		t.Fatalf("materialize dependency.update: %v", err)
	}
	if envelope.Kind != typesv2.MessageDependencyUpdateAssign {
		t.Fatalf("envelope kind = %q", envelope.Kind)
	}
	if envelope.TaskID == "" {
		t.Fatal("expected task id")
	}

	var payload typesv2.DependencyUpdateConfig
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if len(payload.Ecosystems) != 1 || payload.Ecosystems[0] != "go" {
		t.Fatalf("ecosystems = %v", payload.Ecosystems)
	}
}

func TestApplyCommandReceiptProjectsExecFacts(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)

	run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID: "run-receipt", TenantID: "tenant-1", InitialClawID: "claw-receipt",
		WorkspaceYAML: []byte(execWorkspaceYAML),
		WorkflowYAML:  []byte(execRunWorkflowYAML),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	claim, err := store.ClaimEffect(context.Background(), "receipt-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim = %#v, %v", claim, err)
	}
	envelope, err := store.MaterializeCommandTask(context.Background(), claim.Effect.ID, claim.AttemptID, "receipt-worker")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}

	receipt := map[string]interface{}{
		"exit_code": 0,
		"succeeded": true,
		"stdout":    "hello",
	}
	receiptJSON, _ := json.Marshal(receipt)
	result, err := store.ApplyCommandReceipt(context.Background(), typesv2.ControlEnvelope{
		ProtocolVersion:      typesv2.ControlProtocolVersion,
		MessageID:            "receipt-1",
		Kind:                 typesv2.MessageExecRunCompleted,
		RunID:                run.ID,
		AttemptID:            run.CurrentAttemptID,
		TaskID:               envelope.TaskID,
		ExpectedStateVersion: &run.StateVersion,
		Payload:              receiptJSON,
	})
	if err != nil {
		t.Fatalf("apply command receipt: %v", err)
	}
	if result.Disposition != typesv2.DispositionAccepted {
		t.Fatalf("disposition = %q reason=%s", result.Disposition, result.Reason)
	}

	var facts []string
	rows, err := db.Query(`SELECT fact_key FROM workflow_v2_facts WHERE run_id=? ORDER BY fact_key`, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		facts = append(facts, key)
	}

	updated, err := store.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != "done" {
		t.Fatalf("state = %q", updated.State)
	}
	if updated.StateVersion != 2 {
		t.Fatalf("state version = %d", updated.StateVersion)
	}
	if !containsKey(facts, "exec.last_run.succeeded") || !containsKey(facts, "exec.last_run.exit_code") {
		t.Fatalf("facts = %v", facts)
	}
}

func containsKey(keys []string, target string) bool {
	for _, k := range keys {
		if k == target {
			return true
		}
	}
	return false
}

func TestCommandReceiptDuplicateRejected(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)

	run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID: "run-dup", TenantID: "tenant-1", InitialClawID: "claw-dup",
		WorkspaceYAML: []byte(execWorkspaceYAML),
		WorkflowYAML:  []byte(execRunWorkflowYAML),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	claim, err := store.ClaimEffect(context.Background(), "dup-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim = %#v, %v", claim, err)
	}
	envelope, err := store.MaterializeCommandTask(context.Background(), claim.Effect.ID, claim.AttemptID, "dup-worker")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}

	receipt := map[string]interface{}{"succeeded": true, "exit_code": 0}
	receiptJSON, _ := json.Marshal(receipt)
	makeEnvelope := func(id string) typesv2.ControlEnvelope {
		return typesv2.ControlEnvelope{
			ProtocolVersion:      typesv2.ControlProtocolVersion,
			MessageID:            id,
			Kind:                 typesv2.MessageExecRunCompleted,
			RunID:                run.ID,
			AttemptID:            run.CurrentAttemptID,
			TaskID:               envelope.TaskID,
			ExpectedStateVersion: &run.StateVersion,
			Payload:              receiptJSON,
		}
	}
	first, err := store.ApplyCommandReceipt(context.Background(), makeEnvelope("receipt-dup-1"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Disposition != typesv2.DispositionAccepted {
		t.Fatalf("first = %q", first.Disposition)
	}
	duplicate, err := store.ApplyCommandReceipt(context.Background(), makeEnvelope("receipt-dup-1"))
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.Disposition != typesv2.DispositionDuplicate {
		t.Fatalf("duplicate = %q", duplicate.Disposition)
	}
}

func TestApplyCommandReceiptDependencyUpdateTransitions(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)

	run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID: "run-dep-update", TenantID: "tenant-1", InitialClawID: "claw-dep-update",
		WorkspaceYAML: []byte(execWorkspaceYAML),
		WorkflowYAML:  []byte(dependencyUpdateWorkflowYAML),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	claim, err := store.ClaimEffect(context.Background(), "dep-update-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim = %#v, %v", claim, err)
	}
	envelope, err := store.MaterializeCommandTask(context.Background(), claim.Effect.ID, claim.AttemptID, "dep-update-worker")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}

	receipt := map[string]interface{}{
		"succeeded":    true,
		"ecosystems":   []string{"go"},
		"files_changed": []string{"go.mod"},
	}
	receiptJSON, _ := json.Marshal(receipt)
	result, err := store.ApplyCommandReceipt(context.Background(), typesv2.ControlEnvelope{
		ProtocolVersion:      typesv2.ControlProtocolVersion,
		MessageID:            "dep-update-receipt-1",
		Kind:                 typesv2.MessageDependencyUpdateCompleted,
		RunID:                run.ID,
		AttemptID:            run.CurrentAttemptID,
		TaskID:               envelope.TaskID,
		ExpectedStateVersion: &run.StateVersion,
		Payload:              receiptJSON,
	})
	if err != nil {
		t.Fatalf("apply command receipt: %v", err)
	}
	if result.Disposition != typesv2.DispositionAccepted {
		t.Fatalf("disposition = %q reason=%s", result.Disposition, result.Reason)
	}

	updated, err := store.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != "done" {
		t.Fatalf("state = %q", updated.State)
	}
	if updated.StateVersion != 2 {
		t.Fatalf("state version = %d", updated.StateVersion)
	}
}

func TestExecRunEffectMissingCapabilityRejected(t *testing.T) {
	ws := `
schema_version: 2
name: restricted
execution:
  provider: daytona
  capability_restrictions:
    execute_command: false
`
	wf := `
schema_version: 2
name: restricted-wf
enabled: true
initial_state: run
states:
  run:
    phase: build
    on_enter:
      effects:
        - exec.run:
            command: "echo hi"
`
	if _, _, err := typesv2.ParseAndValidateWorkflowPair([]byte(wf), []byte(ws)); err == nil || !strings.Contains(err.Error(), "execute_command") {
		t.Fatalf("expected execute_command capability error, got %v", err)
	}
}

func TestDependencyUpdateEffectMissingEcosystemsRejected(t *testing.T) {
	wf := `
schema_version: 2
name: bad-wf
enabled: true
initial_state: run
states:
  run:
    phase: build
    on_enter:
      effects:
        - dependency.update:
            grouping: all
`
	if _, _, err := typesv2.ParseAndValidateWorkflowPair([]byte(wf), []byte(execWorkspaceYAML)); err == nil || !strings.Contains(err.Error(), "ecosystems") {
		t.Fatalf("expected ecosystems error, got %v", err)
	}
}

func TestExecRunEffectMissingCommandRejected(t *testing.T) {
	wf := `
schema_version: 2
name: bad-wf
enabled: true
initial_state: run
states:
  run:
    phase: build
    on_enter:
      effects:
        - exec.run:
            timeout: "1m"
`
	if _, _, err := typesv2.ParseAndValidateWorkflowPair([]byte(wf), []byte(execWorkspaceYAML)); err == nil || !strings.Contains(err.Error(), "command") {
		t.Fatalf("expected command error, got %v", err)
	}
}
