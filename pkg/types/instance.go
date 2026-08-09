package types

import "time"

// Instance represents a running ElasticClaw instance
type Instance struct {
	Name     string `json:"name" yaml:"name"`
	ID       string `json:"id" yaml:"id"`
	Provider string `json:"provider" yaml:"provider"`

	// Template info
	Template        string `json:"template" yaml:"template"`
	TemplateVersion string `json:"templateVersion" yaml:"templateVersion"`

	// Provider-specific metadata
	ProviderMeta map[string]string `json:"providerMeta,omitempty" yaml:"providerMeta,omitempty"`

	// Identity
	IdentityProfile string    `json:"identityProfile,omitempty" yaml:"identityProfile,omitempty"`
	IdentityExpires time.Time `json:"identityExpires,omitempty" yaml:"identityExpires,omitempty"`

	// State
	StateBackend string `json:"stateBackend" yaml:"stateBackend"`

	// Lifecycle
	Status    InstanceStatus `json:"status" yaml:"status"`
	CreatedAt time.Time      `json:"createdAt" yaml:"createdAt"`
	TTL       string         `json:"ttl,omitempty" yaml:"ttl,omitempty"`

	// Template variables used
	Vars map[string]string `json:"vars,omitempty" yaml:"vars,omitempty"`
}

type InstanceStatus string

const (
	StatusPending   InstanceStatus = "pending"
	StatusRunning   InstanceStatus = "running"
	StatusStarting  InstanceStatus = "starting"
	StatusStopped   InstanceStatus = "stopped"
	StatusPreview   InstanceStatus = "preview"
	StatusUnhealthy InstanceStatus = "unhealthy"
	StatusError     InstanceStatus = "error"
	StatusNotFound  InstanceStatus = "not_found"
	StatusUnknown   InstanceStatus = "unknown"
)

// InstanceHealth represents health check results
type InstanceHealth struct {
	Status    InstanceStatus `json:"status"`
	Message   string         `json:"message,omitempty"`
	CheckedAt time.Time      `json:"checkedAt"`
}
