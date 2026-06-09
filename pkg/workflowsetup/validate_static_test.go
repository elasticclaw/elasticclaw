package workflowsetup

import (
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestValidateStaticWorkspaceYAMLInvalidReturnsCriticalDiagnostic(t *testing.T) {
	resp := ValidateStatic(ValidateRequest{
		WorkspaceConfig: "name: [bad\n",
		Config:          validStaticWorkflowYAML(),
	})

	assertCriticalDiagnostic(t, resp, "workspace-yaml-invalid", "workspace")
}

func TestValidateStaticAcceptsRuntimeWorkspaceYAMLFields(t *testing.T) {
	resp := ValidateStatic(ValidateRequest{
		WorkspaceConfig: `
name: issue-triage
provider: replicated
default_model: anthropic/claude-sonnet-4-5
secret_refs:
  LINEAR_API_KEY: linear_api_key
`,
		Config: validStaticWorkflowYAML(),
	})

	assertNoDiagnostic(t, resp, "workspace-yaml-invalid")
	if resp.Summary.Critical != 0 {
		t.Fatalf("critical diagnostics = %d, want 0: %#v", resp.Summary.Critical, resp.Checks)
	}
}

func TestValidateStaticWorkflowYAMLInvalidReturnsCriticalDiagnostic(t *testing.T) {
	resp := ValidateStatic(ValidateRequest{
		Config: "name: [bad\n",
	})

	assertCriticalDiagnostic(t, resp, "workflow-yaml-invalid", "workflow")
}

func TestValidateStaticBlocksZeroEntryStages(t *testing.T) {
	resp := ValidateStatic(ValidateRequest{
		Config: staticWorkflowYAML(`
pipeline_yaml: |
  stages:
    - id: working
      on_enter:
        inject: start
`),
	})

	assertCriticalDiagnostic(t, resp, "pipeline-entry-stage-count", "workflow.pipeline_yaml.stages")
}

func TestValidateStaticBlocksMultipleEntryStages(t *testing.T) {
	resp := ValidateStatic(ValidateRequest{
		Config: staticWorkflowYAML(`
pipeline_yaml: |
  stages:
    - id: working
      entry: true
    - id: review
      entry: true
`),
	})

	assertCriticalDiagnostic(t, resp, "pipeline-entry-stage-count", "workflow.pipeline_yaml.stages")
}

func TestValidateStaticBlocksDuplicateStageIDs(t *testing.T) {
	resp := ValidateStatic(ValidateRequest{
		Config: staticWorkflowYAML(`
pipeline_yaml: |
  stages:
    - id: working
      entry: true
    - id: working
      triggers:
        - message_contains: done
`),
	})

	assertCriticalDiagnostic(t, resp, "pipeline-stage-id-duplicate", "workflow.pipeline_yaml.stages")
}

func TestValidateStaticBlocksEffectivePipelineYAMLParseError(t *testing.T) {
	resp := ValidateStatic(ValidateRequest{
		Config: staticWorkflowYAML(`
pipeline_yaml: "stages:\n\t- id: bad"
`),
	})

	assertCriticalDiagnostic(t, resp, "pipeline-yaml-invalid", "workflow.pipeline_yaml")
}

func TestValidateStaticBlocksGitHubIssueManualWithoutIssueNumber(t *testing.T) {
	resp := ValidateStatic(ValidateRequest{
		Config: `
name: issue-triage
integration: github-issues
enable_manual_trigger: true
pipeline_yaml: |
  stages:
    - id: working
      entry: true
`,
	})

	assertCriticalDiagnostic(t, resp, "workflow-github-issue-manual-input", "workflow.inputs.issue_number")
}

func TestValidateStaticBlocksNamesOutsideSlugGrammar(t *testing.T) {
	resp := ValidateStatic(ValidateRequest{
		Config: strings.Replace(validStaticWorkflowYAML(), "name: issue-triage", "name: Issue Triage", 1),
	})

	assertCriticalDiagnostic(t, resp, "workflow-name-invalid", "workflow.name")
}

func TestValidateStaticTriggerSourceCount(t *testing.T) {
	resp := ValidateStatic(ValidateRequest{
		Config: `
name: mixed-trigger
trigger:
  github_issues:
    event: issue_labeled
    repositories:
      - elasticclaw/elasticclaw
  linear:
    event: status_changed
    states:
      - Todo
pipeline_yaml: |
  stages:
    - id: working
      entry: true
`,
	})

	assertCriticalDiagnostic(t, resp, "workflow-trigger-source-count", "workflow.trigger")
}

func TestValidateStaticManualOnlyWorkflowCanOmitTrigger(t *testing.T) {
	resp := ValidateStatic(ValidateRequest{
		Config: validStaticWorkflowYAML(),
	})

	if !resp.OK {
		t.Fatalf("ValidateStatic OK = false, diagnostics: %#v", resp.Checks)
	}
	if resp.Summary.Critical != 0 {
		t.Fatalf("critical diagnostics = %d, want 0: %#v", resp.Summary.Critical, resp.Checks)
	}
}

func TestValidateStaticBlocksInvalidReferencesAndIntegrationActions(t *testing.T) {
	resp := ValidateStatic(ValidateRequest{
		Config: staticWorkflowYAML(`
pipeline_yaml: |
  stages:
    - id: working
      entry: true
      gate:
        output: missing_output
    - id: create_pr
      triggers:
        - gate_result:
            stage: missing_gate
            verdict: pass
        - output_matches:
            output: missing_output
            path: status
            any_of: [passed]
      on_enter:
        move_issue: In Progress
`),
	})

	assertCriticalDiagnostic(t, resp, "pipeline-gate-output-missing", "workflow.pipeline_yaml.stages")
	assertCriticalDiagnostic(t, resp, "pipeline-gate-result-stage-missing", "workflow.pipeline_yaml.stages")
	assertCriticalDiagnostic(t, resp, "pipeline-output-matches-output-missing", "workflow.pipeline_yaml.stages")
	assertCriticalDiagnostic(t, resp, "pipeline-move-issue-incompatible", "workflow.pipeline_yaml.stages")
}

func TestValidateStaticBlocksInvalidManualInputConstraints(t *testing.T) {
	resp := ValidateStatic(ValidateRequest{
		Config: `
name: issue-triage
integration: github-issues
enable_manual_trigger: true
inputs:
  - name: issue_number
    type: number
    min: 1
  - name: priority
    type: enum
    options: [low, high]
    default: urgent
  - name: branch
    type: string
    validation: "["
pipeline_yaml: |
  stages:
    - id: working
      entry: true
`,
	})

	assertCriticalDiagnostic(t, resp, "workflow-input-default-invalid", "workflow.inputs[1].default")
	assertCriticalDiagnostic(t, resp, "workflow-input-validation-invalid", "workflow.inputs[2].validation")
}

func TestValidateStaticNormalizesCloneWithoutMutatingWorkflow(t *testing.T) {
	workflow := &types.WorkflowConfig{
		Name:                "issue-triage",
		Integration:         "github-issues",
		EnableManualTrigger: true,
		Inputs: []types.FactoryInput{
			{Name: "issue_number", Type: "number", Min: floatPtr(1)},
		},
		Stages: []types.WorkflowStage{
			{ID: "working", Entry: true},
		},
	}

	resp := ValidateWorkflowConfigStatic(workflow, nil)

	if !resp.OK {
		t.Fatalf("ValidateWorkflowConfigStatic OK = false, diagnostics: %#v", resp.Checks)
	}
	if workflow.PipelineYAML != "" {
		t.Fatalf("input workflow PipelineYAML was mutated: %q", workflow.PipelineYAML)
	}
	if workflow.SchemaVersion != "" {
		t.Fatalf("input workflow SchemaVersion was mutated: %q", workflow.SchemaVersion)
	}
}

func validStaticWorkflowYAML() string {
	return `
name: issue-triage
integration: github-issues
enable_manual_trigger: true
inputs:
  - name: issue_number
    type: number
    min: 1
pipeline_yaml: |
  stages:
    - id: working
      entry: true
      on_enter:
        inject: start
`
}

func staticWorkflowYAML(extra string) string {
	return `
name: issue-triage
integration: github-issues
enable_manual_trigger: true
inputs:
  - name: issue_number
    type: number
    min: 1
` + extra
}

func assertCriticalDiagnostic(t *testing.T, resp ValidateResponse, id, fieldPrefix string) {
	t.Helper()

	for _, check := range resp.Checks {
		if check.ID != id {
			continue
		}
		if check.Severity != SeverityCritical {
			t.Fatalf("%s severity = %q, want critical", id, check.Severity)
		}
		if !check.Blocking {
			t.Fatalf("%s blocking = false, want true", id)
		}
		if check.OK {
			t.Fatalf("%s OK = true, want false", id)
		}
		if !strings.HasPrefix(check.FieldPath, fieldPrefix) {
			t.Fatalf("%s fieldPath = %q, want prefix %q", id, check.FieldPath, fieldPrefix)
		}
		if resp.OK {
			t.Fatalf("response OK = true with critical diagnostic %s", id)
		}
		return
	}
	t.Fatalf("missing diagnostic %q with field prefix %q; got %#v", id, fieldPrefix, resp.Checks)
}

func assertNoDiagnostic(t *testing.T, resp ValidateResponse, id string) {
	t.Helper()

	for _, check := range resp.Checks {
		if check.ID == id {
			t.Fatalf("unexpected diagnostic %q: %#v", id, check)
		}
	}
}

func floatPtr(v float64) *float64 {
	return &v
}
