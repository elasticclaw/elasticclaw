package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/elasticclaw/elasticclaw/pkg/workflowsetup"
	"gopkg.in/yaml.v3"
)

type workflowSetupContextResponse struct {
	Workspace         workflowSetupWorkspaceContext       `json:"workspace"`
	Hub               workflowSetupHubContext             `json:"hub"`
	Readiness         workflowSetupReadinessContext       `json:"readiness"`
	ConcurrencyGroups []workflowsetup.ConcurrencyGroupRef `json:"concurrencyGroups"`
}

type workflowSetupWorkspaceContext struct {
	Name                string                          `json:"name"`
	Repositories        []types.GitHubRepoAccess        `json:"repositories"`
	EnvNames            []string                        `json:"envNames"`
	SecretNames         []string                        `json:"secretNames"`
	DeclaredSecretNames []string                        `json:"declaredSecretNames"`
	WebhookSecretNames  []string                        `json:"webhookSecretNames"`
	IssueTrackers       []workflowsetup.IssueTrackerRef `json:"issueTrackers"`
	GitHubApps          []workflowsetup.GitHubAppRef    `json:"githubApps"`
}

type workflowSetupHubContext struct {
	SecretNames   []string                        `json:"secretNames"`
	IssueTrackers []workflowsetup.IssueTrackerRef `json:"issueTrackers"`
	GitHubApps    []workflowsetup.GitHubAppRef    `json:"githubApps"`
}

type workflowSetupReadinessContext struct {
	ClawTokenSet      bool                                `json:"clawTokenSet"`
	Providers         []workflowsetup.ProviderRef         `json:"providers"`
	DefaultProvider   string                              `json:"defaultProvider,omitempty"`
	ProviderReady     bool                                `json:"providerReady"`
	DefaultModel      string                              `json:"defaultModel,omitempty"`
	LLMKeys           []workflowsetup.LLMKeyRef           `json:"llmKeys"`
	ModelReady        bool                                `json:"modelReady"`
	ConcurrencyGroups []workflowsetup.ConcurrencyGroupRef `json:"concurrencyGroups"`
}

type workflowSetupSaveResponse struct {
	Saved     bool                           `json:"saved"`
	Workspace string                         `json:"workspace"`
	Mode      workflowsetup.SaveMode         `json:"mode"`
	Workflow  WorkflowView                   `json:"workflow"`
	Readiness workflowsetup.ValidateResponse `json:"readiness"`
}

type workflowSetupConvertPreviewRequest struct {
	Workspace string `json:"workspace"`
}

func (s *Server) handleWorkflowSetupPatterns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	jsonOK(w, workflowsetup.Patterns())
}

func (s *Server) handleWorkflowSetupContext(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	workspaceName := strings.TrimSpace(r.PathValue("workspace"))
	if workspaceName == "" {
		jsonError(w, http.StatusBadRequest, "workspace name required")
		return
	}

	env := s.WorkflowSetupEnvironment()
	workspace, err := env.LoadWorkspace(workspaceName)
	if err != nil {
		writeWorkflowSetupLoadError(w, "workspace", err)
		return
	}
	if workspace == nil {
		jsonError(w, http.StatusNotFound, "workspace not found")
		return
	}
	if strings.TrimSpace(workspace.Name) == "" {
		workspace.Name = workspaceName
	}

	snapshot, err := env.Snapshot()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "load workflow setup snapshot: "+err.Error())
		return
	}
	workspaceSecretNames, err := env.WorkspaceSecretNames(workspace.Name)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "load workspace secret names: "+err.Error())
		return
	}
	workspaceIssueTrackers, err := env.WorkspaceIssueTrackers(workspace.Name)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "load workspace issue trackers: "+err.Error())
		return
	}
	workspaceGitHubApps, err := env.WorkspaceGitHubApps(workspace.Name)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "load workspace GitHub Apps: "+err.Error())
		return
	}

	resp := workflowSetupContextResponse{
		Workspace: workflowSetupWorkspaceContext{
			Name:                workspace.Name,
			Repositories:        jsonArray(workspace.Repositories),
			EnvNames:            jsonArray(workspaceEnvNames(workspace.Env)),
			SecretNames:         jsonArray(workspaceSecretNames),
			DeclaredSecretNames: sortedStringCopy(workspace.Secrets),
			WebhookSecretNames:  sortedStringCopy(workspace.WebhookSecrets),
			IssueTrackers:       jsonArray(workspaceIssueTrackers),
			GitHubApps:          jsonArray(workspaceGitHubApps),
		},
		Hub: workflowSetupHubContext{
			SecretNames:   jsonArray(snapshot.HubSecretNames),
			IssueTrackers: jsonArray(snapshot.IssueTrackers),
			GitHubApps:    jsonArray(snapshot.GitHubApps),
		},
		Readiness: workflowSetupReadinessContext{
			ClawTokenSet:      snapshot.ClawTokenSet,
			Providers:         jsonArray(snapshot.Providers),
			DefaultProvider:   snapshot.DefaultProvider,
			ProviderReady:     workflowSetupProviderReady(snapshot),
			DefaultModel:      snapshot.DefaultModel,
			LLMKeys:           jsonArray(snapshot.LLMKeys),
			ModelReady:        workflowSetupModelReady(snapshot),
			ConcurrencyGroups: jsonArray(snapshot.ConcurrencyGroups),
		},
		ConcurrencyGroups: jsonArray(snapshot.ConcurrencyGroups),
	}
	jsonOK(w, resp)
}

func (s *Server) handleWorkflowSetupRender(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req workflowsetup.RenderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	resp, err := workflowsetup.Render(req)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	jsonOK(w, resp)
}

func (s *Server) handleWorkflowSetupConvertPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	factoryName := strings.TrimSpace(r.PathValue("factory"))
	if factoryName == "" {
		jsonError(w, http.StatusBadRequest, "factory name required")
		return
	}
	if err := validateName(factoryName); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid factory name: "+err.Error())
		return
	}

	var req workflowSetupConvertPreviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	workspaceName := strings.TrimSpace(req.Workspace)
	if workspaceName == "" {
		jsonError(w, http.StatusBadRequest, "workspace name required")
		return
	}
	if err := validateName(workspaceName); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid workspace name: "+err.Error())
		return
	}

	env := s.WorkflowSetupEnvironment()
	factory, err := env.LoadFactory(factoryName)
	if err != nil {
		writeWorkflowSetupLoadError(w, "factory", err)
		return
	}

	workspaceRaw, err := workflowSetupLoadWorkspaceRawConfig(env, workspaceName)
	if err != nil {
		writeWorkflowSetupLoadError(w, "workspace", err)
		return
	}

	factoryFiles, err := s.workflowSetupLoadFactoryConvertFiles(factoryName, factory)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "load factory files: "+err.Error())
		return
	}
	workspaceFiles, err := workflowSetupLoadWorkspaceConvertFiles(workspaceName)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "load workspace files: "+err.Error())
		return
	}

	resp := workflowsetup.ConvertFactory(workflowsetup.FactoryConvertRequest{
		Factory:         factory,
		WorkspaceName:   workspaceName,
		WorkspaceConfig: workspaceRaw,
		TemplateFiles:   factoryFiles,
		WorkspaceFiles:  workspaceFiles,
	})
	jsonOK(w, resp)
}

type workflowSetupValidateAPIRequest struct {
	workflowsetup.ValidateRequest
	Workspace string `json:"workspace,omitempty"`
}

func (s *Server) handleWorkflowSetupValidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req workflowSetupValidateAPIRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if strings.TrimSpace(req.WorkspaceConfig) == "" && strings.TrimSpace(req.Workspace) != "" {
		raw, err := workflowSetupLoadWorkspaceRawConfig(s.WorkflowSetupEnvironment(), req.Workspace)
		if err != nil {
			writeWorkflowSetupLoadError(w, "workspace", err)
			return
		}
		req.WorkspaceConfig = raw
	}

	resp := workflowsetup.ValidateReadiness(req.ValidateRequest, s.WorkflowSetupEnvironment())
	jsonOK(w, resp)
}

func (s *Server) handleWorkflowSetupSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req workflowsetup.SaveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	workspaceName := strings.TrimSpace(req.Workspace)
	if workspaceName == "" {
		jsonError(w, http.StatusBadRequest, "workspace name required")
		return
	}
	if err := validateName(workspaceName); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid workspace name: "+err.Error())
		return
	}

	mode := req.Mode
	if mode == "" {
		mode = workflowsetup.SaveModeCreate
	}
	switch mode {
	case workflowsetup.SaveModeCreate, workflowsetup.SaveModeUpdate, workflowsetup.SaveModeUpsert:
	default:
		jsonError(w, http.StatusBadRequest, "invalid save mode")
		return
	}

	rawConfig := req.Workflow.Config
	if strings.TrimSpace(rawConfig) == "" {
		jsonError(w, http.StatusBadRequest, "workflow config required")
		return
	}

	currentHash := workflowsetup.ConfigHash(rawConfig)
	if strings.TrimSpace(req.ValidatedConfigHash) == "" {
		jsonError(w, http.StatusBadRequest, "validatedConfigHash required")
		return
	}
	if strings.TrimSpace(req.ValidatedConfigHash) != currentHash {
		jsonError(w, http.StatusConflict, "validatedConfigHash does not match current workflow config")
		return
	}

	env := s.WorkflowSetupEnvironment()
	workspaceRaw, err := workflowSetupLoadWorkspaceRawConfig(env, workspaceName)
	if err != nil {
		writeWorkflowSetupLoadError(w, "workspace", err)
		return
	}

	workflowName := strings.TrimSpace(req.Workflow.Name)
	readiness := workflowsetup.ValidateReadiness(workflowsetup.ValidateRequest{
		WorkflowName:    workflowName,
		Config:          rawConfig,
		WorkspaceConfig: workspaceRaw,
	}, env)
	if readiness.Summary.Critical > 0 {
		workflowSetupSaveError(w, http.StatusBadRequest, "critical readiness failures block save", readiness)
		return
	}
	if readiness.Summary.Warning > 0 && !req.AllowWarnings {
		workflowSetupSaveError(w, http.StatusBadRequest, "readiness warnings require allowWarnings", readiness)
		return
	}

	workflow, err := workflowSetupParseAuthoredWorkflow(rawConfig)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid workflow config: "+err.Error())
		return
	}
	parsedName := strings.TrimSpace(workflow.Name)
	if parsedName == "" {
		jsonError(w, http.StatusBadRequest, "workflow name required")
		return
	}
	if workflowName != "" && workflowName != parsedName {
		jsonError(w, http.StatusBadRequest, "workflow name does not match config")
		return
	}
	if err := validateName(parsedName); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid workflow name: "+err.Error())
		return
	}

	if err := types.NormalizeWorkflowConfig(workflow); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid workflow: "+err.Error())
		return
	}
	if err := workflow.Validate(); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid workflow: "+err.Error())
		return
	}
	workflow.RawConfig = rawConfig

	exists, err := externalWorkflowExists(workspaceName, parsedName)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "check workflow: "+err.Error())
		return
	}
	switch mode {
	case workflowsetup.SaveModeCreate:
		if exists {
			jsonError(w, http.StatusConflict, "workflow already exists")
			return
		}
	case workflowsetup.SaveModeUpdate:
		if !exists {
			jsonError(w, http.StatusConflict, "workflow does not exist")
			return
		}
	}

	if err := saveExternalWorkflows(workspaceName, []*types.WorkflowConfig{workflow}); err != nil {
		jsonError(w, http.StatusInternalServerError, "save workflow: "+err.Error())
		return
	}

	jsonOK(w, workflowSetupSaveResponse{
		Saved:     true,
		Workspace: workspaceName,
		Mode:      mode,
		Workflow:  workflowToView(workspaceName, workflow),
		Readiness: readiness,
	})
}

func workflowSetupParseAuthoredWorkflow(raw string) (*types.WorkflowConfig, error) {
	workflow := &types.WorkflowConfig{}
	dec := yaml.NewDecoder(strings.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(workflow); err != nil {
		return nil, err
	}
	workflow.RawConfig = raw
	return workflow, nil
}

func workflowSetupSaveError(w http.ResponseWriter, status int, msg string, readiness workflowsetup.ValidateResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error":     msg,
		"readiness": readiness,
	})
}

func workflowSetupLoadWorkspaceRawConfig(env workflowsetup.Environment, workspaceName string) (string, error) {
	workspace, err := env.LoadWorkspace(strings.TrimSpace(workspaceName))
	if err != nil {
		return "", err
	}
	if workspace == nil {
		return "", os.ErrNotExist
	}
	if raw := strings.TrimSpace(workspace.Files["elasticclaw-config.yaml"]); raw != "" {
		return workspace.Files["elasticclaw-config.yaml"], nil
	}
	data, err := marshalWorkspaceElasticClawConfig(workspace, "")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (s *Server) workflowSetupLoadFactoryConvertFiles(factoryName string, factory *types.FactoryConfig) (map[string]string, error) {
	files, err := workflowSetupReadConvertDirectoryFiles(filepath.Join(factoriesDir(), factoryName), nil)
	if err == nil {
		return files, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if factory == nil || strings.TrimSpace(factory.Template) == "" {
		return nil, nil
	}
	files, err = s.resolveTemplateFiles(factory.Template)
	if err != nil {
		return nil, nil
	}
	return files, nil
}

func workflowSetupLoadWorkspaceConvertFiles(workspaceName string) (map[string]string, error) {
	return workflowSetupReadConvertDirectoryFiles(filepath.Join(workspacesDir(), workspaceName), map[string]bool{
		"workflows":             true,
		workspaceManagedDirName: true,
	})
}

func workflowSetupReadConvertDirectoryFiles(root string, skipDirs map[string]bool) (map[string]string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s exists but is not a directory", root)
	}

	files := map[string]string{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if path != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

func writeWorkflowSetupLoadError(w http.ResponseWriter, subject string, err error) {
	if errors.Is(err, os.ErrNotExist) {
		jsonError(w, http.StatusNotFound, subject+" not found")
		return
	}
	jsonError(w, http.StatusInternalServerError, "load "+subject+": "+err.Error())
}

func workflowSetupProviderReady(snapshot workflowsetup.SetupEnvironmentSnapshot) bool {
	for _, provider := range snapshot.Providers {
		if strings.TrimSpace(snapshot.DefaultProvider) != "" &&
			strings.TrimSpace(provider.Name) != snapshot.DefaultProvider &&
			strings.TrimSpace(provider.Type) != snapshot.DefaultProvider {
			continue
		}
		if workflowSetupProviderRuntimeReady(provider) {
			return true
		}
	}
	return false
}

func workflowSetupProviderRuntimeReady(provider workflowsetup.ProviderRef) bool {
	if !provider.Provisionable {
		return false
	}
	if provider.CredentialsSet {
		return true
	}
	providerType := strings.TrimSpace(provider.Type)
	if providerType == "" {
		providerType = strings.TrimSpace(provider.Name)
	}
	switch providerType {
	case "replicated":
		return provider.TokenSet
	case "daytona":
		return provider.APIKeySet
	case "exedev":
		return true
	default:
		return false
	}
}

func workflowSetupModelReady(snapshot workflowsetup.SetupEnvironmentSnapshot) bool {
	if strings.TrimSpace(snapshot.DefaultModel) == "" {
		return false
	}
	for _, key := range snapshot.LLMKeys {
		if key.KeySet {
			return true
		}
	}
	return false
}

func sortedStringCopy(values []string) []string {
	copied := jsonArray(values)
	sort.Strings(copied)
	return copied
}

func jsonArray[T any](values []T) []T {
	if len(values) == 0 {
		return []T{}
	}
	return append([]T(nil), values...)
}
