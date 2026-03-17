package types

// Manifest represents the elasticclaw.yaml file
type Manifest struct {
	Name        string `yaml:"name"`
	Version     string `yaml:"version"`
	Description string `yaml:"description,omitempty"`

	OpenClaw  OpenClawConfig  `yaml:"openclaw,omitempty"`
	Providers ProvidersConfig `yaml:"providers,omitempty"`
	Identity  IdentityConfig  `yaml:"identity,omitempty"`
	State     StateConfig     `yaml:"state,omitempty"`

	TemplateVars []string `yaml:"template_vars,omitempty"`
}

type OpenClawConfig struct {
	RequiredFiles []string `yaml:"required_files,omitempty"`
}

type ProvidersConfig struct {
	Supported []string `yaml:"supported,omitempty"`
}

type IdentityConfig struct {
	Profiles map[string]IdentityProfile `yaml:"profiles,omitempty"`
}

type IdentityProfile struct {
	// Creddy-managed identity
	Creddy *CreddyConfig `yaml:"creddy,omitempty"`
	// Raw credentials (escape hatch)
	Raw *RawCredentials `yaml:"raw,omitempty"`
}

type CreddyConfig struct {
	Server   string              `yaml:"server,omitempty"`
	Bindings []CredentialBinding `yaml:"bindings,omitempty"`
}

type CredentialBinding struct {
	Audience string   `yaml:"audience"`
	Scopes   []string `yaml:"scopes,omitempty"`
	TTL      string   `yaml:"ttl,omitempty"`
}

type RawCredentials struct {
	Env   map[string]string `yaml:"env,omitempty"`
	Files map[string]string `yaml:"files,omitempty"`
}

type StateConfig struct {
	Default           string   `yaml:"default,omitempty"`
	PromotableTargets []string `yaml:"promotable_targets,omitempty"`
}
