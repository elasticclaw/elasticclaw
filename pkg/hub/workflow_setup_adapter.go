package hub

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/elasticclaw/elasticclaw/pkg/workflowsetup"
)

type workflowSetupEnvironment struct {
	server *Server
}

// WorkflowSetupEnvironment returns the sanitized hub view used by workflow setup.
func (s *Server) WorkflowSetupEnvironment() workflowsetup.Environment {
	return &workflowSetupEnvironment{server: s}
}

func (e *workflowSetupEnvironment) Snapshot() (workflowsetup.SetupEnvironmentSnapshot, error) {
	if e == nil || e.server == nil {
		return workflowsetup.SetupEnvironmentSnapshot{}, fmt.Errorf("workflow setup environment has no server")
	}

	e.server.mu.RLock()
	defer e.server.mu.RUnlock()

	cfg := e.server.hubCfg
	if cfg == nil {
		return workflowsetup.SetupEnvironmentSnapshot{
			Providers:         []workflowsetup.ProviderRef{},
			LLMKeys:           []workflowsetup.LLMKeyRef{},
			ConcurrencyGroups: []workflowsetup.ConcurrencyGroupRef{},
			HubSecretNames:    []string{},
			IssueTrackers:     []workflowsetup.IssueTrackerRef{},
			GitHubApps:        []workflowsetup.GitHubAppRef{},
		}, nil
	}

	snapshot := workflowsetup.SetupEnvironmentSnapshot{
		ClawTokenSet:      cfg.ClawToken != "",
		DefaultProvider:   workflowSetupDefaultProvider(cfg.Providers),
		DefaultModel:      cfg.DefaultModel,
		Providers:         workflowSetupProviderRefs(cfg.Providers),
		LLMKeys:           workflowSetupLLMKeyRefs(cfg.LLMKeys),
		ConcurrencyGroups: workflowSetupConcurrencyGroupRefs(cfg.ConcurrencyGroups),
		HubSecretNames:    workflowSetupHubSecretNames(cfg.Secrets),
		IssueTrackers:     workflowSetupIssueTrackerRefs(cfg),
		GitHubApps:        workflowSetupGitHubAppRefs(cfg.GitHubApps),
	}
	return snapshot, nil
}

func (e *workflowSetupEnvironment) LoadWorkspace(name string) (*types.WorkspaceConfig, error) {
	return loadExternalWorkspace(name)
}

func (e *workflowSetupEnvironment) LoadWorkflowRaw(workspaceName, workflowName string) (string, error) {
	return loadExternalWorkflowRaw(workspaceName, workflowName)
}

func (e *workflowSetupEnvironment) WorkspaceSecretNames(workspaceName string) ([]string, error) {
	return workspaceSecretNames(workspaceName)
}

func (e *workflowSetupEnvironment) WorkspaceIssueTrackers(workspaceName string) ([]workflowsetup.IssueTrackerRef, error) {
	views, err := workspaceIssueTrackerViews(workspaceName)
	if err != nil {
		return nil, err
	}
	refs := make([]workflowsetup.IssueTrackerRef, 0, len(views))
	for _, view := range views {
		refs = append(refs, workflowsetup.IssueTrackerRef{
			Type:             view.Type,
			Workspace:        view.Workspace,
			TokenSet:         view.TokenSet,
			WebhookSecretSet: view.WebhookSecretSet,
		})
	}
	return refs, nil
}

func (e *workflowSetupEnvironment) WorkspaceGitHubApps(workspaceName string) ([]workflowsetup.GitHubAppRef, error) {
	views, err := workspaceGitHubAppViews(workspaceName)
	if err != nil {
		return nil, err
	}
	refs := make([]workflowsetup.GitHubAppRef, 0, len(views))
	for _, view := range views {
		refs = append(refs, workflowsetup.GitHubAppRef{
			Name:          view.Name,
			AppID:         view.AppID,
			URL:           view.URL,
			Installation:  view.Installation,
			Installations: append([]string(nil), view.Installations...),
			PrivateKeySet: view.PrivateKeySet,
		})
	}
	return refs, nil
}

func (e *workflowSetupEnvironment) ListFactories() ([]workflowsetup.FactoryRef, error) {
	if e == nil || e.server == nil {
		return nil, fmt.Errorf("workflow setup environment has no server")
	}
	factories := e.resolveFactories()
	refs := make([]workflowsetup.FactoryRef, 0, len(factories))
	for _, factory := range factories {
		if factory == nil {
			continue
		}
		refs = append(refs, workflowSetupFactoryRef(factory))
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs, nil
}

func (e *workflowSetupEnvironment) LoadFactory(name string) (*types.FactoryConfig, error) {
	if e == nil || e.server == nil {
		return nil, fmt.Errorf("workflow setup environment has no server")
	}
	factories := e.resolveFactories()
	for _, factory := range factories {
		if factory != nil && strings.EqualFold(factory.Name, name) {
			return sanitizeWorkflowSetupFactory(factory), nil
		}
	}
	return nil, fmt.Errorf("%w: factory %q", os.ErrNotExist, name)
}

func (e *workflowSetupEnvironment) resolveFactories() []*types.FactoryConfig {
	external, err := loadExternalFactories()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[hub] loadExternalFactories: %v\n", err)
	}

	e.server.mu.RLock()
	var mem []*types.FactoryConfig
	if e.server.hubCfg != nil {
		mem = make([]*types.FactoryConfig, 0, len(e.server.hubCfg.Factories))
		for _, factory := range e.server.hubCfg.Factories {
			if factory == nil {
				continue
			}
			mem = append(mem, cloneWorkflowSetupFactory(factory))
		}
	}
	e.server.mu.RUnlock()

	merged := make(map[string]*types.FactoryConfig, len(mem)+len(external))
	for _, factory := range mem {
		if factory == nil {
			continue
		}
		merged[factory.Name] = factory
	}
	for _, factory := range external {
		if factory == nil {
			continue
		}
		merged[factory.Name] = cloneWorkflowSetupFactory(factory)
	}

	result := make([]*types.FactoryConfig, 0, len(merged))
	for _, factory := range merged {
		result = append(result, factory)
	}
	return result
}

func workflowSetupProviderRefs(providers map[string]types.ProviderConfig) []workflowsetup.ProviderRef {
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)

	refs := make([]workflowsetup.ProviderRef, 0, len(names))
	for _, name := range names {
		provider := providers[name]
		providerType := workflowSetupProviderType(name, provider)
		apiKeySet := provider.APIKey != ""
		tokenSet := provider.Token != ""
		accessTokenSet := provider.AccessToken != ""
		sshKeySet := provider.SSHKeyPath != ""
		refs = append(refs, workflowsetup.ProviderRef{
			Name:           name,
			Type:           providerType,
			Provisionable:  workflowSetupProviderProvisionable(name, provider),
			CredentialsSet: apiKeySet || tokenSet || accessTokenSet || sshKeySet,
			APIKeySet:      apiKeySet,
			TokenSet:       tokenSet,
			AccessTokenSet: accessTokenSet,
			SSHKeySet:      sshKeySet,
		})
	}
	return refs
}

func workflowSetupProviderType(name string, provider types.ProviderConfig) string {
	if provider.Type != "" {
		return provider.Type
	}
	return name
}

func workflowSetupProviderProvisionable(name string, provider types.ProviderConfig) bool {
	switch workflowSetupProviderType(name, provider) {
	case "daytona":
		return provider.APIKey != ""
	case "replicated":
		return provider.Token != ""
	case "exedev", "docker":
		return true
	default:
		return provider.Token != "" || provider.APIKey != "" || provider.AccessToken != ""
	}
}

func workflowSetupDefaultProvider(providers map[string]types.ProviderConfig) string {
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		provider := providers[name]
		if provider.Token != "" || provider.APIKey != "" || provider.AccessToken != "" {
			return name
		}
	}
	return ""
}

func workflowSetupLLMKeyRefs(keys types.LLMKeysList) []workflowsetup.LLMKeyRef {
	refs := make([]workflowsetup.LLMKeyRef, 0, len(keys))
	for _, key := range keys {
		if key == nil {
			continue
		}
		refs = append(refs, workflowsetup.LLMKeyRef{
			Name:         key.Name,
			Provider:     key.Provider,
			KeySet:       llmKeyHasRequiredAPIKey(key),
			Default:      key.Default,
			DefaultModel: key.DefaultModel,
		})
	}
	return refs
}

func workflowSetupConcurrencyGroupRefs(groups []*types.ConcurrencyGroup) []workflowsetup.ConcurrencyGroupRef {
	refs := make([]workflowsetup.ConcurrencyGroupRef, 0, len(groups))
	for _, group := range groups {
		if group == nil {
			continue
		}
		refs = append(refs, workflowsetup.ConcurrencyGroupRef{
			Name:  group.Name,
			Limit: group.Limit,
		})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs
}

func workflowSetupHubSecretNames(secrets map[string]string) []string {
	names := make([]string, 0, len(secrets))
	for name := range secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func workflowSetupIssueTrackerRefs(cfg *types.HubConfig) []workflowsetup.IssueTrackerRef {
	refs := []workflowsetup.IssueTrackerRef{}
	for _, linear := range cfg.Linear {
		if linear == nil {
			continue
		}
		refs = append(refs, workflowsetup.IssueTrackerRef{
			Type:      "linear",
			Workspace: linear.Workspace,
			TokenSet:  linear.Token != "",
		})
	}
	if cfg.Integrations != nil {
		for _, linear := range cfg.Integrations.Linear {
			if linear == nil {
				continue
			}
			refs = append(refs, workflowsetup.IssueTrackerRef{
				Type:             "linear",
				Workspace:        linear.Workspace,
				TokenSet:         linear.Token != "",
				WebhookSecretSet: linear.WebhookSecret != "",
			})
		}
		for _, shortcut := range cfg.Integrations.Shortcut {
			if shortcut == nil {
				continue
			}
			refs = append(refs, workflowsetup.IssueTrackerRef{
				Type:      "shortcut",
				Workspace: shortcut.Workspace,
				TokenSet:  shortcut.Token != "",
			})
		}
		for _, githubIssues := range cfg.Integrations.GitHubIssues {
			if githubIssues == nil {
				continue
			}
			refs = append(refs, workflowsetup.IssueTrackerRef{
				Type:             "github-issues",
				Workspace:        githubIssues.Workspace,
				TokenSet:         githubIssues.Token != "",
				WebhookSecretSet: githubIssues.WebhookSecret != "",
			})
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Type != refs[j].Type {
			return refs[i].Type < refs[j].Type
		}
		return refs[i].Workspace < refs[j].Workspace
	})
	return refs
}

func workflowSetupGitHubAppRefs(apps []*types.GitHubAppConfig) []workflowsetup.GitHubAppRef {
	refs := make([]workflowsetup.GitHubAppRef, 0, len(apps))
	for _, app := range apps {
		if app == nil {
			continue
		}
		refs = append(refs, workflowsetup.GitHubAppRef{
			AppID:         app.AppID,
			URL:           app.URL,
			PrivateKeySet: app.PrivateKeyPEM != "",
		})
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].AppID != refs[j].AppID {
			return refs[i].AppID < refs[j].AppID
		}
		return refs[i].URL < refs[j].URL
	})
	return refs
}

func workflowSetupFactoryRef(factory *types.FactoryConfig) workflowsetup.FactoryRef {
	return workflowsetup.FactoryRef{
		Name:                factory.Name,
		Integration:         factory.Integration,
		Workspace:           factory.Workspace,
		Team:                factory.Team,
		Template:            factory.Template,
		Provider:            factory.Provider,
		Enabled:             isFactoryEnabled(factory),
		ConcurrencyGroup:    factory.ConcurrencyGroup,
		EnableManualTrigger: factory.EnableManualTrigger,
		WebhookSecretSet:    factory.WebhookSecret != "" || factory.WebhookSecretRef != "",
		WebhookSecretRef:    factory.WebhookSecretRef,
		PipelineSet:         factory.PipelineYAML != "",
		SecretRefs:          cloneStringMap(factory.SecretRefs),
	}
}

func sanitizeWorkflowSetupFactory(factory *types.FactoryConfig) *types.FactoryConfig {
	clone := cloneWorkflowSetupFactory(factory)
	if clone == nil {
		return nil
	}
	clone.WebhookSecret = ""
	return clone
}

func cloneWorkflowSetupFactory(factory *types.FactoryConfig) *types.FactoryConfig {
	if factory == nil {
		return nil
	}
	clone := *factory
	if factory.Enabled != nil {
		enabled := *factory.Enabled
		clone.Enabled = &enabled
	}
	clone.Tags = append([]string(nil), factory.Tags...)
	clone.Labels = append([]string(nil), factory.Labels...)
	clone.AllowedLabelers = append([]string(nil), factory.AllowedLabelers...)
	clone.Inputs = cloneFactoryInputs(factory.Inputs)
	clone.SecretRefs = cloneStringMap(factory.SecretRefs)
	clone.Repos = append([]string(nil), factory.Repos...)
	clone.TriggerRepos = append([]string(nil), factory.TriggerRepos...)
	clone.Trigger = cloneGitHubTrigger(factory.Trigger)
	clone.ExternalTrigger = cloneExternalTrigger(factory.ExternalTrigger)
	return &clone
}

func cloneFactoryInputs(inputs []types.FactoryInput) []types.FactoryInput {
	if inputs == nil {
		return nil
	}
	cloned := make([]types.FactoryInput, len(inputs))
	for i, input := range inputs {
		cloned[i] = input
		cloned[i].Options = append([]string(nil), input.Options...)
		if input.Min != nil {
			min := *input.Min
			cloned[i].Min = &min
		}
		if input.Max != nil {
			max := *input.Max
			cloned[i].Max = &max
		}
	}
	return cloned
}

func cloneGitHubTrigger(trigger *types.GitHubTrigger) *types.GitHubTrigger {
	if trigger == nil {
		return nil
	}
	clone := *trigger
	if trigger.Filter != nil {
		filter := *trigger.Filter
		clone.Filter = &filter
	}
	return &clone
}

func cloneExternalTrigger(trigger *types.ExternalTrigger) *types.ExternalTrigger {
	if trigger == nil {
		return nil
	}
	clone := *trigger
	if trigger.Filter != nil {
		filter := *trigger.Filter
		clone.Filter = &filter
	}
	return &clone
}
