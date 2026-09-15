package types

// AgentConfig selects credentials and models for a principal and its children.
// Credential names are references; credential secrets are never stored here.
type AgentConfig struct {
	DefaultModel string          `yaml:"default_model,omitempty" json:"default_model,omitempty"`
	LLMKey       string          `yaml:"llm_key,omitempty" json:"llm_key,omitempty"`
	Subagents    *SubagentConfig `yaml:"subagents,omitempty" json:"subagents,omitempty"`
}

// SubagentConfig inherits unspecified settings from the principal.
type SubagentConfig struct {
	Model         string `yaml:"model,omitempty" json:"model,omitempty"`
	LLMKey        string `yaml:"llm_key,omitempty" json:"llm_key,omitempty"`
	MaxConcurrent int    `yaml:"max_concurrent,omitempty" json:"max_concurrent,omitempty"`
}
