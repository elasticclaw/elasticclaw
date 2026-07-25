package types

import "time"

// ProviderType indicates whether a provider supports hibernation
type ProviderType string

const (
	ProviderTypeEphemeral ProviderType = "ephemeral"
	ProviderTypeStateful  ProviderType = "stateful"
)

// ProviderInfo describes a provider's capabilities
type ProviderInfo struct {
	Name         string       `json:"name"`
	Type         ProviderType `json:"type"`
	Capabilities []string     `json:"capabilities,omitempty"`
}

// CreateRequest contains parameters for creating an instance
type CreateRequest struct {
	Name string

	// Base image or scratch config
	FromImage   string
	FromScratch *ScratchConfig

	// Template files to inject
	TemplateFiles map[string][]byte

	// Environment variables
	Env map[string]string

	// State mount path
	StateMount string

	// PreviewPorts are application ports that should be exposed for browser QA.
	PreviewPorts []int
}

// ScratchConfig for creating from a base distro
type ScratchConfig struct {
	Distro      string
	SetupScript string
}

// ConnectInfo contains connection details for an instance
type ConnectInfo struct {
	Shell *ShellConnect `json:"shell,omitempty"`
	Web   string        `json:"web,omitempty"`
	IDE   string        `json:"ide,omitempty"`
}

// ShellConnect contains shell connection details
type ShellConnect struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

// ExecResult contains the result of running a command
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Image represents a base image from catalog or provider
type Image struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Source    ImageSource       `json:"source"`
	Tags      []string          `json:"tags,omitempty"`
	Signed    bool              `json:"signed"`
	SignedBy  string            `json:"signedBy,omitempty"`
	CreatedAt time.Time         `json:"createdAt,omitempty"`
	Size      int64             `json:"size,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

type ImageSource string

const (
	ImageSourceCatalog  ImageSource = "catalog"
	ImageSourceProvider ImageSource = "provider"
	ImageSourceUser     ImageSource = "user"
)
