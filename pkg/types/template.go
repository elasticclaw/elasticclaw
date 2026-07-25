package types

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// LLMKeyConfig represents a named LLM API key entry in hub.yaml.
type LLMKeyConfig struct {
	Name         string `yaml:"name"          json:"name"`                    // unique label, e.g. "anthropic-prod"
	Provider     string `yaml:"provider"      json:"provider"`                // e.g. "anthropic", "fireworks", "moonshot"
	APIKey       string `yaml:"api_key"       json:"-"`                       // the actual key (never exposed in API)
	Default      bool   `yaml:"default"       json:"default"`                 // use when no llm_key specified
	DefaultModel string `yaml:"default_model" json:"default_model,omitempty"` // preferred model for this key, e.g. "fireworks/accounts/fireworks/models/kimi-k2p7-code"
	AuthProfile  string `yaml:"auth_profile,omitempty" json:"auth_profile,omitempty"`
}

type ModelAuthProfileConfig struct {
	Name      string `yaml:"name" json:"name"`
	Provider  string `yaml:"provider" json:"provider"`
	Mode      string `yaml:"mode" json:"mode"`
	AuthState string `yaml:"auth_state,omitempty" json:"-"`
	UpdatedAt string `yaml:"updated_at,omitempty" json:"updated_at,omitempty"`
}

// LLMKeysList is a custom YAML type that handles both the legacy flat-map format
// and the current named-slice format for llm_keys in hub.yaml.
type LLMKeysList []*LLMKeyConfig

func (l *LLMKeysList) UnmarshalYAML(value *yaml.Node) error {
	// Try new slice format first
	var slice []*LLMKeyConfig
	if err := value.Decode(&slice); err == nil {
		*l = slice
		return nil
	}
	// Fall back to old flat map format: {provider: apiKey}
	var flat map[string]string
	if err := value.Decode(&flat); err != nil {
		return fmt.Errorf("llm_keys: expected list of {name,provider,api_key} or flat map {provider: key}: %w", err)
	}
	// Sort providers for deterministic ordering
	providers := make([]string, 0, len(flat))
	for p, k := range flat {
		if k != "" {
			providers = append(providers, p)
		}
	}
	sort.Strings(providers)
	result := make([]*LLMKeyConfig, len(providers))
	for i, p := range providers {
		result[i] = &LLMKeyConfig{
			Name:     p,
			Provider: p,
			APIKey:   flat[p],
			Default:  i == 0,
		}
	}
	*l = result
	return nil
}

// EnvVarName returns the environment variable name for this provider's API key.
func (k *LLMKeyConfig) EnvVarName() string {
	switch k.Provider {
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	case "fireworks":
		return "FIREWORKS_API_KEY"
	case "codex":
		return "CODEX_API_KEY"
	case "grok":
		return "XAI_API_KEY"
	default:
		// Generic: PROVIDER_API_KEY uppercased
		return strings.ToUpper(k.Provider) + "_API_KEY"
	}
}

// TemplateConfig is the elasticclaw-config.yaml inside a template directory.
type TemplateConfig struct {
	// SchemaVersion is the schema version of this config file.
	// Defaults to "v1" if not specified for backward compatibility.
	SchemaVersion  string               `yaml:"schema_version,omitempty"`
	Name           string               `yaml:"name,omitempty"`
	Repositories   RepositoryAccessList `yaml:"repositories,omitempty"`
	Env            WorkspaceEnv         `yaml:"env,omitempty"`
	WebhookSecrets []string             `yaml:"webhook_secrets,omitempty"`
	Provider       string               `yaml:"provider"`
	Resources      TemplateResources    `yaml:"resources,omitempty"`
	InstanceType   string               `yaml:"instance_type,omitempty"` // e.g. r1.large for Replicated
	Image          string               `yaml:"image,omitempty"`
	TTL            string               `yaml:"ttl,omitempty"`
	// DefaultModel overrides the hub-level default model for this template.
	// Format: provider/model, e.g. anthropic/claude-opus-4-5
	DefaultModel string `yaml:"default_model,omitempty"`
	// LLMKey specifies which named LLM key (from hub.yaml llm_keys) to use.
	// Omit to use the hub default key.
	LLMKey string `yaml:"llm_key,omitempty"`
	// Snapshot is the Daytona snapshot name (e.g. "daytona-medium", "daytona-large").
	// Overrides hub providers.daytona.default_snapshot.
	Snapshot string `yaml:"snapshot,omitempty"`
	// GitHub specifies GitHub repos this template's claw needs access to.
	GitHub *GitHubTemplateConfig `yaml:"github,omitempty"`
	// Linear specifies which Linear workspace this template's claw should use.
	Linear *LinearTemplateConfig `yaml:"linear,omitempty"`
	// Nix installs the Determinate Systems variant of Nix during bootstrap.
	// Adds ~2-3 min to bootstrap time. Opt-in only.
	Nix bool `yaml:"nix,omitempty"`
	// Docker installs Docker Engine (via the official Docker apt repo) during
	// bootstrap. Useful for projects that need docker build/run but can't
	// include Docker in a Nix flake for portability. Opt-in only.
	Docker bool `yaml:"docker,omitempty"`
	// Tags are static labels applied to every claw created from this template.
	// Merged with the auto template=<name> tag and any --tag CLI flags.
	Tags []string `yaml:"tags,omitempty"`
	// Color sets the accent color for this claw in the UI.
	// One of: slate, red, orange, amber, lime, green, emerald, teal,
	//         cyan, sky, blue, indigo, violet, purple, pink, rose
	// If unset, a color is auto-assigned from the claw name.
	Color string `yaml:"color,omitempty"`
	// Secrets is a list of secret references to inject as environment variables
	// into the claw. Each entry can be a plain string (legacy: secret name from
	// hub.yaml secrets, injected with that name) or a typed SecretRef that
	// resolves the right secret and maps it to the correct env var name.
	// DEPRECATED: Use secret_refs instead. Kept for backward compatibility.
	Secrets SecretRefList `yaml:"secrets,omitempty"`
	// SecretRefs maps env var names to hub secret names to inject into claws
	// created from this template. Resolved at claw creation time.
	// Example: {LINEAR_API_KEY: linear_api_key}
	SecretRefs map[string]string `yaml:"secret_refs,omitempty"`
	// MCPs is a list of MCP server names from hub.yaml mcp_servers to enable
	// in this template's claws. Each claw will start these MCP servers as
	// subprocesses and register their tools with the gateway.
	MCPs []MCPRef `yaml:"mcps,omitempty"`
}

// GetVersion returns the effective schema version for this template config.
// Returns "v1" if SchemaVersion is not specified for backward compatibility.
func (c *TemplateConfig) GetVersion() string {
	if c.SchemaVersion != "" {
		return c.SchemaVersion
	}
	return "v1"
}

type TemplateResources struct {
	CPU    string `yaml:"cpu,omitempty"`
	Memory string `yaml:"memory,omitempty"`
	Disk   string `yaml:"disk,omitempty"`
}

// LinearConfig holds a Linear API token for one workspace.
// Configure in hub.yaml under 'linear:' as a list.
type LinearConfig struct {
	Token     string `yaml:"token"`               // Linear API key (lin_api_...)
	Workspace string `yaml:"workspace,omitempty"` // human label for matching; defaults to first entry
}

// LinearTemplateConfig specifies which Linear workspace a template's claw should use.
type LinearTemplateConfig struct {
	Workspace string `yaml:"workspace,omitempty"` // match against hub.yaml linear[].workspace
	Team      string `yaml:"team,omitempty"`      // optional team name for context
}

// SecretRef is a typed reference to a secret that should be injected into a claw.
// Use in template/factory secrets lists to resolve the right secret and map it
// to the correct env var name without hardcoding token values or env var names.
type SecretRef struct {
	// Type is the secret kind: "linear", "shortcut", "github", or "custom".
	// For integration types (linear, shortcut, github), the hub resolves the
	// actual token from integrations config using Workspace as a selector.
	// For "custom", Name is the key in HubConfig.Secrets.
	Type string `yaml:"type"`
	// Name is the hub secret key for type="custom". Ignored for integration types.
	Name string `yaml:"name,omitempty"`
	// Workspace selects which integration workspace to use (for linear, shortcut).
	// If empty, the first matching integration config is used.
	Workspace string `yaml:"workspace,omitempty"`
	// As is the env var name to inject the secret as. If empty, a default is
	// used based on Type (e.g. LINEAR_API_KEY for linear).
	As string `yaml:"as,omitempty"`
}

// EnvVarName returns the environment variable name for this secret ref.
// Returns "" for invalid refs (e.g. custom with no name).
func (r SecretRef) EnvVarName() string {
	if r.As != "" {
		return r.As
	}
	switch r.Type {
	case "linear":
		return "LINEAR_API_KEY"
	case "shortcut":
		return "SHORTCUT_API_KEY"
	case "github-issues":
		return "GITHUB_ISSUES_API_KEY"
	case "jira":
		return "JIRA_API_KEY"
	case "github":
		return "GITHUB_TOKEN"
	case "custom":
		if r.Name == "" {
			return ""
		}
		return strings.ToUpper(r.Name)
	default:
		return ""
	}
}

// SecretRefList is a custom YAML type that accepts both legacy plain strings
// and typed SecretRef objects in the same list.
type SecretRefList []SecretRef

func (l *SecretRefList) UnmarshalYAML(value *yaml.Node) error {
	// Try list of SecretRef first
	var refs []SecretRef
	if err := value.Decode(&refs); err == nil {
		*l = refs
		return nil
	}
	// Fall back to legacy list of strings: each string becomes a custom SecretRef
	var names []string
	if err := value.Decode(&names); err != nil {
		return fmt.Errorf("secrets: expected list of {type,workspace,name,as} or legacy list of secret names: %w", err)
	}
	refs = make([]SecretRef, len(names))
	for i, n := range names {
		refs[i] = SecretRef{Type: "custom", Name: n}
	}
	*l = refs
	return nil
}

// MCPSource is the installation source for an MCP server.
type MCPSource string

const (
	MCPSourceNpx      MCPSource = "npx"
	MCPSourceUvx      MCPSource = "uvx"
	MCPSourceSmithery MCPSource = "smithery"
	MCPSourceDocker   MCPSource = "docker"
	MCPSourceSSE      MCPSource = "sse"
)

// MCPServerHubConfig defines an MCP server available to claws.
// Configured in hub.yaml under 'mcp_servers:' as a list.
type MCPServerHubConfig struct {
	Name    string            `yaml:"name"`              // unique identifier, e.g. "github"
	Source  MCPSource         `yaml:"source"`            // npx, uvx, smithery, docker, sse
	Package string            `yaml:"package,omitempty"` // for npx/uvx/smithery: e.g. "@modelcontextprotocol/server-github"
	Image   string            `yaml:"image,omitempty"`   // for docker: e.g. "mcp/postgres"
	URL     string            `yaml:"url,omitempty"`     // for sse: the SSE endpoint URL
	Enabled bool              `yaml:"enabled"`           // default true
	Config  map[string]string `yaml:"config,omitempty"`  // non-secret env vars (e.g. {"repository": "owner/repo"})
	// Secrets maps env var name → secret ref name in HubConfig.Secrets.
	// Example: {"GITHUB_TOKEN": "github_token"} resolves HubConfig.Secrets["github_token"]
	// and injects it as GITHUB_TOKEN in the MCP server's environment.
	Secrets map[string]string `yaml:"secrets,omitempty"`
	// Command overrides the default command for stdio-based sources.
	// If empty, the hub generates: ["npx", "-y", Package] or ["uvx", Package] etc.
	Command []string `yaml:"command,omitempty"`
}

// MCPRef references an MCP server in a template config.
type MCPRef struct {
	Name   string            `yaml:"name"`             // matches MCPServerHubConfig.Name
	Config map[string]string `yaml:"config,omitempty"` // template-level overrides
}

// GitHubAppConfig holds GitHub App credentials for one GitHub App.
// Configure in hub.yaml under 'github:' as a list.
type GitHubAppConfig struct {
	URL           string `yaml:"url,omitempty"` // GitHub App URL for reference/logging only
	AppID         int64  `yaml:"app_id"`
	PrivateKeyPEM string `yaml:"private_key_pem"` // PEM-encoded RSA private key (paste directly in yaml)
}

// GitHubRepoAccess specifies a repo and the permissions needed. Workspace
// repository lists may also use glob patterns.
type GitHubRepoAccess struct {
	Repo        string `yaml:"repo"        json:"repo"`        // e.g. "owner/repo", "*-infra-*", or "owner/*"
	Permissions string `yaml:"permissions" json:"permissions"` // "read" or "write" (default: "read")
}

// RepositoryAccessList accepts the current object form:
//
//	repositories:
//	  - repo: owner/repo
//	    permissions: write
//	  - repo: "*-infra-*"
//	    permissions: read
//
// and the earlier shorthand form:
//
//	repositories:
//	  - owner/repo
type RepositoryAccessList []GitHubRepoAccess

func (l *RepositoryAccessList) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.SequenceNode {
		return fmt.Errorf("repositories: expected a list")
	}
	out := make([]GitHubRepoAccess, 0, len(value.Content))
	for i, item := range value.Content {
		switch item.Kind {
		case yaml.ScalarNode:
			out = append(out, GitHubRepoAccess{Repo: strings.TrimSpace(item.Value), Permissions: "read"})
		case yaml.MappingNode:
			var repo GitHubRepoAccess
			if err := item.Decode(&repo); err != nil {
				return fmt.Errorf("repositories[%d]: %w", i, err)
			}
			if repo.Permissions == "" {
				repo.Permissions = "read"
			}
			out = append(out, repo)
		default:
			return fmt.Errorf("repositories[%d]: expected repo string or {repo, permissions}", i)
		}
	}
	*l = out
	return nil
}

func (l *RepositoryAccessList) UnmarshalJSON(data []byte) error {
	var raw []interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	out := make([]GitHubRepoAccess, 0, len(raw))
	for i, item := range raw {
		switch v := item.(type) {
		case string:
			out = append(out, GitHubRepoAccess{Repo: strings.TrimSpace(v), Permissions: "read"})
		case map[string]interface{}:
			repo, _ := v["repo"].(string)
			permissions, _ := v["permissions"].(string)
			if permissions == "" {
				permissions = "read"
			}
			out = append(out, GitHubRepoAccess{Repo: strings.TrimSpace(repo), Permissions: permissions})
		default:
			return fmt.Errorf("repositories[%d]: expected repo string or {repo, permissions}", i)
		}
	}
	*l = out
	return nil
}

// WorkspaceEnv maps environment variable names to either inline values or hub
// secret references.
type WorkspaceEnv map[string]WorkspaceEnvVar

type WorkspaceEnvVar struct {
	Value  string `yaml:"value,omitempty" json:"value,omitempty"`
	Secret string `yaml:"secret,omitempty" json:"secret,omitempty"`
}

func (v WorkspaceEnvVar) MarshalYAML() (interface{}, error) {
	if v.Secret == "" {
		return v.Value, nil
	}
	return struct {
		Secret string `yaml:"secret,omitempty"`
		Value  string `yaml:"value,omitempty"`
	}{
		Secret: v.Secret,
		Value:  v.Value,
	}, nil
}

func (e *WorkspaceEnv) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("env: expected a map")
	}
	out := WorkspaceEnv{}
	for i := 0; i+1 < len(value.Content); i += 2 {
		key := strings.TrimSpace(value.Content[i].Value)
		if key == "" {
			return fmt.Errorf("env: variable name cannot be empty")
		}
		valNode := value.Content[i+1]
		switch valNode.Kind {
		case yaml.ScalarNode:
			out[key] = WorkspaceEnvVar{Value: valNode.Value}
		case yaml.MappingNode:
			var envVar WorkspaceEnvVar
			if err := valNode.Decode(&envVar); err != nil {
				return fmt.Errorf("env.%s: %w", key, err)
			}
			out[key] = envVar
		default:
			return fmt.Errorf("env.%s: expected value or {value, secret}", key)
		}
	}
	*e = out
	return nil
}

// GitHubTemplateConfig specifies GitHub access needed by a template.
type GitHubTemplateConfig struct {
	Repos []GitHubRepoAccess `yaml:"repos" json:"repos"`
}

// HubConfig is used in two contexts:
//   - ~/.elasticclaw/hub.yaml: CLI connection + full server config
//
// BrandingConfig controls white-label appearance of the web UI.
type BrandingConfig struct {
	// AppName replaces "ElasticClaw" in the top-left header and page title.
	AppName string `yaml:"app_name,omitempty" json:"appName,omitempty"`
	// LogoURL is a URL to an externally-hosted image used as the empty-state mascot.
	// Replaces the default lobster mascot PNG.
	LogoURL string `yaml:"logo_url,omitempty" json:"logoUrl,omitempty"`
}

// AuthConfig holds GitHub OAuth and access control config for the hub web UI.
type AuthConfig struct {
	GitHubOAuth         *GitHubOAuthConfig `yaml:"github_oauth,omitempty"`
	Access              *AccessConfig      `yaml:"access,omitempty"`
	SessionSecret       string             `yaml:"session_secret,omitempty"`
	DisablePasswordAuth bool               `yaml:"disable_password_auth,omitempty"`
}

// GitHubOAuthConfig holds GitHub OAuth app credentials and allowlist.
type GitHubOAuthConfig struct {
	ClientID     string   `yaml:"client_id"`
	ClientSecret string   `yaml:"client_secret"`
	AllowedUsers []string `yaml:"allowed_users,omitempty"` // GitHub logins
	AllowedOrgs  []string `yaml:"allowed_orgs,omitempty"`  // GitHub org names
	AllowedTeams []string `yaml:"allowed_teams,omitempty"` // "org/team" format
}

// AccessConfig holds tag-based RBAC rules for the hub web UI.
type AccessConfig struct {
	Admins               []string `yaml:"admins,omitempty"`                 // GitHub logins — bypass all tag checks
	ViewRequiresTags     []string `yaml:"view_requires_tags,omitempty"`     // any matching tag grants view
	InteractRequiresTags []string `yaml:"interact_requires_tags,omitempty"` // any matching tag grants interact
}

// ArtifactStorageConfig selects the hub-owned artifact store used for
// content-addressed blobs, manifests, and refs.
type ArtifactStorageConfig struct {
	Backend string                      `yaml:"backend,omitempty" json:"backend,omitempty"` // local or s3
	Local   *LocalArtifactStorageConfig `yaml:"local,omitempty" json:"local,omitempty"`
	S3      *S3ArtifactStorageConfig    `yaml:"s3,omitempty" json:"s3,omitempty"`
}

type LocalArtifactStorageConfig struct {
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
}

type S3ArtifactStorageConfig struct {
	Bucket          string `yaml:"bucket" json:"bucket"`
	Region          string `yaml:"region,omitempty" json:"region,omitempty"`
	Endpoint        string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	Prefix          string `yaml:"prefix,omitempty" json:"prefix,omitempty"`
	AccessKeyID     string `yaml:"access_key_id,omitempty" json:"accessKeyId,omitempty"`
	SecretAccessKey string `yaml:"secret_access_key,omitempty" json:"secretAccessKey,omitempty"`
	SessionToken    string `yaml:"session_token,omitempty" json:"sessionToken,omitempty"`
	PathStyle       bool   `yaml:"path_style,omitempty" json:"pathStyle,omitempty"`
}

type HubConfig struct {
	// SchemaVersion is the schema version of this hub config file.
	// Defaults to "v1" if not specified for backward compatibility.
	SchemaVersion string `yaml:"schema_version,omitempty"`

	// CLI connection fields
	URL   string `yaml:"url"`
	Token string `yaml:"token"`

	// Hub server fields
	// PublicURL is the URL claws use to connect back to the hub from remote VMs.
	// If not set, falls back to URL.
	PublicURL string                    `yaml:"public_url,omitempty"`
	ClawToken string                    `yaml:"claw_token,omitempty"`
	Providers map[string]ProviderConfig `yaml:"providers,omitempty"`
	// SSHPublicKeys are extra keys added to every provisioned VM for debug access.
	SSHPublicKeys []string `yaml:"ssh_public_keys,omitempty"`
	// BridgeImage is the OCI artifact reference for the claw-bridge binary.
	// Defaults to ghcr.io/elasticclaw/claw-bridge:latest if not set.
	BridgeImage string `yaml:"bridge_image,omitempty"`
	// DefaultModel is the model used by all claws unless overridden in the template.
	// Format: provider/model, e.g. anthropic/claude-sonnet-4-6
	DefaultModel string `yaml:"default_model,omitempty"`
	// LLMKeys is a list of named LLM API keys. One can be marked default:true.
	// Legacy flat map {"anthropic": "sk-..."} is still accepted for backwards compat.
	LLMKeys LLMKeysList `yaml:"llm_keys,omitempty"`
	// ModelAuthProfiles stores CLI-backed model login state for providers such as Codex and Grok.
	ModelAuthProfiles []*ModelAuthProfileConfig `yaml:"model_auth_profiles,omitempty"`
	// Linear is a list of Linear workspace configs for injecting API tokens into claws.
	Linear []*LinearConfig `yaml:"linear,omitempty"`

	// GitHubApps is a list of GitHub App configs for minting installation tokens.
	// The hub tries each app to find one whose installation covers the requested repos.
	// One app can cover multiple orgs if installed on all of them.
	// Example:
	//   github_apps:
	//     - app_id: 123456
	//       private_key_pem: |
	//         -----BEGIN RSA PRIVATE KEY-----
	//         ...
	//         -----END RSA PRIVATE KEY-----
	GitHubApps []*GitHubAppConfig `yaml:"github,omitempty"`

	// UIPassword is the password for the web UI. If not set, defaults to "admin".
	UIPassword string `yaml:"ui_password,omitempty"`

	// Branding allows white-labeling the web UI.
	Branding *BrandingConfig `yaml:"branding,omitempty"`

	// Integrations holds external service configs (Linear, future: Shortcut, etc.)
	Integrations *IntegrationsConfig `yaml:"integrations,omitempty"`

	// Factories defines automation rules that spin up claws based on integration events.
	Factories []*FactoryConfig `yaml:"factories,omitempty"`
	// Secrets is a named map of secret values referenced by factories via webhook_secret_ref.
	Secrets map[string]string `yaml:"secrets,omitempty"`

	// MCPServers is a list of MCP server configurations available to claws.
	MCPServers []*MCPServerHubConfig `yaml:"mcp_servers,omitempty"`

	// Auth holds GitHub OAuth and access control config for the hub web UI.
	Auth *AuthConfig `yaml:"auth,omitempty"`

	// ArtifactStorage configures the hub-owned artifact store used by
	// checkpoints, volumes, and future large artifacts.
	ArtifactStorage *ArtifactStorageConfig `yaml:"artifact_storage,omitempty" json:"artifactStorage,omitempty"`

	// ConcurrencyGroups limits the number of simultaneously running claws per group.
	// Each group has a name and a limit. 0 means unlimited.
	// Factories can be assigned to a group; unassigned factories use the "global" group.
	ConcurrencyGroups []*ConcurrencyGroup `yaml:"concurrency_groups,omitempty" json:"concurrencyGroups,omitempty"`

	// MaxConcurrentClaws limits the number of simultaneously running claws.
	// DEPRECATED: Use ConcurrencyGroups instead. Kept for backward compat.
	// When the limit is reached, new factory-created claws enter 'pending' status
	// and are promoted to 'provisioning' when a running claw terminates.
	// 0 means unlimited (default).
	MaxConcurrentClaws int `yaml:"max_concurrent_claws,omitempty" json:"maxConcurrentClaws"`

	// Liveness configures recovery of claws and runs stranded by hub or
	// infrastructure failures.
	Liveness *LivenessConfig `yaml:"liveness,omitempty" json:"liveness,omitempty"`
}

// LivenessConfig controls boot reconciliation and the periodic safety-net reaper.
// Durations use Go duration strings. Empty values receive conservative defaults.
type LivenessConfig struct {
	Enabled                *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	OfflineGrace           string `yaml:"offline_grace,omitempty" json:"offlineGrace,omitempty"`
	ProvisioningMaxAge     string `yaml:"provisioning_max_age,omitempty" json:"provisioningMaxAge,omitempty"`
	ClaimTTL               string `yaml:"claim_ttl,omitempty" json:"claimTtl,omitempty"`
	ReaperInterval         string `yaml:"reaper_interval,omitempty" json:"reaperInterval,omitempty"`
	GatewayUnhealthyChecks *int   `yaml:"gateway_unhealthy_checks,omitempty" json:"gatewayUnhealthyChecks,omitempty"`
	BusyTurnMax            string `yaml:"busy_turn_max,omitempty" json:"busyTurnMax,omitempty"`
	SilentDeathMax         string `yaml:"silent_death_max,omitempty" json:"silentDeathMax,omitempty"`
	// PRConditionsMaxWait is the maximum time a PR may wait for pr_conditions before the run errors.
	PRConditionsMaxWait string `yaml:"pr_conditions_max_wait,omitempty" json:"prConditionsMaxWait,omitempty"`
}

// IntegrationsConfig holds configs for external integrations.
type IntegrationsConfig struct {
	Linear       []*LinearIntegrationConfig       `yaml:"linear,omitempty"`
	Shortcut     []*ShortcutIntegrationConfig     `yaml:"shortcut,omitempty"`
	GitHubIssues []*GitHubIssuesIntegrationConfig `yaml:"github_issues,omitempty"`
	Jira         []*JiraIntegrationConfig         `yaml:"jira,omitempty"`
}

// ExternalTriggerConfig holds config for external webhook triggers.
// Unlike other integrations, this doesn't need API tokens - just webhook secrets.
type ExternalTriggerConfig struct {
	Name          string `yaml:"name" json:"name"`                                        // unique identifier for this trigger source
	WebhookSecret string `yaml:"webhook_secret,omitempty" json:"webhookSecret,omitempty"` // HMAC secret for validating webhooks
}

// ShortcutIntegrationConfig holds credentials for one Shortcut workspace.
type ShortcutIntegrationConfig struct {
	Workspace string `yaml:"workspace"` // human label
	Token     string `yaml:"token"`     // Shortcut API token
}

// LinearIntegrationConfig holds credentials for one Linear workspace.
type LinearIntegrationConfig struct {
	Workspace     string `yaml:"workspace"`                // human label
	Token         string `yaml:"token"`                    // Linear API token (lin_api_...)
	WebhookSecret string `yaml:"webhook_secret,omitempty"` // HMAC secret for validating webhooks
}

// GitHubIssuesIntegrationConfig holds credentials for one GitHub Issues integration.
type GitHubIssuesIntegrationConfig struct {
	Workspace     string `yaml:"workspace"`                // human label (e.g. "my-org")
	Token         string `yaml:"token"`                    // GitHub personal access token
	WebhookSecret string `yaml:"webhook_secret,omitempty"` // HMAC secret for validating webhooks
}

// JiraIntegrationConfig holds credentials for one Jira site.
type JiraIntegrationConfig struct {
	Workspace     string `yaml:"workspace"`                // human label
	BaseURL       string `yaml:"base_url,omitempty"`       // Jira base URL, e.g. https://jira.example.com
	Username      string `yaml:"username,omitempty"`       // optional for basic auth
	Token         string `yaml:"token"`                    // Jira PAT/API token
	WebhookSecret string `yaml:"webhook_secret,omitempty"` // shared webhook secret
}

// FactoryInput defines a user-provided input for manual factory triggers.
type FactoryInput struct {
	Name        string   `yaml:"name" json:"name"`
	Type        string   `yaml:"type" json:"type"` // "string", "number", "bool", "enum"
	Required    bool     `yaml:"required,omitempty" json:"required"`
	Default     string   `yaml:"default,omitempty" json:"default,omitempty"`
	Description string   `yaml:"description,omitempty" json:"description,omitempty"`
	Options     []string `yaml:"options,omitempty" json:"options,omitempty"`       // for enum type
	Validation  string   `yaml:"validation,omitempty" json:"validation,omitempty"` // regex pattern
	Min         *float64 `yaml:"min,omitempty" json:"min,omitempty"`               // inclusive minimum for number type
	Max         *float64 `yaml:"max,omitempty" json:"max,omitempty"`               // inclusive maximum for number type
}

// ConcurrencyGroup limits the number of simultaneously running claws per group.
type ConcurrencyGroup struct {
	Name  string `yaml:"name" json:"name"`
	Limit int    `yaml:"limit" json:"limit"` // 0 = unlimited
}

// FactoryConfig defines an automation rule that creates claws based on integration events.
type FactoryConfig struct {
	// SchemaVersion is the schema version of this config file.
	// Defaults to "v1" if not specified for backward compatibility.
	SchemaVersion    string   `yaml:"schema_version,omitempty" json:"schemaVersion,omitempty"`
	Name             string   `yaml:"name" json:"name"`
	Enabled          *bool    `yaml:"enabled,omitempty" json:"enabled,omitempty"`                       // nil = true (default on); set false to pause
	Integration      string   `yaml:"integration" json:"integration"`                                   // "linear", "shortcut", "github-issues", or "github"
	Workspace        string   `yaml:"workspace,omitempty" json:"workspace,omitempty"`                   // matches integrations.<type>[].workspace
	Team             string   `yaml:"team,omitempty" json:"team,omitempty"`                             // Linear team key (e.g. "ELA")
	TriggerStatus    string   `yaml:"trigger_status,omitempty" json:"trigger_status,omitempty"`         // entering this status → create claw
	WorkingStatus    string   `yaml:"working_status,omitempty" json:"working_status,omitempty"`         // claw moves issue here when it starts working
	FinishedStatus   string   `yaml:"finished_status,omitempty" json:"finished_status,omitempty"`       // claw moves issue here when it finishes working
	DoneStatus       string   `yaml:"done_status,omitempty" json:"done_status,omitempty"`               // claw moves issue here when done (PR merged)
	AgentStatusError string   `yaml:"agent_status_error,omitempty" json:"agent_status_error,omitempty"` // claw moves issue here when agent stops with an error
	TerminateOnLeave bool     `yaml:"terminate_on_leave,omitempty" json:"terminate_on_leave,omitempty"` // leaving trigger_status → kill claw
	Template         string   `yaml:"template" json:"template"`                                         // template name (must be pushed to hub)
	Provider         string   `yaml:"provider,omitempty" json:"provider,omitempty"`                     // override the default provider for this factory
	NamePattern      string   `yaml:"name_pattern,omitempty" json:"name_pattern,omitempty"`             // claw name pattern, e.g. "{issue_id}"
	WebhookSecret    string   `yaml:"webhook_secret,omitempty" json:"webhook_secret,omitempty"`         // HMAC-SHA256 secret for validating webhooks
	Tags             []string `yaml:"tags,omitempty" json:"tags,omitempty"`                             // tags applied to created claws
	Color            string   `yaml:"color,omitempty" json:"color,omitempty"`                           // color applied to created claws
	RunKind          string   `yaml:"run_kind,omitempty" json:"run_kind,omitempty"`                     // analytics run kind: "code_task" or "pr_task"
	AnalyticsEnabled *bool    `yaml:"analytics_enabled,omitempty" json:"analytics_enabled,omitempty"`   // nil = infer from integration
	RequiresPR       *bool    `yaml:"requires_pr,omitempty" json:"requires_pr,omitempty"`               // nil = infer from analytics eligibility
	// Labels: all must be present on the issue to trigger (AND)
	Labels []string `yaml:"labels,omitempty" json:"labels,omitempty"`
	// ExcludeLabels: none may be present on the issue to trigger (NOT)
	ExcludeLabels []string `yaml:"exclude_labels,omitempty" json:"exclude_labels,omitempty"`
	// AssignedTo filter: "@user", "!@user" (exclude), "any", "none"
	AssignedTo string `yaml:"assigned_to,omitempty" json:"assigned_to,omitempty"`
	// Projects limits Jira factory triggers to these project keys.
	Projects []string `yaml:"projects,omitempty" json:"projects,omitempty"`
	// AllowedLabelers restricts who can trigger claw creation by labeling an issue.
	// Only users in this list (GitHub logins, case-insensitive) can trigger.
	// If empty, any user with label permissions can trigger.
	AllowedLabelers []string `yaml:"allowed_labelers,omitempty" json:"allowed_labelers,omitempty"`
	// WebhookSecretRef is a named key in HubConfig.Secrets (use instead of inline WebhookSecret for repo-defined factories)
	WebhookSecretRef string `yaml:"webhook_secret_ref,omitempty" json:"webhook_secret_ref,omitempty"`
	// PipelineYAML is the raw pipeline.yaml content stored alongside this factory
	PipelineYAML string `yaml:"pipeline_yaml,omitempty" json:"pipeline_yaml,omitempty"`
	// Inputs are user-defined parameters for manual factory triggers (CLI/UI).
	// Not used by webhook-triggered factories.
	Inputs []FactoryInput `yaml:"inputs,omitempty" json:"inputs,omitempty"`
	// ConcurrencyGroup assigns this factory to a concurrency group.
	// If empty, the factory uses the "global" group.
	ConcurrencyGroup string `yaml:"concurrency_group,omitempty" json:"concurrency_group,omitempty"`
	// EnableManualTrigger allows this factory to be triggered manually from the dashboard.
	EnableManualTrigger bool `yaml:"enable_manual_trigger,omitempty" json:"enable_manual_trigger,omitempty"`
	// SecretRefs maps env var names to hub secret names to inject into claws
	// created by this factory. Resolved at claw creation time.
	SecretRefs map[string]string `yaml:"secret_refs,omitempty" json:"secret_refs,omitempty"`
	// GitHub factory fields (integration: github)
	Repos        []string       `yaml:"repos,omitempty" json:"repos,omitempty"`                 // legacy trigger repos for GitHub factories
	TriggerRepos []string       `yaml:"trigger_repos,omitempty" json:"trigger_repos,omitempty"` // repos/orgs allowed to trigger this factory, e.g. ["can-io/canio", "can-io/*"]
	Trigger      *GitHubTrigger `yaml:"trigger,omitempty" json:"trigger,omitempty"`

	// External trigger fields (integration: external)
	// ExternalTrigger defines what external event triggers this factory.
	ExternalTrigger *ExternalTrigger `yaml:"external_trigger,omitempty" json:"external_trigger,omitempty"`
}

// GetVersion returns the effective schema version for this factory config.
// Returns "v1" if SchemaVersion is not specified for backward compatibility.
func (c *FactoryConfig) GetVersion() string {
	if c.SchemaVersion != "" {
		return c.SchemaVersion
	}
	return "v1"
}

// GitHubTrigger defines what GitHub event triggers this factory.
type GitHubTrigger struct {
	On     string               `yaml:"on" json:"on"`         // "pull_request" | "issue"
	Action string               `yaml:"action" json:"action"` // "opened" | "synchronize" | "reopened" | "closed"
	Filter *GitHubTriggerFilter `yaml:"filter,omitempty" json:"filter,omitempty"`
}

// GitHubTriggerFilter further constrains which events match the trigger.
type GitHubTriggerFilter struct {
	Author     string `yaml:"author,omitempty" json:"author,omitempty"`           // e.g. "dependabot[bot]"
	BaseBranch string `yaml:"base_branch,omitempty" json:"base_branch,omitempty"` // e.g. "main"
}

// ExternalTrigger defines what external event triggers this factory.
// This is used for generic webhooks and external service notifications.
type ExternalTrigger struct {
	// Source is the external source type: "github-release", "generic-webhook", etc.
	Source string `yaml:"source" json:"source"`
	// Filter defines matching criteria for the external event.
	Filter *ExternalTriggerFilter `yaml:"filter,omitempty" json:"filter,omitempty"`
}

// ExternalTriggerFilter defines matching criteria for external triggers.
type ExternalTriggerFilter struct {
	// Repository matches against the source repository (e.g., "postgres/postgres")
	Repository string `yaml:"repository,omitempty" json:"repository,omitempty"`
	// EventType matches the event type (e.g., "released", "published")
	EventType string `yaml:"event_type,omitempty" json:"event_type,omitempty"`
	// TagPattern is a glob pattern for matching release tags (e.g., "v*.*.*")
	TagPattern string `yaml:"tag_pattern,omitempty" json:"tag_pattern,omitempty"`
}

type ProviderConfig struct {
	// Type identifies the provider kind (e.g. "noop" for tests).
	Type string `yaml:"type,omitempty"`

	// Daytona
	APIURL string `yaml:"api_url,omitempty"`
	APIKey string `yaml:"api_key,omitempty"`
	Target string `yaml:"target,omitempty"`

	// Daytona snapshot size (maps to Daytona SnapshotParams.Snapshot)
	// Use default_snapshot in hub.yaml; template can override with 'snapshot:' field
	DefaultSnapshot string `yaml:"default_snapshot,omitempty"`

	// Vercel Sandbox
	AccessToken string `yaml:"access_token,omitempty"` // Vercel access token
	TeamID      string `yaml:"team_id,omitempty"`      // optional Vercel team ID
	ProjectID   string `yaml:"project_id,omitempty"`   // optional Vercel project ID

	// Replicated CMX
	Token               string `yaml:"token,omitempty"`
	DefaultTTL          string `yaml:"default_ttl,omitempty"`
	DefaultInstanceType string `yaml:"default_instance_type,omitempty"`
	// SSHPublicKey is injected automatically from the hub's generated identity — do not configure manually.
	SSHPublicKey string `yaml:"-"`
	// ExtraSSHPublicKeys are additional keys from hub config's ssh_public_keys list.
	ExtraSSHPublicKeys []string `yaml:"-"`

	// exe.dev
	SSHKeyPath string `yaml:"ssh_key_path,omitempty"` // optional SSH private key path
	// Default resources for new VMs
	DefaultCPU    int    `yaml:"default_cpu,omitempty"`
	DefaultMemory string `yaml:"default_memory,omitempty"` // e.g. "4GB"
	DefaultDisk   string `yaml:"default_disk,omitempty"`   // e.g. "20GB"

	// docker (local) provider
	// Image is the agent container image to run (default: elasticclaw/claw-agent:dev).
	Image string `yaml:"image,omitempty"`
	// Network is the Docker network to attach agent containers to so they can
	// reach the hub by service name (e.g. "elasticclaw-dev").
	Network string `yaml:"network,omitempty"`

	// AWS Lambda MicroVMs (alpha)
	AWSRegion                  string   `yaml:"aws_region,omitempty"`
	AWSProfile                 string   `yaml:"aws_profile,omitempty"`
	ImageIdentifier            string   `yaml:"image_identifier,omitempty"`
	ImageVersion               string   `yaml:"image_version,omitempty"`
	ExecutionRoleARN           string   `yaml:"execution_role_arn,omitempty"`
	IngressNetworkConnectors   []string `yaml:"ingress_network_connectors,omitempty"`
	EgressNetworkConnectors    []string `yaml:"egress_network_connectors,omitempty"`
	IdleMaxDurationSeconds     int      `yaml:"idle_max_duration_seconds,omitempty"`
	SuspendedDurationSeconds   int      `yaml:"suspended_duration_seconds,omitempty"`
	AutoResume                 *bool    `yaml:"auto_resume,omitempty"`
	MaximumDurationSeconds     int      `yaml:"maximum_duration_seconds,omitempty"`
	MicroVMBridgePort          int      `yaml:"bridge_port,omitempty"`
	AuthTokenExpirationMinutes int      `yaml:"auth_token_expiration_minutes,omitempty"`
}

// MCPConfig is the resolved MCP server configuration passed to a claw at creation time.
// Secrets are resolved from hub.yaml Secrets map before sending.
type MCPConfig struct {
	Name    string            `json:"name"`
	Command []string          `json:"command"`
	Env     map[string]string `json:"env,omitempty"`
}

// CreateClawRequest is POSTed by the CLI to the hub to provision a new claw.
type CreateClawRequest struct {
	Name         string            `json:"name"`
	TemplateName string            `json:"template_name"`
	Provider     string            `json:"provider"`
	Resources    TemplateResources `json:"resources,omitempty"`
	InstanceType string            `json:"instance_type,omitempty"`
	Image        string            `json:"image,omitempty"`
	TTL          string            `json:"ttl,omitempty"`
	// DefaultModel overrides hub default model for this claw (from template config).
	DefaultModel string `json:"default_model,omitempty"`
	// LLMKey is the name of the LLM key from hub.yaml to use for this claw.
	// When set and DefaultModel is empty, the hub resolves the model from the key.
	LLMKey   string            `json:"llm_key,omitempty"`
	Snapshot string            `json:"snapshot,omitempty"`
	Files    map[string]string `json:"files"`
	Env      map[string]string `json:"env,omitempty"`
	// SecretRefs maps env var names to hub secret names to inject into claws.
	// This mirrors TemplateConfig.SecretRefs for CLI-created claws.
	SecretRefs map[string]string     `json:"secret_refs,omitempty"`
	GitHub     *GitHubTemplateConfig `json:"github,omitempty"`
	Linear     *LinearTemplateConfig `json:"linear,omitempty"`
	Nix        bool                  `json:"nix,omitempty"`
	Flake      string                `json:"flake,omitempty"`
	Docker     bool                  `json:"docker,omitempty"`
	Tags       []string              `json:"tags,omitempty"`
	Color      string                `json:"color,omitempty"`
	// MCPs is the list of resolved MCP server configs to start in the claw.
	MCPs []*MCPConfig `json:"mcps,omitempty"`
	// ProviderName is set by the hub — the stable name used with the provider (ec-<shortid>).
	// Never set by the CLI; Name is the display name.
	ProviderName string `json:"provider_name,omitempty"`
	// PreviewPort is set by workflow configuration for an ephemeral browser preview.
	PreviewPort int `json:"preview_port,omitempty"`
}

// GetVersion returns the effective schema version for this hub config.
// Returns "v1" if SchemaVersion is not specified for backward compatibility.
func (c *HubConfig) GetVersion() string {
	if c.SchemaVersion != "" {
		return c.SchemaVersion
	}
	return "v1"
}
