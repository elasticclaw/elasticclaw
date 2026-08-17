package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	v2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
	"github.com/google/uuid"
)

// isWorkflowV2 reports whether a workflow push payload is schema v2.
// Prefers RawConfig when present so integer schema_version: 2 is detected.
func isWorkflowV2(workflow *types.WorkflowConfig) bool {
	if workflow == nil {
		return false
	}
	if raw := strings.TrimSpace(workflow.RawConfig); raw != "" {
		version, err := v2.DetectSchemaVersion([]byte(raw))
		if err == nil && v2.IsV2(version) {
			return true
		}
	}
	return v2.IsV2(workflow.SchemaVersion)
}

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
	Name                 string                 `json:"name"`
	SchemaVersion        string                 `json:"schemaVersion,omitempty"`
	WorkspaceName        string                 `json:"workspaceName"`
	Source               string                 `json:"source"`
	Integration          string                 `json:"integration"`
	IntegrationWorkspace string                 `json:"integrationWorkspace,omitempty"`
	TriggerStatus        string                 `json:"triggerStatus,omitempty"`
	DoneStatus           string                 `json:"doneStatus,omitempty"`
	Projects             []string               `json:"projects,omitempty"`
	Labels               []string               `json:"labels,omitempty"`
	ExcludeLabels        []string               `json:"exclude_labels,omitempty"`
	AssignedTo           string                 `json:"assignedTo,omitempty"`
	Enabled              bool                   `json:"enabled"`
	RuntimeAvailable     bool                   `json:"runtimeAvailable"`
	HasWebhookSecret     bool                   `json:"hasWebhookSecret"`
	WebhookSecretRef     string                 `json:"webhookSecretRef,omitempty"`
	PipelineYAML         string                 `json:"pipelineYAML,omitempty"`
	EnableManualTrigger  bool                   `json:"enableManualTrigger,omitempty"`
	SecretRefs           map[string]string      `json:"secretRefs,omitempty"`
	Volumes              []types.WorkflowVolume `json:"volumes,omitempty"`
	Inputs               []types.FactoryInput   `json:"inputs,omitempty"`
	RawConfig            string                 `json:"rawConfig,omitempty"`
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

	// Author-time notify validation runs over every nested workflow BEFORE any
	// workspace is written, so a bad "via" fails the whole push as a 400
	// instead of a partial save. (Nil workspaces/workflows fall through to the
	// save loop below, which already reports them.)
	var invalid []string
	for _, workspace := range req.Workspaces {
		if workspace == nil {
			continue
		}
		for _, workflow := range workspace.Workflows {
			if err := s.validateWorkflowNotifyVias(workflow); err != nil {
				invalid = append(invalid, fmt.Sprintf("workspace %q: %v", workspace.Name, err))
			}
		}
	}
	if len(invalid) > 0 {
		http.Error(w, "invalid workspace: "+strings.Join(invalid, "; "), http.StatusBadRequest)
		return
	}

	var saveErrs []string
	clientErr := false
	for _, workspace := range req.Workspaces {
		if workspace == nil {
			saveErrs = append(saveErrs, "workspace cannot be nil")
			clientErr = true
			continue
		}
		if err := saveExternalWorkspace(workspace); err != nil {
			saveErrs = append(saveErrs, fmt.Sprintf("save workspace %q: %v", workspace.Name, err))
			if strings.Contains(err.Error(), "invalid workspace v2") || strings.Contains(err.Error(), "workspace name") {
				clientErr = true
			}
		}
	}
	if len(saveErrs) > 0 {
		status := http.StatusInternalServerError
		if clientErr {
			status = http.StatusBadRequest
		}
		http.Error(w, strings.Join(saveErrs, "; "), status)
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
	if r.Method == http.MethodPost {
		s.handleWorkspaceWorkflowsPush(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
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

type WorkflowPushRequest struct {
	Workflows []*types.WorkflowConfig `json:"workflows"`
}

func (s *Server) handleWorkspaceWorkflowsPush(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		http.Error(w, "workspace name required", http.StatusBadRequest)
		return
	}
	var req WorkflowPushRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Workflows) == 0 {
		http.Error(w, "no workflows provided", http.StatusBadRequest)
		return
	}
	for _, workflow := range req.Workflows {
		if workflow == nil {
			http.Error(w, "workflow cannot be nil", http.StatusBadRequest)
			return
		}
		workflow.Name = strings.TrimSpace(workflow.Name)
		// V2 workflows use a separate schema; do not run v1 normalize/validate on them.
		if isWorkflowV2(workflow) {
			continue
		}
		if err := types.NormalizeWorkflowConfig(workflow); err != nil {
			http.Error(w, "invalid workflow: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := workflow.Validate(); err != nil {
			http.Error(w, "invalid workflow: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.validateWorkflowNotifyVias(workflow); err != nil {
			http.Error(w, "invalid workflow: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	if err := saveExternalWorkflows(name, req.Workflows); err != nil {
		if isWorkspaceNotFound(err) {
			// Missing workspace is a client mistake (wrong --workspace name), not a server fault.
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		// Validation failures at the store boundary are client errors.
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "invalid workflow v2") || strings.Contains(err.Error(), "invalid workspace v2") {
			status = http.StatusBadRequest
		}
		http.Error(w, "save workflows: "+err.Error(), status)
		return
	}
	if s.cronScheduler != nil {
		if err := s.cronScheduler.reload(); err != nil {
			log.Printf("[cron] failed to reload workflows after workflow push for workspace %s: %v", name, err)
		}
	}
	workflows := make([]WorkflowView, 0, len(req.Workflows))
	for _, workflow := range req.Workflows {
		workflows = append(workflows, workflowToView(name, workflow))
	}
	jsonOK(w, map[string]interface{}{"pushed": len(req.Workflows), "workflows": workflows})
}

func (s *Server) handleWorkspaceWorkflowDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPatch {
		s.handleWorkspaceWorkflowPatch(w, r)
		return
	}
	if r.Method == http.MethodDelete {
		s.handleWorkspaceWorkflowDelete(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	workspaceName := strings.TrimSpace(r.PathValue("workspace"))
	workflowName := strings.TrimSpace(r.PathValue("workflow"))
	if workspaceName == "" || workflowName == "" {
		http.Error(w, "workspace and workflow names required", http.StatusBadRequest)
		return
	}
	workspace, workflow, ok, err := s.resolveWorkflowConfig(workspaceName, workflowName)
	if err != nil {
		http.Error(w, "failed to load workflow: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "workflow not found", http.StatusNotFound)
		return
	}
	view := workflowToView(workspace.Name, workflow)
	view.RawConfig = workflow.RawConfig
	jsonOK(w, view)
}

type WorkflowPatchRequest struct {
	Enabled                  *bool `json:"enabled"`
	EnableManualTrigger      *bool `json:"enableManualTrigger"`
	EnableManualTriggerSnake *bool `json:"enable_manual_trigger"`
}

func (s *Server) handleWorkspaceWorkflowPatch(w http.ResponseWriter, r *http.Request) {
	workspaceName := strings.TrimSpace(r.PathValue("workspace"))
	workflowName := strings.TrimSpace(r.PathValue("workflow"))
	if workspaceName == "" || workflowName == "" {
		http.Error(w, "workspace and workflow names required", http.StatusBadRequest)
		return
	}
	var req WorkflowPatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Enabled == nil && req.EnableManualTrigger == nil && req.EnableManualTriggerSnake == nil {
		http.Error(w, "no workflow fields provided", http.StatusBadRequest)
		return
	}
	if req.EnableManualTrigger != nil && req.EnableManualTriggerSnake != nil {
		http.Error(w, "provide only one of enableManualTrigger or enable_manual_trigger", http.StatusBadRequest)
		return
	}

	workspace, workflow, ok, err := s.resolveWorkflowConfig(workspaceName, workflowName)
	if err != nil {
		http.Error(w, "failed to load workflow: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "workflow not found", http.StatusNotFound)
		return
	}
	if isWorkflowV2(workflow) {
		http.Error(w, "workflow v2 activation is managed by the v2 runtime and cannot be changed through the v1 workflow patch API", http.StatusConflict)
		return
	}
	if req.Enabled != nil {
		workflow.Enabled = req.Enabled
	}
	if req.EnableManualTrigger != nil {
		workflow.EnableManualTrigger = *req.EnableManualTrigger
	}
	if req.EnableManualTriggerSnake != nil {
		workflow.EnableManualTrigger = *req.EnableManualTriggerSnake
	}
	workflow.RawConfig = ""
	// Deliberately no validateWorkflowNotifyVias here: a patch only toggles
	// flags on an already-persisted pipeline without changing its content, and
	// disabling a workflow stranded by a later hub.yaml notifier delete/rename
	// must keep working — the doctor's checkNotifyActions owns flagging that
	// drift.
	if err := saveExternalWorkflows(workspace.Name, []*types.WorkflowConfig{workflow}); err != nil {
		http.Error(w, "save workflow: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if s.cronScheduler != nil {
		if err := s.cronScheduler.reload(); err != nil {
			log.Printf("[cron] failed to reload workflows after workflow patch for workspace %s workflow %s: %v", workspace.Name, workflow.Name, err)
		}
	}
	jsonOK(w, workflowToView(workspace.Name, workflow))
}

func (s *Server) handleWorkspaceWorkflowDelete(w http.ResponseWriter, r *http.Request) {
	workspaceName := strings.TrimSpace(r.PathValue("workspace"))
	workflowName := strings.TrimSpace(r.PathValue("workflow"))
	if workspaceName == "" || workflowName == "" {
		http.Error(w, "workspace and workflow names required", http.StatusBadRequest)
		return
	}

	// Track the names used for the actual deletion so the cron scheduler key
	// is removed correctly. Fall back to raw (untrimmed) path values to handle
	// workflows persisted before push-time trimming was introduced.
	deletedWorkspaceName := workspaceName
	deletedWorkflowName := workflowName
	if err := deleteExternalWorkflow(workspaceName, workflowName); err != nil {
		if !errors.Is(err, errWorkflowNotFound) {
			http.Error(w, "delete workflow: "+err.Error(), http.StatusInternalServerError)
			return
		}
		rawWorkspaceName := r.PathValue("workspace")
		rawWorkflowName := r.PathValue("workflow")
		if err := deleteExternalWorkflow(rawWorkspaceName, rawWorkflowName); err != nil {
			if errors.Is(err, errWorkflowNotFound) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			http.Error(w, "delete workflow: "+err.Error(), http.StatusInternalServerError)
			return
		}
		deletedWorkspaceName = rawWorkspaceName
		deletedWorkflowName = rawWorkflowName
	}
	if s.cronScheduler != nil {
		s.cronScheduler.removeWorkflow(deletedWorkspaceName, deletedWorkflowName)
		if err := s.cronScheduler.reload(); err != nil {
			log.Printf("[cron] failed to reload workflows after workflow delete for workspace %s workflow %s: %v", deletedWorkspaceName, deletedWorkflowName, err)
		}
	}
	jsonOK(w, map[string]string{"deleted": deletedWorkflowName})
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
	if isWorkflowV2(workflow) {
		s.triggerWorkflowV2Config(w, r, workspace, workflow)
		return
	}
	if !workflow.EnableManualTrigger {
		jsonError(w, http.StatusForbidden, "workflow does not support manual triggers")
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

	if workflow.Integration == "github-issues" || (workflow.Trigger != nil && workflow.Trigger.GitHubIssues != nil) {
		clawID, created, err := s.createClawForManualGitHubIssueWorkflow(r.Context(), workspace, workflow, validatedInputs)
		if err != nil {
			if isFactoryTriggerAlreadyClaimed(err) {
				jsonError(w, http.StatusConflict, "workflow trigger already in progress for this GitHub issue")
				return
			}
			jsonError(w, http.StatusInternalServerError, "failed to create claw: "+err.Error())
			return
		}
		if created {
			now := time.Now().UTC()
			runID := uuid.New().String()
			contextData := map[string]interface{}{
				"run_id":         runID,
				"trigger_type":   "manual",
				"workflow_name":  workflow.Name,
				"workspace_name": workspace.Name,
				"issue_number":   validatedInputs["issue_number"],
			}
			contextJSON, _ := json.Marshal(contextData)
			s.recordWorkflowRun(runID, "", workspace.Name, workflow.Name, "manual", "running", clawID, string(contextJSON), now)
		}
		status := "existing"
		if created {
			status = "created"
		}
		jsonOK(w, map[string]string{
			"claw_id": clawID,
			"status":  status,
		})
		return
	}

	clawID, _, err := s.createClawFromWorkflowContext(r.Context(), workspace, workflow, validatedInputs, "manual workflow trigger")
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to create claw: "+err.Error())
		return
	}
	now := time.Now().UTC()
	runID := uuid.New().String()
	contextData := map[string]interface{}{
		"run_id":         runID,
		"trigger_type":   "manual",
		"workflow_name":  workflow.Name,
		"workspace_name": workspace.Name,
		"inputs":         validatedInputs,
	}
	contextJSON, _ := json.Marshal(contextData)
	s.recordWorkflowRun(runID, "", workspace.Name, workflow.Name, "manual", "running", clawID, string(contextJSON), now)
	jsonOK(w, map[string]string{
		"claw_id": clawID,
		"status":  "created",
	})
}

func (s *Server) createClawForManualGitHubIssueWorkflow(ctx context.Context, workspace *types.WorkspaceConfig, workflow *types.WorkflowConfig, inputs map[string]string) (string, bool, error) {
	rawIssueNumber := strings.TrimSpace(inputs["issue_number"])
	if rawIssueNumber == "" {
		return "", false, fmt.Errorf(`missing required input "issue_number"`)
	}
	issueNumberFloat, err := strconv.ParseFloat(rawIssueNumber, 64)
	if err != nil {
		return "", false, fmt.Errorf("invalid issue_number %q: %w", rawIssueNumber, err)
	}
	issueNumber := int(issueNumberFloat)
	if issueNumber < 1 || float64(issueNumber) != issueNumberFloat {
		return "", false, fmt.Errorf("issue_number must be a positive integer")
	}

	repos := githubIssuesWorkflowTriggerRepos(workflow)
	exactRepos := make([]string, 0, len(repos))
	for _, repo := range repos {
		repo = strings.TrimSpace(repo)
		if repo == "" || strings.HasSuffix(repo, "/*") {
			continue
		}
		exactRepos = append(exactRepos, repo)
	}
	if len(exactRepos) != 1 {
		return "", false, fmt.Errorf("manual GitHub issue workflow requires exactly one exact trigger repository, got %d", len(exactRepos))
	}
	repo := exactRepos[0]

	token := s.resolveGitHubIssuesTokenForWorkflow(workspace.Name, workflow)
	if token == "" {
		return "", false, fmt.Errorf("no GitHub Issues token configured for workflow %s/%s", workspace.Name, workflow.Name)
	}

	base := s.githubBaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	issue, err := s.queryGitHubIssue(repo, token, base, issueNumber)
	if err != nil {
		return "", false, err
	}

	payload := buildGitHubIssuesPollPayloadForAction(issue, repo, "manual")
	return s.createClawForGitHubIssueWorkflowContext(ctx, workspace, workflow, payload, "manual workflow trigger")
}

func (s *Server) queryGitHubIssue(repo, token, base string, issueNumber int) (githubIssuesPollItem, error) {
	item, err := githubAPIWithBase(base, fmt.Sprintf("repos/%s/issues/%d", repo, issueNumber), token)
	if err != nil {
		return githubIssuesPollItem{}, err
	}
	var issue githubIssuesPollItem
	b, err := json.Marshal(item)
	if err != nil {
		return githubIssuesPollItem{}, fmt.Errorf("marshal GitHub issue response: %w", err)
	}
	if err := json.Unmarshal(b, &issue); err != nil {
		return githubIssuesPollItem{}, fmt.Errorf("parse GitHub issue response: %w", err)
	}
	if issue.Number != issueNumber {
		return githubIssuesPollItem{}, fmt.Errorf("GitHub issue %s/%d not found or unreadable", repo, issueNumber)
	}
	return issue, nil
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
	var projects []string
	if workflow.Integration == "linear" || (workflow.Trigger != nil && workflow.Trigger.Linear != nil) {
		projects = append([]string(nil), linearWorkflowProjects(workflow)...)
	}
	return WorkflowView{
		Name:                 workflow.Name,
		SchemaVersion:        workflow.SchemaVersion,
		WorkspaceName:        workspaceName,
		Source:               "workflow",
		Integration:          workflow.Integration,
		IntegrationWorkspace: workflow.Workspace,
		TriggerStatus:        workflow.TriggerStatus,
		Projects:             projects,
		Labels:               append([]string(nil), workflow.Labels...),
		ExcludeLabels:        append([]string(nil), workflow.ExcludeLabels...),
		AssignedTo:           workflow.AssignedTo,
		Enabled:              workflow.Enabled == nil || *workflow.Enabled,
		RuntimeAvailable:     true,
		EnableManualTrigger:  workflow.EnableManualTrigger,
		SecretRefs:           cloneStringMap(workflow.SecretRefs),
		Volumes:              append([]types.WorkflowVolume(nil), workflow.Volumes...),
		Inputs:               append([]types.FactoryInput(nil), workflow.Inputs...),
	}
}

func (s *Server) resolveWorkflowConfig(workspaceName, workflowName string) (*types.WorkspaceConfig, *types.WorkflowConfig, bool, error) {
	workspace, err := loadExternalWorkspace(workspaceName)
	if err != nil {
		// Typed workspace-not-found (and legacy os.IsNotExist) must map to 404,
		// not 500 — loadExternalWorkspaceConfig no longer returns bare IsNotExist.
		if isWorkspaceNotFound(err) || os.IsNotExist(err) {
			return nil, nil, false, nil
		}
		return nil, nil, false, err
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
