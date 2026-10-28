package types

import (
	"strings"
	"testing"
)

func TestTriggerFromLegacyPipelineValidation(t *testing.T) {
	const invalid = `stages:
  - id: working
  - id: done
    triggers:
      - message_contains: "[DONE]"
        from: missing
`
	workflow := &WorkflowConfig{Name: "scoped", PipelineYAML: invalid}
	workspace := &WorkspaceConfig{Name: "workspace", Workflows: []*WorkflowConfig{workflow}}
	factory := &FactoryConfig{Name: "scoped", Integration: "linear", Template: "agent", PipelineYAML: invalid}
	for name, validate := range map[string]func() error{
		"workspace": workspace.Validate,
		"workflow":  workflow.Validate,
		"factory":   factory.Validate,
		"normalize": func() error { return NormalizeWorkflowConfig(workflow) },
	} {
		t.Run(name, func(t *testing.T) {
			err := validate()
			if err == nil || !strings.Contains(err.Error(), `from: unknown stage "missing"`) {
				t.Fatalf("expected unknown stage error, got %v", err)
			}
		})
	}
}
