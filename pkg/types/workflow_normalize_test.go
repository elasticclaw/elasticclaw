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
  github_issues:
    event: issue_labeled
    repositories:
      - elasticclaw/elasticclaw
    states:
      - open
    labels:
      - agent-ready
    labelers:
      - "*"
stages:
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

func TestWorkflowV1LinearTriggerValidates(t *testing.T) {
	data := []byte(`
schema_version: v1
name: linear-story
trigger:
  linear:
    event: status_changed
    team: ENG
    states:
      - Todo
    labels:
      - agent-ready
    assigned_to: marc
stages:
  - id: working
    entry: true
    on_enter:
      move_issue: In Progress
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
	if workflow.Integration != "linear" {
		t.Fatalf("integration = %q, want linear", workflow.Integration)
	}
	if workflow.Workspace != "" {
		t.Fatalf("workspace = %q, want empty", workflow.Workspace)
	}
	if workflow.Team != "ENG" {
		t.Fatalf("team = %q, want ENG", workflow.Team)
	}
	if workflow.TriggerStatus != "Todo" {
		t.Fatalf("trigger status = %q, want Todo", workflow.TriggerStatus)
	}
	if workflow.AssignedTo != "marc" {
		t.Fatalf("assigned_to = %q, want marc", workflow.AssignedTo)
	}
	if !strings.Contains(workflow.PipelineYAML, "move_issue:") {
		t.Fatalf("pipeline yaml did not contain move_issue: %q", workflow.PipelineYAML)
	}
}

func TestWorkflowConfigRejectsJobsKey(t *testing.T) {
	data := []byte(`
schema_version: v1
name: github-issue
jobs:
  - id: working
    entry: true
`)

	var workflow WorkflowConfig
	err := yaml.Unmarshal(data, &workflow)
	if err == nil {
		t.Fatal("yaml.Unmarshal() expected error for jobs key")
	}
	if !strings.Contains(err.Error(), `rename it to "stages"`) {
		t.Fatalf("yaml.Unmarshal() error = %v, want stages guidance", err)
	}
}
