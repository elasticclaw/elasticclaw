package workflowv2_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	workflowv2 "github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

const commandWorkspaceYAML = `
schema_version: 2
name: command-workspace
repositories:
  primary:
    provider: github
    repository: org/repo
execution:
  provider: daytona
`

const commandWorkflowYAML = `
schema_version: 2
name: command-workflow
enabled: true
manual_trigger: true
initial_state: manual
states:
  manual:
    description: Waiting for a start command.
    phase: setup
    on_enter:
      effects:
        - agent.task:
            prompt: Manual trigger received. Starting the workflow.
  done:
    description: Finished.
    phase: done
    terminal: true
commands:
  start:
    from: manual
    to: done
`

func TestApplyCommandTransitionsToDestinationState(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)

	run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID: "run-cmd", TenantID: "tenant-1", InitialClawID: "claw-cmd",
		WorkspaceYAML: []byte(commandWorkspaceYAML),
		WorkflowYAML:  []byte(commandWorkflowYAML),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := store.AssembleOrganizationContext(context.Background(), run.ID, workflowv2.KnowledgeResolverFunc(func(_ context.Context, _ workflowv2.Run, _ string, _ typesv2.KnowledgeSource) (typesv2.ContextBundleSource, error) {
		return typesv2.ContextBundleSource{}, fmt.Errorf("unexpected knowledge resolution")
	})); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteActivation(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}

	result, err := store.ApplyCommand(context.Background(), run.ID, "start", workflowv2.CommandInput{
		ID:        "cmd-1",
		MessageID: "cmd-1",
		Reason:    "manual trigger",
		Provenance: typesv2.EvidenceProvenance{
			Producer:   string(workflowv2.ProducerOperator),
			ObservedAt: time.Now().UTC(),
		},
	})
	if err != nil {
		t.Fatalf("apply command: %v", err)
	}
	if result.Disposition != typesv2.DispositionAccepted {
		t.Fatalf("disposition = %q reason=%s", result.Disposition, result.Reason)
	}
	if result.Run.State != "done" {
		t.Fatalf("state = %q", result.Run.State)
	}
	if result.Run.Status != workflowv2.RunCompleted {
		t.Fatalf("status = %q", result.Run.Status)
	}
	if result.Transition == nil || result.Transition.ToState != "done" {
		t.Fatalf("transition = %#v", result.Transition)
	}
}

func TestApplyCommandRejectsUnknownCommand(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)

	run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID: "run-unknown-cmd", TenantID: "tenant-1", InitialClawID: "claw-cmd",
		WorkspaceYAML: []byte(commandWorkspaceYAML),
		WorkflowYAML:  []byte(commandWorkflowYAML),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := store.AssembleOrganizationContext(context.Background(), run.ID, workflowv2.KnowledgeResolverFunc(func(_ context.Context, _ workflowv2.Run, _ string, _ typesv2.KnowledgeSource) (typesv2.ContextBundleSource, error) {
		return typesv2.ContextBundleSource{}, fmt.Errorf("unexpected knowledge resolution")
	})); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteActivation(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}

	_, err = store.ApplyCommand(context.Background(), run.ID, "restart", workflowv2.CommandInput{
		ID:        "cmd-bad",
		MessageID: "cmd-bad",
		Provenance: typesv2.EvidenceProvenance{
			Producer:   string(workflowv2.ProducerOperator),
			ObservedAt: time.Now().UTC(),
		},
	})
	if err == nil || !strings.Contains(err.Error(), "not defined") {
		t.Fatalf("expected unknown command error, got %v", err)
	}
}

func TestApplyCommandRejectsCommandFromWrongState(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)

	run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID: "run-wrong-state", TenantID: "tenant-1", InitialClawID: "claw-cmd",
		WorkspaceYAML: []byte(commandWorkspaceYAML),
		WorkflowYAML:  []byte(commandWorkflowYAML),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := store.AssembleOrganizationContext(context.Background(), run.ID, workflowv2.KnowledgeResolverFunc(func(_ context.Context, _ workflowv2.Run, _ string, _ typesv2.KnowledgeSource) (typesv2.ContextBundleSource, error) {
		return typesv2.ContextBundleSource{}, fmt.Errorf("unexpected knowledge resolution")
	})); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteActivation(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyCommand(context.Background(), run.ID, "start", workflowv2.CommandInput{
		ID:        "cmd-1",
		MessageID: "cmd-1",
		Provenance: typesv2.EvidenceProvenance{
			Producer:   string(workflowv2.ProducerOperator),
			ObservedAt: time.Now().UTC(),
		},
	}); err != nil {
		t.Fatal(err)
	}
	_, err = store.ApplyCommand(context.Background(), run.ID, "start", workflowv2.CommandInput{
		ID:        "cmd-2",
		MessageID: "cmd-2",
		Provenance: typesv2.EvidenceProvenance{
			Producer:   string(workflowv2.ProducerOperator),
			ObservedAt: time.Now().UTC(),
		},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot be applied from state") {
		t.Fatalf("expected wrong state error, got %v", err)
	}
}
