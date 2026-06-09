package workflowsetup

import (
	"fmt"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// ReadinessOptions controls checks that are unsafe for normal production
// readiness but useful for package-level tests.
type ReadinessOptions struct {
	AllowNoopProvider bool
}

// ValidateReadiness validates static workflow shape and local runtime
// prerequisites needed before saving a workflow. It does not perform network
// checks.
func ValidateReadiness(req ValidateRequest, env Environment) ValidateResponse {
	return ValidateReadinessWithOptions(req, env, ReadinessOptions{})
}

// ValidateReadinessWithOptions is ValidateReadiness with explicit test-only
// readiness overrides.
func ValidateReadinessWithOptions(req ValidateRequest, env Environment, opts ReadinessOptions) ValidateResponse {
	staticResp := ValidateStatic(req)
	checks := append([]Diagnostic(nil), staticResp.Checks...)

	if env == nil {
		checks = append(checks, readinessCriticalDiagnostic(
			"readiness-environment-missing",
			"readiness",
			"environment",
			"Readiness environment is unavailable",
			"Provide a workflow setup environment snapshot before saving.",
		))
		checks = append(checks, networkNotCheckedDiagnostic())
		return validateResponse(req.Config, checks)
	}

	snapshot, err := env.Snapshot()
	if err != nil {
		checks = append(checks, readinessCriticalDiagnostic(
			"readiness-environment-snapshot-failed",
			"readiness",
			"environment.snapshot",
			"Readiness snapshot could not be loaded",
			err.Error(),
		))
		checks = append(checks, networkNotCheckedDiagnostic())
		return validateResponse(req.Config, checks)
	}

	workspace, _ := ParseWorkspaceConfig(req.WorkspaceConfig)
	workflow, err := parseWorkflowConfig(req.Config)
	if err == nil {
		workflow = cloneWorkflowConfig(workflow)
		if normalizeErr := types.NormalizeWorkflowConfig(workflow); normalizeErr != nil {
			workflow = nil
		}
	} else {
		workflow = nil
	}

	workspaceName := parsedWorkspaceName(workspace)
	checks = append(checks, validateSnapshotReadiness(snapshot, workspace, workflow, opts)...)
	checks = append(checks, validateSecretReadiness(env, snapshot, workspace, workflow, workspaceName)...)
	checks = append(checks, validateIssueTrackerReadiness(env, snapshot, workflow, workspaceName)...)
	checks = append(checks, validateTriggerOverlapReadiness(env, workspaceName, workflow)...)
	checks = append(checks, networkNotCheckedDiagnostic())

	return validateResponse(req.Config, checks)
}

func validateSnapshotReadiness(snapshot SetupEnvironmentSnapshot, workspace *ParsedWorkspaceConfig, workflow *types.WorkflowConfig, opts ReadinessOptions) []Diagnostic {
	var checks []Diagnostic
	if !snapshot.ClawTokenSet {
		checks = append(checks, readinessCriticalDiagnostic(
			"readiness-claw-token-missing",
			"readiness",
			"snapshot.clawTokenSet",
			"claw_token is not configured",
			"Set claw_token in hub config before enabling workflow-created claws.",
		))
	}

	checks = append(checks, validateProviderReadiness(snapshot, workspace, workflow, opts)...)
	checks = append(checks, validateLLMReadiness(snapshot, workspace)...)
	checks = append(checks, validateConcurrencyReadiness(snapshot, workflow)...)
	return checks
}

func validateProviderReadiness(snapshot SetupEnvironmentSnapshot, workspace *ParsedWorkspaceConfig, workflow *types.WorkflowConfig, opts ReadinessOptions) []Diagnostic {
	var checks []Diagnostic
	if len(snapshot.Providers) == 0 {
		checks = append(checks, readinessCriticalDiagnostic(
			"readiness-provider-missing",
			"readiness",
			"snapshot.providers",
			"No execution providers are configured",
			"Configure at least one provider before saving runtime workflows.",
		))
		return checks
	}

	resolved, fieldPath := resolvedProvider(workspace, workflow, snapshot)
	if resolved == "" {
		return append(checks, readinessCriticalDiagnostic(
			"readiness-provider-unresolved",
			"readiness",
			"workflow.provider",
			"No execution provider could be resolved",
			"Set workflow.provider, workspace provider, or snapshot.defaultProvider.",
		))
	}

	provider, ok := findProvider(snapshot.Providers, resolved)
	if !ok {
		return append(checks, readinessCriticalDiagnostic(
			"readiness-provider-not-found",
			"readiness",
			fieldPath,
			"Resolved provider is not configured",
			fmt.Sprintf("Provider %q was resolved but is not present in the readiness snapshot.", resolved),
		))
	}

	providerType := normalizedProviderType(provider)
	switch providerType {
	case "docker":
		checks = append(checks, readinessCriticalDiagnostic(
			"readiness-provider-runtime-unsupported",
			"readiness",
			fieldPath,
			"Docker provider is not supported by workflow runtime",
			"Use replicated, daytona, or exedev until workflow runtime supports docker.",
		))
	case "noop":
		if !opts.AllowNoopProvider {
			checks = append(checks, readinessCriticalDiagnostic(
				"readiness-provider-noop-disabled",
				"readiness",
				fieldPath,
				"noop provider is only available in explicit test mode",
				"Enable readiness test mode for noop, or select a production provider.",
			))
		}
	case "replicated", "daytona", "exedev":
		if !providerRuntimeConfigured(provider, providerType) {
			checks = append(checks, readinessCriticalDiagnostic(
				"readiness-provider-unconfigured",
				"readiness",
				fieldPath,
				"Resolved provider is not provisionable",
				fmt.Sprintf("Provider %q must be provisionable and have credentials configured.", resolved),
			))
		}
	default:
		checks = append(checks, readinessCriticalDiagnostic(
			"readiness-provider-runtime-unsupported",
			"readiness",
			fieldPath,
			"Provider is not supported by workflow runtime",
			fmt.Sprintf("Provider type %q is not supported by workflow readiness.", providerType),
		))
	}
	return checks
}

func resolvedProvider(workspace *ParsedWorkspaceConfig, workflow *types.WorkflowConfig, snapshot SetupEnvironmentSnapshot) (string, string) {
	if workflow != nil && strings.TrimSpace(workflow.Provider) != "" {
		return strings.TrimSpace(workflow.Provider), "workflow.provider"
	}
	if workspace != nil && workspace.Template != nil && strings.TrimSpace(workspace.Template.Provider) != "" {
		return strings.TrimSpace(workspace.Template.Provider), "workspace.provider"
	}
	if strings.TrimSpace(snapshot.DefaultProvider) != "" {
		return strings.TrimSpace(snapshot.DefaultProvider), "snapshot.defaultProvider"
	}
	return "", "workflow.provider"
}

func findProvider(providers []ProviderRef, name string) (ProviderRef, bool) {
	name = strings.TrimSpace(name)
	for _, provider := range providers {
		if strings.TrimSpace(provider.Name) == name {
			return provider, true
		}
	}
	for _, provider := range providers {
		if strings.TrimSpace(provider.Type) == name {
			return provider, true
		}
	}
	return ProviderRef{}, false
}

func normalizedProviderType(provider ProviderRef) string {
	if strings.TrimSpace(provider.Type) != "" {
		return strings.TrimSpace(provider.Type)
	}
	return strings.TrimSpace(provider.Name)
}

func providerRuntimeConfigured(provider ProviderRef, providerType string) bool {
	if !provider.Provisionable {
		return false
	}
	if provider.CredentialsSet {
		return true
	}
	switch providerType {
	case "replicated":
		return provider.TokenSet
	case "daytona":
		return provider.APIKeySet
	case "exedev":
		return provider.SSHKeySet || provider.AccessTokenSet
	default:
		return false
	}
}

func validateLLMReadiness(snapshot SetupEnvironmentSnapshot, workspace *ParsedWorkspaceConfig) []Diagnostic {
	var checks []Diagnostic
	if !hasConfiguredLLMKey(snapshot.LLMKeys) {
		checks = append(checks, readinessCriticalDiagnostic(
			"readiness-llm-key-missing",
			"readiness",
			"snapshot.llmKeys",
			"No LLM key is configured",
			"Configure at least one LLM key before enabling runtime workflows.",
		))
	}

	if resolvedModel(workspace, snapshot) == "" {
		checks = append(checks, readinessCriticalDiagnostic(
			"readiness-model-missing",
			"readiness",
			"workspace.default_model",
			"No default model could be resolved",
			"Set workspace default_model or snapshot.defaultModel.",
		))
	}
	return checks
}

func hasConfiguredLLMKey(keys []LLMKeyRef) bool {
	for _, key := range keys {
		if key.KeySet {
			return true
		}
	}
	return false
}

func resolvedModel(workspace *ParsedWorkspaceConfig, snapshot SetupEnvironmentSnapshot) string {
	if workspace != nil && workspace.Template != nil && strings.TrimSpace(workspace.Template.DefaultModel) != "" {
		return strings.TrimSpace(workspace.Template.DefaultModel)
	}
	return strings.TrimSpace(snapshot.DefaultModel)
}

func validateConcurrencyReadiness(snapshot SetupEnvironmentSnapshot, workflow *types.WorkflowConfig) []Diagnostic {
	if workflow == nil {
		return nil
	}
	group := strings.TrimSpace(workflow.ConcurrencyGroup)
	if group == "" || group == "global" {
		return nil
	}
	for _, candidate := range snapshot.ConcurrencyGroups {
		if strings.TrimSpace(candidate.Name) == group {
			return nil
		}
	}
	return []Diagnostic{readinessCriticalDiagnostic(
		"readiness-concurrency-group-missing",
		"readiness",
		"workflow.concurrency_group",
		"Concurrency group is not configured",
		fmt.Sprintf("Concurrency group %q must exist in the readiness snapshot, or use global.", group),
	)}
}

func validateSecretReadiness(env Environment, snapshot SetupEnvironmentSnapshot, workspace *ParsedWorkspaceConfig, workflow *types.WorkflowConfig, workspaceName string) []Diagnostic {
	var checks []Diagnostic
	knownSecrets := map[string]bool{}
	for _, name := range snapshot.HubSecretNames {
		if strings.TrimSpace(name) != "" {
			knownSecrets[strings.TrimSpace(name)] = true
		}
	}
	if workspaceName != "" {
		names, err := env.WorkspaceSecretNames(workspaceName)
		if err != nil {
			checks = append(checks, readinessWarningDiagnostic(
				"readiness-workspace-secrets-not-checked",
				"readiness",
				"workspace.secrets",
				"Workspace secret names could not be loaded",
				err.Error(),
			))
		} else {
			for _, name := range names {
				if strings.TrimSpace(name) != "" {
					knownSecrets[strings.TrimSpace(name)] = true
				}
			}
		}
	}

	if workspace != nil && workspace.Workspace != nil {
		for envName, envVar := range workspace.Workspace.Env {
			if strings.TrimSpace(envVar.Secret) == "" {
				continue
			}
			checks = append(checks, validateSecretRef(knownSecrets, envVar.Secret, fmt.Sprintf("workspace.env.%s.secret", envName))...)
		}
	}
	if workspace != nil && workspace.Template != nil {
		for envName, secretName := range workspace.Template.SecretRefs {
			checks = append(checks, validateSecretRef(knownSecrets, secretName, fmt.Sprintf("workspace.secret_refs.%s", envName))...)
		}
	}
	if workflow != nil {
		for envName, secretName := range workflow.SecretRefs {
			checks = append(checks, validateSecretRef(knownSecrets, secretName, fmt.Sprintf("workflow.secret_refs.%s", envName))...)
		}
	}
	return checks
}

func validateSecretRef(knownSecrets map[string]bool, secretName, fieldPath string) []Diagnostic {
	secretName = strings.TrimSpace(secretName)
	if secretName != "" && knownSecrets[secretName] {
		return nil
	}
	return []Diagnostic{readinessCriticalDiagnostic(
		"readiness-secret-ref-missing",
		"readiness",
		fieldPath,
		"Secret reference cannot be resolved",
		"Reference a workspace-managed secret or a hub secret name.",
	)}
}

func validateIssueTrackerReadiness(env Environment, snapshot SetupEnvironmentSnapshot, workflow *types.WorkflowConfig, workspaceName string) []Diagnostic {
	if workflow == nil || !issueIntegrationRequiresTracker(workflow.Integration) {
		return nil
	}

	trackers := append([]IssueTrackerRef(nil), snapshot.IssueTrackers...)
	if workspaceName != "" {
		workspaceTrackers, err := env.WorkspaceIssueTrackers(workspaceName)
		if err != nil {
			return []Diagnostic{readinessWarningDiagnostic(
				"readiness-workspace-issue-trackers-not-checked",
				"readiness",
				"workspace.issueTrackers",
				"Workspace issue trackers could not be loaded",
				err.Error(),
			)}
		}
		trackers = workspaceTrackers
	}

	fieldPath := issueTrackerFieldPath(workflow)
	tracker, status := findIssueTracker(trackers, workflow.Integration, workflowIssueTrackerWorkspace(workflow))
	if status == issueTrackerAmbiguous {
		return []Diagnostic{readinessCriticalDiagnostic(
			"readiness-issue-tracker-ambiguous",
			"readiness",
			fieldPath,
			"Issue tracker workspace is ambiguous",
			fmt.Sprintf("Set a %s trigger workspace, or configure exactly one workspace-managed tracker of that type.", workflow.Integration),
		)}
	}
	if status != issueTrackerResolved || !tracker.TokenSet {
		return []Diagnostic{readinessCriticalDiagnostic(
			"readiness-issue-tracker-missing",
			"readiness",
			fieldPath,
			"Issue tracker credentials are not configured",
			fmt.Sprintf("Configure credentials for %s before saving this workflow.", workflow.Integration),
		)}
	}

	var checks []Diagnostic
	if workflow.Integration == "github-issues" && workflow.EnableManualTrigger {
		checks = append(checks, validateGitHubIssuesManualRepo(workflow)...)
	}
	if !workflowIsManualOnly(workflow) && !tracker.WebhookSecretSet {
		checks = append(checks, readinessCriticalDiagnostic(
			"readiness-webhook-secret-missing",
			"readiness",
			fieldPath,
			"Webhook secret is not configured",
			"Configure a webhook secret for automatic issue-source workflows.",
		))
	}
	return checks
}

type issueTrackerResolution int

const (
	issueTrackerMissing issueTrackerResolution = iota
	issueTrackerResolved
	issueTrackerAmbiguous
)

func issueIntegrationRequiresTracker(integration string) bool {
	switch integration {
	case "github-issues", "linear", "shortcut":
		return true
	default:
		return false
	}
}

func findIssueTracker(trackers []IssueTrackerRef, trackerType, workspace string) (IssueTrackerRef, issueTrackerResolution) {
	trackerType = strings.TrimSpace(trackerType)
	workspace = strings.TrimSpace(workspace)
	var candidates []IssueTrackerRef
	for _, tracker := range trackers {
		if strings.TrimSpace(tracker.Type) == trackerType {
			candidates = append(candidates, tracker)
		}
	}
	if workspace != "" {
		for _, tracker := range candidates {
			if strings.EqualFold(strings.TrimSpace(tracker.Workspace), workspace) {
				return tracker, issueTrackerResolved
			}
		}
		return IssueTrackerRef{}, issueTrackerMissing
	}
	if len(candidates) == 1 {
		return candidates[0], issueTrackerResolved
	}
	if len(candidates) > 1 {
		return IssueTrackerRef{}, issueTrackerAmbiguous
	}
	return IssueTrackerRef{}, issueTrackerMissing
}

func workflowIssueTrackerWorkspace(workflow *types.WorkflowConfig) string {
	if workflow == nil {
		return ""
	}
	if strings.TrimSpace(workflow.Workspace) != "" {
		return strings.TrimSpace(workflow.Workspace)
	}
	if workflow.Trigger == nil {
		return ""
	}
	if workflow.Trigger.Linear != nil {
		return strings.TrimSpace(workflow.Trigger.Linear.Workspace)
	}
	if workflow.Trigger.Shortcut != nil {
		return strings.TrimSpace(workflow.Trigger.Shortcut.Workspace)
	}
	return ""
}

func issueTrackerFieldPath(workflow *types.WorkflowConfig) string {
	if workflow == nil || workflow.Trigger == nil {
		return "workflow.integration"
	}
	if workflow.Trigger.GitHubIssues != nil {
		return "workflow.trigger.github_issues"
	}
	if workflow.Trigger.Linear != nil {
		return "workflow.trigger.linear"
	}
	if workflow.Trigger.Shortcut != nil {
		return "workflow.trigger.shortcut"
	}
	return "workflow.integration"
}

func workflowIsManualOnly(workflow *types.WorkflowConfig) bool {
	return workflow != nil && workflow.EnableManualTrigger && workflowTriggerSourceCount(workflow.Trigger) == 0
}

func validateGitHubIssuesManualRepo(workflow *types.WorkflowConfig) []Diagnostic {
	repos := githubIssuesManualRepos(workflow)
	if len(repos) == 1 && repositoryPattern.MatchString(repos[0]) {
		return nil
	}
	return []Diagnostic{readinessCriticalDiagnostic(
		"readiness-github-issues-manual-repo",
		"readiness",
		"workflow.trigger.github_issues.repositories",
		"GitHub issue manual workflow requires exactly one repository",
		"Set one exact owner/repo repository; wildcard and org-only selectors cannot be used for manual issue runs.",
	)}
}

func githubIssuesManualRepos(workflow *types.WorkflowConfig) []string {
	if workflow == nil {
		return nil
	}
	var raw []string
	if workflow.Trigger != nil && workflow.Trigger.GitHubIssues != nil {
		raw = workflow.Trigger.GitHubIssues.Repositories
	} else if len(workflow.TriggerRepos) > 0 {
		raw = workflow.TriggerRepos
	} else {
		raw = workflow.Repos
	}
	seen := map[string]bool{}
	var repos []string
	for _, repo := range raw {
		repo = strings.TrimSpace(repo)
		if repo == "" || seen[repo] {
			continue
		}
		seen[repo] = true
		repos = append(repos, repo)
	}
	return repos
}

func parsedWorkspaceName(workspace *ParsedWorkspaceConfig) string {
	if workspace == nil || workspace.Workspace == nil {
		return ""
	}
	return strings.TrimSpace(workspace.Workspace.Name)
}

func readinessCriticalDiagnostic(id, category, fieldPath, title, detail string) Diagnostic {
	diagnostic := criticalDiagnostic(id, category, fieldPath, title, detail)
	diagnostic.Step = "validate-readiness"
	return diagnostic
}

func readinessWarningDiagnostic(id, category, fieldPath, title, detail string) Diagnostic {
	return Diagnostic{
		ID:        id,
		Category:  category,
		Severity:  SeverityWarning,
		OK:        false,
		Blocking:  false,
		Step:      "validate-readiness",
		FieldPath: fieldPath,
		Title:     title,
		Detail:    detail,
		FixTarget: fieldPath,
		FixLabel:  "Review config",
		Retryable: true,
		Status:    "warning",
	}
}

func networkNotCheckedDiagnostic() Diagnostic {
	return Diagnostic{
		ID:        "readiness-network-checks",
		Category:  "readiness",
		Severity:  SeverityInfo,
		OK:        false,
		Blocking:  false,
		Step:      "validate-readiness",
		FieldPath: "network",
		Title:     "Network checks were not run",
		Detail:    "Readiness validation is local-only; provider, LLM, repository, and tracker network calls are not checked.",
		FixTarget: "network",
		FixLabel:  "Run runtime verification",
		Retryable: false,
		Status:    "not_checked",
	}
}
