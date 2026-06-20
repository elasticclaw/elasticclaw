package types

import "gopkg.in/yaml.v3"

// NormalizeWorkflowConfig derives the legacy runtime fields still used by the
// hub from the v1 workflow schema. It keeps the authored YAML as the source of
// truth while the runtime migrates to the new shape.
func NormalizeWorkflowConfig(workflow *WorkflowConfig) error {
	if workflow == nil {
		return nil
	}
	if workflow.SchemaVersion == "" {
		workflow.SchemaVersion = "v1"
	}
	if workflow.Trigger != nil {
		switch {
		case workflow.Trigger.GitHubIssues != nil:
			workflow.Integration = "github-issues"
			workflow.TriggerRepos = append([]string(nil), workflow.Trigger.GitHubIssues.Repositories...)
			workflow.Labels = append([]string(nil), workflow.Trigger.GitHubIssues.Labels...)
			workflow.AssignedTo = workflow.Trigger.GitHubIssues.AssignedTo
			workflow.AllowedLabelers = nil
			for _, labeler := range workflow.Trigger.GitHubIssues.Labelers {
				if labeler == "*" {
					continue
				}
				workflow.AllowedLabelers = append(workflow.AllowedLabelers, labeler)
			}
			if len(workflow.Trigger.GitHubIssues.States) > 0 {
				workflow.TriggerStatus = workflow.Trigger.GitHubIssues.States[0]
			} else if len(workflow.Trigger.GitHubIssues.Labels) > 0 {
				workflow.TriggerStatus = workflow.Trigger.GitHubIssues.Labels[0]
			}
		case workflow.Trigger.Linear != nil:
			workflow.Integration = "linear"
			workflow.Workspace = workflow.Trigger.Linear.Workspace
			workflow.Team = workflow.Trigger.Linear.Team
			workflow.Labels = append([]string(nil), workflow.Trigger.Linear.Labels...)
			workflow.AssignedTo = workflow.Trigger.Linear.AssignedTo
			if len(workflow.Trigger.Linear.States) > 0 {
				workflow.TriggerStatus = workflow.Trigger.Linear.States[0]
			}
		case workflow.Trigger.Shortcut != nil:
			workflow.Integration = "shortcut"
			workflow.Workspace = workflow.Trigger.Shortcut.Workspace
			workflow.Labels = append([]string(nil), workflow.Trigger.Shortcut.Labels...)
			workflow.AssignedTo = workflow.Trigger.Shortcut.AssignedTo
			if len(workflow.Trigger.Shortcut.States) > 0 {
				workflow.TriggerStatus = workflow.Trigger.Shortcut.States[0]
			}
		case workflow.Trigger.Jira != nil:
			workflow.Integration = "jira"
			workflow.Workspace = workflow.Trigger.Jira.Workspace
			workflow.Labels = append([]string(nil), workflow.Trigger.Jira.Labels...)
			workflow.AssignedTo = workflow.Trigger.Jira.AssignedTo
			if len(workflow.Trigger.Jira.Projects) > 0 {
				workflow.TriggerRepos = append([]string(nil), workflow.Trigger.Jira.Projects...)
			}
			if len(workflow.Trigger.Jira.States) > 0 {
				workflow.TriggerStatus = workflow.Trigger.Jira.States[0]
			}
		case workflow.Trigger.Cron != nil:
			workflow.Integration = "cron"
		}
	}
	if len(workflow.Stages) > 0 {
		data, err := yaml.Marshal(struct {
			Stages []WorkflowStage `yaml:"stages"`
		}{Stages: workflow.Stages})
		if err != nil {
			return err
		}
		workflow.PipelineYAML = string(data)
	}
	return nil
}
