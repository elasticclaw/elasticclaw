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

func TestWorkflowV1GitHubIssuesTriggerPreservesAgentStatusError(t *testing.T) {
	data := []byte(`
schema_version: v1
name: github-issue
trigger:
  github_issues:
    event: issue_labeled
    repositories:
      - elasticclaw/elasticclaw
    labels:
      - agent-ready
    agent_status_error: agent-error
stages:
  - id: working
    entry: true
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
	if got := workflow.Trigger.GitHubIssues.AgentStatusError; got != "agent-error" {
		t.Fatalf("agent_status_error = %q, want agent-error", got)
	}
}

func TestWorkflowConfigRejectsInvalidRunKind(t *testing.T) {
	workflow := &WorkflowConfig{Name: "invalid-kind", RunKind: "typo"}
	err := workflow.Validate()
	if err == nil {
		t.Fatal("Validate() expected error")
	}
	if !strings.Contains(err.Error(), "invalid run_kind") {
		t.Fatalf("Validate() error = %v, want invalid run_kind", err)
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

func TestWorkflowNormalizePreservesExcludeLabels(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{
			name: "linear",
			data: []byte(`
schema_version: v1
name: linear-story
trigger:
  linear:
    event: status_changed
    states: [Todo]
    exclude_labels: [Bug]
stages:
  - id: working
    entry: true
`),
		},
		{
			name: "shortcut",
			data: []byte(`
schema_version: v1
name: shortcut-story
trigger:
  shortcut:
    event: status_changed
    states: [Todo]
    exclude_labels: [Bug]
stages:
  - id: working
    entry: true
`),
		},
		{
			name: "github issues",
			data: []byte(`
schema_version: v1
name: github-issue
trigger:
  github_issues:
    event: issue_labeled
    repositories: [elasticclaw/elasticclaw]
    labels: [agent-ready]
    exclude_labels: [Bug]
stages:
  - id: working
    entry: true
`),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var workflow WorkflowConfig
			if err := yaml.Unmarshal(tt.data, &workflow); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if err := NormalizeWorkflowConfig(&workflow); err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if got := strings.Join(workflow.ExcludeLabels, ","); got != "Bug" {
				t.Fatalf("ExcludeLabels = %q, want Bug", got)
			}
			if err := workflow.Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
		})
	}
}

func TestWorkflowV1LinearTriggerPreservesAgentStatusError(t *testing.T) {
	data := []byte(`
schema_version: v1
name: linear-story
trigger:
  linear:
    event: status_changed
    states:
      - Todo
    agent_status_error: Agent Error
stages:
  - id: working
    entry: true
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
	if got := workflow.Trigger.Linear.AgentStatusError; got != "Agent Error" {
		t.Fatalf("agent_status_error = %q, want Agent Error", got)
	}
}

func TestWorkflowV1TriggersRejectBlankAgentStatusError(t *testing.T) {
	tests := []struct {
		name string
		wf   *WorkflowConfig
		want string
	}{
		{
			name: "github issues",
			wf: &WorkflowConfig{
				Name: "github-issue",
				Trigger: &WorkflowTrigger{GitHubIssues: &GitHubIssuesWorkflowTrigger{
					Event:            "issue_labeled",
					Repositories:     []string{"elasticclaw/elasticclaw"},
					AgentStatusError: "   ",
				}},
			},
			want: "trigger.github_issues.agent_status_error cannot be blank",
		},
		{
			name: "linear",
			wf: &WorkflowConfig{
				Name: "linear-story",
				Trigger: &WorkflowTrigger{Linear: &LinearWorkflowTrigger{
					Event:            "status_changed",
					States:           []string{"Todo"},
					AgentStatusError: "   ",
				}},
			},
			want: "trigger.linear.agent_status_error cannot be blank",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.wf.Validate()
			if err == nil {
				t.Fatal("Validate() expected error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestWorkflowNormalizePreservesStageGate(t *testing.T) {
	data := []byte(`
schema_version: v1
name: linear-story
trigger:
  linear:
    event: status_changed
    workspace: Fasterway
    states:
      - Todo
stages:
  - id: android_validation
    label: Android Validation
    triggers:
      - message_contains: "[DONE]"
    on_enter:
      run:
        command: python3 scripts/run_android_codebuild.py --source-dir next_mobile
        output: android_validation
    gate:
      output: android_validation
      pass:
        path: status
        values:
          - passed
          - skipped
      fail:
        path: status
        values:
          - failed
          - error
      required: true
      treat_skipped_as_pass: true
`)
	var workflow WorkflowConfig
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := NormalizeWorkflowConfig(&workflow); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	for _, want := range []string{
		"gate:",
		"output: android_validation",
		"path: status",
		"treat_skipped_as_pass: true",
	} {
		if !strings.Contains(workflow.PipelineYAML, want) {
			t.Fatalf("pipeline yaml did not contain %q: %s", want, workflow.PipelineYAML)
		}
	}
}

func TestWorkflowV1ShortcutTriggerValidates(t *testing.T) {
	data := []byte(`
schema_version: v1
name: shortcut-story
trigger:
  shortcut:
    event: status_changed
    workspace: eng
    states:
      - Todo
    labels:
      - agent-ready
    assigned_to: marc
stages:
  - id: working
    entry: true
    on_enter:
      move_story: In Progress
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
	if workflow.Integration != "shortcut" {
		t.Fatalf("integration = %q, want shortcut", workflow.Integration)
	}
	if workflow.Workspace != "eng" {
		t.Fatalf("workspace = %q, want eng", workflow.Workspace)
	}
	if workflow.TriggerStatus != "Todo" {
		t.Fatalf("trigger status = %q, want Todo", workflow.TriggerStatus)
	}
}

func TestWorkflowV1CronTriggerValidates(t *testing.T) {
	data := []byte(`
schema_version: v1
name: dependency-update
trigger:
  cron:
    schedule: "0 9 * * 1"
    timezone: America/Chicago
    overlap_policy: skip
    timeout: 2h
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
	if workflow.Integration != "cron" {
		t.Fatalf("integration = %q, want cron", workflow.Integration)
	}
	if !strings.Contains(workflow.PipelineYAML, "stages:") {
		t.Fatalf("pipeline yaml did not contain stages: %q", workflow.PipelineYAML)
	}
}

func TestWorkflowV1RejectsLegacyFlatTrigger(t *testing.T) {
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
stages:
  - id: working
    entry: true
`)
	var workflow WorkflowConfig
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := NormalizeWorkflowConfig(&workflow); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	err := workflow.Validate()
	if err == nil {
		t.Fatal("Validate() expected error for legacy flat trigger")
	}
	want := `workflow "github-issue": trigger must define exactly one source`
	if err.Error() != want {
		t.Fatalf("Validate() error = %v, want %s", err, want)
	}
}

func TestWorkflowV1TriggerSourceCountError(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{
			name: "no source",
			yaml: `
schema_version: v1
name: github-issue
trigger: {}
stages:
  - id: working
    entry: true
`,
		},
		{
			name: "multiple sources",
			yaml: `
schema_version: v1
name: github-issue
trigger:
  github_issues:
    event: issue_labeled
  linear:
    event: status_changed
    states:
      - Todo
stages:
  - id: working
    entry: true
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var workflow WorkflowConfig
			if err := yaml.Unmarshal([]byte(tt.yaml), &workflow); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			err := workflow.Validate()
			if err == nil {
				t.Fatal("Validate() expected error")
			}
			want := `workflow "github-issue": trigger must define exactly one source`
			if err.Error() != want {
				t.Fatalf("Validate() error = %v, want %s", err, want)
			}
		})
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
