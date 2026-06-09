package workflowsetup

import (
	"fmt"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func validateTriggerOverlapReadiness(env Environment, workspaceName string, workflow *types.WorkflowConfig) []Diagnostic {
	if workflow == nil || workflow.Trigger == nil || strings.TrimSpace(workspaceName) == "" {
		return nil
	}

	workspace, err := env.LoadWorkspace(workspaceName)
	if err != nil {
		return []Diagnostic{readinessWarningDiagnostic(
			"readiness-trigger-overlap-not-checked",
			"readiness",
			"workflow.trigger",
			"Existing workflows could not be loaded",
			err.Error(),
		)}
	}
	if workspace == nil || len(workspace.Workflows) == 0 {
		return nil
	}

	var checks []Diagnostic
	for _, existing := range workspace.Workflows {
		existing = loadComparableWorkflow(env, workspaceName, existing)
		if existing == nil || !workflowIsEnabled(existing) || sameWorkflow(workflow, existing) {
			continue
		}
		if githubIssuesTriggerOverlap(workflow.Trigger.GitHubIssues, existing.Trigger.GitHubIssues) {
			checks = append(checks, triggerOverlapDiagnostic("workflow.trigger.github_issues", "GitHub Issues", existing.Name))
		}
		if linearTriggerOverlap(workflow.Trigger.Linear, existing.Trigger.Linear) {
			checks = append(checks, triggerOverlapDiagnostic("workflow.trigger.linear", "Linear", existing.Name))
		}
		if shortcutTriggerOverlap(workflow.Trigger.Shortcut, existing.Trigger.Shortcut) {
			checks = append(checks, triggerOverlapDiagnostic("workflow.trigger.shortcut", "Shortcut", existing.Name))
		}
	}
	return checks
}

func triggerOverlapDiagnostic(fieldPath, source, workflowName string) Diagnostic {
	return readinessWarningDiagnostic(
		"readiness-trigger-overlap",
		"readiness",
		fieldPath,
		"Workflow trigger overlaps an enabled workflow",
		fmt.Sprintf("This %s trigger may match the same events as enabled workflow %q.", source, workflowName),
	)
}

func loadComparableWorkflow(env Environment, workspaceName string, workflow *types.WorkflowConfig) *types.WorkflowConfig {
	if workflow == nil {
		return nil
	}

	if strings.TrimSpace(workflow.RawConfig) != "" {
		if parsed, err := parseWorkflowConfig(workflow.RawConfig); err == nil {
			workflow = parsed
		}
	} else if strings.TrimSpace(workflow.Name) != "" {
		if raw, err := env.LoadWorkflowRaw(workspaceName, workflow.Name); err == nil && strings.TrimSpace(raw) != "" {
			if parsed, parseErr := parseWorkflowConfig(raw); parseErr == nil {
				workflow = parsed
			}
		}
	}

	workflow = cloneWorkflowConfig(workflow)
	if workflow == nil {
		return nil
	}
	if err := types.NormalizeWorkflowConfig(workflow); err != nil {
		return nil
	}
	if workflow.Trigger == nil {
		return nil
	}
	return workflow
}

func workflowIsEnabled(workflow *types.WorkflowConfig) bool {
	return workflow != nil && (workflow.Enabled == nil || *workflow.Enabled)
}

func sameWorkflow(a, b *types.WorkflowConfig) bool {
	if a == nil || b == nil {
		return false
	}
	return strings.TrimSpace(a.Name) != "" && strings.TrimSpace(a.Name) == strings.TrimSpace(b.Name)
}

func githubIssuesTriggerOverlap(a, b *types.GitHubIssuesWorkflowTrigger) bool {
	if a == nil || b == nil {
		return false
	}
	if normalizedStringValuesDisjoint(a.Event, b.Event, normalizeGitHubIssuesEvent) {
		return false
	}
	if stringSetsDisjoint(a.States, b.States) {
		return false
	}
	if stringSetsDisjoint(a.Labels, b.Labels) {
		return false
	}
	if repoSelectorsDisjoint(a.Repositories, b.Repositories) {
		return false
	}
	if githubIssuesLabelersCanProveDisjoint(a.Event, b.Event) && stringSelectorsDisjoint(a.Labelers, b.Labelers) {
		return false
	}
	if assignedToFiltersDisjoint(a.AssignedTo, b.AssignedTo) {
		return false
	}
	return true
}

func linearTriggerOverlap(a, b *types.LinearWorkflowTrigger) bool {
	if a == nil || b == nil {
		return false
	}
	if normalizedStringValuesDisjoint(a.Event, b.Event, normalizeLinearEvent) {
		return false
	}
	if stringValuesDisjoint(a.Workspace, b.Workspace) {
		return false
	}
	if stringValuesDisjoint(a.Team, b.Team) {
		return false
	}
	if stringSetsDisjoint(a.States, b.States) {
		return false
	}
	if stringSetsDisjoint(a.Labels, b.Labels) {
		return false
	}
	if assignedToFiltersDisjoint(a.AssignedTo, b.AssignedTo) {
		return false
	}
	return true
}

func shortcutTriggerOverlap(a, b *types.ShortcutWorkflowTrigger) bool {
	if a == nil || b == nil {
		return false
	}
	if normalizedStringValuesDisjoint(a.Event, b.Event, normalizeShortcutEvent) {
		return false
	}
	if stringValuesDisjoint(a.Workspace, b.Workspace) {
		return false
	}
	if stringSetsDisjoint(a.States, b.States) {
		return false
	}
	if stringSetsDisjoint(a.Labels, b.Labels) {
		return false
	}
	if assignedToFiltersDisjoint(a.AssignedTo, b.AssignedTo) {
		return false
	}
	return true
}

func stringValuesDisjoint(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	return a != "" && b != "" && a != b
}

func normalizedStringValuesDisjoint(a, b string, normalize func(string) string) bool {
	a = normalize(a)
	b = normalize(b)
	return a != "" && b != "" && a != b
}

func normalizeGitHubIssuesEvent(event string) string {
	event = strings.ToLower(strings.TrimSpace(event))
	switch event {
	case "labeled", "issue_labeled":
		return "issue_labeled"
	case "unlabeled", "issue_unlabeled":
		return "issue_unlabeled"
	default:
		return event
	}
}

func githubIssuesLabelersCanProveDisjoint(aEvent, bEvent string) bool {
	return githubIssuesLabelerEvent(aEvent) && githubIssuesLabelerEvent(bEvent)
}

func githubIssuesLabelerEvent(event string) bool {
	switch normalizeGitHubIssuesEvent(event) {
	case "issue_labeled", "issue_unlabeled":
		return true
	default:
		return false
	}
}

func normalizeLinearEvent(event string) string {
	event = strings.ToLower(strings.TrimSpace(event))
	switch event {
	case "status", "status_changed", "issue_status_changed":
		return "status_changed"
	default:
		return event
	}
}

func normalizeShortcutEvent(event string) string {
	event = strings.ToLower(strings.TrimSpace(event))
	switch event {
	case "status", "status_changed", "story_status_changed":
		return "status_changed"
	default:
		return event
	}
}

func stringSetsDisjoint(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, value := range a {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			seen[value] = true
		}
	}
	if len(seen) == 0 {
		return false
	}
	foundComparable := false
	for _, value := range b {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		foundComparable = true
		if seen[value] {
			return false
		}
	}
	return foundComparable
}

func stringSelectorsDisjoint(a, b []string) bool {
	left := normalizedStringSet(a)
	right := normalizedStringSet(b)
	if len(left) == 0 || len(right) == 0 {
		return false
	}
	if left["*"] || right["*"] {
		return false
	}
	for value := range left {
		if right[value] {
			return false
		}
	}
	return true
}

func normalizedStringSet(values []string) map[string]bool {
	normalized := map[string]bool{}
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			normalized[value] = true
		}
	}
	return normalized
}

type assignedToFilter struct {
	kind  string
	value string
}

func assignedToFiltersDisjoint(a, b string) bool {
	left := parseAssignedToFilter(a)
	right := parseAssignedToFilter(b)
	if left.kind == "" || right.kind == "" {
		return false
	}
	if (left.kind == "any" && right.kind == "none") || (left.kind == "none" && right.kind == "any") {
		return true
	}
	if left.kind == "none" && right.kind == "include" {
		return true
	}
	if left.kind == "include" && right.kind == "none" {
		return true
	}
	if left.kind == "include" && right.kind == "include" {
		return left.value != right.value
	}
	if left.kind == "include" && right.kind == "exclude" {
		return left.value == right.value
	}
	if left.kind == "exclude" && right.kind == "include" {
		return left.value == right.value
	}
	return false
}

func parseAssignedToFilter(value string) assignedToFilter {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return assignedToFilter{}
	}
	switch value {
	case "any", "none":
		return assignedToFilter{kind: value}
	}
	if strings.HasPrefix(value, "!") {
		assignee := strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(value, "!")), "@")
		if assignee == "" {
			return assignedToFilter{}
		}
		return assignedToFilter{kind: "exclude", value: assignee}
	}
	return assignedToFilter{kind: "include", value: strings.TrimPrefix(value, "@")}
}

func repoSelectorsDisjoint(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	for _, left := range a {
		for _, right := range b {
			if repoSelectorsOverlap(left, right) {
				return false
			}
		}
	}
	return true
}

func repoSelectorsOverlap(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return true
	}
	if a == b {
		return true
	}
	aOwner, aRepo := splitRepoSelector(a)
	bOwner, bRepo := splitRepoSelector(b)
	if aOwner == "" || bOwner == "" || aOwner != bOwner {
		return false
	}
	return aRepo == "*" || bRepo == "*"
}

func splitRepoSelector(selector string) (string, string) {
	parts := strings.SplitN(selector, "/", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}
