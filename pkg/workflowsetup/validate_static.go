package workflowsetup

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/elasticclaw/elasticclaw/pkg/workflow/pipeline"
	"gopkg.in/yaml.v3"
)

// ValidateStatic validates workspace and workflow YAML without mutating disk.
func ValidateStatic(req ValidateRequest) ValidateResponse {
	var checks []Diagnostic

	workspace, err := ParseWorkspaceConfig(req.WorkspaceConfig)
	if err != nil {
		checks = append(checks, criticalDiagnostic(
			"workspace-yaml-invalid",
			"workspace",
			"workspace.config",
			"Workspace YAML is invalid",
			err.Error(),
		))
	}

	workflow, err := parseWorkflowConfig(req.Config)
	if err != nil {
		checks = append(checks, criticalDiagnostic(
			"workflow-yaml-invalid",
			"workflow",
			"workflow.config",
			"Workflow YAML is invalid",
			err.Error(),
		))
		return validateResponse(req.Config, checks)
	}

	checks = append(checks, validateWorkspaceSemantics(workspace)...)
	checks = append(checks, validateWorkflowConfig(workflow)...)

	return validateResponse(req.Config, checks)
}

// ValidateWorkflowConfigStatic validates a workflow object without mutating it.
func ValidateWorkflowConfigStatic(workflow *types.WorkflowConfig, workspace *ParsedWorkspaceConfig) ValidateResponse {
	checks := validateWorkspaceSemantics(workspace)
	checks = append(checks, validateWorkflowConfig(workflow)...)
	hashInput := ""
	if workflow != nil {
		hashInput = workflow.RawConfig
	}
	return validateResponse(hashInput, checks)
}

func parseWorkflowConfig(raw string) (*types.WorkflowConfig, error) {
	workflow := &types.WorkflowConfig{}
	dec := yaml.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(workflow); err != nil && err != io.EOF {
		return nil, fmt.Errorf("invalid workflow config: %w", err)
	}
	workflow.RawConfig = raw
	return workflow, nil
}

func validateWorkspaceSemantics(workspace *ParsedWorkspaceConfig) []Diagnostic {
	if workspace == nil || workspace.Workspace == nil {
		return nil
	}

	var checks []Diagnostic
	if workspace.Workspace.Name != "" {
		if err := ValidateSlug(workspace.Workspace.Name); err != nil {
			checks = append(checks, criticalDiagnostic(
				"workspace-name-invalid",
				"workspace",
				"workspace.name",
				"Workspace name is invalid",
				err.Error(),
			))
		}
	}
	if err := workspace.Workspace.Validate(); err != nil {
		checks = append(checks, criticalDiagnostic(
			"workspace-config-invalid",
			"workspace",
			"workspace",
			"Workspace config is invalid",
			err.Error(),
		))
	}
	return checks
}

func validateWorkflowConfig(workflow *types.WorkflowConfig) []Diagnostic {
	if workflow == nil {
		return []Diagnostic{criticalDiagnostic(
			"workflow-config-invalid",
			"workflow",
			"workflow",
			"Workflow config is missing",
			"Provide workflow YAML before validating.",
		)}
	}

	effective := cloneWorkflowConfig(workflow)
	if err := types.NormalizeWorkflowConfig(effective); err != nil {
		return []Diagnostic{criticalDiagnostic(
			"workflow-normalize-invalid",
			"workflow",
			"workflow",
			"Workflow normalization failed",
			err.Error(),
		)}
	}

	var checks []Diagnostic
	checks = append(checks, validateWorkflowNames(effective)...)
	checks = append(checks, validateWorkflowTriggerCount(effective)...)
	checks = append(checks, validateManualInputs(effective)...)
	checks = append(checks, validateGitHubIssueManualInput(effective)...)

	if err := effective.Validate(); err != nil {
		checks = append(checks, criticalDiagnostic(
			"workflow-schema-invalid",
			"workflow",
			"workflow",
			"Workflow schema is invalid",
			err.Error(),
		))
	}

	parsedPipeline, err := pipeline.Parse([]byte(effective.PipelineYAML))
	if err != nil {
		checks = append(checks, criticalDiagnostic(
			"pipeline-yaml-invalid",
			"pipeline",
			"workflow.pipeline_yaml",
			"Pipeline YAML is invalid",
			err.Error(),
		))
		return checks
	}
	checks = append(checks, validatePipeline(effective, parsedPipeline)...)

	return checks
}

func validateWorkflowNames(workflow *types.WorkflowConfig) []Diagnostic {
	var checks []Diagnostic
	if strings.TrimSpace(workflow.Name) != "" {
		if err := ValidateSlug(workflow.Name); err != nil {
			checks = append(checks, criticalDiagnostic(
				"workflow-name-invalid",
				"workflow",
				"workflow.name",
				"Workflow name is invalid",
				err.Error(),
			))
		}
	}
	return checks
}

func validateWorkflowTriggerCount(workflow *types.WorkflowConfig) []Diagnostic {
	sourceCount := workflowTriggerSourceCount(workflow.Trigger)
	if sourceCount == 1 {
		return nil
	}
	if sourceCount == 0 && workflow.EnableManualTrigger {
		return nil
	}
	return []Diagnostic{criticalDiagnostic(
		"workflow-trigger-source-count",
		"workflow",
		"workflow.trigger",
		"Workflow trigger must define exactly one source",
		"Define exactly one trigger source, or omit trigger only for a manual-only workflow.",
	)}
}

func workflowTriggerSourceCount(trigger *types.WorkflowTrigger) int {
	if trigger == nil {
		return 0
	}
	count := 0
	if trigger.GitHubIssues != nil {
		count++
	}
	if trigger.Linear != nil {
		count++
	}
	if trigger.Shortcut != nil {
		count++
	}
	return count
}

func validateManualInputs(workflow *types.WorkflowConfig) []Diagnostic {
	var checks []Diagnostic
	seenNames := map[string]int{}
	for i, input := range workflow.Inputs {
		fieldPath := fmt.Sprintf("workflow.inputs[%d]", i)
		name := strings.TrimSpace(input.Name)
		if name == "" {
			checks = append(checks, criticalDiagnostic(
				"workflow-input-name-required",
				"workflow",
				fieldPath+".name",
				"Manual input name is required",
				"Set a stable input name.",
			))
		} else {
			if err := ValidateSlug(name); err != nil {
				checks = append(checks, criticalDiagnostic(
					"workflow-input-name-invalid",
					"workflow",
					fieldPath+".name",
					"Manual input name is invalid",
					err.Error(),
				))
			}
			if prev, ok := seenNames[name]; ok {
				checks = append(checks, criticalDiagnostic(
					"workflow-input-name-duplicate",
					"workflow",
					fieldPath+".name",
					"Manual input name is duplicated",
					fmt.Sprintf("Input name %q is already used at workflow.inputs[%d].", name, prev),
				))
			} else {
				seenNames[name] = i
			}
		}

		switch input.Type {
		case "":
			checks = append(checks, criticalDiagnostic(
				"workflow-input-type-required",
				"workflow",
				fieldPath+".type",
				"Manual input type is required",
				"Set type to string, number, bool, or enum.",
			))
			continue
		case "string", "number", "bool", "enum":
		default:
			checks = append(checks, criticalDiagnostic(
				"workflow-input-type-invalid",
				"workflow",
				fieldPath+".type",
				"Manual input type is invalid",
				"Set type to string, number, bool, or enum.",
			))
			continue
		}

		checks = append(checks, validateManualInputOptions(fieldPath, input)...)
		checks = append(checks, validateManualInputRegex(fieldPath, input)...)
		checks = append(checks, validateManualInputMinMax(fieldPath, input)...)
		checks = append(checks, validateManualInputDefault(fieldPath, input)...)
	}
	return checks
}

func validateManualInputOptions(fieldPath string, input types.FactoryInput) []Diagnostic {
	if input.Type != "enum" {
		return nil
	}
	if len(input.Options) == 0 {
		return []Diagnostic{criticalDiagnostic(
			"workflow-input-options-required",
			"workflow",
			fieldPath+".options",
			"Enum manual input requires options",
			"Add at least one option for this enum input.",
		)}
	}

	var checks []Diagnostic
	seen := map[string]int{}
	for i, option := range input.Options {
		if strings.TrimSpace(option) == "" {
			checks = append(checks, criticalDiagnostic(
				"workflow-input-option-empty",
				"workflow",
				fmt.Sprintf("%s.options[%d]", fieldPath, i),
				"Enum option cannot be empty",
				"Remove the empty option or replace it with a value.",
			))
			continue
		}
		if prev, ok := seen[option]; ok {
			checks = append(checks, criticalDiagnostic(
				"workflow-input-option-duplicate",
				"workflow",
				fmt.Sprintf("%s.options[%d]", fieldPath, i),
				"Enum option is duplicated",
				fmt.Sprintf("Option %q is already used at %s.options[%d].", option, fieldPath, prev),
			))
		} else {
			seen[option] = i
		}
	}
	return checks
}

func validateManualInputRegex(fieldPath string, input types.FactoryInput) []Diagnostic {
	if input.Validation == "" {
		return nil
	}
	if _, err := regexp.Compile(input.Validation); err != nil {
		return []Diagnostic{criticalDiagnostic(
			"workflow-input-validation-invalid",
			"workflow",
			fieldPath+".validation",
			"Manual input validation regex is invalid",
			err.Error(),
		)}
	}
	return nil
}

func validateManualInputMinMax(fieldPath string, input types.FactoryInput) []Diagnostic {
	if input.Min != nil && input.Max != nil && *input.Min > *input.Max {
		return []Diagnostic{criticalDiagnostic(
			"workflow-input-minmax-invalid",
			"workflow",
			fieldPath+".min",
			"Manual input min is greater than max",
			"Set min less than or equal to max.",
		)}
	}
	if input.Type == "number" {
		return nil
	}
	if input.Min == nil && input.Max == nil {
		return nil
	}
	return []Diagnostic{criticalDiagnostic(
		"workflow-input-minmax-incompatible",
		"workflow",
		fieldPath+".min",
		"Manual input min and max require number type",
		"Remove min and max or change the input type to number.",
	)}
}

func validateManualInputDefault(fieldPath string, input types.FactoryInput) []Diagnostic {
	if input.Default == "" {
		return nil
	}

	switch input.Type {
	case "string":
		if input.Validation != "" {
			pattern, err := regexp.Compile(input.Validation)
			if err == nil && !pattern.MatchString(input.Default) {
				return []Diagnostic{criticalDiagnostic(
					"workflow-input-default-invalid",
					"workflow",
					fieldPath+".default",
					"Manual input default does not match validation",
					"Update the default value or the validation regex.",
				)}
			}
		}
	case "number":
		value, err := strconv.ParseFloat(input.Default, 64)
		if err != nil {
			return []Diagnostic{criticalDiagnostic(
				"workflow-input-default-invalid",
				"workflow",
				fieldPath+".default",
				"Manual input default must be a number",
				err.Error(),
			)}
		}
		if input.Min != nil && value < *input.Min {
			return []Diagnostic{criticalDiagnostic(
				"workflow-input-default-invalid",
				"workflow",
				fieldPath+".default",
				"Manual input default is below min",
				"Set a default greater than or equal to min.",
			)}
		}
		if input.Max != nil && value > *input.Max {
			return []Diagnostic{criticalDiagnostic(
				"workflow-input-default-invalid",
				"workflow",
				fieldPath+".default",
				"Manual input default is above max",
				"Set a default less than or equal to max.",
			)}
		}
	case "bool":
		if _, err := strconv.ParseBool(input.Default); err != nil {
			return []Diagnostic{criticalDiagnostic(
				"workflow-input-default-invalid",
				"workflow",
				fieldPath+".default",
				"Manual input default must be a bool",
				err.Error(),
			)}
		}
	case "enum":
		for _, option := range input.Options {
			if input.Default == option {
				return nil
			}
		}
		return []Diagnostic{criticalDiagnostic(
			"workflow-input-default-invalid",
			"workflow",
			fieldPath+".default",
			"Manual input default is not an enum option",
			"Set default to one of the configured options.",
		)}
	}

	return nil
}

func validateGitHubIssueManualInput(workflow *types.WorkflowConfig) []Diagnostic {
	if !workflow.EnableManualTrigger || workflow.Integration != "github-issues" {
		return nil
	}
	for _, input := range workflow.Inputs {
		if input.Name != "issue_number" {
			continue
		}
		if input.Type == "number" && input.Min != nil && *input.Min >= 1 {
			return nil
		}
		break
	}
	return []Diagnostic{criticalDiagnostic(
		"workflow-github-issue-manual-input",
		"workflow",
		"workflow.inputs.issue_number",
		"GitHub issue manual trigger requires issue_number",
		"Add an issue_number input with type number and min 1.",
	)}
}

func validatePipeline(workflow *types.WorkflowConfig, p *pipeline.Pipeline) []Diagnostic {
	var checks []Diagnostic
	stageIDs := map[string]int{}
	gateStageIDs := map[string]bool{}
	outputs := map[string]bool{}
	entryCount := 0

	for i, stage := range p.Stages {
		stagePath := fmt.Sprintf("workflow.pipeline_yaml.stages[%d]", i)
		if strings.TrimSpace(stage.ID) == "" {
			checks = append(checks, criticalDiagnostic(
				"pipeline-stage-id-required",
				"pipeline",
				stagePath+".id",
				"Pipeline stage ID is required",
				"Set a stable stage ID.",
			))
		} else {
			if err := ValidateSlug(stage.ID); err != nil {
				checks = append(checks, criticalDiagnostic(
					"pipeline-stage-id-invalid",
					"pipeline",
					stagePath+".id",
					"Pipeline stage ID is invalid",
					err.Error(),
				))
			}
			if prev, ok := stageIDs[stage.ID]; ok {
				checks = append(checks, criticalDiagnostic(
					"pipeline-stage-id-duplicate",
					"pipeline",
					stagePath+".id",
					"Pipeline stage ID is duplicated",
					fmt.Sprintf("Stage ID %q is already used at workflow.pipeline_yaml.stages[%d].", stage.ID, prev),
				))
			} else {
				stageIDs[stage.ID] = i
			}
		}

		if stage.Entry {
			entryCount++
		}
		if stage.OnEnter.Run.Output != "" {
			outputs[stage.OnEnter.Run.Output] = true
		}
		if stage.OnEnter.Judge.Output != "" {
			outputs[stage.OnEnter.Judge.Output] = true
		}
		if stage.Gate != nil {
			gateStageIDs[stage.ID] = true
		}
	}

	if entryCount != 1 {
		checks = append(checks, criticalDiagnostic(
			"pipeline-entry-stage-count",
			"pipeline",
			"workflow.pipeline_yaml.stages",
			"Pipeline must define exactly one entry stage",
			fmt.Sprintf("Found %d entry stages; set entry: true on exactly one stage.", entryCount),
		))
	}

	for i, stage := range p.Stages {
		stagePath := fmt.Sprintf("workflow.pipeline_yaml.stages[%d]", i)
		checks = append(checks, validatePipelineTriggers(stagePath, stage, stageIDs, gateStageIDs, outputs)...)
		checks = append(checks, validatePipelineGate(stagePath, stage, outputs)...)
		checks = append(checks, validateMoveIssueCompatibility(stagePath, workflow.Integration, stage)...)
	}

	return checks
}

func validatePipelineTriggers(stagePath string, stage pipeline.Stage, stageIDs map[string]int, gateStageIDs map[string]bool, outputs map[string]bool) []Diagnostic {
	var checks []Diagnostic
	for i, trigger := range stage.Triggers {
		triggerPath := fmt.Sprintf("%s.triggers[%d]", stagePath, i)
		if pipelineTriggerSourceCount(trigger) != 1 {
			checks = append(checks, criticalDiagnostic(
				"pipeline-trigger-source-count",
				"pipeline",
				triggerPath,
				"Pipeline trigger must define exactly one source",
				"Set exactly one trigger source in this trigger item.",
			))
		}

		if trigger.GateResult != nil {
			checks = append(checks, validateGateResultTrigger(triggerPath+".gate_result", trigger.GateResult, stageIDs, gateStageIDs)...)
		}
		if trigger.OutputMatches != nil {
			checks = append(checks, validateOutputMatchesTrigger(triggerPath+".output_matches", trigger.OutputMatches, outputs)...)
		}
	}
	return checks
}

func pipelineTriggerSourceCount(trigger pipeline.Trigger) int {
	count := 0
	if trigger.MessageContains != "" {
		count++
	}
	if trigger.PRMerged {
		count++
	}
	if trigger.PRClosed {
		count++
	}
	if trigger.PRConditions != nil {
		count++
	}
	if trigger.JudgeVerdict != "" {
		count++
	}
	if trigger.GateResult != nil {
		count++
	}
	if trigger.OutputMatches != nil {
		count++
	}
	return count
}

func validateGateResultTrigger(fieldPath string, trigger *pipeline.GateResultTrigger, stageIDs map[string]int, gateStageIDs map[string]bool) []Diagnostic {
	var checks []Diagnostic
	if strings.TrimSpace(trigger.Stage) == "" {
		checks = append(checks, criticalDiagnostic(
			"pipeline-gate-result-stage-required",
			"pipeline",
			fieldPath+".stage",
			"gate_result.stage is required",
			"Set gate_result.stage to a gate stage ID.",
		))
	} else if _, ok := stageIDs[trigger.Stage]; !ok {
		checks = append(checks, criticalDiagnostic(
			"pipeline-gate-result-stage-missing",
			"pipeline",
			fieldPath+".stage",
			"gate_result.stage references a missing stage",
			"Set gate_result.stage to an existing gate stage ID.",
		))
	} else if !gateStageIDs[trigger.Stage] {
		checks = append(checks, criticalDiagnostic(
			"pipeline-gate-result-stage-not-gate",
			"pipeline",
			fieldPath+".stage",
			"gate_result.stage must reference a gate stage",
			"Add a gate to the referenced stage or update gate_result.stage.",
		))
	}

	switch strings.ToLower(strings.TrimSpace(trigger.Verdict)) {
	case "pass", "fail":
	default:
		checks = append(checks, criticalDiagnostic(
			"pipeline-gate-result-verdict-invalid",
			"pipeline",
			fieldPath+".verdict",
			"gate_result.verdict is invalid",
			"Set gate_result.verdict to pass or fail.",
		))
	}
	return checks
}

func validateOutputMatchesTrigger(fieldPath string, trigger *pipeline.OutputMatchesTrigger, outputs map[string]bool) []Diagnostic {
	var checks []Diagnostic
	if strings.TrimSpace(trigger.Output) == "" {
		checks = append(checks, criticalDiagnostic(
			"pipeline-output-matches-output-required",
			"pipeline",
			fieldPath+".output",
			"output_matches.output is required",
			"Set output_matches.output to a named run or judge output.",
		))
	} else if !outputs[trigger.Output] {
		checks = append(checks, criticalDiagnostic(
			"pipeline-output-matches-output-missing",
			"pipeline",
			fieldPath+".output",
			"output_matches.output references a missing output",
			"Set output_matches.output to a named run or judge output.",
		))
	}
	if strings.TrimSpace(trigger.Path) == "" {
		checks = append(checks, criticalDiagnostic(
			"pipeline-output-matches-path-required",
			"pipeline",
			fieldPath+".path",
			"output_matches.path is required",
			"Set output_matches.path to the JSON path to inspect.",
		))
	}
	if len(trigger.AnyOf) == 0 {
		checks = append(checks, criticalDiagnostic(
			"pipeline-output-matches-any-of-required",
			"pipeline",
			fieldPath+".any_of",
			"output_matches.any_of is required",
			"Set at least one expected value.",
		))
	}
	return checks
}

func validatePipelineGate(stagePath string, stage pipeline.Stage, outputs map[string]bool) []Diagnostic {
	if stage.Gate == nil {
		return nil
	}
	if strings.TrimSpace(stage.Gate.Output) == "" {
		return []Diagnostic{criticalDiagnostic(
			"pipeline-gate-output-required",
			"pipeline",
			stagePath+".gate.output",
			"gate.output is required",
			"Set gate.output to a named run or judge output.",
		)}
	}
	if outputs[stage.Gate.Output] {
		return nil
	}
	return []Diagnostic{criticalDiagnostic(
		"pipeline-gate-output-missing",
		"pipeline",
		stagePath+".gate.output",
		"gate.output references a missing output",
		"Set gate.output to a named run or judge output.",
	)}
}

func validateMoveIssueCompatibility(stagePath, integration string, stage pipeline.Stage) []Diagnostic {
	if stage.OnEnter.MoveIssue.Status == "" {
		return nil
	}
	switch integration {
	case "linear", "shortcut":
		return nil
	default:
		return []Diagnostic{criticalDiagnostic(
			"pipeline-move-issue-incompatible",
			"pipeline",
			stagePath+".on_enter.move_issue",
			"move_issue is incompatible with this integration",
			"Use move_issue only for linear or shortcut workflows.",
		)}
	}
}

func cloneWorkflowConfig(workflow *types.WorkflowConfig) *types.WorkflowConfig {
	if workflow == nil {
		return nil
	}
	clone := *workflow
	clone.Tags = append([]string(nil), workflow.Tags...)
	clone.Labels = append([]string(nil), workflow.Labels...)
	clone.AllowedLabelers = append([]string(nil), workflow.AllowedLabelers...)
	clone.Inputs = append([]types.FactoryInput(nil), workflow.Inputs...)
	clone.Repos = append([]string(nil), workflow.Repos...)
	clone.TriggerRepos = append([]string(nil), workflow.TriggerRepos...)
	clone.SecretRefs = cloneStringMap(workflow.SecretRefs)
	clone.Stages = cloneWorkflowStages(workflow.Stages)
	clone.Trigger = cloneWorkflowTrigger(workflow.Trigger)
	return &clone
}

func cloneWorkflowTrigger(trigger *types.WorkflowTrigger) *types.WorkflowTrigger {
	if trigger == nil {
		return nil
	}
	clone := *trigger
	if trigger.GitHubIssues != nil {
		github := *trigger.GitHubIssues
		github.Repositories = append([]string(nil), trigger.GitHubIssues.Repositories...)
		github.States = append([]string(nil), trigger.GitHubIssues.States...)
		github.Labels = append([]string(nil), trigger.GitHubIssues.Labels...)
		github.Labelers = append([]string(nil), trigger.GitHubIssues.Labelers...)
		clone.GitHubIssues = &github
	}
	if trigger.Linear != nil {
		linear := *trigger.Linear
		linear.States = append([]string(nil), trigger.Linear.States...)
		linear.Labels = append([]string(nil), trigger.Linear.Labels...)
		clone.Linear = &linear
	}
	if trigger.Shortcut != nil {
		shortcut := *trigger.Shortcut
		shortcut.States = append([]string(nil), trigger.Shortcut.States...)
		shortcut.Labels = append([]string(nil), trigger.Shortcut.Labels...)
		clone.Shortcut = &shortcut
	}
	return &clone
}

func cloneWorkflowStages(stages []types.WorkflowStage) []types.WorkflowStage {
	if len(stages) == 0 {
		return nil
	}
	cloned := make([]types.WorkflowStage, len(stages))
	for i, stage := range stages {
		cloned[i] = stage
		cloned[i].Triggers = cloneTriggerMaps(stage.Triggers)
		cloned[i].OnEnter = cloneInterfaceMap(stage.OnEnter)
		cloned[i].Gate = cloneInterfaceMap(stage.Gate)
	}
	return cloned
}

func cloneTriggerMaps(triggers []map[string]interface{}) []map[string]interface{} {
	if len(triggers) == 0 {
		return nil
	}
	cloned := make([]map[string]interface{}, len(triggers))
	for i, trigger := range triggers {
		cloned[i] = cloneInterfaceMap(trigger)
	}
	return cloned
}

func cloneStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(input))
	for key, value := range input {
		cloned[key] = value
	}
	return cloned
}

func cloneInterfaceMap(input map[string]interface{}) map[string]interface{} {
	if len(input) == 0 {
		return nil
	}
	cloned := make(map[string]interface{}, len(input))
	for key, value := range input {
		cloned[key] = cloneInterfaceValue(value)
	}
	return cloned
}

func cloneInterfaceValue(value interface{}) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		return cloneInterfaceMap(typed)
	case []interface{}:
		cloned := make([]interface{}, len(typed))
		for i, item := range typed {
			cloned[i] = cloneInterfaceValue(item)
		}
		return cloned
	default:
		return value
	}
}
