package hub

import (
	"net/http"
	"sort"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

const defaultWorkspaceName = "default"

// WorkspaceView is the compatibility view that presents today's hub-level
// config as the new workspace/workflow model.
type WorkspaceView struct {
	Name      string          `json:"name"`
	Source    string          `json:"source"`
	Access    WorkspaceAccess `json:"access"`
	Workflows []WorkflowView  `json:"workflows"`
}

// WorkspaceAccess is the maximum access available to workflows in a workspace.
// Values are names or repo selectors only; secret values are never exposed.
type WorkspaceAccess struct {
	Repositories   []string `json:"repositories"`
	Secrets        []string `json:"secrets"`
	WebhookSecrets []string `json:"webhookSecrets"`
}

// WorkflowView is a workflow-shaped projection of a legacy factory.
type WorkflowView struct {
	Name                 string               `json:"name"`
	WorkspaceName        string               `json:"workspaceName"`
	Source               string               `json:"source"`
	LegacyFactoryName    string               `json:"legacyFactoryName"`
	Integration          string               `json:"integration"`
	IntegrationWorkspace string               `json:"integrationWorkspace,omitempty"`
	TriggerStatus        string               `json:"triggerStatus,omitempty"`
	DoneStatus           string               `json:"doneStatus,omitempty"`
	Template             string               `json:"template"`
	Labels               []string             `json:"labels,omitempty"`
	AssignedTo           string               `json:"assignedTo,omitempty"`
	Enabled              bool                 `json:"enabled"`
	HasWebhookSecret     bool                 `json:"hasWebhookSecret"`
	WebhookSecretRef     string               `json:"webhookSecretRef,omitempty"`
	PipelineYAML         string               `json:"pipelineYAML,omitempty"`
	EnableManualTrigger  bool                 `json:"enableManualTrigger,omitempty"`
	SecretRefs           map[string]string    `json:"secretRefs,omitempty"`
	Inputs               []types.FactoryInput `json:"inputs,omitempty"`
}

func (s *Server) handleWorkspacesList(w http.ResponseWriter, _ *http.Request) {
	jsonOK(w, s.workspaceViews())
}

func (s *Server) handleWorkspaceWorkflowsList(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		http.Error(w, "workspace name required", http.StatusBadRequest)
		return
	}
	for _, workspace := range s.workspaceViews() {
		if strings.EqualFold(workspace.Name, name) {
			jsonOK(w, workspace.Workflows)
			return
		}
	}
	http.Error(w, "workspace not found", http.StatusNotFound)
}

func (s *Server) workspaceViews() []WorkspaceView {
	s.mu.RLock()
	cfg := s.hubCfg
	s.mu.RUnlock()

	factories := s.resolveFactories()
	workspace := projectDefaultWorkspace(cfg, factories)
	return []WorkspaceView{workspace}
}

func projectDefaultWorkspace(cfg *types.HubConfig, factories []*types.FactoryConfig) WorkspaceView {
	if cfg == nil {
		cfg = &types.HubConfig{}
	}

	access := WorkspaceAccess{
		Repositories:   collectWorkspaceRepositories(factories),
		Secrets:        sortedMapKeys(cfg.Secrets),
		WebhookSecrets: collectWebhookSecretNames(cfg, factories),
	}

	workflows := make([]WorkflowView, 0, len(factories))
	for _, f := range factories {
		if f == nil {
			continue
		}
		workflows = append(workflows, workflowFromFactory(f))
	}
	sort.Slice(workflows, func(i, j int) bool {
		return strings.ToLower(workflows[i].Name) < strings.ToLower(workflows[j].Name)
	})

	return WorkspaceView{
		Name:      defaultWorkspaceName,
		Source:    "compatibility",
		Access:    access,
		Workflows: workflows,
	}
}

func workflowFromFactory(f *types.FactoryConfig) WorkflowView {
	return WorkflowView{
		Name:                 f.Name,
		WorkspaceName:        defaultWorkspaceName,
		Source:               "factory",
		LegacyFactoryName:    f.Name,
		Integration:          f.Integration,
		IntegrationWorkspace: f.Workspace,
		TriggerStatus:        f.TriggerStatus,
		DoneStatus:           f.DoneStatus,
		Template:             f.Template,
		Labels:               append([]string(nil), f.Labels...),
		AssignedTo:           f.AssignedTo,
		Enabled:              isFactoryEnabled(f),
		HasWebhookSecret:     f.WebhookSecret != "" || f.WebhookSecretRef != "",
		WebhookSecretRef:     f.WebhookSecretRef,
		PipelineYAML:         f.PipelineYAML,
		EnableManualTrigger:  f.EnableManualTrigger,
		SecretRefs:           cloneStringMap(f.SecretRefs),
		Inputs:               append([]types.FactoryInput(nil), f.Inputs...),
	}
}

func collectWorkspaceRepositories(factories []*types.FactoryConfig) []string {
	repos := map[string]struct{}{}
	for _, f := range factories {
		if f == nil {
			continue
		}
		for _, repo := range f.Repos {
			addNonEmpty(repos, repo)
		}
		for _, repo := range f.TriggerRepos {
			addNonEmpty(repos, repo)
		}
		if f.ExternalTrigger != nil && f.ExternalTrigger.Filter != nil {
			addNonEmpty(repos, f.ExternalTrigger.Filter.Repository)
		}
	}
	return sortedSetKeys(repos)
}

func collectWebhookSecretNames(cfg *types.HubConfig, factories []*types.FactoryConfig) []string {
	names := map[string]struct{}{}
	if cfg != nil && cfg.Integrations != nil {
		for _, linear := range cfg.Integrations.Linear {
			if linear != nil && linear.WebhookSecret != "" {
				addNonEmpty(names, "linear:"+linear.Workspace)
			}
		}
		for _, githubIssues := range cfg.Integrations.GitHubIssues {
			if githubIssues != nil && githubIssues.WebhookSecret != "" {
				addNonEmpty(names, "github-issues:"+githubIssues.Workspace)
			}
		}
	}
	for _, f := range factories {
		if f == nil {
			continue
		}
		addNonEmpty(names, f.WebhookSecretRef)
		if f.WebhookSecret != "" && f.WebhookSecretRef == "" {
			addNonEmpty(names, "factory:"+f.Name)
		}
	}
	return sortedSetKeys(names)
}

func sortedMapKeys(values map[string]string) []string {
	if len(values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedSetKeys(values map[string]struct{}) []string {
	if len(values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func addNonEmpty(values map[string]struct{}, value string) {
	value = strings.TrimSpace(value)
	if value != "" {
		values[value] = struct{}{}
	}
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}
