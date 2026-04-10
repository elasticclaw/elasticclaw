package types

// TemplateConfig is the elasticclaw-config.yaml inside a template directory.
type TemplateConfig struct {
	Provider     string            `yaml:"provider"`
	Resources    TemplateResources `yaml:"resources,omitempty"`
	InstanceType string            `yaml:"instance_type,omitempty"` // e.g. r1.large for Replicated
	Image        string            `yaml:"image,omitempty"`
	TTL          string            `yaml:"ttl,omitempty"`
}

type TemplateResources struct {
	CPU    string `yaml:"cpu,omitempty"`
	Memory string `yaml:"memory,omitempty"`
	Disk   string `yaml:"disk,omitempty"`
}

// HubConfig is used in two contexts:
//   - ~/.elasticclaw/hub.yaml: CLI connection + full server config
type HubConfig struct {
	// CLI connection fields
	URL   string `yaml:"url"`
	Token string `yaml:"token"`

	// Hub server fields
	ClawToken string                   `yaml:"claw_token,omitempty"`
	Providers map[string]ProviderConfig `yaml:"providers,omitempty"`
	// SSHPublicKeys are extra keys added to every provisioned VM for debug access.
	SSHPublicKeys []string `yaml:"ssh_public_keys,omitempty"`
	// BridgeImage is the OCI artifact reference for the claw-bridge binary.
	// Defaults to ghcr.io/elasticclaw/claw-bridge:latest if not set.
	BridgeImage string `yaml:"bridge_image,omitempty"`
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
	InstanceType string            `json:"instance_type,omitempty"` // provider-specific, e.g. r1.large
	Image        string            `json:"image,omitempty"`
	TTL          string            `json:"ttl,omitempty"`
	Files        map[string]string `json:"files"` // filename -> base64 content
	Env          map[string]string `json:"env,omitempty"`
}
