package types

import "fmt"

// WorkspaceConfig defines a persisted workspace. Workflows live beside this
// file in external storage and are loaded into Workflows at API boundaries.
type WorkspaceConfig struct {
	SchemaVersion  string            `yaml:"schema_version,omitempty" json:"schemaVersion,omitempty"`
	Name           string            `yaml:"name" json:"name"`
	Repositories   []string          `yaml:"repositories,omitempty" json:"repositories,omitempty"`
	Secrets        []string          `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	WebhookSecrets []string          `yaml:"webhook_secrets,omitempty" json:"webhookSecrets,omitempty"`
	Workflows      []*WorkflowConfig `yaml:"-" json:"workflows,omitempty"`
	Files          map[string]string `yaml:"-" json:"files,omitempty"`
}

// WorkflowConfig is the persisted workflow schema.
type WorkflowConfig struct {
	SchemaVersion       string            `yaml:"schema_version,omitempty" json:"schemaVersion,omitempty"`
	Name                string            `yaml:"name" json:"name"`
	Enabled             *bool             `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Integration         string            `yaml:"integration,omitempty" json:"integration,omitempty"`
	Workspace           string            `yaml:"workspace,omitempty" json:"workspace,omitempty"`
	Team                string            `yaml:"team,omitempty" json:"team,omitempty"`
	TriggerStatus       string            `yaml:"trigger_status,omitempty" json:"trigger_status,omitempty"`
	WorkingStatus       string            `yaml:"working_status,omitempty" json:"working_status,omitempty"`
	FinishedStatus      string            `yaml:"finished_status,omitempty" json:"finished_status,omitempty"`
	TerminateOnLeave    bool              `yaml:"terminate_on_leave,omitempty" json:"terminate_on_leave,omitempty"`
	Provider            string            `yaml:"provider,omitempty" json:"provider,omitempty"`
	NamePattern         string            `yaml:"name_pattern,omitempty" json:"name_pattern,omitempty"`
	Tags                []string          `yaml:"tags,omitempty" json:"tags,omitempty"`
	Color               string            `yaml:"color,omitempty" json:"color,omitempty"`
	Labels              []string          `yaml:"labels,omitempty" json:"labels,omitempty"`
	AssignedTo          string            `yaml:"assigned_to,omitempty" json:"assigned_to,omitempty"`
	AllowedLabelers     []string          `yaml:"allowed_labelers,omitempty" json:"allowed_labelers,omitempty"`
	SecretRefs          map[string]string `yaml:"secret_refs,omitempty" json:"secret_refs,omitempty"`
	Inputs              []FactoryInput    `yaml:"inputs,omitempty" json:"inputs,omitempty"`
	ConcurrencyGroup    string            `yaml:"concurrency_group,omitempty" json:"concurrency_group,omitempty"`
	EnableManualTrigger bool              `yaml:"enable_manual_trigger,omitempty" json:"enable_manual_trigger,omitempty"`
	Repos               []string          `yaml:"repos,omitempty" json:"repos,omitempty"`
	TriggerRepos        []string          `yaml:"trigger_repos,omitempty" json:"trigger_repos,omitempty"`
	Trigger             *GitHubTrigger    `yaml:"trigger,omitempty" json:"trigger,omitempty"`
}

func (w *WorkspaceConfig) Validate() error {
	if w == nil {
		return fmt.Errorf("workspace config is nil")
	}
	if w.Name == "" {
		return fmt.Errorf("workspace name is required")
	}
	for _, workflow := range w.Workflows {
		if workflow == nil {
			return fmt.Errorf("workspace %q: workflow cannot be nil", w.Name)
		}
		if err := workflow.Validate(); err != nil {
			return fmt.Errorf("workspace %q: %w", w.Name, err)
		}
	}
	return nil
}
