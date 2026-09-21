package workflowv2_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	workflowv2 "github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

// Mirrors the sbctl-update-go shape from #692 testing: a prepare state with an
// exec.run effect, and a prepare_ready transition guarded on the dotted
// exec.last_run.succeeded fact, entering a state whose on_enter schedules a
// dependency.update effect. Regression: guards written with dotted predicate
// keys must resolve against the nested fact tree.
const prepareExecRunWorkflowYAML = `
schema_version: 2
name: repro-prepare
enabled: true
manual_trigger: true
initial_state: prepare
states:
  prepare:
    phase: setup
    on_enter:
      effects:
        - exec.run:
            command: "echo hi"
            timeout: "1m"
  update_deps:
    phase: build
    on_enter:
      effects:
        - dependency.update:
            ecosystems: [go]
            paths: ["sbctl"]
            timeout: "1m"
transitions:
  prepare_ready:
    from: prepare
    on: exec.run.completed
    when:
      exec.last_run:
        succeeded:
          equals: true
    set:
      work.branch: deps/sbctl-update-go
    to: update_deps
`

func TestApplyCommandReceiptExecRunTransitionsOnDottedFactGuard(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)

	run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID: "run-issue-692", TenantID: "tenant-1", InitialClawID: "claw-issue-692",
		WorkspaceYAML: []byte(execWorkspaceYAML),
		WorkflowYAML:  []byte(prepareExecRunWorkflowYAML),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	claim, err := store.ClaimEffect(context.Background(), "repro-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim = %#v, %v", claim, err)
	}
	envelope, err := store.MaterializeCommandTask(context.Background(), claim.Effect.ID, claim.AttemptID, "repro-worker")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}

	receipt := map[string]interface{}{
		"exit_code": 0,
		"succeeded": true,
		"stdout":    "branch ready",
		"stderr":    "nix evaluation warnings",
	}
	receiptJSON, _ := json.Marshal(receipt)
	version := run.StateVersion
	result, err := store.ApplyCommandReceipt(context.Background(), typesv2.ControlEnvelope{
		ProtocolVersion:      typesv2.ControlProtocolVersion,
		MessageID:            "repro-receipt-1",
		Kind:                 typesv2.MessageExecRunCompleted,
		RunID:                run.ID,
		AttemptID:            run.CurrentAttemptID,
		TaskID:               envelope.TaskID,
		ExpectedStateVersion: &version,
		Payload:              receiptJSON,
	})
	if err != nil {
		t.Fatalf("apply receipt: %v", err)
	}
	if result.Disposition != typesv2.DispositionAccepted {
		t.Fatalf("disposition = %q reason=%s", result.Disposition, result.Reason)
	}

	updated, err := store.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != "update_deps" {
		t.Fatalf("state = %q (version %d), want update_deps — transition did not fire", updated.State, updated.StateVersion)
	}
	var branch string
	if err := db.QueryRow(`SELECT value_json FROM workflow_v2_facts WHERE run_id=? AND fact_key='work.branch'`, run.ID).Scan(&branch); err != nil {
		t.Fatalf("work.branch fact: %v", err)
	}
	if branch != `"deps/sbctl-update-go"` {
		t.Fatalf("work.branch = %s", branch)
	}
}
