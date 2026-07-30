package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

type RoutinePreflightCheck struct {
	ID          string     `json:"id"`
	Category    string     `json:"category"`
	Status      string     `json:"status"` // pass, warning, error
	Title       string     `json:"title"`
	Description string     `json:"description"`
	FixAction   *FixAction `json:"fixAction,omitempty"`
}

type RoutinePreflightResponse struct {
	Ready  bool                    `json:"ready"`
	Status string                  `json:"status"` // ready, needs_setup
	Checks []RoutinePreflightCheck `json:"checks"`
}

type routinePreflightRequest struct {
	Workflow *types.WorkflowConfig `json:"workflow"`
}

func (s *Server) handleRoutinePreflight(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	workspaceName := strings.TrimSpace(r.PathValue("workspace"))
	workspace, err := loadExternalWorkspace(workspaceName)
	if err != nil {
		http.Error(w, "workspace not found", http.StatusNotFound)
		return
	}
	var req routinePreflightRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256*1024)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.Workflow == nil {
		http.Error(w, "workflow required", http.StatusBadRequest)
		return
	}
	jsonOK(w, s.preflightRoutine(workspace, req.Workflow))
}

func (s *Server) handleSavedRoutinePreflight(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	workspaceName := strings.TrimSpace(r.PathValue("workspace"))
	workflowName := strings.TrimSpace(r.PathValue("workflow"))
	workspace, workflow, ok, err := s.resolveWorkflowConfig(workspaceName, workflowName)
	if err != nil {
		http.Error(w, "failed to load workflow: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "workflow not found", http.StatusNotFound)
		return
	}
	jsonOK(w, s.preflightRoutine(workspace, workflow))
}

func (s *Server) preflightRoutine(workspace *types.WorkspaceConfig, workflow *types.WorkflowConfig) RoutinePreflightResponse {
	checks := make([]RoutinePreflightCheck, 0, 8)
	add := func(id, category, status, title, description, target, label string) {
		var action *FixAction
		if target != "" {
			action = &FixAction{Type: "navigate", Target: target, Label: label}
		}
		checks = append(checks, RoutinePreflightCheck{
			ID: id, Category: category, Status: status, Title: title,
			Description: description, FixAction: action,
		})
	}

	workflowCopy := *workflow
	workflow = &workflowCopy
	if err := types.NormalizeWorkflowConfig(workflow); err != nil {
		add("workflow-schema", "workflow", "error", "Invalid routine configuration", err.Error(), "", "")
	} else if err := workflow.Validate(); err != nil {
		add("workflow-schema", "workflow", "error", "Invalid routine configuration", err.Error(), "", "")
	} else if workflow.Trigger == nil || workflow.Trigger.Cron == nil {
		add("workflow-schema", "workflow", "error", "Routine schedule missing", "A routine must use a cron trigger.", "", "")
	} else {
		add("workflow-schema", "workflow", "pass", "Routine configuration is valid", "The workflow, cron schedule, timezone, and timeout are valid.", "", "")
	}

	var tmplCfg *types.TemplateConfig
	if raw := workspace.Files["elasticclaw-config.yaml"]; raw != "" {
		parsed, err := config.ParseTemplateConfig([]byte(raw))
		if err != nil {
			add("workspace-config", "workspace", "error", "Workspace configuration is invalid", err.Error(), "", "")
		} else {
			tmplCfg = parsed
			add("workspace-config", "workspace", "pass", "Workspace configuration loaded", "The workspace agent configuration can be parsed.", "", "")
		}
	} else {
		add("workspace-config", "workspace", "warning", "Workspace agent configuration not found", "No elasticclaw-config.yaml was found; Hub defaults will be used.", "", "")
	}

	s.mu.RLock()
	hubCfg := *s.hubCfg
	hubCfg.Providers = cloneProviderConfigs(s.hubCfg.Providers)
	hubCfg.LLMKeys = append(types.LLMKeysList(nil), s.hubCfg.LLMKeys...)
	hubCfg.ModelAuthProfiles = append([]*types.ModelAuthProfileConfig(nil), s.hubCfg.ModelAuthProfiles...)
	hubCfg.GitHubApps = append([]*types.GitHubAppConfig(nil), s.hubCfg.GitHubApps...)
	hubCfg.Secrets = cloneStringMap(s.hubCfg.Secrets)
	hubCfg.MCPServers = append([]*types.MCPServerHubConfig(nil), s.hubCfg.MCPServers...)
	s.mu.RUnlock()

	provider := strings.TrimSpace(workflow.Provider)
	if provider == "" && tmplCfg != nil {
		provider = strings.TrimSpace(tmplCfg.Provider)
	}
	if provider == "" {
		provider = defaultProviderFromConfig(&hubCfg)
	}
	if provider == "" {
		add("sandbox-provider", "sandbox", "error", "No sandbox provider selected", "Configure one sandbox provider or select one in the workspace.", "/settings/runtimes", "Configure sandbox")
	} else if providerErr := validateRoutineProvider(provider, hubCfg.Providers[provider], hubCfg.Providers); providerErr != "" {
		add("sandbox-provider", "sandbox", "error", "Sandbox provider is not ready", providerErr, "/settings/runtimes", "Configure sandbox")
	} else {
		add("sandbox-provider", "sandbox", "pass", "Sandbox provider is configured", fmt.Sprintf("The routine will use the %q provider.", provider), "", "")
	}

	selectedKey, model := "", hubCfg.DefaultModel
	if tmplCfg != nil {
		selectedKey, model = tmplCfg.LLMKey, firstNonEmpty(tmplCfg.DefaultModel, model)
	}
	model, selectedKey = resolveModelAndLLMKey(&hubCfg, selectedKey, model)
	activeKey := resolveActiveKey(hubCfg.LLMKeys, selectedKey)
	switch {
	case activeKey == nil:
		add("model", "model", "error", "No model credential is available", "Configure an LLM key or supported authentication profile for agent runs.", "/settings/models", "Configure model")
	case !llmKeyHasRequiredAPIKey(activeKey):
		add("model", "model", "error", "Model credential is incomplete", fmt.Sprintf("Model key %q has no API key or authentication profile.", activeKey.Name), "/settings/models", "Configure model")
	case activeKey.AuthProfile != "" && !routineAuthProfileReady(hubCfg.ModelAuthProfiles, activeKey):
		add("model", "model", "error", "Model authentication profile is unavailable", fmt.Sprintf("Authentication profile %q for %s has no stored credential.", activeKey.AuthProfile, activeKey.Provider), "/settings/models", "Reconnect model")
	case strings.TrimSpace(model) == "":
		add("model", "model", "error", "No model selected", fmt.Sprintf("Model key %q does not resolve to a model.", activeKey.Name), "/settings/models", "Select model")
	default:
		add("model", "model", "pass", "Model authentication is configured", fmt.Sprintf("Agent runs will use %s with key %q.", model, activeKey.Name), "", "")
	}

	workspaceSecrets, secretErr := loadWorkspaceSecrets(workspace.Name)
	if secretErr != nil {
		add("secrets", "secrets", "error", "Workspace secrets could not be read", secretErr.Error(), "/settings/secrets", "Manage secrets")
	} else {
		missing := missingRoutineSecrets(workflow, tmplCfg, workspaceSecrets, hubCfg.Secrets)
		if len(missing) > 0 {
			add("secrets", "secrets", "error", "Required secrets are missing", "Configure these secret names: "+strings.Join(missing, ", ")+". Never paste secret values into agent chat.", "/settings/secrets", "Configure secrets")
		} else {
			add("secrets", "secrets", "pass", "Required secrets are available", "Every explicitly referenced workspace and workflow secret exists.", "", "")
		}
	}

	repositories := workspace.Repositories
	if len(repositories) == 0 {
		add("github", "github", "warning", "No repositories are assigned", "The routine can run, but the agent will not receive a repository checkout.", "", "")
	} else {
		workspaceApps, _ := loadWorkspaceGitHubAppConfigs(workspace.Name)
		apps := append(workspaceApps, hubCfg.GitHubApps...)
		validApps := 0
		for _, app := range apps {
			if app != nil && app.AppID != 0 && strings.TrimSpace(app.PrivateKeyPEM) != "" {
				validApps++
			}
		}
		if validApps == 0 {
			add("github", "github", "error", "GitHub App credentials are missing", "This workspace declares repositories, but no usable workspace or Hub GitHub App is configured.", "/settings/github", "Configure GitHub App")
		} else {
			add("github", "github", "pass", "GitHub App credentials are configured", fmt.Sprintf("%d repository selector(s) will be requested. Installation scope is confirmed when a short-lived token is minted.", len(repositories)), "", "")
		}
	}

	if tmplCfg != nil {
		missingMCP := missingRoutineMCPs(tmplCfg, hubCfg.MCPServers, workspaceSecrets, hubCfg.Secrets)
		if len(missingMCP) > 0 {
			add("mcp", "mcp", "error", "MCP configuration is incomplete", strings.Join(missingMCP, "; "), "/settings/mcp", "Configure MCP servers")
		} else if len(tmplCfg.MCPs) > 0 {
			add("mcp", "mcp", "pass", "MCP servers are configured", fmt.Sprintf("%d workspace MCP server(s) are available.", len(tmplCfg.MCPs)), "", "")
		}
	}

	if workflow.Integration != "" {
		trackerType := workflow.Integration
		if trackerType == "github" {
			trackerType = "github-issues"
		}
		switch trackerType {
		case "linear", "shortcut", "github-issues", "jira":
			tracker, ok := findWorkspaceIssueTracker(workspace.Name, trackerType, workflow.Workspace)
			if !ok || strings.TrimSpace(tracker.Token) == "" {
				add("integration", "integration", "error", "Issue tracker credential is missing", fmt.Sprintf("The %s integration used by this workflow has no workspace token.", trackerType), "/settings/issue-trackers", "Configure issue tracker")
			} else {
				add("integration", "integration", "pass", "Issue tracker credential is configured", fmt.Sprintf("The %s workspace token is available.", trackerType), "", "")
			}
		}
	}

	ready := true
	for _, check := range checks {
		if check.Status == "error" {
			ready = false
			break
		}
	}
	status := "ready"
	if !ready {
		status = "needs_setup"
	}
	return RoutinePreflightResponse{Ready: ready, Status: status, Checks: checks}
}

func cloneProviderConfigs(in map[string]types.ProviderConfig) map[string]types.ProviderConfig {
	out := make(map[string]types.ProviderConfig, len(in))
	for name, provider := range in {
		out[name] = provider
	}
	return out
}

func defaultProviderFromConfig(cfg *types.HubConfig) string {
	var only string
	for name, provider := range cfg.Providers {
		if provider.Token != "" || provider.APIKey != "" || provider.AccessToken != "" {
			return name
		}
		if name != "noop" && provider.Type != "noop" {
			if only != "" {
				return ""
			}
			only = name
		}
	}
	return only
}

func validateRoutineProvider(name string, provider types.ProviderConfig, configured map[string]types.ProviderConfig) string {
	if _, ok := configured[name]; !ok {
		return fmt.Sprintf("Provider %q is not configured on this Hub.", name)
	}
	switch name {
	case "daytona":
		if provider.APIKey == "" {
			return "Daytona is missing its API key."
		}
	case "replicated":
		if provider.Token == "" {
			return "Replicated is missing its token."
		}
	case "lambda-microvms":
		if provider.ImageIdentifier == "" {
			return "Lambda MicroVMs is missing its image identifier."
		}
	case "docker", "exedev":
		return ""
	default:
		return fmt.Sprintf("Provider %q is not supported for routine agents.", name)
	}
	return ""
}

func routineAuthProfileReady(profiles []*types.ModelAuthProfileConfig, key *types.LLMKeyConfig) bool {
	for _, profile := range profiles {
		if profile != nil && profile.Name == key.AuthProfile && profile.Provider == key.Provider {
			return strings.TrimSpace(profile.AuthState) != ""
		}
	}
	return false
}

func missingRoutineSecrets(workflow *types.WorkflowConfig, tmplCfg *types.TemplateConfig, workspaceSecrets, hubSecrets map[string]string) []string {
	required := map[string]bool{}
	for _, ref := range workflow.SecretRefs {
		required[ref] = true
	}
	if tmplCfg != nil {
		for _, env := range tmplCfg.Env {
			if env.Secret != "" {
				required[env.Secret] = true
			}
		}
		for _, ref := range tmplCfg.SecretRefs {
			required[ref] = true
		}
		for _, ref := range tmplCfg.Secrets {
			if ref.Type == "custom" && ref.Name != "" {
				required[ref.Name] = true
			}
		}
	}
	missing := make([]string, 0)
	for name := range required {
		if strings.TrimSpace(workspaceSecrets[name]) == "" && strings.TrimSpace(hubSecrets[name]) == "" {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

func missingRoutineMCPs(tmplCfg *types.TemplateConfig, configured []*types.MCPServerHubConfig, workspaceSecrets, hubSecrets map[string]string) []string {
	servers := map[string]*types.MCPServerHubConfig{}
	for _, server := range configured {
		if server != nil {
			servers[server.Name] = server
		}
	}
	var missing []string
	for _, ref := range tmplCfg.MCPs {
		server := servers[ref.Name]
		if server == nil || !server.Enabled {
			missing = append(missing, fmt.Sprintf("MCP server %q is not enabled", ref.Name))
			continue
		}
		for envName, secretRef := range server.Secrets {
			if strings.TrimSpace(workspaceSecrets[secretRef]) == "" && strings.TrimSpace(hubSecrets[secretRef]) == "" {
				missing = append(missing, fmt.Sprintf("MCP server %q requires secret %q for %s", ref.Name, secretRef, envName))
			}
		}
	}
	sort.Strings(missing)
	return missing
}
