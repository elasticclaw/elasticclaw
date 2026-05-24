package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// WorkspaceView is the API view of a persisted workspace.
type WorkspaceView struct {
	Name      string          `json:"name"`
	Source    string          `json:"source"`
	Config    string          `json:"config,omitempty"`
	Access    WorkspaceAccess `json:"access"`
	Workflows []WorkflowView  `json:"workflows"`
}

// WorkspaceAccess is the maximum access available to workflows in a workspace.
// Values are names or repo selectors only; secret values are never exposed.
type WorkspaceAccess struct {
	Repositories   []types.GitHubRepoAccess `json:"repositories"`
	Env            []string                 `json:"env"`
	Secrets        []string                 `json:"secrets"`
	WebhookSecrets []string                 `json:"webhookSecrets"`
}

// WorkflowView is a workflow-shaped projection of a legacy factory.
type WorkflowView struct {
	Name                 string               `json:"name"`
	WorkspaceName        string               `json:"workspaceName"`
	Source               string               `json:"source"`
	Integration          string               `json:"integration"`
	IntegrationWorkspace string               `json:"integrationWorkspace,omitempty"`
	TriggerStatus        string               `json:"triggerStatus,omitempty"`
	DoneStatus           string               `json:"doneStatus,omitempty"`
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

func (s *Server) handleWorkspacesCRUD(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleWorkspacesList(w, r)
	case http.MethodPost:
		s.handleWorkspacesPush(w, r)
	case http.MethodDelete:
		name := r.URL.Query().Get("name")
		if name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		s.handleWorkspaceDelete(w, r, name)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

type WorkspacePushRequest struct {
	Workspaces []*types.WorkspaceConfig `json:"workspaces"`
}

func (s *Server) handleWorkspacesPush(w http.ResponseWriter, r *http.Request) {
	var req WorkspacePushRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Workspaces) == 0 {
		http.Error(w, "no workspaces provided", http.StatusBadRequest)
		return
	}

	var saveErrs []string
	for _, workspace := range req.Workspaces {
		if workspace == nil {
			saveErrs = append(saveErrs, "workspace cannot be nil")
			continue
		}
		if err := saveExternalWorkspace(workspace); err != nil {
			saveErrs = append(saveErrs, fmt.Sprintf("save workspace %q: %v", workspace.Name, err))
		}
	}
	if len(saveErrs) > 0 {
		http.Error(w, strings.Join(saveErrs, "; "), http.StatusInternalServerError)
		return
	}

	views := make([]WorkspaceView, 0, len(req.Workspaces))
	for _, workspace := range req.Workspaces {
		views = append(views, workspaceToView(workspace))
	}
	jsonOK(w, map[string]interface{}{
		"pushed":     len(req.Workspaces),
		"workspaces": views,
	})
}

func (s *Server) handleWorkspaceDelete(w http.ResponseWriter, _ *http.Request, name string) {
	if err := deleteExternalWorkspace(name); err != nil {
		http.Error(w, "delete error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]string{"deleted": name})
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

func (s *Server) handleWorkspaceWorkflowDetail(w http.ResponseWriter, r *http.Request) {
	workspaceName := strings.TrimSpace(r.PathValue("workspace"))
	workflowName := strings.TrimSpace(r.PathValue("workflow"))
	if workspaceName == "" || workflowName == "" {
		http.Error(w, "workspace and workflow names required", http.StatusBadRequest)
		return
	}
	workflow, ok := s.findWorkflowView(workspaceName, workflowName)
	if !ok {
		http.Error(w, "workflow not found", http.StatusNotFound)
		return
	}
	jsonOK(w, workflow)
}

func (s *Server) handleWorkspaceWorkflowTrigger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	workspaceName := strings.TrimSpace(r.PathValue("workspace"))
	workflowName := strings.TrimSpace(r.PathValue("workflow"))
	if workspaceName == "" || workflowName == "" {
		jsonError(w, http.StatusBadRequest, "workspace and workflow names required")
		return
	}
	workspace, workflow, ok, err := s.resolveWorkflowConfig(workspaceName, workflowName)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to load workflow: "+err.Error())
		return
	}
	if !ok {
		jsonError(w, http.StatusNotFound, "workflow not found")
		return
	}

	s.triggerWorkflowConfig(w, r, workspace, workflow)
}

func (s *Server) triggerWorkflowConfig(w http.ResponseWriter, r *http.Request, workspace *types.WorkspaceConfig, workflow *types.WorkflowConfig) {
	if !workflow.EnableManualTrigger {
		jsonError(w, http.StatusForbidden, "workflow does not support manual triggers")
		return
	}
	if workflow.Enabled != nil && !*workflow.Enabled {
		jsonError(w, http.StatusForbidden, "workflow is disabled")
		return
	}

	var req FactoryTriggerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	validatedInputs, err := validateFactoryInputs(workflow.Inputs, req.Inputs)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "validation error: "+err.Error())
		return
	}

	clawID, _, err := s.createClawFromWorkflow(workspace, workflow, validatedInputs, "manual workflow trigger")
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to create claw: "+err.Error())
		return
	}
	jsonOK(w, map[string]string{
		"claw_id": clawID,
		"status":  "created",
	})
}

func (s *Server) findWorkflowView(workspaceName, workflowName string) (WorkflowView, bool) {
	for _, workspace := range s.workspaceViews() {
		if !strings.EqualFold(workspace.Name, workspaceName) {
			continue
		}
		for _, workflow := range workspace.Workflows {
			if strings.EqualFold(workflow.Name, workflowName) {
				return workflow, true
			}
		}
	}
	return WorkflowView{}, false
}

func (s *Server) workspaceViews() []WorkspaceView {
	persisted, err := loadExternalWorkspaces()
	if err != nil {
		return []WorkspaceView{}
	}
	views := make([]WorkspaceView, 0, len(persisted))
	for _, workspace := range persisted {
		if workspace == nil {
			continue
		}
		views = append(views, workspaceToView(workspace))
	}
	sort.Slice(views, func(i, j int) bool {
		return strings.ToLower(views[i].Name) < strings.ToLower(views[j].Name)
	})
	return views
}

func workspaceToView(workspace *types.WorkspaceConfig) WorkspaceView {
	if workspace == nil {
		return WorkspaceView{}
	}
	workflows := make([]WorkflowView, 0, len(workspace.Workflows))
	for _, workflow := range workspace.Workflows {
		if workflow == nil {
			continue
		}
		workflows = append(workflows, workflowToView(workspace.Name, workflow))
	}
	sort.Slice(workflows, func(i, j int) bool {
		return strings.ToLower(workflows[i].Name) < strings.ToLower(workflows[j].Name)
	})

	return WorkspaceView{
		Name:   workspace.Name,
		Source: "workspace",
		Config: workspace.Files["elasticclaw-config.yaml"],
		Access: WorkspaceAccess{
			Repositories:   append([]types.GitHubRepoAccess(nil), workspace.Repositories...),
			Env:            workspaceEnvNames(workspace.Env),
			Secrets:        append([]string(nil), workspace.Secrets...),
			WebhookSecrets: append([]string(nil), workspace.WebhookSecrets...),
		},
		Workflows: workflows,
	}
}

func workspaceEnvNames(env types.WorkspaceEnv) []string {
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func workflowToView(workspaceName string, workflow *types.WorkflowConfig) WorkflowView {
	return WorkflowView{
		Name:                 workflow.Name,
		WorkspaceName:        workspaceName,
		Source:               "workflow",
		Integration:          workflow.Integration,
		IntegrationWorkspace: workflow.Workspace,
		TriggerStatus:        workflow.TriggerStatus,
		Labels:               append([]string(nil), workflow.Labels...),
		AssignedTo:           workflow.AssignedTo,
		Enabled:              workflow.Enabled == nil || *workflow.Enabled,
		EnableManualTrigger:  workflow.EnableManualTrigger,
		SecretRefs:           cloneStringMap(workflow.SecretRefs),
		Inputs:               append([]types.FactoryInput(nil), workflow.Inputs...),
	}
}

func (s *Server) resolveWorkflowConfig(workspaceName, workflowName string) (*types.WorkspaceConfig, *types.WorkflowConfig, bool, error) {
	workspace, err := loadExternalWorkspace(workspaceName)
	if err != nil {
		return nil, nil, false, nil
	}
	for _, workflow := range workspace.Workflows {
		if workflow != nil && strings.EqualFold(workflow.Name, workflowName) {
			return workspace, workflow, true, nil
		}
	}
	return nil, nil, false, nil
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
