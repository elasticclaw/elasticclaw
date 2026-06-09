package workflowsetup

const (
	PatternGitHubIssue    = "github-issue"
	PatternLinearStatus   = "linear-status"
	PatternShortcutStatus = "shortcut-status"
	PatternManualTask     = "manual-task"
)

var builtInPatterns = []PatternMetadata{
	{
		ID:          PatternGitHubIssue,
		Label:       "GitHub issue",
		Description: "Create workflow runs from labeled GitHub issues, with optional manual issue-number triggers.",
		RequiredFields: []PatternField{
			{Path: "config.repository", Label: "Repository", Description: "GitHub repository to watch, in owner/repo format."},
		},
		AdvancedFields: []PatternField{
			{Path: "config.event", Label: "Event", Description: "GitHub issue event that starts the workflow."},
			{Path: "config.labels", Label: "Labels", Description: "Issue labels required to start the workflow."},
			{Path: "config.triggerLabel", Label: "Trigger label", Description: "Issue label removed when work starts; used as the default trigger label when labels are unset."},
			{Path: "config.workingLabel", Label: "Working label", Description: "Issue label added when work starts."},
			{Path: "config.reviewLabel", Label: "Review label", Description: "Issue label added when a pull request is opened."},
			{Path: "config.doneLabel", Label: "Done label", Description: "Issue label added when the pull request merges."},
			{Path: "config.closedLabel", Label: "Closed label", Description: "Issue label added when the pull request closes without merging."},
			{Path: "config.states", Label: "States", Description: "Issue states allowed to start the workflow."},
			{Path: "config.labelers", Label: "Labelers", Description: "GitHub users allowed to trigger by labeling."},
			{Path: "config.enableManualTrigger", Label: "Manual trigger", Description: "Allow the workflow to be started manually with an issue number."},
			{Path: "config.concurrencyGroup", Label: "Concurrency group", Description: "Concurrency group assigned to runs from this workflow."},
			{Path: "config.includePreCommit", Label: "Pre-commit stage", Description: "Add a command stage before PR handoff."},
		},
		Defaults: map[string]interface{}{
			"event":               "issue_labeled",
			"labels":              []string{"agent-ready"},
			"triggerLabel":        "agent-ready",
			"workingLabel":        "agent-working",
			"reviewLabel":         "agent-review",
			"doneLabel":           "agent-done",
			"closedLabel":         "agent-needs-attention",
			"states":              []string{"open"},
			"labelers":            []string{"*"},
			"enableManualTrigger": true,
			"concurrencyGroup":    "global",
			"includePreCommit":    false,
		},
		ValidationFieldPaths: []string{
			"workflow.name",
			"trigger.github_issues.repositories",
			"trigger.github_issues.event",
			"inputs.issue_number",
		},
	},
	{
		ID:          PatternLinearStatus,
		Label:       "Linear status",
		Description: "Create workflow runs from Linear status changes and move issues through PR lifecycle states.",
		RequiredFields: []PatternField{
			{Path: "config.workspace", Label: "Workspace", Description: "Linear integration workspace name."},
			{Path: "config.triggerStatus", Label: "Trigger status", Description: "Linear status that starts the workflow."},
		},
		AdvancedFields: []PatternField{
			{Path: "config.team", Label: "Team", Description: "Optional Linear team key filter."},
			{Path: "config.workingStatus", Label: "Working status", Description: "Status to move the issue to when work starts."},
			{Path: "config.prOpenedStatus", Label: "PR opened status", Description: "Status to move the issue to when a PR is opened."},
			{Path: "config.mergedStatus", Label: "Merged status", Description: "Status to move the issue to when the PR merges."},
			{Path: "config.closedNoMergeStatus", Label: "Closed without merge status", Description: "Status to move the issue to when the PR closes without merging."},
			{Path: "config.concurrencyGroup", Label: "Concurrency group", Description: "Concurrency group assigned to runs from this workflow."},
			{Path: "config.includePreCommit", Label: "Pre-commit stage", Description: "Add a command stage before PR handoff."},
		},
		Defaults: map[string]interface{}{
			"event":            "status_changed",
			"triggerStatus":    "Ready for Agent",
			"concurrencyGroup": "global",
			"includePreCommit": false,
		},
		ValidationFieldPaths: []string{
			"workflow.name",
			"trigger.linear.workspace",
			"trigger.linear.states",
			"stages.on_enter.move_issue",
		},
	},
	{
		ID:          PatternShortcutStatus,
		Label:       "Shortcut status",
		Description: "Create workflow runs from Shortcut story state changes and move stories through PR lifecycle states.",
		RequiredFields: []PatternField{
			{Path: "config.workspace", Label: "Workspace", Description: "Shortcut integration workspace name."},
			{Path: "config.triggerStatus", Label: "Trigger state", Description: "Shortcut workflow state that starts the workflow."},
		},
		AdvancedFields: []PatternField{
			{Path: "config.workingStatus", Label: "Working state", Description: "State to move the story to when work starts."},
			{Path: "config.prOpenedStatus", Label: "PR opened state", Description: "State to move the story to when a PR is opened."},
			{Path: "config.mergedStatus", Label: "Merged state", Description: "State to move the story to when the PR merges."},
			{Path: "config.closedNoMergeStatus", Label: "Closed without merge state", Description: "State to move the story to when the PR closes without merging."},
			{Path: "config.concurrencyGroup", Label: "Concurrency group", Description: "Concurrency group assigned to runs from this workflow."},
			{Path: "config.includePreCommit", Label: "Pre-commit stage", Description: "Add a command stage before PR handoff."},
		},
		Defaults: map[string]interface{}{
			"event":            "status_changed",
			"triggerStatus":    "Ready for Agent",
			"concurrencyGroup": "global",
			"includePreCommit": false,
		},
		ValidationFieldPaths: []string{
			"workflow.name",
			"trigger.shortcut.workspace",
			"trigger.shortcut.states",
			"stages.on_enter.move_issue",
		},
	},
	{
		ID:          PatternManualTask,
		Label:       "Manual task",
		Description: "Create a manually triggered workflow with configurable inputs and completion stages.",
		RequiredFields: []PatternField{
			{Path: "config.inputs", Label: "Inputs", Description: "Manual trigger inputs shown before starting the workflow."},
		},
		AdvancedFields: []PatternField{
			{Path: "config.concurrencyGroup", Label: "Concurrency group", Description: "Concurrency group assigned to runs from this workflow."},
			{Path: "config.includePreCommit", Label: "Pre-commit stage", Description: "Add a command stage before completion."},
			{Path: "config.preCommitCommand", Label: "Pre-commit command", Description: "Command to run in the pre-commit stage."},
			{Path: "config.doneSignal", Label: "Done signal", Description: "Message token that marks the workflow complete."},
		},
		Defaults: map[string]interface{}{
			"enableManualTrigger": true,
			"concurrencyGroup":    "global",
			"includePreCommit":    false,
			"doneSignal":          "[DONE]",
			"inputs": []map[string]interface{}{
				{
					"name":        "task",
					"type":        "string",
					"required":    true,
					"description": "Task to complete",
				},
			},
		},
		ValidationFieldPaths: []string{
			"workflow.name",
			"enable_manual_trigger",
			"inputs",
			"stages",
		},
	},
}

// Patterns lists the built-in workflow setup patterns supported by the backend.
func Patterns() []PatternMetadata {
	patterns := make([]PatternMetadata, len(builtInPatterns))
	for i, pattern := range builtInPatterns {
		patterns[i] = copyPatternMetadata(pattern)
	}
	return patterns
}

func patternByID(id string) (PatternMetadata, bool) {
	for _, pattern := range builtInPatterns {
		if pattern.ID == id {
			return copyPatternMetadata(pattern), true
		}
	}
	return PatternMetadata{}, false
}

func copyPatternMetadata(pattern PatternMetadata) PatternMetadata {
	copied := pattern
	copied.RequiredFields = append([]PatternField(nil), pattern.RequiredFields...)
	copied.AdvancedFields = append([]PatternField(nil), pattern.AdvancedFields...)
	copied.ValidationFieldPaths = append([]string(nil), pattern.ValidationFieldPaths...)
	copied.Defaults = make(map[string]interface{}, len(pattern.Defaults))
	for key, value := range pattern.Defaults {
		copied.Defaults[key] = value
	}
	return copied
}
