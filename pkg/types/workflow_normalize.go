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
		default:
			switch workflow.Trigger.Type {
			case "github_issues":
				workflow.Integration = "github-issues"
				workflow.TriggerRepos = append([]string(nil), workflow.Trigger.Repositories...)
				workflow.Labels = append([]string(nil), workflow.Trigger.Labels...)
				workflow.AllowedLabelers = nil
				for _, labeler := range workflow.Trigger.Labelers {
					if labeler == "*" {
						continue
					}
					workflow.AllowedLabelers = append(workflow.AllowedLabelers, labeler)
				}
				if len(workflow.Trigger.States) > 0 {
					workflow.TriggerStatus = workflow.Trigger.States[0]
				} else if len(workflow.Trigger.Labels) > 0 {
					workflow.TriggerStatus = workflow.Trigger.Labels[0]
				}
			case "linear":
				workflow.Integration = "linear"
				workflow.Workspace = workflow.Trigger.Workspace
				workflow.Team = workflow.Trigger.Team
				workflow.Labels = append([]string(nil), workflow.Trigger.Labels...)
				workflow.AssignedTo = workflow.Trigger.AssignedTo
				if len(workflow.Trigger.States) > 0 {
					workflow.TriggerStatus = workflow.Trigger.States[0]
				}
			}
		}
	}
	if len(workflow.Jobs) > 0 {
		data, err := yaml.Marshal(struct {
			Stages []WorkflowJob `yaml:"stages"`
		}{Stages: workflow.Jobs})
		if err != nil {
			return err
		}
		workflow.PipelineYAML = string(data)
	}
	return nil
}
