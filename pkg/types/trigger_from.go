package types

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ParseTriggerFrom decodes a v1 trigger's optional source-stage restriction.
// Call only when the from key is present; empty restrictions are invalid.
func ParseTriggerFrom(raw interface{}) ([]string, error) {
	var sources []string
	switch value := raw.(type) {
	case string:
		sources = []string{value}
	case []string:
		sources = value
	case []interface{}:
		for _, item := range value {
			source, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("trigger from must be a stage ID or a list of stage IDs")
			}
			sources = append(sources, source)
		}
	default:
		return nil, fmt.Errorf("trigger from must be a stage ID or a list of stage IDs")
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("trigger from must not be empty")
	}
	for _, source := range sources {
		if strings.TrimSpace(source) == "" {
			return nil, fmt.Errorf("trigger from stage ID must not be blank")
		}
	}
	return sources, nil
}

func validateWorkflowTriggerSources(stages []WorkflowStage, pipelineYAML string) error {
	if len(stages) == 0 && pipelineYAML != "" {
		var legacy struct {
			Stages []WorkflowStage `yaml:"stages"`
		}
		if err := yaml.Unmarshal([]byte(pipelineYAML), &legacy); err != nil {
			return fmt.Errorf("pipeline YAML: %w", err)
		}
		stages = legacy.Stages
	}
	stageIDs := make(map[string]bool, len(stages))
	for _, stage := range stages {
		stageIDs[stage.ID] = true
	}
	for _, stage := range stages {
		for i, trigger := range stage.Triggers {
			raw, present := trigger["from"]
			if !present {
				continue
			}
			sources, err := ParseTriggerFrom(raw)
			if err != nil {
				return fmt.Errorf("stage %q trigger[%d]: %w", stage.ID, i, err)
			}
			for _, source := range sources {
				if !stageIDs[source] {
					return fmt.Errorf("stage %q trigger[%d] from: unknown stage %q", stage.ID, i, source)
				}
			}
		}
	}
	return nil
}
