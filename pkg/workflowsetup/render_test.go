package workflowsetup

import (
	"reflect"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/elasticclaw/elasticclaw/pkg/workflow/pipeline"
	"gopkg.in/yaml.v3"
)

func TestRenderPatternsMetadata(t *testing.T) {
	patterns := Patterns()

	gotIDs := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		gotIDs = append(gotIDs, pattern.ID)
		if pattern.Label == "" {
			t.Fatalf("pattern %q label is empty", pattern.ID)
		}
		if pattern.Description == "" {
			t.Fatalf("pattern %q description is empty", pattern.ID)
		}
		if len(pattern.RequiredFields) == 0 {
			t.Fatalf("pattern %q required fields are empty", pattern.ID)
		}
		if len(pattern.AdvancedFields) == 0 {
			t.Fatalf("pattern %q advanced fields are empty", pattern.ID)
		}
		if len(pattern.Defaults) == 0 {
			t.Fatalf("pattern %q defaults are empty", pattern.ID)
		}
		if len(pattern.ValidationFieldPaths) == 0 {
			t.Fatalf("pattern %q validation field paths are empty", pattern.ID)
		}
		if pattern.ID == PatternLinearStatus || pattern.ID == PatternShortcutStatus || pattern.ID == PatternManualTask {
			if got, ok := pattern.Defaults["concurrencyGroup"].(string); !ok || got != "global" {
				t.Fatalf("pattern %q concurrencyGroup default = %#v, want global", pattern.ID, pattern.Defaults["concurrencyGroup"])
			}
			if !hasPatternField(pattern.AdvancedFields, "config.concurrencyGroup") {
				t.Fatalf("pattern %q advanced fields missing config.concurrencyGroup", pattern.ID)
			}
		}
	}

	wantIDs := []string{"github-issue", "linear-status", "shortcut-status", "manual-task"}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("pattern ids = %#v, want %#v", gotIDs, wantIDs)
	}
}

func TestRenderGithubIssuePattern(t *testing.T) {
	resp, err := Render(RenderRequest{
		WorkflowName: "issue-triage",
		PatternID:    "github-issue",
		Config: map[string]interface{}{
			"repository":           "owner/repo",
			"enableManualTrigger":  true,
			"concurrencyGroup":     "repo:owner/repo",
			"includePreCommit":     true,
			"preCommitCommand":     "go test ./...",
			"preCommitReadySignal": "[READY_TO_COMMIT]",
			"labels":               []interface{}{"agent-ready"},
			"workingLabel":         "agent-working",
			"reviewLabel":          "agent-review",
			"doneLabel":            "agent-done",
			"closedLabel":          "agent-needs-attention",
		},
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if resp.WorkflowName != "issue-triage" {
		t.Fatalf("workflowName = %q, want issue-triage", resp.WorkflowName)
	}
	if resp.ConfigHash != ConfigHash(resp.Config) {
		t.Fatalf("configHash = %q, want %q", resp.ConfigHash, ConfigHash(resp.Config))
	}

	workflow := parseRenderedWorkflow(t, resp.Config)
	if workflow.Trigger == nil || workflow.Trigger.GitHubIssues == nil {
		t.Fatalf("github_issues trigger missing: %#v", workflow.Trigger)
	}
	trigger := workflow.Trigger.GitHubIssues
	if !reflect.DeepEqual(trigger.Repositories, []string{"owner/repo"}) {
		t.Fatalf("repositories = %#v, want owner/repo", trigger.Repositories)
	}
	if got, want := trigger.Event, "issue_labeled"; got != want {
		t.Fatalf("event = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(trigger.Labels, []string{"agent-ready"}) {
		t.Fatalf("labels = %#v, want agent-ready", trigger.Labels)
	}
	if !reflect.DeepEqual(trigger.States, []string{"open"}) {
		t.Fatalf("states = %#v, want open", trigger.States)
	}
	if !workflow.EnableManualTrigger {
		t.Fatalf("enable_manual_trigger = false, want true")
	}
	if workflow.ConcurrencyGroup != "repo:owner/repo" {
		t.Fatalf("concurrency_group = %q, want repo:owner/repo", workflow.ConcurrencyGroup)
	}

	input := findInput(workflow.Inputs, "issue_number")
	if input == nil {
		t.Fatalf("manual issue_number input missing in %#v", workflow.Inputs)
	}
	if input.Type != "number" || !input.Required {
		t.Fatalf("issue_number input = %#v, want required number", *input)
	}
	if input.Min == nil || *input.Min != 1 {
		t.Fatalf("issue_number min = %#v, want 1", input.Min)
	}

	assertStageIDs(t, workflow, []string{"working", "pre_commit", "pr_opened", "merged", "closed_no_merge"})
	parsedPipeline := parseRenderedPipeline(t, workflow)
	if parsedPipeline.StageForPRMerged() == nil {
		t.Fatalf("merged stage does not include pr_merged trigger")
	}
	if parsedPipeline.StageForPRClosed() == nil {
		t.Fatalf("closed_no_merge stage does not include pr_closed trigger")
	}
	assertStageStringList(t, workflow, "working", "remove_labels", []string{"agent-ready"})
	assertStageStringList(t, workflow, "working", "add_labels", []string{"agent-working"})
	assertStageHasInject(t, workflow, "working")
	assertStageStringList(t, workflow, "pr_opened", "add_labels", []string{"agent-review"})
	assertStageStringList(t, workflow, "pr_opened", "remove_labels", []string{"agent-working"})
	assertStageStringList(t, workflow, "merged", "add_labels", []string{"agent-done"})
	assertStageStringList(t, workflow, "closed_no_merge", "add_labels", []string{"agent-needs-attention"})
	assertStageHasInject(t, workflow, "closed_no_merge")
	entryStage := parsedPipeline.EntryStage()
	if entryStage == nil {
		t.Fatalf("parsed pipeline entry stage missing")
	}
	if !reflect.DeepEqual(entryStage.OnEnter.RemoveLabels, []string{"agent-ready"}) {
		t.Fatalf("parsed working remove_labels = %#v, want agent-ready", entryStage.OnEnter.RemoveLabels)
	}
	if !reflect.DeepEqual(entryStage.OnEnter.AddLabels, []string{"agent-working"}) {
		t.Fatalf("parsed working add_labels = %#v, want agent-working", entryStage.OnEnter.AddLabels)
	}
	if !reflect.DeepEqual(parsedPipeline.StageForPRMerged().OnEnter.AddLabels, []string{"agent-done"}) {
		t.Fatalf("parsed merged add_labels = %#v, want agent-done", parsedPipeline.StageForPRMerged().OnEnter.AddLabels)
	}
}

func TestRenderLinearStatusPattern(t *testing.T) {
	resp, err := Render(RenderRequest{
		WorkflowName: "linear-lifecycle",
		PatternID:    "linear-status",
		Config: map[string]interface{}{
			"workspace":           "product",
			"team":                "ENG",
			"triggerStatus":       "Ready for Agent",
			"workingStatus":       "In Progress",
			"prOpenedStatus":      "In Review",
			"mergedStatus":        "Done",
			"closedNoMergeStatus": "Needs Attention",
		},
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	workflow := parseRenderedWorkflow(t, resp.Config)
	if workflow.Trigger == nil || workflow.Trigger.Linear == nil {
		t.Fatalf("linear trigger missing: %#v", workflow.Trigger)
	}
	if workflow.Trigger.Linear.Workspace != "product" {
		t.Fatalf("linear workspace = %q, want product", workflow.Trigger.Linear.Workspace)
	}
	if workflow.Trigger.Linear.Team != "ENG" {
		t.Fatalf("linear team = %q, want ENG", workflow.Trigger.Linear.Team)
	}
	if !reflect.DeepEqual(workflow.Trigger.Linear.States, []string{"Ready for Agent"}) {
		t.Fatalf("linear states = %#v, want Ready for Agent", workflow.Trigger.Linear.States)
	}
	if workflow.ConcurrencyGroup != "global" {
		t.Fatalf("concurrency_group = %q, want global", workflow.ConcurrencyGroup)
	}
	if !strings.Contains(resp.Config, "concurrency_group: global") {
		t.Fatalf("rendered YAML missing concurrency_group: global\n%s", resp.Config)
	}

	assertStageIDs(t, workflow, []string{"working", "pr_opened", "merged", "closed_no_merge"})
	assertMoveIssueStatus(t, workflow, "working", "In Progress")
	assertMoveIssueStatus(t, workflow, "pr_opened", "In Review")
	assertMoveIssueStatus(t, workflow, "merged", "Done")
	assertMoveIssueStatus(t, workflow, "closed_no_merge", "Needs Attention")
}

func TestRenderLinearStatusRequiresWorkspace(t *testing.T) {
	_, err := Render(RenderRequest{
		WorkflowName: "linear-lifecycle",
		PatternID:    "linear-status",
		Config: map[string]interface{}{
			"triggerStatus": "Ready for Agent",
			"workspace":     " ",
		},
	})
	if err == nil {
		t.Fatalf("Render() error = nil, want workspace required error")
	}
	if !strings.Contains(err.Error(), "config.workspace is required") {
		t.Fatalf("Render() error = %q, want config.workspace required", err.Error())
	}
}

func TestRenderShortcutStatusPattern(t *testing.T) {
	resp, err := Render(RenderRequest{
		WorkflowName: "shortcut-lifecycle",
		PatternID:    "shortcut-status",
		Config: map[string]interface{}{
			"workspace":           "shortcut",
			"triggerStatus":       "Ready for Agent",
			"workingStatus":       "In Progress",
			"prOpenedStatus":      "In Review",
			"mergedStatus":        "Done",
			"closedNoMergeStatus": "Needs Attention",
		},
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	workflow := parseRenderedWorkflow(t, resp.Config)
	if workflow.Trigger == nil || workflow.Trigger.Shortcut == nil {
		t.Fatalf("shortcut trigger missing: %#v", workflow.Trigger)
	}
	if workflow.Trigger.Shortcut.Workspace != "shortcut" {
		t.Fatalf("shortcut workspace = %q, want shortcut", workflow.Trigger.Shortcut.Workspace)
	}
	if !reflect.DeepEqual(workflow.Trigger.Shortcut.States, []string{"Ready for Agent"}) {
		t.Fatalf("shortcut states = %#v, want Ready for Agent", workflow.Trigger.Shortcut.States)
	}
	if workflow.ConcurrencyGroup != "global" {
		t.Fatalf("concurrency_group = %q, want global", workflow.ConcurrencyGroup)
	}
	if !strings.Contains(resp.Config, "concurrency_group: global") {
		t.Fatalf("rendered YAML missing concurrency_group: global\n%s", resp.Config)
	}

	assertStageIDs(t, workflow, []string{"working", "pr_opened", "merged", "closed_no_merge"})
	assertMoveIssueStatus(t, workflow, "working", "In Progress")
	assertMoveIssueStatus(t, workflow, "pr_opened", "In Review")
	assertMoveIssueStatus(t, workflow, "merged", "Done")
	assertMoveIssueStatus(t, workflow, "closed_no_merge", "Needs Attention")
}

func TestRenderShortcutStatusRequiresWorkspace(t *testing.T) {
	_, err := Render(RenderRequest{
		WorkflowName: "shortcut-lifecycle",
		PatternID:    "shortcut-status",
		Config: map[string]interface{}{
			"triggerStatus": "Ready for Agent",
		},
	})
	if err == nil {
		t.Fatalf("Render() error = nil, want workspace required error")
	}
	if !strings.Contains(err.Error(), "config.workspace is required") {
		t.Fatalf("Render() error = %q, want config.workspace required", err.Error())
	}
}

func TestRenderManualTaskPattern(t *testing.T) {
	resp, err := Render(RenderRequest{
		WorkflowName: "manual-fix",
		PatternID:    "manual-task",
		Config: map[string]interface{}{
			"includePreCommit": true,
			"preCommitCommand": "go test ./pkg/workflowsetup",
			"inputs": []interface{}{
				map[string]interface{}{
					"name":        "task",
					"type":        "string",
					"required":    true,
					"description": "Task to complete",
				},
				map[string]interface{}{
					"name":    "priority",
					"type":    "enum",
					"options": []interface{}{"low", "high"},
					"default": "low",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	workflow := parseRenderedWorkflow(t, resp.Config)
	if !workflow.EnableManualTrigger {
		t.Fatalf("enable_manual_trigger = false, want true")
	}
	if workflow.Trigger != nil {
		t.Fatalf("manual-task trigger = %#v, want nil", workflow.Trigger)
	}
	if workflow.ConcurrencyGroup != "global" {
		t.Fatalf("concurrency_group = %q, want global", workflow.ConcurrencyGroup)
	}
	if !strings.Contains(resp.Config, "concurrency_group: global") {
		t.Fatalf("rendered YAML missing concurrency_group: global\n%s", resp.Config)
	}
	if findInput(workflow.Inputs, "task") == nil {
		t.Fatalf("manual task input missing in %#v", workflow.Inputs)
	}
	priority := findInput(workflow.Inputs, "priority")
	if priority == nil {
		t.Fatalf("priority input missing in %#v", workflow.Inputs)
	}
	if priority.Type != "enum" || !reflect.DeepEqual(priority.Options, []string{"low", "high"}) || priority.Default != "low" {
		t.Fatalf("priority input = %#v, want configurable enum", *priority)
	}

	assertStageIDs(t, workflow, []string{"working", "pre_commit", "complete"})
	parsedPipeline := parseRenderedPipeline(t, workflow)
	if parsedPipeline.StageForMessageContains("implementation finished [DONE]") == nil {
		t.Fatalf("complete stage does not include [DONE] message trigger")
	}
}

func parseRenderedWorkflow(t *testing.T, config string) types.WorkflowConfig {
	t.Helper()

	var workflow types.WorkflowConfig
	if err := yaml.Unmarshal([]byte(config), &workflow); err != nil {
		t.Fatalf("rendered YAML did not parse as WorkflowConfig: %v\n%s", err, config)
	}
	if err := workflow.Validate(); err != nil {
		t.Fatalf("rendered WorkflowConfig did not validate: %v\n%s", err, config)
	}
	return workflow
}

func hasPatternField(fields []PatternField, path string) bool {
	for _, field := range fields {
		if field.Path == path {
			return true
		}
	}
	return false
}

func parseRenderedPipeline(t *testing.T, workflow types.WorkflowConfig) *pipeline.Pipeline {
	t.Helper()

	data, err := yaml.Marshal(struct {
		Stages []types.WorkflowStage `yaml:"stages"`
	}{Stages: workflow.Stages})
	if err != nil {
		t.Fatalf("marshal rendered stages: %v", err)
	}
	parsedPipeline, err := pipeline.Parse(data)
	if err != nil {
		t.Fatalf("rendered stages did not parse as pipeline: %v\n%s", err, data)
	}
	return parsedPipeline
}

func assertStageIDs(t *testing.T, workflow types.WorkflowConfig, want []string) {
	t.Helper()

	got := make([]string, 0, len(workflow.Stages))
	for _, stage := range workflow.Stages {
		got = append(got, stage.ID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stage ids = %#v, want %#v", got, want)
	}
}

func assertMoveIssueStatus(t *testing.T, workflow types.WorkflowConfig, stageID string, want string) {
	t.Helper()

	for _, stage := range workflow.Stages {
		if stage.ID != stageID {
			continue
		}
		if stage.OnEnter == nil {
			t.Fatalf("stage %q on_enter missing", stageID)
		}
		rawMoveIssue, ok := stage.OnEnter["move_issue"].(map[string]interface{})
		if !ok {
			t.Fatalf("stage %q move_issue = %#v, want mapping", stageID, stage.OnEnter["move_issue"])
		}
		if got, ok := rawMoveIssue["status"].(string); !ok || got != want {
			t.Fatalf("stage %q move_issue.status = %#v, want %q", stageID, rawMoveIssue["status"], want)
		}
		return
	}
	t.Fatalf("stage %q missing", stageID)
}

func assertStageStringList(t *testing.T, workflow types.WorkflowConfig, stageID string, action string, want []string) {
	t.Helper()

	for _, stage := range workflow.Stages {
		if stage.ID != stageID {
			continue
		}
		if stage.OnEnter == nil {
			t.Fatalf("stage %q on_enter missing", stageID)
		}
		got := stringList(stage.OnEnter[action])
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("stage %q %s = %#v, want %#v", stageID, action, got, want)
		}
		return
	}
	t.Fatalf("stage %q missing", stageID)
}

func assertStageHasInject(t *testing.T, workflow types.WorkflowConfig, stageID string) {
	t.Helper()

	for _, stage := range workflow.Stages {
		if stage.ID != stageID {
			continue
		}
		if stage.OnEnter == nil {
			t.Fatalf("stage %q on_enter missing", stageID)
		}
		inject, ok := stage.OnEnter["inject"].(string)
		if !ok || strings.TrimSpace(inject) == "" {
			t.Fatalf("stage %q inject = %#v, want non-empty string", stageID, stage.OnEnter["inject"])
		}
		return
	}
	t.Fatalf("stage %q missing", stageID)
}

func findInput(inputs []types.FactoryInput, name string) *types.FactoryInput {
	for i := range inputs {
		if inputs[i].Name == name {
			return &inputs[i]
		}
	}
	return nil
}
