package types

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// WorkspaceConfig defines a persisted workspace. Workflows live beside this
// file in external storage and are loaded into Workflows at API boundaries.
type WorkspaceConfig struct {
	SchemaVersion  string               `yaml:"schema_version,omitempty" json:"schemaVersion,omitempty"`
	Name           string               `yaml:"name" json:"name"`
	Repositories   RepositoryAccessList `yaml:"repositories,omitempty" json:"repositories,omitempty"`
	Env            WorkspaceEnv         `yaml:"env,omitempty" json:"env,omitempty"`
	Secrets        []string             `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	WebhookSecrets []string             `yaml:"webhook_secrets,omitempty" json:"webhookSecrets,omitempty"`
	Workflows      []*WorkflowConfig    `yaml:"-" json:"workflows,omitempty"`
	Files          map[string]string    `yaml:"-" json:"files,omitempty"`
}

// WorkflowConfig is the persisted workflow schema.
type WorkflowConfig struct {
	SchemaVersion       string            `yaml:"schema_version,omitempty" json:"schemaVersion,omitempty"`
	Name                string            `yaml:"name" json:"name"`
	RawConfig           string            `yaml:"-" json:"rawConfig,omitempty"`
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
	Trigger             *WorkflowTrigger  `yaml:"trigger,omitempty" json:"trigger,omitempty"`
	Stages              []WorkflowStage   `yaml:"stages,omitempty" json:"stages,omitempty"`
	PipelineYAML        string            `yaml:"pipeline_yaml,omitempty" json:"pipelineYAML,omitempty"`
}

// UnmarshalYAML rejects removed workflow keys so old workflow files fail loudly
// instead of silently loading without runtime stages.
func (w *WorkflowConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(value.Content); i += 2 {
			if value.Content[i].Value == "jobs" {
				return fmt.Errorf("workflow uses removed key %q; rename it to %q", "jobs", "stages")
			}
		}
	}
	type workflowConfig WorkflowConfig
	var decoded workflowConfig
	if err := value.Decode(&decoded); err != nil {
		return err
	}
	*w = WorkflowConfig(decoded)
	return nil
}

// WorkflowTrigger defines the event source and filters for a workflow.
type WorkflowTrigger struct {
	GitHubIssues *GitHubIssuesWorkflowTrigger `yaml:"github_issues,omitempty" json:"github_issues,omitempty"`
	Linear       *LinearWorkflowTrigger       `yaml:"linear,omitempty" json:"linear,omitempty"`
	Shortcut     *ShortcutWorkflowTrigger     `yaml:"shortcut,omitempty" json:"shortcut,omitempty"`
}

type GitHubIssuesWorkflowTrigger struct {
	Event        string   `yaml:"event,omitempty" json:"event,omitempty"`
	Repositories []string `yaml:"repositories,omitempty" json:"repositories,omitempty"`
	States       []string `yaml:"states,omitempty" json:"states,omitempty"`
	Labels       []string `yaml:"labels,omitempty" json:"labels,omitempty"`
	Labelers     []string `yaml:"labelers,omitempty" json:"labelers,omitempty"`
	AssignedTo   string   `yaml:"assigned_to,omitempty" json:"assigned_to,omitempty"`
}

type LinearWorkflowTrigger struct {
	Event      string   `yaml:"event,omitempty" json:"event,omitempty"`
	Workspace  string   `yaml:"workspace,omitempty" json:"workspace,omitempty"`
	Team       string   `yaml:"team,omitempty" json:"team,omitempty"`
	States     []string `yaml:"states,omitempty" json:"states,omitempty"`
	Labels     []string `yaml:"labels,omitempty" json:"labels,omitempty"`
	AssignedTo string   `yaml:"assigned_to,omitempty" json:"assigned_to,omitempty"`
}

type ShortcutWorkflowTrigger struct {
	Event      string   `yaml:"event,omitempty" json:"event,omitempty"`
	Workspace  string   `yaml:"workspace,omitempty" json:"workspace,omitempty"`
	States     []string `yaml:"states,omitempty" json:"states,omitempty"`
	Labels     []string `yaml:"labels,omitempty" json:"labels,omitempty"`
	AssignedTo string   `yaml:"assigned_to,omitempty" json:"assigned_to,omitempty"`
}

// WorkflowStage is one step in a workflow state machine. It intentionally mirrors
// the persisted pipeline stage shape without making pkg/types depend on hub.
type WorkflowStage struct {
	ID       string                   `yaml:"id" json:"id"`
	Label    string                   `yaml:"label,omitempty" json:"label,omitempty"`
	Entry    bool                     `yaml:"entry,omitempty" json:"entry,omitempty"`
	Terminal bool                     `yaml:"terminal,omitempty" json:"terminal,omitempty"`
	Triggers []map[string]interface{} `yaml:"triggers,omitempty" json:"triggers,omitempty"`
	OnEnter  map[string]interface{}   `yaml:"on_enter,omitempty" json:"onEnter,omitempty"`
	Gate     map[string]interface{}   `yaml:"gate,omitempty" json:"gate,omitempty"`
}

func (w *WorkspaceConfig) Validate() error {
	if w == nil {
		return fmt.Errorf("workspace config is nil")
	}
	if w.Name == "" {
		return fmt.Errorf("workspace name is required")
	}
	if err := validateRepositoryAccessList("repositories", w.Repositories); err != nil {
		return fmt.Errorf("workspace %q: %w", w.Name, err)
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
