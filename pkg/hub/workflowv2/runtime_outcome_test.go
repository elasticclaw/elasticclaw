package workflowv2_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
)

func TestTerminalOutcomeOverridesStateName(t *testing.T) {
	for _, tc := range []struct {
		state, outcome string
		status         workflowv2.RunStatus
	}{
		{"cancelled", "no_changes", workflowv2.RunCompleted},
		{"done", "cancelled", workflowv2.RunCancelled},
		{"failed", "failed", workflowv2.RunCompleted},
		{"failed", "", workflowv2.RunCompleted},
		{"cancelled", "", workflowv2.RunCancelled},
	} {
		for _, initialTerminal := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%s/initial=%t", tc.state, tc.outcome, initialTerminal), func(t *testing.T) {
				store := workflowv2.NewStore(openRuntimeDB(t))
				initial := "working"
				if initialTerminal {
					initial = tc.state
				}
				doc := fmt.Sprintf(`schema_version: 2
name: outcome-test
enabled: true
initial_state: %s
states:
  working: {phase: build}
  %s: {phase: done, terminal: true, outcome: %q}
transitions:
  finish: {from: working, on: workflow.finished, to: %s}
`, initial, tc.state, tc.outcome, tc.state)
				run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
					TenantID: "tenant-1", WorkspaceYAML: []byte(runtimeWorkspaceYAML), WorkflowYAML: []byte(doc),
				})
				if err != nil {
					t.Fatal(err)
				}
				if !initialTerminal {
					result, err := store.ApplyEvent(context.Background(), run.ID, workflowv2.EventInput{
						Kind: "workflow.finished", Producer: workflowv2.ProducerHub, ID: "finish", MessageID: "finish",
					})
					if err != nil {
						t.Fatal(err)
					}
					run = result.Run
				}
				if run.Status != tc.status || run.FinishedAt == nil {
					t.Fatalf("terminal run: %+v, want %s", run, tc.status)
				}
			})
		}
	}
}
