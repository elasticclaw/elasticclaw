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
	// GitHub specifies GitHub repos this template's claw needs access to.
	GitHub       *GitHubTemplateConfig `yaml:"github,omitempty"`
}

type TemplateResources struct {
	CPU    string `yaml:"cpu,omitempty"`
	Memory string `yaml:"memory,omitempty"`
	Disk   string `yaml:"disk,omitempty"`
}

// GitHubAppConfig holds GitHub App credentials for one GitHub App.
// Configure in hub.yaml under 'github_apps:' as a named map.
type GitHubAppConfig struct {
	AppID         int64  `yaml:"app_id"`
	PrivateKeyPEM string `yaml:"private_key_pem"` // PEM-encoded RSA private key (paste directly in yaml)
}

// GitHubTemplateConfig specifies GitHub access needed by a template.
type GitHubTemplateConfig struct {
	Repos []string `yaml:"repos"` // e.g. ["owner/repo", "owner/repo2"]
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
	// GitHubApps is a named map of GitHub App configs for minting installation tokens.
	// The hub tries each configured app to find one whose installation covers the requested repos.
	// Example:
	//   github_apps:
	//     my-org-app:
	//       app_id: 123456
	//       private_key_pem: |
	//         -----BEGIN RSA PRIVATE KEY-----
	//         ...
	//         -----END RSA PRIVATE KEY-----
	GitHubApps map[string]*GitHubAppConfig `yaml:"github_apps,omitempty"`
}

type ProviderConfig struct {
	// Daytona
	APIURL string `yaml:"api_url,omitempty"`
	APIKey string `yaml:"api_key,omitempty"`
	Target string `yaml:"target,omitempty"`

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
	DefaultModel string            `json:"default_model,omitempty"`
	Files        map[string]string     `json:"files"`
	Env          map[string]string     `json:"env,omitempty"`
	GitHub       *GitHubTemplateConfig `json:"github,omitempty"`
}
