package types

// HubProfile holds connection details for one ElasticClaw hub.
type HubProfile struct {
	URL    string `yaml:"url"`
	Token  string `yaml:"token"`
	SSHURI string `yaml:"ssh_uri,omitempty"` // optional SSH target for hub upgrade (e.g. ssh://marc@canio-factory)
	SSHKey string `yaml:"ssh_key,omitempty"` // optional SSH private key path for hub upgrade
}

// Profile represents an execution context (legacy, kept for compat)
type Profile struct {
	Name      string `yaml:"-"` // Set from filename
	Provider  string `yaml:"provider,omitempty"`
	State     string `yaml:"state,omitempty"`
	Identity  string `yaml:"identity,omitempty"`
	Namespace string `yaml:"namespace,omitempty"`

	// Provider-specific config
	Providers map[string]map[string]interface{} `yaml:"providers,omitempty"`
}

// GlobalConfig represents ~/.elasticclaw/config.yaml
type GlobalConfig struct {
	ActiveProfile string                 `yaml:"active_profile,omitempty"`
	Profiles      map[string]*HubProfile `yaml:"profiles,omitempty"`
	// Hub is the legacy single-hub field. Migrated to Profiles["default"] on first use.
	Hub           *HubConfig `yaml:"hub,omitempty"`
}

// LockFile represents .elasticclaw/lock.yaml
type LockFile struct {
	Template      TemplateLock `yaml:"template"`
	Images        []ImageLock  `yaml:"images,omitempty"`
	LockedAt      string       `yaml:"locked_at"`
	SchemaVersion string      `yaml:"schema_version"`
	ToolVersion   string      `yaml:"tool_version,omitempty"`
}

type TemplateLock struct {
	Source   string `yaml:"source"`
	Version  string `yaml:"version,omitempty"`
	Revision string `yaml:"revision,omitempty"`
	Digest   string `yaml:"digest,omitempty"`
}

type ImageLock struct {
	Name   string `yaml:"name"`
	Digest string `yaml:"digest"`
}
