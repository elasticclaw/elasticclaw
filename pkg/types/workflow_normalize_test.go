package types

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWorkflowV1GitHubIssuesTriggerValidates(t *testing.T) {
	data := []byte(`
schema_version: v1
name: github-issue
trigger:
  type: github_issues
  event: issue_labeled
  repositories:
    - elasticclaw/elasticclaw
  states:
    - open
  labels:
    - agent-ready
  labelers:
    - "*"
jobs:
  - id: working
    entry: true
    on_enter:
      inject: start
`)
	var workflow WorkflowConfig
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := NormalizeWorkflowConfig(&workflow); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if err := workflow.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if workflow.Integration != "github-issues" {
		t.Fatalf("integration = %q, want github-issues", workflow.Integration)
	}
	if !strings.Contains(workflow.PipelineYAML, "stages:") {
		t.Fatalf("pipeline yaml did not contain stages: %q", workflow.PipelineYAML)
	}
}
