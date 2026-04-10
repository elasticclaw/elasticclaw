package types

// TemplateConfig is the elasticclaw-config.yaml inside a template directory.
type TemplateConfig struct {
	Provider  string             `yaml:"provider"`
	Resources TemplateResources  `yaml:"resources,omitempty"`
	Image     string             `yaml:"image,omitempty"`
	TTL       string             `yaml:"ttl,omitempty"`
}

type TemplateResources struct {
	CPU    string `yaml:"cpu,omitempty"`
	Memory string `yaml:"memory,omitempty"`
	Disk   string `yaml:"disk,omitempty"`
}

// HubConfig is ~/.elasticclaw/hub.yaml or /etc/elasticclaw/hub.yaml.
type HubConfig struct {
	HubURL    string                     `yaml:"hub_url"`
	ClawToken string                     `yaml:"claw_token"`
	Providers map[string]ProviderConfig  `yaml:"providers,omitempty"`
}

type ProviderConfig struct {
	APIURL string `yaml:"api_url,omitempty"`
	APIKey string `yaml:"api_key,omitempty"`
	Target string `yaml:"target,omitempty"`

	// local provider
	Enabled bool `yaml:"enabled,omitempty"`
}

// CreateClawRequest is POSTed by the CLI to the hub to provision a new claw.
type CreateClawRequest struct {
	Name         string            `json:"name"`
	TemplateName string            `json:"template_name"`
	Provider     string            `json:"provider"`
	Resources    TemplateResources `json:"resources,omitempty"`
	Image        string            `json:"image,omitempty"`
	TTL          string            `json:"ttl,omitempty"`
	Files        map[string]string `json:"files"` // filename -> base64 content
	Env          map[string]string `json:"env,omitempty"`
}
