package types

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
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
	"linear": true, "shortcut": true, "github-issues": true, "jira": true, "github": true, "external": true,
}

// Valid workflow integration types. Cron is workflow-only; factories still use
// validIntegrations above.
var validWorkflowIntegrations = buildWorkflowIntegrations()

func buildWorkflowIntegrations() map[string]bool {
	integrations := make(map[string]bool, len(validIntegrations)+1)
	for integration := range validIntegrations {
		integrations[integration] = true
	}
	integrations["cron"] = true
	return integrations
}

// Valid provider types
var validProviders = map[string]bool{
	"replicated": true, "daytona": true, "exedev": true, "docker": true,
}

// Valid MCP sources
var validMCPSources = map[string]bool{
	"npx": true, "uvx": true, "smithery": true, "docker": true, "sse": true,
}

// Valid factory input types
var validFactoryInputTypes = map[string]bool{
	"string": true, "number": true, "bool": true, "enum": true,
}

var validTaskRunKinds = map[string]bool{
	"code_task": true, "pr_task": true,
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

// Valid cron overlap policies
var validCronOverlapPolicies = map[string]bool{
	"skip": true, "queue": true, "parallel": true,
}

// repoRegex validates owner/repo format
var repoRegex = regexp.MustCompile(`^[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+$`)
var repoWildcardRegex = regexp.MustCompile(`^[a-zA-Z0-9_.-]+/\*$`)

// namePatternRegex validates that name_pattern only contains allowed placeholders.
// Allows multiple placeholders separated by literal characters, e.g. {issue_id}-{title}.
var namePatternRegex = regexp.MustCompile(`^([a-zA-Z0-9_-]*\{[a-zA-Z0-9_]+\})*[a-zA-Z0-9_-]*$`)
var volumeNameRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func validateRunKind(entityType, entityName, runKind string) error {
	if runKind != "" && !validTaskRunKinds[runKind] {
		return fmt.Errorf("%s %q: invalid run_kind %q (must be one of: code_task, pr_task)", entityType, entityName, runKind)
	}
	return nil
}

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
		return fmt.Errorf("factory %q: invalid integration %q (must be one of: linear, shortcut, github-issues, jira, github, external)", f.Name, f.Integration)
	}

	// Template is required
	if strings.TrimSpace(f.Template) == "" {
		return fmt.Errorf("factory %q: template is required", f.Name)
	}

	// Validate color if provided
	if f.Color != "" && !validColors[f.Color] {
		return fmt.Errorf("factory %q: invalid color %q", f.Name, f.Color)
	}
	if err := validateRunKind("factory", f.Name, f.RunKind); err != nil {
		return err
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
		if !validRepoSelector(repo) {
			return fmt.Errorf("factory %q: repos[%d] invalid format %q (expected owner/repo or owner/*)", f.Name, i, repo)
		}
	}
	for i, repo := range f.TriggerRepos {
		if repo == "" {
			return fmt.Errorf("factory %q: trigger_repos[%d] cannot be empty", f.Name, i)
		}
		if !validRepoSelector(repo) {
			return fmt.Errorf("factory %q: trigger_repos[%d] invalid format %q (expected owner/repo or owner/*)", f.Name, i, repo)
		}
	}
	for i, project := range f.Projects {
		if strings.TrimSpace(project) == "" {
			return fmt.Errorf("factory %q: projects[%d] cannot be empty", f.Name, i)
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
	if w.Integration != "" && !validWorkflowIntegrations[w.Integration] {
		return fmt.Errorf("workflow %q: invalid integration %q (must be one of: linear, shortcut, github-issues, jira, github, external, cron)", w.Name, w.Integration)
	}
	if w.Color != "" && !validColors[w.Color] {
		return fmt.Errorf("workflow %q: invalid color %q", w.Name, w.Color)
	}
	if err := validateRunKind("workflow", w.Name, w.RunKind); err != nil {
		return err
	}
	if w.NamePattern != "" && !namePatternRegex.MatchString(w.NamePattern) {
		return fmt.Errorf("workflow %q: invalid name_pattern %q (must contain only alphanumeric, hyphens, underscores, and {placeholders})", w.Name, w.NamePattern)
	}
	if w.Provider != "" && !validProviders[w.Provider] {
		return fmt.Errorf("workflow %q: invalid provider %q (must be one of: replicated, daytona, exedev)", w.Name, w.Provider)
	}
	if w.Trigger != nil {
		if err := validateWorkflowTrigger(w.Name, w.Trigger); err != nil {
			return err
		}
	}
	for i, repo := range w.Repos {
		if repo == "" {
			return fmt.Errorf("workflow %q: repos[%d] cannot be empty", w.Name, i)
		}
		if !validRepoSelector(repo) {
			return fmt.Errorf("workflow %q: repos[%d] invalid format %q (expected owner/repo or owner/*)", w.Name, i, repo)
		}
	}
	for i, repo := range w.TriggerRepos {
		if repo == "" {
			return fmt.Errorf("workflow %q: trigger_repos[%d] cannot be empty", w.Name, i)
		}
		if !validRepoSelector(repo) {
			return fmt.Errorf("workflow %q: trigger_repos[%d] invalid format %q (expected owner/repo or owner/*)", w.Name, i, repo)
		}
	}
	for i, input := range w.Inputs {
		if err := validateFactoryInput(w.Name, i, input); err != nil {
			return err
		}
	}
	seenVolumes := map[string]struct{}{}
	for i, volume := range w.Volumes {
		if err := validateWorkflowVolume(w.Name, i, volume, seenVolumes); err != nil {
			return err
		}
	}
	for i, stage := range w.Stages {
		if strings.TrimSpace(stage.ID) == "" {
			return fmt.Errorf("workflow %q: stages[%d].id is required", w.Name, i)
		}
	}
	return nil
}

func validateWorkflowVolume(workflowName string, index int, volume WorkflowVolume, seen map[string]struct{}) error {
	name := strings.TrimSpace(volume.Name)
	if name == "" {
		return fmt.Errorf("workflow %q: volumes[%d].name is required", workflowName, index)
	}
	if !volumeNameRegex.MatchString(name) {
		return fmt.Errorf("workflow %q: volumes[%d].name %q must contain only alphanumeric, hyphens, or underscores", workflowName, index, volume.Name)
	}
	lowerName := strings.ToLower(name)
	if _, ok := seen[lowerName]; ok {
		return fmt.Errorf("workflow %q: duplicate volume name %q", workflowName, volume.Name)
	}
	seen[lowerName] = struct{}{}
	source := strings.TrimSpace(volume.Source)
	if !strings.HasPrefix(source, "hub://volumes/") || strings.TrimPrefix(source, "hub://volumes/") == "" {
		return fmt.Errorf("workflow %q: volumes[%d].source must use hub://volumes/<name>", workflowName, index)
	}
	mount := strings.TrimSpace(volume.Mount)
	if mount == "" || !strings.HasPrefix(mount, "/") {
		return fmt.Errorf("workflow %q: volumes[%d].mount must be an absolute path outside the repository", workflowName, index)
	}
	if strings.Contains(mount, "..") {
		return fmt.Errorf("workflow %q: volumes[%d].mount must not contain path traversal", workflowName, index)
	}
	if mount == "/" || strings.HasPrefix(mount, "/home/daytona/.openclaw/workspace") || strings.HasPrefix(mount, "/workspace") {
		return fmt.Errorf("workflow %q: volumes[%d].mount must be outside the repository workspace", workflowName, index)
	}
	mountKey := "mount:" + mount
	if _, ok := seen[mountKey]; ok {
		return fmt.Errorf("workflow %q: duplicate volume mount %q", workflowName, mount)
	}
	seen[mountKey] = struct{}{}
	mode := strings.TrimSpace(volume.Mode)
	if mode == "" {
		mode = "ro"
	}
	if mode != "ro" && mode != "rw" {
		return fmt.Errorf("workflow %q: volumes[%d].mode must be ro or rw", workflowName, index)
	}
	return nil
}

func validateWorkflowTrigger(workflowName string, trigger *WorkflowTrigger) error {
	nestedCount := 0
	if trigger.GitHubIssues != nil {
		nestedCount++
	}
	if trigger.Linear != nil {
		nestedCount++
	}
	if trigger.Shortcut != nil {
		nestedCount++
	}
	if trigger.Jira != nil {
		nestedCount++
	}
	if trigger.Cron != nil {
		nestedCount++
	}
	if nestedCount != 1 {
		return fmt.Errorf("workflow %q: trigger must define exactly one source", workflowName)
	}
	if trigger.GitHubIssues != nil {
		return validateGitHubIssuesWorkflowTrigger(workflowName, trigger.GitHubIssues)
	}
	if trigger.Linear != nil {
		return validateLinearWorkflowTrigger(workflowName, trigger.Linear)
	}
	if trigger.Cron != nil {
		return validateCronWorkflowTrigger(workflowName, trigger.Cron)
	}
	if trigger.Jira != nil {
		return validateJiraWorkflowTrigger(workflowName, trigger.Jira)
	}
	return validateShortcutWorkflowTrigger(workflowName, trigger.Shortcut)
}

func validateCronWorkflowTrigger(workflowName string, trigger *CronWorkflowTrigger) error {
	if trigger.Schedule == "" {
		return fmt.Errorf("workflow %q: cron trigger requires schedule", workflowName)
	}
	// Validate cron expression syntax using the same parser as the scheduler
	parser := cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	if _, err := parser.Parse(trigger.Schedule); err != nil {
		return fmt.Errorf("workflow %q: invalid cron schedule %q: %v", workflowName, trigger.Schedule, err)
	}
	if trigger.OverlapPolicy != "" && !validCronOverlapPolicies[trigger.OverlapPolicy] {
		return fmt.Errorf("workflow %q: invalid cron overlap_policy %q (must be one of: skip, queue, parallel)", workflowName, trigger.OverlapPolicy)
	}
	if trigger.Timezone != "" {
		if _, err := time.LoadLocation(trigger.Timezone); err != nil {
			return fmt.Errorf("workflow %q: invalid cron timezone %q: %v", workflowName, trigger.Timezone, err)
		}
	}
	if trigger.Timeout != "" {
		if _, err := time.ParseDuration(trigger.Timeout); err != nil {
			return fmt.Errorf("workflow %q: invalid cron timeout %q: %v", workflowName, trigger.Timeout, err)
		}
	}
	return nil
}

func validateGitHubIssuesWorkflowTrigger(workflowName string, trigger *GitHubIssuesWorkflowTrigger) error {
	if trigger.Event == "" {
		return fmt.Errorf("workflow %q: trigger.github_issues.event is required", workflowName)
	}
	switch trigger.Event {
	case "issue_labeled", "issue_opened", "issue_reopened", "issue_edited", "issue_assigned", "issue_unassigned":
	default:
		return fmt.Errorf("workflow %q: invalid trigger.github_issues.event %q", workflowName, trigger.Event)
	}
	for i, repo := range trigger.Repositories {
		if repo == "" {
			return fmt.Errorf("workflow %q: trigger.github_issues.repositories[%d] cannot be empty", workflowName, i)
		}
		if !validRepoSelector(repo) {
			return fmt.Errorf("workflow %q: trigger.github_issues.repositories[%d] invalid format %q (expected owner/repo or owner/*)", workflowName, i, repo)
		}
	}
	if trigger.AgentStatusError != "" && strings.TrimSpace(trigger.AgentStatusError) == "" {
		return fmt.Errorf("workflow %q: trigger.github_issues.agent_status_error cannot be blank", workflowName)
	}
	return nil
}

func validateLinearWorkflowTrigger(workflowName string, trigger *LinearWorkflowTrigger) error {
	if trigger.Event == "" {
		return fmt.Errorf("workflow %q: trigger.linear.event is required", workflowName)
	}
	switch trigger.Event {
	case "status", "status_changed", "issue_status_changed":
	default:
		return fmt.Errorf("workflow %q: invalid trigger.linear.event %q", workflowName, trigger.Event)
	}
	if len(trigger.States) == 0 {
		return fmt.Errorf("workflow %q: trigger.linear.states must include at least one status", workflowName)
	}
	for i, state := range trigger.States {
		if strings.TrimSpace(state) == "" {
			return fmt.Errorf("workflow %q: trigger.linear.states[%d] cannot be empty", workflowName, i)
		}
	}
	if trigger.AgentStatusError != "" && strings.TrimSpace(trigger.AgentStatusError) == "" {
		return fmt.Errorf("workflow %q: trigger.linear.agent_status_error cannot be blank", workflowName)
	}
	return nil
}

func validateShortcutWorkflowTrigger(workflowName string, trigger *ShortcutWorkflowTrigger) error {
	if trigger.Event == "" {
		return fmt.Errorf("workflow %q: trigger.shortcut.event is required", workflowName)
	}
	switch trigger.Event {
	case "status", "status_changed", "story_status_changed":
	default:
		return fmt.Errorf("workflow %q: invalid trigger.shortcut.event %q", workflowName, trigger.Event)
	}
	if len(trigger.States) == 0 {
		return fmt.Errorf("workflow %q: trigger.shortcut.states must include at least one status", workflowName)
	}
	for i, state := range trigger.States {
		if strings.TrimSpace(state) == "" {
			return fmt.Errorf("workflow %q: trigger.shortcut.states[%d] cannot be empty", workflowName, i)
		}
	}
	return nil
}

func validateJiraWorkflowTrigger(workflowName string, trigger *JiraWorkflowTrigger) error {
	if trigger.Event == "" {
		return fmt.Errorf("workflow %q: trigger.jira.event is required", workflowName)
	}
	switch trigger.Event {
	case "status", "status_changed", "issue_status_changed", "issue_created", "issue_updated":
	default:
		return fmt.Errorf("workflow %q: invalid trigger.jira.event %q", workflowName, trigger.Event)
	}
	if len(trigger.States) == 0 {
		return fmt.Errorf("workflow %q: trigger.jira.states must include at least one status", workflowName)
	}
	for i, state := range trigger.States {
		if strings.TrimSpace(state) == "" {
			return fmt.Errorf("workflow %q: trigger.jira.states[%d] cannot be empty", workflowName, i)
		}
	}
	for i, project := range trigger.Projects {
		if strings.TrimSpace(project) == "" {
			return fmt.Errorf("workflow %q: trigger.jira.projects[%d] cannot be empty", workflowName, i)
		}
	}
	if trigger.AgentStatusError != "" && strings.TrimSpace(trigger.AgentStatusError) == "" {
		return fmt.Errorf("workflow %q: trigger.jira.agent_status_error cannot be blank", workflowName)
	}
	return nil
}

func validRepoSelector(repo string) bool {
	return repoRegex.MatchString(repo) || repoWildcardRegex.MatchString(repo)
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

func validateRepositoryAccessList(field string, repos []GitHubRepoAccess) error {
	for i, repo := range repos {
		if repo.Repo == "" {
			return fmt.Errorf("%s[%d]: repo is required", field, i)
		}
		if !repoRegex.MatchString(repo.Repo) {
			return fmt.Errorf("%s[%d]: invalid repo format %q (expected owner/repo)", field, i, repo.Repo)
		}
		if repo.Permissions != "" && repo.Permissions != "read" && repo.Permissions != "write" {
			return fmt.Errorf("%s[%d]: invalid permissions %q (must be read or write)", field, i, repo.Permissions)
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

	if err := validateRepositoryAccessList("repositories", t.Repositories); err != nil {
		return err
	}

	// Validate GitHub repos if provided
	if t.GitHub != nil {
		if err := validateRepositoryAccessList("github.repos", t.GitHub.Repos); err != nil {
			return err
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

	validTypes := map[string]bool{"linear": true, "shortcut": true, "github": true, "github-issues": true, "jira": true, "custom": true}
	if !validTypes[secret.Type] {
		return fmt.Errorf("%s: invalid type %q (must be one of: linear, shortcut, github, github-issues, jira, custom)", path, secret.Type)
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
