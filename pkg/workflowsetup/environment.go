package workflowsetup

import "github.com/elasticclaw/elasticclaw/pkg/types"

// Environment is the read-only hub surface used by workflow setup.
// It intentionally exposes sanitized readiness metadata and targeted loaders
// instead of the mutable hub configuration.
type Environment interface {
	Snapshot() (SetupEnvironmentSnapshot, error)
	LoadWorkspace(name string) (*types.WorkspaceConfig, error)
	LoadWorkflowRaw(workspaceName, workflowName string) (string, error)
	WorkspaceSecretNames(workspaceName string) ([]string, error)
	WorkspaceIssueTrackers(workspaceName string) ([]IssueTrackerRef, error)
	WorkspaceGitHubApps(workspaceName string) ([]GitHubAppRef, error)
	ListFactories() ([]FactoryRef, error)
	LoadFactory(name string) (*types.FactoryConfig, error)
}

// SetupEnvironmentSnapshot is sanitized readiness data for workflow setup.
type SetupEnvironmentSnapshot struct {
	ClawTokenSet      bool                  `json:"clawTokenSet"`
	Providers         []ProviderRef         `json:"providers"`
	DefaultProvider   string                `json:"defaultProvider,omitempty"`
	DefaultModel      string                `json:"defaultModel,omitempty"`
	LLMKeys           []LLMKeyRef           `json:"llmKeys"`
	ConcurrencyGroups []ConcurrencyGroupRef `json:"concurrencyGroups"`
	HubSecretNames    []string              `json:"hubSecretNames"`
	IssueTrackers     []IssueTrackerRef     `json:"issueTrackers"`
	GitHubApps        []GitHubAppRef        `json:"githubApps"`
}

// ProviderRef describes an execution provider without credential values.
type ProviderRef struct {
	Name           string `json:"name"`
	Type           string `json:"type"`
	Provisionable  bool   `json:"provisionable"`
	CredentialsSet bool   `json:"credentialsSet"`
	APIKeySet      bool   `json:"apiKeySet,omitempty"`
	TokenSet       bool   `json:"tokenSet,omitempty"`
	AccessTokenSet bool   `json:"accessTokenSet,omitempty"`
	SSHKeySet      bool   `json:"sshKeySet,omitempty"`
}

// LLMKeyRef describes a named LLM key without the API key value.
type LLMKeyRef struct {
	Name         string `json:"name"`
	Provider     string `json:"provider"`
	KeySet       bool   `json:"keySet"`
	Default      bool   `json:"default"`
	DefaultModel string `json:"defaultModel,omitempty"`
}

// ConcurrencyGroupRef describes a concurrency group limit.
type ConcurrencyGroupRef struct {
	Name  string `json:"name"`
	Limit int    `json:"limit"`
}

// IssueTrackerRef describes issue tracker credentials without token values.
type IssueTrackerRef struct {
	Type             string `json:"type"`
	Workspace        string `json:"workspace"`
	TokenSet         bool   `json:"tokenSet"`
	WebhookSecretSet bool   `json:"webhookSecretSet"`
}

// GitHubAppRef describes GitHub App credentials without private keys.
type GitHubAppRef struct {
	Name          string   `json:"name,omitempty"`
	AppID         int64    `json:"appId"`
	URL           string   `json:"url,omitempty"`
	Installation  string   `json:"installation,omitempty"`
	Installations []string `json:"installations,omitempty"`
	PrivateKeySet bool     `json:"privateKeySet"`
}

// FactoryRef is a sanitized factory list item. LoadFactory returns factory
// conversion details with secret values removed.
type FactoryRef struct {
	Name                string            `json:"name"`
	Integration         string            `json:"integration"`
	Workspace           string            `json:"workspace,omitempty"`
	Team                string            `json:"team,omitempty"`
	Template            string            `json:"template,omitempty"`
	Provider            string            `json:"provider,omitempty"`
	Enabled             bool              `json:"enabled"`
	ConcurrencyGroup    string            `json:"concurrencyGroup,omitempty"`
	EnableManualTrigger bool              `json:"enableManualTrigger,omitempty"`
	WebhookSecretSet    bool              `json:"webhookSecretSet"`
	WebhookSecretRef    string            `json:"webhookSecretRef,omitempty"`
	PipelineSet         bool              `json:"pipelineSet"`
	SecretRefs          map[string]string `json:"secretRefs,omitempty"`
}
