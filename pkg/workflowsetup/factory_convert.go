package workflowsetup

import (
	"bytes"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/elasticclaw/elasticclaw/pkg/workflow/pipeline"
	"gopkg.in/yaml.v3"
)

const (
	FactoryConvertStatusReady   = "ready"
	FactoryConvertStatusBlocked = "blocked"
)

// FactoryConvertRequest asks workflow setup to convert a legacy factory into
// workflow v1 YAML for a target workspace.
type FactoryConvertRequest struct {
	Factory         *types.FactoryConfig `json:"factory"`
	WorkspaceName   string               `json:"workspaceName,omitempty"`
	WorkspaceConfig string               `json:"workspaceConfig,omitempty"`
	TemplateFiles   map[string]string    `json:"templateFiles,omitempty"`
	WorkspaceFiles  map[string]string    `json:"workspaceFiles,omitempty"`
}

// FactoryConvertResponse returns the converted workflow YAML and conversion
// diagnostics. Status is "ready" only when no critical diagnostic was found.
type FactoryConvertResponse struct {
	WorkflowName string       `json:"workflowName"`
	Config       string       `json:"config"`
	ConfigHash   string       `json:"configHash"`
	Diagnostics  []Diagnostic `json:"diagnostics"`
	Summary      Summary      `json:"summary"`
	Status       string       `json:"status"`
}

// ConvertFactory converts a supported legacy factory into workflow v1 YAML. It
// does not mutate the input factory.
func ConvertFactory(req FactoryConvertRequest) FactoryConvertResponse {
	factory := cloneFactoryForConvert(req.Factory)
	if factory == nil {
		return factoryConvertResponse("", "", []Diagnostic{
			factoryConvertCriticalDiagnostic(
				"factory-convert-factory-missing",
				"factory",
				"factory",
				"Factory config is missing",
				"Provide a legacy factory config to convert.",
			),
		})
	}

	var diagnostics []Diagnostic
	diagnostics = append(diagnostics, validateFactoryConvertSupport(factory)...)
	if hasCriticalDiagnostics(diagnostics) {
		return factoryConvertResponse(factory.Name, "", diagnostics)
	}

	workspace, err := ParseWorkspaceConfig(req.WorkspaceConfig)
	if err != nil {
		diagnostics = append(diagnostics, factoryConvertCriticalDiagnostic(
			"factory-convert-workspace-config-invalid",
			"workspace",
			"workspace.config",
			"Workspace config is invalid",
			err.Error(),
		))
	} else if workspace == nil {
		diagnostics = append(diagnostics, factoryConvertCriticalDiagnostic(
			"factory-convert-workspace-config-missing",
			"workspace",
			"workspace.config",
			"Workspace config is required",
			"Provide the target workspace elasticclaw-config.yaml so conversion parity can be checked.",
		))
	}
	diagnostics = append(diagnostics, validateFactoryWorkspaceParity(factory, req.WorkspaceName, workspace)...)
	diagnostics = append(diagnostics, validateFactoryTriggerParity(factory)...)
	diagnostics = append(diagnostics, validateFactoryFileParity(req.TemplateFiles, req.WorkspaceFiles)...)
	if hasCriticalDiagnostics(diagnostics) {
		return factoryConvertResponse(factory.Name, "", diagnostics)
	}

	parsedPipeline, err := pipeline.Parse([]byte(factory.PipelineYAML))
	if err != nil {
		diagnostics = append(diagnostics, factoryConvertCriticalDiagnostic(
			"factory-convert-pipeline-invalid",
			"pipeline",
			"factory.pipeline_yaml",
			"Factory pipeline YAML is invalid",
			err.Error(),
		))
	}
	var stages []types.WorkflowStage
	if err == nil {
		stages, err = workflowStagesFromFactoryPipeline(factory.PipelineYAML)
		if err != nil {
			diagnostics = append(diagnostics, factoryConvertCriticalDiagnostic(
				"factory-convert-pipeline-invalid",
				"pipeline",
				"factory.pipeline_yaml",
				"Factory pipeline YAML is invalid",
				err.Error(),
			))
		}
		if parsedPipeline != nil && len(stages) != len(parsedPipeline.Stages) {
			diagnostics = append(diagnostics, factoryConvertCriticalDiagnostic(
				"factory-convert-pipeline-invalid",
				"pipeline",
				"factory.pipeline_yaml.stages",
				"Factory pipeline stages could not be preserved",
				"Parsed pipeline stage count does not match converted workflow stages.",
			))
		}
	}
	if hasCriticalDiagnostics(diagnostics) && strings.TrimSpace(factory.PipelineYAML) != "" && len(stages) == 0 {
		return factoryConvertResponse(factory.Name, "", diagnostics)
	}

	workflow := workflowFromFactory(factory, stages)
	config, marshalErr := marshalConvertedWorkflow(workflow)
	if marshalErr != nil {
		diagnostics = append(diagnostics, factoryConvertCriticalDiagnostic(
			"factory-convert-workflow-marshal-failed",
			"workflow",
			"workflow",
			"Converted workflow could not be marshaled",
			marshalErr.Error(),
		))
		return factoryConvertResponse(factory.Name, "", diagnostics)
	}

	staticResp := ValidateStatic(ValidateRequest{
		WorkflowName:    workflow.Name,
		Config:          config,
		WorkspaceConfig: req.WorkspaceConfig,
	})
	diagnostics = append(diagnostics, staticResp.Checks...)

	return factoryConvertResponse(workflow.Name, config, diagnostics)
}

func validateFactoryConvertSupport(factory *types.FactoryConfig) []Diagnostic {
	switch factory.Integration {
	case "github-issues", "linear", "shortcut":
		return nil
	case "github":
		if factory.Trigger != nil && factory.Trigger.On == "pull_request" {
			return []Diagnostic{factoryConvertCriticalDiagnostic(
				"factory-convert-unsupported-github-pr",
				"factory",
				"factory.trigger.on",
				"GitHub pull request factories are not supported",
				"Convert only github-issues, linear, or shortcut factories in this command.",
			)}
		}
		return []Diagnostic{factoryConvertCriticalDiagnostic(
			"factory-convert-unsupported-github",
			"factory",
			"factory.integration",
			"GitHub factories are not supported",
			"Convert only github-issues, linear, or shortcut factories in this command.",
		)}
	case "external":
		return []Diagnostic{factoryConvertCriticalDiagnostic(
			"factory-convert-unsupported-external",
			"factory",
			"factory.integration",
			"External factories are not supported",
			"External webhook factories do not have workflow v1 parity yet.",
		)}
	default:
		return []Diagnostic{factoryConvertCriticalDiagnostic(
			"factory-convert-unsupported-integration",
			"factory",
			"factory.integration",
			"Factory integration is not supported",
			fmt.Sprintf("Integration %q cannot be converted to workflow v1.", factory.Integration),
		)}
	}
}

func validateFactoryWorkspaceParity(factory *types.FactoryConfig, workspaceName string, workspace *ParsedWorkspaceConfig) []Diagnostic {
	var diagnostics []Diagnostic
	targetWorkspace := strings.TrimSpace(workspaceName)
	configWorkspace := parsedWorkspaceName(workspace)
	if targetWorkspace == "" {
		targetWorkspace = configWorkspace
	}
	if targetWorkspace == "" {
		diagnostics = append(diagnostics, factoryConvertCriticalDiagnostic(
			"factory-convert-workspace-name-missing",
			"workspace",
			"workspace.name",
			"Target workspace name is required",
			"Pass the workspace name or provide workspace config with a name.",
		))
	}
	if configWorkspace != "" && targetWorkspace != "" && !strings.EqualFold(configWorkspace, targetWorkspace) {
		diagnostics = append(diagnostics, factoryConvertCriticalDiagnostic(
			"factory-convert-workspace-name-mismatch",
			"workspace",
			"workspace.name",
			"Workspace config name does not match the target workspace",
			fmt.Sprintf("Workspace config name %q does not match target workspace %q.", configWorkspace, targetWorkspace),
		))
	}

	template := strings.TrimSpace(factory.Template)
	if template == "" {
		diagnostics = append(diagnostics, factoryConvertCriticalDiagnostic(
			"factory-convert-template-missing",
			"factory",
			"factory.template",
			"Factory template is required for conversion",
			"Set factory.template to the workspace that replaces this legacy template.",
		))
	} else if targetWorkspace != "" && !strings.EqualFold(template, targetWorkspace) {
		diagnostics = append(diagnostics, factoryConvertCriticalDiagnostic(
			"factory-convert-template-workspace-mismatch",
			"factory",
			"factory.template",
			"Factory template does not match target workspace",
			fmt.Sprintf("Factory template %q cannot be reconciled with target workspace %q.", template, targetWorkspace),
		))
	}

	provider := strings.TrimSpace(factory.Provider)
	if provider == "" && workspace != nil && workspace.Template != nil {
		provider = strings.TrimSpace(workspace.Template.Provider)
	}
	if provider == "" {
		diagnostics = append(diagnostics, factoryConvertCriticalDiagnostic(
			"factory-convert-provider-unresolved",
			"runtime",
			"workflow.provider",
			"No execution provider could be resolved",
			"Set factory.provider or provider in the target workspace config before converting.",
		))
	}

	knownSecrets := factoryConvertKnownSecretNames(workspace)
	for envName, secretName := range factory.SecretRefs {
		secretName = strings.TrimSpace(secretName)
		if secretName == "" || knownSecrets[secretName] {
			continue
		}
		diagnostics = append(diagnostics, factoryConvertCriticalDiagnostic(
			"factory-convert-secret-ref-missing",
			"runtime",
			"factory.secret_refs."+envName,
			"Factory secret reference cannot be reconciled",
			fmt.Sprintf("Secret %q is not declared by the target workspace config.", secretName),
		))
	}

	return diagnostics
}

func validateFactoryTriggerParity(factory *types.FactoryConfig) []Diagnostic {
	switch factory.Integration {
	case "github-issues":
		if len(githubIssuesFactoryRepos(factory)) > 0 {
			return nil
		}
		return []Diagnostic{factoryConvertCriticalDiagnostic(
			"factory-convert-github-issues-repositories-missing",
			"factory",
			"factory.trigger_repos",
			"GitHub Issues factory trigger repositories are ambiguous",
			"Set trigger_repos or repos so the workflow trigger can target explicit repositories.",
		)}
	case "linear", "shortcut":
		if strings.TrimSpace(factory.TriggerStatus) != "" {
			return nil
		}
		return []Diagnostic{factoryConvertCriticalDiagnostic(
			"factory-convert-trigger-status-missing",
			"factory",
			"factory.trigger_status",
			"Factory trigger status is required for conversion",
			"Set trigger_status so the workflow trigger can target an explicit status.",
		)}
	default:
		return nil
	}
}

func validateFactoryFileParity(templateFiles, workspaceFiles map[string]string) []Diagnostic {
	if templateFiles == nil {
		return []Diagnostic{factoryConvertCriticalDiagnostic(
			"factory-convert-template-files-unchecked",
			"template",
			"factory.files",
			"Legacy template files were not checked",
			"Provide the files from the legacy factory/template directory so conversion can prove workspace parity.",
		)}
	}

	required := comparableFactoryTemplateFiles(templateFiles)
	if len(required) == 0 {
		return nil
	}
	if workspaceFiles == nil {
		return []Diagnostic{factoryConvertCriticalDiagnostic(
			"factory-convert-workspace-files-unchecked",
			"workspace",
			"workspace.files",
			"Workspace files were not checked",
			"Provide the target workspace files so conversion can prove template file parity.",
		)}
	}

	var diagnostics []Diagnostic
	workspaceComparable := normalizeFileMap(workspaceFiles)
	names := sortedMapKeys(required)
	for _, name := range names {
		want := required[name]
		got, ok := workspaceComparable[name]
		if !ok {
			diagnostics = append(diagnostics, factoryConvertCriticalDiagnostic(
				"factory-convert-template-file-missing",
				"template",
				"workspace.files."+name,
				"Workspace is missing a legacy template file",
				fmt.Sprintf("File %q exists in the legacy factory/template source but not in the target workspace.", name),
			))
			continue
		}
		if got != want {
			diagnostics = append(diagnostics, factoryConvertCriticalDiagnostic(
				"factory-convert-template-file-mismatch",
				"template",
				"workspace.files."+name,
				"Workspace legacy template file content differs",
				fmt.Sprintf("File %q differs between the legacy factory/template source and target workspace.", name),
			))
		}
	}
	return diagnostics
}

func comparableFactoryTemplateFiles(files map[string]string) map[string]string {
	normalized := normalizeFileMap(files)
	for _, ignored := range []string{"factory.yaml", "pipeline.yaml"} {
		delete(normalized, ignored)
	}
	return normalized
}

func normalizeFileMap(files map[string]string) map[string]string {
	normalized := make(map[string]string, len(files))
	for name, content := range files {
		clean := normalizeRelativeFilePath(name)
		if clean == "" {
			continue
		}
		normalized[clean] = content
	}
	return normalized
}

func normalizeRelativeFilePath(name string) string {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	if name == "" {
		return ""
	}
	clean := path.Clean(name)
	if clean == "." || strings.HasPrefix(clean, "../") || clean == ".." || strings.HasPrefix(clean, "/") {
		return ""
	}
	return clean
}

func sortedMapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func factoryConvertKnownSecretNames(workspace *ParsedWorkspaceConfig) map[string]bool {
	known := map[string]bool{}
	if workspace == nil || workspace.Workspace == nil {
		return known
	}
	for _, name := range workspace.Workspace.Secrets {
		if strings.TrimSpace(name) != "" {
			known[strings.TrimSpace(name)] = true
		}
	}
	for _, envVar := range workspace.Workspace.Env {
		if strings.TrimSpace(envVar.Secret) != "" {
			known[strings.TrimSpace(envVar.Secret)] = true
		}
	}
	if workspace.Template != nil {
		for _, secretName := range workspace.Template.SecretRefs {
			if strings.TrimSpace(secretName) != "" {
				known[strings.TrimSpace(secretName)] = true
			}
		}
	}
	return known
}

func workflowStagesFromFactoryPipeline(raw string) ([]types.WorkflowStage, error) {
	var wrapper struct {
		Stages []types.WorkflowStage `yaml:"stages"`
	}
	dec := yaml.NewDecoder(bytes.NewReader([]byte(raw)))
	if err := dec.Decode(&wrapper); err != nil && err != io.EOF {
		return nil, fmt.Errorf("decode workflow stages: %w", err)
	}
	return wrapper.Stages, nil
}

func workflowFromFactory(factory *types.FactoryConfig, stages []types.WorkflowStage) types.WorkflowConfig {
	enabled := false
	workflow := types.WorkflowConfig{
		SchemaVersion:       "v1",
		Name:                strings.TrimSpace(factory.Name),
		Enabled:             &enabled,
		Integration:         factory.Integration,
		Workspace:           factory.Workspace,
		Team:                factory.Team,
		TriggerStatus:       factory.TriggerStatus,
		WorkingStatus:       factory.WorkingStatus,
		FinishedStatus:      firstNonEmpty(factory.FinishedStatus, factory.DoneStatus),
		TerminateOnLeave:    factory.TerminateOnLeave,
		Provider:            factory.Provider,
		NamePattern:         factory.NamePattern,
		Tags:                append([]string(nil), factory.Tags...),
		Color:               factory.Color,
		Labels:              append([]string(nil), factory.Labels...),
		AssignedTo:          factory.AssignedTo,
		AllowedLabelers:     append([]string(nil), factory.AllowedLabelers...),
		SecretRefs:          cloneStringMap(factory.SecretRefs),
		Inputs:              cloneFactoryConvertInputs(factory.Inputs),
		ConcurrencyGroup:    firstNonEmpty(factory.ConcurrencyGroup, "global"),
		EnableManualTrigger: factory.EnableManualTrigger,
		Repos:               append([]string(nil), factory.Repos...),
		TriggerRepos:        append([]string(nil), factory.TriggerRepos...),
		Stages:              stages,
		PipelineYAML:        factory.PipelineYAML,
	}

	switch factory.Integration {
	case "github-issues":
		workflow.Trigger = githubIssuesWorkflowTriggerFromFactory(factory)
	case "linear":
		workflow.Trigger = &types.WorkflowTrigger{
			Linear: &types.LinearWorkflowTrigger{
				Event:      "status_changed",
				Workspace:  factory.Workspace,
				Team:       factory.Team,
				States:     compactStringList([]string{factory.TriggerStatus}),
				Labels:     append([]string(nil), factory.Labels...),
				AssignedTo: factory.AssignedTo,
			},
		}
	case "shortcut":
		workflow.Trigger = &types.WorkflowTrigger{
			Shortcut: &types.ShortcutWorkflowTrigger{
				Event:      "status_changed",
				Workspace:  factory.Workspace,
				States:     compactStringList([]string{factory.TriggerStatus}),
				Labels:     append([]string(nil), factory.Labels...),
				AssignedTo: factory.AssignedTo,
			},
		}
	}
	return workflow
}

func githubIssuesWorkflowTriggerFromFactory(factory *types.FactoryConfig) *types.WorkflowTrigger {
	states, labels := githubIssuesStatesAndLabels(factory)
	labelers := append([]string(nil), factory.AllowedLabelers...)
	if len(labelers) == 0 {
		labelers = []string{"*"}
	}
	event := "issue_labeled"
	if len(labels) == 0 {
		event = "issue_opened"
	}
	if factory.Trigger != nil && factory.Trigger.On == "issue" {
		switch factory.Trigger.Action {
		case "opened":
			event = "issue_opened"
		case "reopened":
			event = "issue_reopened"
		}
	}
	return &types.WorkflowTrigger{
		GitHubIssues: &types.GitHubIssuesWorkflowTrigger{
			Event:        event,
			Repositories: githubIssuesFactoryRepos(factory),
			States:       states,
			Labels:       labels,
			Labelers:     labelers,
			AssignedTo:   factory.AssignedTo,
		},
	}
}

func githubIssuesStatesAndLabels(factory *types.FactoryConfig) ([]string, []string) {
	triggerStatus := strings.TrimSpace(factory.TriggerStatus)
	labels := append([]string(nil), factory.Labels...)
	switch triggerStatus {
	case "":
		return []string{"open"}, compactStringList(labels)
	case "open", "closed":
		return []string{triggerStatus}, compactStringList(labels)
	default:
		return []string{"open"}, prependUniqueString(triggerStatus, labels)
	}
}

func githubIssuesFactoryRepos(factory *types.FactoryConfig) []string {
	if len(factory.TriggerRepos) > 0 {
		return compactStringList(factory.TriggerRepos)
	}
	return compactStringList(factory.Repos)
}

func marshalConvertedWorkflow(workflow types.WorkflowConfig) (string, error) {
	data, err := yaml.Marshal(workflow)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func factoryConvertResponse(workflowName, config string, diagnostics []Diagnostic) FactoryConvertResponse {
	summary := SummarizeDiagnostics(diagnostics)
	status := FactoryConvertStatusReady
	if summary.Critical > 0 {
		status = FactoryConvertStatusBlocked
	}
	return FactoryConvertResponse{
		WorkflowName: workflowName,
		Config:       config,
		ConfigHash:   ConfigHash(config),
		Diagnostics:  append([]Diagnostic(nil), diagnostics...),
		Summary:      summary,
		Status:       status,
	}
}

func factoryConvertCriticalDiagnostic(id, category, fieldPath, title, detail string) Diagnostic {
	diagnostic := criticalDiagnostic(id, category, fieldPath, title, detail)
	diagnostic.Step = "factory-convert"
	return diagnostic
}

func hasCriticalDiagnostics(diagnostics []Diagnostic) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == SeverityCritical && diagnostic.Blocking {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func prependUniqueString(value string, values []string) []string {
	result := []string{}
	seen := map[string]bool{}
	value = strings.TrimSpace(value)
	if value != "" {
		result = append(result, value)
		seen[value] = true
	}
	for _, candidate := range values {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || seen[candidate] {
			continue
		}
		result = append(result, candidate)
		seen[candidate] = true
	}
	return result
}

func compactStringList(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}

func cloneFactoryForConvert(factory *types.FactoryConfig) *types.FactoryConfig {
	if factory == nil {
		return nil
	}
	clone := *factory
	if factory.Enabled != nil {
		enabled := *factory.Enabled
		clone.Enabled = &enabled
	}
	clone.Tags = append([]string(nil), factory.Tags...)
	clone.Labels = append([]string(nil), factory.Labels...)
	clone.AllowedLabelers = append([]string(nil), factory.AllowedLabelers...)
	clone.Inputs = cloneFactoryConvertInputs(factory.Inputs)
	clone.SecretRefs = cloneStringMap(factory.SecretRefs)
	clone.Repos = append([]string(nil), factory.Repos...)
	clone.TriggerRepos = append([]string(nil), factory.TriggerRepos...)
	if factory.Trigger != nil {
		trigger := *factory.Trigger
		if factory.Trigger.Filter != nil {
			filter := *factory.Trigger.Filter
			trigger.Filter = &filter
		}
		clone.Trigger = &trigger
	}
	if factory.ExternalTrigger != nil {
		trigger := *factory.ExternalTrigger
		if factory.ExternalTrigger.Filter != nil {
			filter := *factory.ExternalTrigger.Filter
			trigger.Filter = &filter
		}
		clone.ExternalTrigger = &trigger
	}
	return &clone
}

func cloneFactoryConvertInputs(inputs []types.FactoryInput) []types.FactoryInput {
	if inputs == nil {
		return nil
	}
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
