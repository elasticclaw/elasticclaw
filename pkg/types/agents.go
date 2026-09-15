package types

import "fmt"

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

// MaxSubagentConcurrency bounds an authored max_concurrent value.
const MaxSubagentConcurrency = 32

// Validate checks the structural limits of an authored subagent block. A nil
// block inherits from the principal and is always valid.
func (c *SubagentConfig) Validate() error {
	if c == nil {
		return nil
	}
	if c.MaxConcurrent < 0 || c.MaxConcurrent > MaxSubagentConcurrency {
		return fmt.Errorf("subagents: max_concurrent must be between 1 and %d, or omitted", MaxSubagentConcurrency)
	}
	return nil
}
