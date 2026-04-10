package types

// Profile represents an execution context
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
	ActiveProfile string     `yaml:"active_profile,omitempty"`
	Hub           *HubConfig `yaml:"hub,omitempty"`
}

// LockFile represents .elasticclaw/lock.yaml
type LockFile struct {
	Template      TemplateLock `yaml:"template"`
	Images        []ImageLock  `yaml:"images,omitempty"`
	LockedAt      string       `yaml:"locked_at"`
	ElasticClawVer string      `yaml:"elasticclaw_version"`
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
