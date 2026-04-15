package types

// TemplateConfig is the elasticclaw-config.yaml inside a template directory.
type TemplateConfig struct {
	Provider     string            `yaml:"provider"`
	Resources    TemplateResources `yaml:"resources,omitempty"`
	InstanceType string            `yaml:"instance_type,omitempty"` // e.g. r1.large for Replicated
	Image        string            `yaml:"image,omitempty"`
	TTL          string            `yaml:"ttl,omitempty"`
	// DefaultModel overrides the hub-level default model for this template.
	// Format: provider/model, e.g. anthropic/claude-opus-4-5
	DefaultModel string                `yaml:"default_model,omitempty"`
	// Snapshot is the Daytona snapshot name (e.g. "daytona-medium", "daytona-large").
	// Overrides hub providers.daytona.default_snapshot.
	Snapshot string                    `yaml:"snapshot,omitempty"`
	// GitHub specifies GitHub repos this template's claw needs access to.
	GitHub  *GitHubTemplateConfig  `yaml:"github,omitempty"`
	// Linear specifies which Linear workspace this template's claw should use.
	Linear  *LinearTemplateConfig  `yaml:"linear,omitempty"`
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

// GitHubAppConfig holds GitHub App credentials for one GitHub App.
// Configure in hub.yaml under 'github:' as a list.
type GitHubAppConfig struct {
	URL           string `yaml:"url,omitempty"`           // GitHub App URL for reference/logging only
	AppID         int64  `yaml:"app_id"`
	PrivateKeyPEM string `yaml:"private_key_pem"` // PEM-encoded RSA private key (paste directly in yaml)
}

// GitHubRepoAccess specifies a repo and the permissions needed.
type GitHubRepoAccess struct {
	Repo        string `yaml:"repo"        json:"repo"`        // e.g. "owner/repo"
	Permissions string `yaml:"permissions" json:"permissions"` // "read" or "write" (default: "read")
}

// GitHubTemplateConfig specifies GitHub access needed by a template.
type GitHubTemplateConfig struct {
	Repos []GitHubRepoAccess `yaml:"repos" json:"repos"`
}

// HubConfig is used in two contexts:
//   - ~/.elasticclaw/hub.yaml: CLI connection + full server config
type HubConfig struct {
	// CLI connection fields
	URL   string `yaml:"url"`
	Token string `yaml:"token"`

	// Hub server fields
	// PublicURL is the URL claws use to connect back to the hub from remote VMs.
	// If not set, falls back to URL.
	PublicURL string                   `yaml:"public_url,omitempty"`
	ClawToken string                   `yaml:"claw_token,omitempty"`
	Providers map[string]ProviderConfig `yaml:"providers,omitempty"`
	// SSHPublicKeys are extra keys added to every provisioned VM for debug access.
	SSHPublicKeys []string `yaml:"ssh_public_keys,omitempty"`
	// BridgeImage is the OCI artifact reference for the claw-bridge binary.
	// Defaults to ghcr.io/elasticclaw/claw-bridge:latest if not set.
	BridgeImage string `yaml:"bridge_image,omitempty"`
	// DefaultModel is the model used by all claws unless overridden in the template.
	// Format: provider/model, e.g. anthropic/claude-sonnet-4-6
	DefaultModel string            `yaml:"default_model,omitempty"`
	// LLMKeys is a flat map of provider name to API key, e.g. {"anthropic": "sk-ant-..."}
	LLMKeys map[string]string      `yaml:"llm_keys,omitempty"`
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
	// RelayURL is the relay server for NAT traversal.
	// Defaults to wss://relay.elasticclaw.ai if not set.
	// Set to "none" to disable the relay entirely.
	// When set, hub connects outbound to the relay; bridges dial the relay instead of the hub directly.
	RelayURL string `yaml:"relay_url,omitempty"`

	// RelaySecret is the HMAC secret for deriving relay tokens (must match RELAY_SECRET on the relay server).
	RelaySecret string `yaml:"relay_secret,omitempty"`

	GitHubApps []*GitHubAppConfig `yaml:"github,omitempty"`
}

type ProviderConfig struct {
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

	// local provider
	Enabled bool `yaml:"enabled,omitempty"`
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
	DefaultModel string                `json:"default_model,omitempty"`
	Snapshot     string                `json:"snapshot,omitempty"`
	Files        map[string]string     `json:"files"`
	Env          map[string]string     `json:"env,omitempty"`
	GitHub       *GitHubTemplateConfig `json:"github,omitempty"`
	Linear       *LinearTemplateConfig `json:"linear,omitempty"`
}
