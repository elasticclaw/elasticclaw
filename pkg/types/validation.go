package types

import (
	"fmt"
	"regexp"
	"strings"
)

// Valid colors for claw cards
var validColors = map[string]bool{
	"slate": true, "red": true, "orange": true, "amber": true,
	"lime": true, "green": true, "emerald": true, "teal": true,
	"cyan": true, "sky": true, "blue": true, "indigo": true,
	"violet": true, "purple": true, "pink": true, "rose": true,
}

// Valid integration types
var validIntegrations = map[string]bool{
	"linear": true, "shortcut": true, "github-issues": true, "github": true, "external": true,
}

// Valid provider types
var validProviders = map[string]bool{
	"replicated": true, "daytona": true, "exedev": true,
}

// Valid MCP sources
var validMCPSources = map[string]bool{
	"npx": true, "uvx": true, "smithery": true, "docker": true, "sse": true,
}

// Valid factory input types
var validFactoryInputTypes = map[string]bool{
	"string": true, "number": true, "bool": true, "enum": true,
}

// Valid GitHub trigger actions
var validGitHubTriggerActions = map[string]bool{
	"opened": true, "synchronize": true, "reopened": true, "closed": true,
}

// Valid GitHub trigger types
var validGitHubTriggerTypes = map[string]bool{
	"pull_request": true, "issue": true,
}

// Valid external trigger sources
var validExternalTriggerSources = map[string]bool{
	"github-release": true, "generic-webhook": true,
}

// repoRegex validates owner/repo format
var repoRegex = regexp.MustCompile(`^[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+$`)

// namePatternRegex validates that name_pattern only contains allowed placeholders.
// Allows multiple placeholders separated by literal characters, e.g. {issue_id}-{title}.
var namePatternRegex = regexp.MustCompile(`^([a-zA-Z0-9_-]*\{[a-zA-Z0-9_]+\})*[a-zA-Z0-9_-]*$`)

// Validate validates a FactoryConfig and returns an error if invalid.
func (f *FactoryConfig) Validate() error {
	if f == nil {
		return fmt.Errorf("factory config is nil")
	}

	// Name is required
	if strings.TrimSpace(f.Name) == "" {
		return fmt.Errorf("factory name is required")
	}

	// Integration is required and must be valid
	if f.Integration == "" {
		return fmt.Errorf("factory %q: integration is required", f.Name)
	}
	if !validIntegrations[f.Integration] {
		return fmt.Errorf("factory %q: invalid integration %q (must be one of: linear, shortcut, github-issues, github, external)", f.Name, f.Integration)
	}

	// Template is required
	if strings.TrimSpace(f.Template) == "" {
		return fmt.Errorf("factory %q: template is required", f.Name)
	}

	// Validate color if provided
	if f.Color != "" && !validColors[f.Color] {
		return fmt.Errorf("factory %q: invalid color %q", f.Name, f.Color)
	}

	// Validate name_pattern if provided
	if f.NamePattern != "" && !namePatternRegex.MatchString(f.NamePattern) {
		return fmt.Errorf("factory %q: invalid name_pattern %q (must contain only alphanumeric, hyphens, underscores, and {placeholders})", f.Name, f.NamePattern)
	}

	// Validate provider if provided
	if f.Provider != "" && !validProviders[f.Provider] {
		return fmt.Errorf("factory %q: invalid provider %q (must be one of: replicated, daytona, exedev)", f.Name, f.Provider)
	}

	// Validate inputs
	for i, input := range f.Inputs {
		if err := validateFactoryInput(f.Name, i, input); err != nil {
			return err
		}
	}

	// Validate GitHub-specific fields for github integration
	if f.Integration == "github" {
		if f.Trigger == nil && !f.EnableManualTrigger {
			return fmt.Errorf("factory %q: trigger is required for github integration (or set enable_manual_trigger: true)", f.Name)
		}
		if f.Trigger != nil {
			if err := validateGitHubTrigger(f.Name, f.Trigger); err != nil {
				return err
			}
		}
	}

	// Validate external trigger fields for external integration
	if f.Integration == "external" {
		if f.ExternalTrigger == nil && !f.EnableManualTrigger {
			return fmt.Errorf("factory %q: external_trigger is required for external integration (or set enable_manual_trigger: true)", f.Name)
		}
		if f.ExternalTrigger != nil {
			if err := validateExternalTrigger(f.Name, f.ExternalTrigger); err != nil {
				return err
			}
		}
	}

	// Validate repos format if provided
	for i, repo := range f.Repos {
		if repo == "" {
			return fmt.Errorf("factory %q: repos[%d] cannot be empty", f.Name, i)
		}
		// Allow wildcard patterns like "owner/*"
		if !strings.HasSuffix(repo, "/*") && !repoRegex.MatchString(repo) {
			return fmt.Errorf("factory %q: repos[%d] invalid format %q (expected owner/repo or owner/*)", f.Name, i, repo)
		}
	}
	for i, repo := range f.TriggerRepos {
		if repo == "" {
			return fmt.Errorf("factory %q: trigger_repos[%d] cannot be empty", f.Name, i)
		}
		if !strings.HasSuffix(repo, "/*") && !repoRegex.MatchString(repo) {
			return fmt.Errorf("factory %q: trigger_repos[%d] invalid format %q (expected owner/repo or owner/*)", f.Name, i, repo)
		}
	}

	return nil
}

// Validate validates a WorkflowConfig and returns an error if invalid.
func (w *WorkflowConfig) Validate() error {
	if w == nil {
		return fmt.Errorf("workflow config is nil")
	}
	if strings.TrimSpace(w.Name) == "" {
		return fmt.Errorf("workflow name is required")
	}
	if strings.TrimSpace(w.Template) == "" {
		return fmt.Errorf("workflow %q: template is required", w.Name)
	}
	if w.Integration != "" && !validIntegrations[w.Integration] {
		return fmt.Errorf("workflow %q: invalid integration %q (must be one of: linear, shortcut, github-issues, github, external)", w.Name, w.Integration)
	}
	if w.Color != "" && !validColors[w.Color] {
		return fmt.Errorf("workflow %q: invalid color %q", w.Name, w.Color)
	}
	if w.NamePattern != "" && !namePatternRegex.MatchString(w.NamePattern) {
		return fmt.Errorf("workflow %q: invalid name_pattern %q (must contain only alphanumeric, hyphens, underscores, and {placeholders})", w.Name, w.NamePattern)
	}
	if w.Provider != "" && !validProviders[w.Provider] {
		return fmt.Errorf("workflow %q: invalid provider %q (must be one of: replicated, daytona, exedev)", w.Name, w.Provider)
	}
	if w.Trigger != nil {
		if err := validateGitHubTrigger(w.Name, w.Trigger); err != nil {
			return err
		}
	}
	for i, repo := range w.Repos {
		if repo == "" {
			return fmt.Errorf("workflow %q: repos[%d] cannot be empty", w.Name, i)
		}
		if !strings.HasSuffix(repo, "/*") && !repoRegex.MatchString(repo) {
			return fmt.Errorf("workflow %q: repos[%d] invalid format %q (expected owner/repo or owner/*)", w.Name, i, repo)
		}
	}
	for i, repo := range w.TriggerRepos {
		if repo == "" {
			return fmt.Errorf("workflow %q: trigger_repos[%d] cannot be empty", w.Name, i)
		}
		if !strings.HasSuffix(repo, "/*") && !repoRegex.MatchString(repo) {
			return fmt.Errorf("workflow %q: trigger_repos[%d] invalid format %q (expected owner/repo or owner/*)", w.Name, i, repo)
		}
	}
	for i, input := range w.Inputs {
		if err := validateFactoryInput(w.Name, i, input); err != nil {
			return err
		}
	}
	return nil
}

func validateFactoryInput(factoryName string, index int, input FactoryInput) error {
	if strings.TrimSpace(input.Name) == "" {
		return fmt.Errorf("factory %q: inputs[%d] name is required", factoryName, index)
	}

	if input.Type == "" {
		return fmt.Errorf("factory %q: inputs[%d] type is required", factoryName, index)
	}

	if !validFactoryInputTypes[input.Type] {
		return fmt.Errorf("factory %q: inputs[%d] invalid type %q (must be one of: string, number, bool, enum)", factoryName, index, input.Type)
	}

	// Validate enum has options
	if input.Type == "enum" && len(input.Options) == 0 {
		return fmt.Errorf("factory %q: inputs[%d] enum type requires options", factoryName, index)
	}

	// Validate regex pattern if provided
	if input.Validation != "" {
		if _, err := regexp.Compile(input.Validation); err != nil {
			return fmt.Errorf("factory %q: inputs[%d] invalid validation regex: %w", factoryName, index, err)
		}
	}

	// Validate min/max for numbers
	if input.Type == "number" {
		if input.Min != nil && input.Max != nil && *input.Min > *input.Max {
			return fmt.Errorf("factory %q: inputs[%d] min cannot be greater than max", factoryName, index)
		}
	}

	return nil
}

func validateGitHubTrigger(factoryName string, trigger *GitHubTrigger) error {
	if trigger.On == "" {
		return fmt.Errorf("factory %q: trigger.on is required", factoryName)
	}
	if !validGitHubTriggerTypes[trigger.On] {
		return fmt.Errorf("factory %q: invalid trigger.on %q (must be pull_request or issue)", factoryName, trigger.On)
	}

	if trigger.Action == "" {
		return fmt.Errorf("factory %q: trigger.action is required", factoryName)
	}
	if !validGitHubTriggerActions[trigger.Action] {
		return fmt.Errorf("factory %q: invalid trigger.action %q (must be one of: opened, synchronize, reopened, closed)", factoryName, trigger.Action)
	}

	return nil
}

func validateExternalTrigger(factoryName string, trigger *ExternalTrigger) error {
	if trigger.Source == "" {
		return fmt.Errorf("factory %q: external_trigger.source is required", factoryName)
	}
	if !validExternalTriggerSources[trigger.Source] {
		return fmt.Errorf("factory %q: invalid external_trigger.source %q (must be one of: github-release, generic-webhook)", factoryName, trigger.Source)
	}

	// Validate filter if provided
	if trigger.Filter != nil {
		if trigger.Filter.Repository != "" && !repoRegex.MatchString(trigger.Filter.Repository) {
			return fmt.Errorf("factory %q: invalid external_trigger.filter.repository %q (expected owner/repo)", factoryName, trigger.Filter.Repository)
		}
		if trigger.Filter.TagPattern != "" {
			// Simple validation: tag pattern should be non-empty and contain valid characters
			if strings.ContainsAny(trigger.Filter.TagPattern, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f") {
				return fmt.Errorf("factory %q: invalid external_trigger.filter.tag_pattern (contains control characters)", factoryName)
			}
		}
	}

	return nil
}

// Validate validates a TemplateConfig and returns an error if invalid.
func (t *TemplateConfig) Validate() error {
	if t == nil {
		return fmt.Errorf("template config is nil")
	}

	// Provider is required
	if t.Provider == "" {
		return fmt.Errorf("template provider is required")
	}
	if !validProviders[t.Provider] {
		return fmt.Errorf("invalid provider %q (must be one of: replicated, daytona, exedev)", t.Provider)
	}

	// Validate color if provided
	if t.Color != "" && !validColors[t.Color] {
		return fmt.Errorf("invalid color %q", t.Color)
	}

	// Validate GitHub repos if provided
	if t.GitHub != nil {
		for i, repo := range t.GitHub.Repos {
			if repo.Repo == "" {
				return fmt.Errorf("github.repos[%d]: repo is required", i)
			}
			if !repoRegex.MatchString(repo.Repo) {
				return fmt.Errorf("github.repos[%d]: invalid repo format %q (expected owner/repo)", i, repo.Repo)
			}
			if repo.Permissions != "" && repo.Permissions != "read" && repo.Permissions != "write" {
				return fmt.Errorf("github.repos[%d]: invalid permissions %q (must be read or write)", i, repo.Permissions)
			}
		}
	}

	// Validate MCPs if provided
	for i, mcp := range t.MCPs {
		if strings.TrimSpace(mcp.Name) == "" {
			return fmt.Errorf("mcps[%d]: name is required", i)
		}
	}

	// Validate secrets (legacy) if provided
	for i, secret := range t.Secrets {
		if err := validateSecretRef("secrets["+fmt.Sprintf("%d", i)+"]", secret); err != nil {
			return err
		}
	}

	return nil
}

func validateSecretRef(path string, secret SecretRef) error {
	if secret.Type == "" {
		return fmt.Errorf("%s: type is required", path)
	}

	validTypes := map[string]bool{"linear": true, "shortcut": true, "github": true, "github-issues": true, "custom": true}
	if !validTypes[secret.Type] {
		return fmt.Errorf("%s: invalid type %q (must be one of: linear, shortcut, github, github-issues, custom)", path, secret.Type)
	}

	if secret.Type == "custom" && secret.Name == "" {
		return fmt.Errorf("%s: name is required for custom type", path)
	}

	return nil
}

// ValidateMCPServerConfig validates an MCPServerHubConfig
func ValidateMCPServerConfig(mcp *MCPServerHubConfig) error {
	if mcp == nil {
		return fmt.Errorf("MCP server config is nil")
	}

	if strings.TrimSpace(mcp.Name) == "" {
		return fmt.Errorf("MCP server name is required")
	}

	if mcp.Source == "" {
		return fmt.Errorf("MCP server %q: source is required", mcp.Name)
	}

	if !validMCPSources[string(mcp.Source)] {
		return fmt.Errorf("MCP server %q: invalid source %q (must be one of: npx, uvx, smithery, docker, sse)", mcp.Name, mcp.Source)
	}

	// Validate required fields based on source
	switch mcp.Source {
	case MCPSourceNpx, MCPSourceUvx, MCPSourceSmithery:
		if mcp.Package == "" {
			return fmt.Errorf("MCP server %q: package is required for %s source", mcp.Name, mcp.Source)
		}
	case MCPSourceDocker:
		if mcp.Image == "" {
			return fmt.Errorf("MCP server %q: image is required for docker source", mcp.Name)
		}
	case MCPSourceSSE:
		if mcp.URL == "" {
			return fmt.Errorf("MCP server %q: url is required for sse source", mcp.Name)
		}
	}

	return nil
}

// ValidateProviderConfig validates a ProviderConfig
func ValidateProviderConfig(name string, cfg *ProviderConfig) error {
	if cfg == nil {
		return fmt.Errorf("provider %q: config is nil", name)
	}

	// Type is required. If name is provided but type is empty, use name as type.
	providerType := cfg.Type
	if providerType == "" && name != "" {
		providerType = name
	}

	if providerType == "" {
		return fmt.Errorf("provider type is required")
	}

	if !validProviders[providerType] {
		return fmt.Errorf("invalid provider type %q (must be one of: replicated, daytona, exedev)", providerType)
	}

	return nil
}

// ValidateHubConfig performs basic validation on HubConfig
func (h *HubConfig) Validate() error {
	if h == nil {
		return fmt.Errorf("hub config is nil")
	}

	// Validate factories
	for i, factory := range h.Factories {
		if err := factory.Validate(); err != nil {
			return fmt.Errorf("factories[%d]: %w", i, err)
		}
	}

	// Validate MCP servers
	for i, mcp := range h.MCPServers {
		if err := ValidateMCPServerConfig(mcp); err != nil {
			return fmt.Errorf("mcp_servers[%d]: %w", i, err)
		}
	}

	// Validate providers
	for name, provider := range h.Providers {
		if err := ValidateProviderConfig(name, &provider); err != nil {
			return err
		}
	}

	return nil
}
