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
