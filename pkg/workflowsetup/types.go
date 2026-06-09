package workflowsetup

// PatternField describes a field surfaced by a workflow setup pattern.
type PatternField struct {
	Path        string `json:"path"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// PatternMetadata describes the fields and defaults for a setup pattern.
type PatternMetadata struct {
	ID                   string                 `json:"id"`
	Label                string                 `json:"label"`
	Description          string                 `json:"description"`
	RequiredFields       []PatternField         `json:"requiredFields"`
	AdvancedFields       []PatternField         `json:"advancedFields"`
	Defaults             map[string]interface{} `json:"defaults"`
	ValidationFieldPaths []string               `json:"validationFieldPaths"`
}

// RenderRequest asks the setup layer to render user-provided pattern config.
type RenderRequest struct {
	WorkflowName string                 `json:"workflowName"`
	PatternID    string                 `json:"patternId"`
	Config       map[string]interface{} `json:"config"`
}

// RenderResponse returns rendered workflow YAML and non-blocking warnings.
type RenderResponse struct {
	WorkflowName string       `json:"workflowName"`
	Config       string       `json:"config"`
	ConfigHash   string       `json:"configHash"`
	Warnings     []Diagnostic `json:"warnings"`
}

// ValidateRequest asks the setup layer to validate rendered workflow YAML.
type ValidateRequest struct {
	WorkflowName    string `json:"workflowName"`
	Config          string `json:"config"`
	WorkspaceConfig string `json:"workspaceConfig,omitempty"`
}

// ValidateResponse returns the validation result and individual checks.
type ValidateResponse struct {
	OK         bool         `json:"ok"`
	ConfigHash string       `json:"configHash"`
	Summary    Summary      `json:"summary"`
	Checks     []Diagnostic `json:"checks"`
}

// SaveMode describes how a workflow save should be applied.
type SaveMode string

const (
	SaveModeCreate SaveMode = "create"
	SaveModeUpdate SaveMode = "update"
	SaveModeUpsert SaveMode = "upsert"
)

// SaveWorkflow is the workflow payload nested inside a save request.
type SaveWorkflow struct {
	Name   string `json:"name"`
	Config string `json:"config"`
}

// SaveRequest asks the setup layer to persist a validated workflow config.
type SaveRequest struct {
	Workspace           string       `json:"workspace"`
	Workflow            SaveWorkflow `json:"workflow"`
	Mode                SaveMode     `json:"mode"`
	ValidatedConfigHash string       `json:"validatedConfigHash"`
	AllowWarnings       bool         `json:"allowWarnings"`
}
