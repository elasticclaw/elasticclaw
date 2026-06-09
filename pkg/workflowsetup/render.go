package workflowsetup

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"gopkg.in/yaml.v3"
)

var repositoryPattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+$`)

// Render renders a built-in workflow setup pattern into editable workflow v1 YAML.
func Render(req RenderRequest) (RenderResponse, error) {
	pattern, ok := patternByID(req.PatternID)
	if !ok {
		return RenderResponse{}, fmt.Errorf("unknown workflow pattern %q", req.PatternID)
	}

	workflowName := renderWorkflowName(req.WorkflowName, pattern.ID)
	if err := ValidateSlug(workflowName); err != nil {
		return RenderResponse{}, err
	}

	var (
		workflow types.WorkflowConfig
		err      error
	)
	switch pattern.ID {
	case PatternGitHubIssue:
		workflow, err = renderGitHubIssueWorkflow(workflowName, req.Config)
	case PatternLinearStatus:
		workflow, err = renderLinearStatusWorkflow(workflowName, req.Config)
	case PatternShortcutStatus:
		workflow, err = renderShortcutStatusWorkflow(workflowName, req.Config)
	case PatternManualTask:
		workflow, err = renderManualTaskWorkflow(workflowName, req.Config)
	default:
		err = fmt.Errorf("workflow pattern %q is registered without a renderer", pattern.ID)
	}
	if err != nil {
		return RenderResponse{}, err
	}

	if err := workflow.Validate(); err != nil {
		return RenderResponse{}, err
	}

	data, err := yaml.Marshal(workflow)
	if err != nil {
		return RenderResponse{}, fmt.Errorf("marshal workflow YAML: %w", err)
	}
	config := string(data)
	return RenderResponse{
		WorkflowName: workflowName,
		Config:       config,
		ConfigHash:   ConfigHash(config),
		Warnings:     []Diagnostic{},
	}, nil
}

func renderWorkflowName(requestedName string, fallback string) string {
	name := strings.TrimSpace(requestedName)
	if name == "" {
		name = fallback
	}
	if err := ValidateSlug(name); err == nil {
		return name
	}
	candidate := SlugCandidate(name)
	if candidate == "" {
		return fallback
	}
	return candidate
}

func renderGitHubIssueWorkflow(name string, config map[string]interface{}) (types.WorkflowConfig, error) {
	repository := stringConfig(config, "repository", "")
	if repository == "" {
		return types.WorkflowConfig{}, fmt.Errorf("config.repository is required for %s", PatternGitHubIssue)
	}
	if !repositoryPattern.MatchString(repository) {
		return types.WorkflowConfig{}, fmt.Errorf("config.repository %q must be owner/repo", repository)
	}

	enableManualTrigger := boolConfig(config, "enableManualTrigger", true)
	triggerLabel := stringConfig(config, "triggerLabel", "agent-ready")
	triggerLabels := stringListConfig(config, "labels", []string{triggerLabel})
	stages := prLifecycleStages(lifecycleStageOptions{
		IncludePreCommit: boolConfig(config, "includePreCommit", false),
		PreCommitCommand: stringConfig(config, "preCommitCommand", "go test ./..."),
		ReadySignal:      stringConfig(config, "preCommitReadySignal", "[READY_TO_COMMIT]"),
		WorkingInject:    "Issue: {{.Issue.Identifier}} - {{.Issue.Title}}\nURL: {{.Issue.URL}}\n\nWork on the GitHub issue. When you open a pull request, reply with [DONE] and the PR URL.",
		TriggerLabels:    triggerLabels,
		WorkingLabel:     stringConfig(config, "workingLabel", "agent-working"),
		ReviewLabel:      stringConfig(config, "reviewLabel", "agent-review"),
		DoneLabel:        stringConfig(config, "doneLabel", "agent-done"),
		ClosedLabel:      stringConfig(config, "closedLabel", "agent-needs-attention"),
	})

	workflow := types.WorkflowConfig{
		SchemaVersion:       "v1",
		Name:                name,
		Integration:         "github-issues",
		ConcurrencyGroup:    stringConfig(config, "concurrencyGroup", "global"),
		EnableManualTrigger: enableManualTrigger,
		Trigger: &types.WorkflowTrigger{
			GitHubIssues: &types.GitHubIssuesWorkflowTrigger{
				Event:        stringConfig(config, "event", "issue_labeled"),
				Repositories: []string{repository},
				States:       stringListConfig(config, "states", []string{"open"}),
				Labels:       triggerLabels,
				Labelers:     stringListConfig(config, "labelers", []string{"*"}),
				AssignedTo:   stringConfig(config, "assignedTo", ""),
			},
		},
		Stages: stages,
	}
	if enableManualTrigger {
		min := 1.0
		workflow.Inputs = []types.FactoryInput{
			{
				Name:        "issue_number",
				Type:        "number",
				Required:    true,
				Description: "GitHub issue number to run manually.",
				Min:         &min,
			},
		}
	}
	return workflow, nil
}

func renderLinearStatusWorkflow(name string, config map[string]interface{}) (types.WorkflowConfig, error) {
	workspace := stringConfig(config, "workspace", "")
	if workspace == "" {
		return types.WorkflowConfig{}, fmt.Errorf("config.workspace is required for %s", PatternLinearStatus)
	}
	triggerStatus := stringConfig(config, "triggerStatus", "Ready for Agent")
	workflow := types.WorkflowConfig{
		SchemaVersion:    "v1",
		Name:             name,
		Integration:      "linear",
		ConcurrencyGroup: stringConfig(config, "concurrencyGroup", "global"),
		Trigger: &types.WorkflowTrigger{
			Linear: &types.LinearWorkflowTrigger{
				Event:     stringConfig(config, "event", "status_changed"),
				Workspace: workspace,
				Team:      stringConfig(config, "team", ""),
				States:    []string{triggerStatus},
			},
		},
		Stages: prLifecycleStages(lifecycleStageOptions{
			IncludePreCommit:     boolConfig(config, "includePreCommit", false),
			PreCommitCommand:     stringConfig(config, "preCommitCommand", "go test ./..."),
			ReadySignal:          stringConfig(config, "preCommitReadySignal", "[READY_TO_COMMIT]"),
			WorkingInject:        "Work on the Linear issue. When you open a pull request, reply with [DONE] and the PR URL.",
			WorkingStatus:        stringConfig(config, "workingStatus", ""),
			PROpenedStatus:       stringConfig(config, "prOpenedStatus", ""),
			MergedStatus:         stringConfig(config, "mergedStatus", ""),
			ClosedNoMergeStatus:  stringConfig(config, "closedNoMergeStatus", ""),
			MoveIssueOnTerminals: true,
		}),
	}
	return workflow, nil
}

func renderShortcutStatusWorkflow(name string, config map[string]interface{}) (types.WorkflowConfig, error) {
	workspace := stringConfig(config, "workspace", "")
	if workspace == "" {
		return types.WorkflowConfig{}, fmt.Errorf("config.workspace is required for %s", PatternShortcutStatus)
	}
	triggerStatus := stringConfig(config, "triggerStatus", "Ready for Agent")
	workflow := types.WorkflowConfig{
		SchemaVersion:    "v1",
		Name:             name,
		Integration:      "shortcut",
		ConcurrencyGroup: stringConfig(config, "concurrencyGroup", "global"),
		Trigger: &types.WorkflowTrigger{
			Shortcut: &types.ShortcutWorkflowTrigger{
				Event:     stringConfig(config, "event", "status_changed"),
				Workspace: workspace,
				States:    []string{triggerStatus},
			},
		},
		Stages: prLifecycleStages(lifecycleStageOptions{
			IncludePreCommit:     boolConfig(config, "includePreCommit", false),
			PreCommitCommand:     stringConfig(config, "preCommitCommand", "go test ./..."),
			ReadySignal:          stringConfig(config, "preCommitReadySignal", "[READY_TO_COMMIT]"),
			WorkingInject:        "Work on the Shortcut story. When you open a pull request, reply with [DONE] and the PR URL.",
			WorkingStatus:        stringConfig(config, "workingStatus", ""),
			PROpenedStatus:       stringConfig(config, "prOpenedStatus", ""),
			MergedStatus:         stringConfig(config, "mergedStatus", ""),
			ClosedNoMergeStatus:  stringConfig(config, "closedNoMergeStatus", ""),
			MoveIssueOnTerminals: true,
		}),
	}
	return workflow, nil
}

func renderManualTaskWorkflow(name string, config map[string]interface{}) (types.WorkflowConfig, error) {
	inputs, err := inputsConfig(config, "inputs", []types.FactoryInput{
		{
			Name:        "task",
			Type:        "string",
			Required:    true,
			Description: "Task to complete",
		},
	})
	if err != nil {
		return types.WorkflowConfig{}, err
	}

	return types.WorkflowConfig{
		SchemaVersion:       "v1",
		Name:                name,
		ConcurrencyGroup:    stringConfig(config, "concurrencyGroup", "global"),
		EnableManualTrigger: true,
		Inputs:              inputs,
		Stages: manualTaskStages(manualStageOptions{
			IncludePreCommit: boolConfig(config, "includePreCommit", false),
			PreCommitCommand: stringConfig(config, "preCommitCommand", "go test ./..."),
			ReadySignal:      stringConfig(config, "preCommitReadySignal", "[READY_TO_COMMIT]"),
			DoneSignal:       stringConfig(config, "doneSignal", "[DONE]"),
		}),
	}, nil
}

type lifecycleStageOptions struct {
	IncludePreCommit     bool
	PreCommitCommand     string
	ReadySignal          string
	WorkingInject        string
	TriggerLabels        []string
	WorkingLabel         string
	ReviewLabel          string
	DoneLabel            string
	ClosedLabel          string
	WorkingStatus        string
	PROpenedStatus       string
	MergedStatus         string
	ClosedNoMergeStatus  string
	MoveIssueOnTerminals bool
}

func prLifecycleStages(opts lifecycleStageOptions) []types.WorkflowStage {
	working := types.WorkflowStage{
		ID:    "working",
		Label: "Working",
		Entry: true,
		OnEnter: map[string]interface{}{
			"inject": opts.WorkingInject,
		},
	}
	addStageLabels(&working, "remove_labels", opts.TriggerLabels)
	addStageLabels(&working, "add_labels", []string{opts.WorkingLabel})
	addMoveIssue(&working, opts.WorkingStatus)

	stages := []types.WorkflowStage{working}
	if opts.IncludePreCommit {
		stages = append(stages, preCommitStage(opts.ReadySignal, opts.PreCommitCommand))
	}

	prOpened := types.WorkflowStage{
		ID:    "pr_opened",
		Label: "PR opened",
		Triggers: []map[string]interface{}{
			{"message_contains": "[DONE]"},
		},
		OnEnter: map[string]interface{}{
			"inject": "Pull request detected. ElasticClaw will watch it for merge or close.",
		},
	}
	addStageLabels(&prOpened, "add_labels", []string{opts.ReviewLabel})
	addStageLabels(&prOpened, "remove_labels", []string{opts.WorkingLabel})
	addMoveIssue(&prOpened, opts.PROpenedStatus)
	stages = append(stages, prOpened)

	merged := types.WorkflowStage{
		ID:       "merged",
		Label:    "Merged",
		Terminal: true,
		Triggers: []map[string]interface{}{
			{"pr_merged": nil},
		},
		OnEnter: map[string]interface{}{},
	}
	addStageLabels(&merged, "add_labels", []string{opts.DoneLabel})
	if opts.MoveIssueOnTerminals {
		addMoveIssue(&merged, opts.MergedStatus)
	} else {
		merged.OnEnter["close_issue"] = true
	}
	if len(merged.OnEnter) == 0 {
		merged.OnEnter = nil
	}
	stages = append(stages, merged)

	closedNoMerge := types.WorkflowStage{
		ID:       "closed_no_merge",
		Label:    "Closed without merge",
		Terminal: true,
		Triggers: []map[string]interface{}{
			{"pr_closed": nil},
		},
		OnEnter: map[string]interface{}{},
	}
	addStageLabels(&closedNoMerge, "add_labels", []string{opts.ClosedLabel})
	if strings.TrimSpace(opts.ClosedLabel) != "" {
		closedNoMerge.OnEnter["inject"] = "Pull request closed without merging. Review the issue and decide whether it should be retried or handled manually."
	}
	if opts.MoveIssueOnTerminals {
		addMoveIssue(&closedNoMerge, opts.ClosedNoMergeStatus)
	}
	if len(closedNoMerge.OnEnter) == 0 {
		closedNoMerge.OnEnter = nil
	}
	stages = append(stages, closedNoMerge)

	return stages
}

type manualStageOptions struct {
	IncludePreCommit bool
	PreCommitCommand string
	ReadySignal      string
	DoneSignal       string
}

func manualTaskStages(opts manualStageOptions) []types.WorkflowStage {
	working := types.WorkflowStage{
		ID:    "working",
		Label: "Working",
		Entry: true,
		OnEnter: map[string]interface{}{
			"inject": "Complete the manual task. Reply with " + opts.DoneSignal + " when finished.",
		},
	}
	stages := []types.WorkflowStage{working}
	if opts.IncludePreCommit {
		stages = append(stages, preCommitStage(opts.ReadySignal, opts.PreCommitCommand))
	}
	stages = append(stages, types.WorkflowStage{
		ID:       "complete",
		Label:    "Complete",
		Terminal: true,
		Triggers: []map[string]interface{}{
			{"message_contains": opts.DoneSignal},
		},
		OnEnter: map[string]interface{}{
			"inject": "Workflow complete.",
		},
	})
	return stages
}

func preCommitStage(readySignal string, command string) types.WorkflowStage {
	return types.WorkflowStage{
		ID:    "pre_commit",
		Label: "Pre-commit",
		Triggers: []map[string]interface{}{
			{"message_contains": readySignal},
		},
		OnEnter: map[string]interface{}{
			"run": map[string]interface{}{
				"command": command,
			},
			"inject": "Pre-commit checks passed. Open a pull request and reply with [DONE] and the PR URL.",
		},
	}
}

func addMoveIssue(stage *types.WorkflowStage, status string) {
	if strings.TrimSpace(status) == "" {
		return
	}
	if stage.OnEnter == nil {
		stage.OnEnter = map[string]interface{}{}
	}
	stage.OnEnter["move_issue"] = map[string]interface{}{
		"status": strings.TrimSpace(status),
	}
}

func addStageLabels(stage *types.WorkflowStage, action string, labels []string) {
	labels = cleanStrings(labels)
	if len(labels) == 0 {
		return
	}
	if stage.OnEnter == nil {
		stage.OnEnter = map[string]interface{}{}
	}
	stage.OnEnter[action] = labels
}

func stringConfig(config map[string]interface{}, key string, fallback string) string {
	if config == nil {
		return fallback
	}
	raw, ok := config[key]
	if !ok || raw == nil {
		return fallback
	}
	switch value := raw.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			return fallback
		}
		return strings.TrimSpace(value)
	case fmt.Stringer:
		return strings.TrimSpace(value.String())
	default:
		rendered := strings.TrimSpace(fmt.Sprint(value))
		if rendered == "" {
			return fallback
		}
		return rendered
	}
}

func boolConfig(config map[string]interface{}, key string, fallback bool) bool {
	if config == nil {
		return fallback
	}
	raw, ok := config[key]
	if !ok || raw == nil {
		return fallback
	}
	switch value := raw.(type) {
	case bool:
		return value
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(value))
		if err == nil {
			return parsed
		}
	}
	return fallback
}

func stringListConfig(config map[string]interface{}, key string, fallback []string) []string {
	if config == nil {
		return append([]string(nil), fallback...)
	}
	raw, ok := config[key]
	if !ok || raw == nil {
		return append([]string(nil), fallback...)
	}
	values := stringList(raw)
	if len(values) == 0 {
		return append([]string(nil), fallback...)
	}
	return values
}

func stringList(raw interface{}) []string {
	switch value := raw.(type) {
	case []string:
		return cleanStrings(value)
	case []interface{}:
		values := make([]string, 0, len(value))
		for _, item := range value {
			text := strings.TrimSpace(fmt.Sprint(item))
			if text != "" {
				values = append(values, text)
			}
		}
		return values
	case string:
		if strings.TrimSpace(value) == "" {
			return nil
		}
		parts := strings.Split(value, ",")
		values := make([]string, 0, len(parts))
		for _, part := range parts {
			text := strings.TrimSpace(part)
			if text != "" {
				values = append(values, text)
			}
		}
		return values
	default:
		return nil
	}
}

func cleanStrings(values []string) []string {
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			cleaned = append(cleaned, value)
		}
	}
	return cleaned
}

func inputsConfig(config map[string]interface{}, key string, fallback []types.FactoryInput) ([]types.FactoryInput, error) {
	if config == nil {
		return cloneInputs(fallback), nil
	}
	raw, ok := config[key]
	if !ok || raw == nil {
		return cloneInputs(fallback), nil
	}
	switch value := raw.(type) {
	case []types.FactoryInput:
		return cloneInputs(value), nil
	case []interface{}:
		inputs := make([]types.FactoryInput, 0, len(value))
		for i, item := range value {
			input, err := inputFromConfig(item)
			if err != nil {
				return nil, fmt.Errorf("config.%s[%d]: %w", key, i, err)
			}
			inputs = append(inputs, input)
		}
		return inputs, nil
	default:
		return nil, fmt.Errorf("config.%s must be a list of inputs", key)
	}
}

func inputFromConfig(raw interface{}) (types.FactoryInput, error) {
	switch value := raw.(type) {
	case types.FactoryInput:
		return value, nil
	case map[string]interface{}:
		input := types.FactoryInput{
			Name:        strings.TrimSpace(fmt.Sprint(value["name"])),
			Type:        strings.TrimSpace(fmt.Sprint(value["type"])),
			Required:    boolFromRaw(value["required"], false),
			Default:     stringFromRaw(value["default"]),
			Description: stringFromRaw(value["description"]),
			Options:     stringList(value["options"]),
			Validation:  stringFromRaw(value["validation"]),
			Min:         floatPtrFromRaw(value["min"]),
			Max:         floatPtrFromRaw(value["max"]),
		}
		return input, nil
	default:
		return types.FactoryInput{}, fmt.Errorf("input must be an object")
	}
}

func cloneInputs(inputs []types.FactoryInput) []types.FactoryInput {
	cloned := make([]types.FactoryInput, len(inputs))
	for i, input := range inputs {
		cloned[i] = input
		cloned[i].Options = append([]string(nil), input.Options...)
		if input.Min != nil {
			min := *input.Min
			cloned[i].Min = &min
		}
		if input.Max != nil {
			max := *input.Max
			cloned[i].Max = &max
		}
	}
	return cloned
}

func stringFromRaw(raw interface{}) string {
	if raw == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(raw))
}

func boolFromRaw(raw interface{}, fallback bool) bool {
	switch value := raw.(type) {
	case bool:
		return value
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(value))
		if err == nil {
			return parsed
		}
	}
	return fallback
}

func floatPtrFromRaw(raw interface{}) *float64 {
	if raw == nil {
		return nil
	}
	var parsed float64
	switch value := raw.(type) {
	case int:
		parsed = float64(value)
	case int64:
		parsed = float64(value)
	case float64:
		parsed = value
	case float32:
		parsed = float64(value)
	case string:
		var err error
		parsed, err = strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return nil
		}
	default:
		return nil
	}
	return &parsed
}
