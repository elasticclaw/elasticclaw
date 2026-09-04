package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
	"github.com/google/uuid"
)

type workflowCreateOptions struct {
	ctx                  context.Context
	inputs               map[string]string
	workspaceFiles       map[string]string
	clawName             string
	githubIssueID        string
	linearIssueID        string
	shortcutStoryID      string
	jiraIssueID          string
	issueLabels          []string
	issueLabelsAvailable bool
	issueCreatedAt       time.Time
	reason               string
	triggerActor         *triggerActor
	// beforeProvision runs after the claw and analytics row exist but before
	// provider work begins. Workflow v2 uses it to atomically create the run and
	// initial attempt, closing the bridge-registration race.
	beforeProvision func(context.Context, string, string) error
}

const hubInternalTemplateFilePrefix = "__hub__/"

func workspaceTemplateFiles(files map[string]string) map[string]string {
	filtered := make(map[string]string, len(files)+1)
	for name, content := range files {
		if strings.HasPrefix(name, hubInternalTemplateFilePrefix) {
			continue
		}
		filtered[name] = content
	}
	appendBrowserEvidenceTools(filtered)
	return filtered
}

func (s *Server) createClawFromWorkflow(workspace *types.WorkspaceConfig, workflow *types.WorkflowConfig, inputs map[string]string, reason string) (string, bool, error) {
	return s.createClawFromWorkflowContext(context.Background(), workspace, workflow, inputs, reason)
}

func (s *Server) createClawFromWorkflowContext(ctx context.Context, workspace *types.WorkspaceConfig, workflow *types.WorkflowConfig, inputs map[string]string, reason string) (string, bool, error) {
	return s.createClawFromWorkflowWithOptions(workspace, workflow, workflowCreateOptions{ctx: ctx, inputs: inputs, reason: reason})
}

func (s *Server) createClawFromWorkflowWithOptions(workspace *types.WorkspaceConfig, workflow *types.WorkflowConfig, opts workflowCreateOptions) (string, bool, error) {
	templateFiles := cloneStringMap(workspace.Files)
	if opts.workspaceFiles != nil {
		templateFiles = cloneStringMap(opts.workspaceFiles)
	}

	var tmplCfg *types.TemplateConfig
	var workspaceV2 *typesv2.Workspace
	if isWorkflowV2(workflow) {
		resolved, err := typesv2.ParseAndValidateWorkspace([]byte(templateFiles["elasticclaw-config.yaml"]))
		if err != nil {
			return "", false, fmt.Errorf("parse workspace v2 execution config: %w", err)
		}
		workspaceV2 = resolved.Workspace
	} else if cfgContent, ok := templateFiles["elasticclaw-config.yaml"]; ok {
		var err error
		tmplCfg, err = config.ParseTemplateConfig([]byte(cfgContent))
		if err != nil {
			log.Printf("[workflow:%s/%s] warning: could not parse workspace elasticclaw-config.yaml: %v", workspace.Name, workflow.Name, err)
			tmplCfg = nil
		}
	}

	if opts.inputs != nil {
		templateFiles["CONTEXT.md"] = buildWorkflowManualTriggerContext(workflow, opts.inputs, templateFiles["CONTEXT.md"])
		inputsJSON, _ := json.Marshal(opts.inputs)
		templateFiles["TRIGGER_INPUTS.json"] = string(inputsJSON)
	}

	clawName := workflow.Name
	if opts.clawName != "" {
		clawName = opts.clawName
	} else if workflow.NamePattern != "" {
		clawName = workflow.NamePattern
		for k, v := range opts.inputs {
			clawName = strings.ReplaceAll(clawName, "{"+k+"}", v)
		}
	}
	if clawName == workflow.Name && opts.inputs != nil && len(workflow.Inputs) > 0 {
		if firstVal := opts.inputs[workflow.Inputs[0].Name]; firstVal != "" {
			clawName = firstVal
		}
	}

	var tenantID string
	if err := s.db.QueryRow(`SELECT id FROM tenants LIMIT 1`).Scan(&tenantID); err != nil {
		return "", false, fmt.Errorf("no tenant: %w", err)
	}

	provider := workflow.Provider
	if workspaceV2 != nil && workspaceV2.Execution != nil {
		provider = strings.TrimSpace(workspaceV2.Execution.Provider)
	}
	if provider == "" && tmplCfg != nil {
		provider = tmplCfg.Provider
	}
	if provider == "" {
		provider = s.defaultProvider()
	}
	if provider == "" {
		return "", false, fmt.Errorf("no provider configured")
	}

	s.mu.RLock()
	defaultModel := s.hubCfg.DefaultModel
	provCfg, ok := s.hubCfg.Providers[provider]
	s.mu.RUnlock()
	if !ok {
		return "", false, fmt.Errorf("provider %q is not configured on this hub", provider)
	}
	clawID := uuid.New().String()
	env, resolvedSecrets, err := s.resolveClawEnv(workspace.Name, workflow, tmplCfg, clawID)
	if err != nil {
		return "", false, fmt.Errorf("resolve claw environment: %w", err)
	}
	// Workspace identity is hub-managed and must not be overridden by
	// workspace or workflow environment configuration.
	env["ELASTICCLAW_TEMPLATE"] = workspace.Name
	templateFiles["SECRETS.md"] = buildSecretsFile(resolvedSecrets)
	templateFiles = injectFigmaAPIDocs(templateFiles, env)

	var (
		instanceType    string
		llmKey          string
		nixEnabled      int
		dockerEnabled   int
		repositories    []types.GitHubRepoAccess
		linearWorkspace string
	)
	if len(workspace.Repositories) > 0 {
		repositories = append([]types.GitHubRepoAccess(nil), workspace.Repositories...)
	}
	if hasRepositoryGlob(repositories) {
		parentCtx := opts.ctx
		if parentCtx == nil {
			parentCtx = context.Background()
		}
		expandCtx, cancel := context.WithTimeout(parentCtx, 45*time.Second)
		defer cancel()
		repositories, err = s.expandWorkspaceRepositories(expandCtx, workspace.Name, repositories)
		if err != nil {
			return "", false, fmt.Errorf("expand workspace repositories: %w", err)
		}
	}
	if tmplCfg != nil {
		instanceType = tmplCfg.InstanceType
		llmKey = tmplCfg.LLMKey
		if tmplCfg.DefaultModel != "" {
			defaultModel = tmplCfg.DefaultModel
		}
		if tmplCfg.Nix {
			nixEnabled = 1
		}
		if tmplCfg.Docker {
			dockerEnabled = 1
		}
		if tmplCfg.Linear != nil {
			linearWorkspace = tmplCfg.Linear.Workspace
		}
	}
	if workspaceV2 != nil && workspaceV2.Execution != nil {
		if workspaceV2.Execution.Nix {
			nixEnabled = 1
		}
		if workspaceV2.Execution.Docker {
			dockerEnabled = 1
		}
	}
	s.mu.RLock()
	defaultModel, llmKey = resolveModelAndLLMKey(s.hubCfg, llmKey, defaultModel)
	s.mu.RUnlock()

	tags := mergeTags(workspace.Name, workflow.Tags, nil)
	tags = append(tags, "workspace:"+workspace.Name, "workflow:"+workflow.Name)
	if opts.inputs != nil {
		tags = append(tags, "manual-trigger")
	}
	tagsJSON, _ := json.Marshal(tags)
	repositoriesJSON, _ := json.Marshal(repositories)

	groupName, groupLimit := s.resolveWorkflowGroupLimit(workflow)
	workflowVolumes, err := normalizeWorkflowVolumes(tenantID, workflow)
	if err != nil {
		return "", false, err
	}
	workflowVolumesJSON, _ := json.Marshal(workflowVolumes)
	if opts.issueLabelsAvailable {
		labelsJSON, _ := json.Marshal(normalizeIssueLabels(opts.issueLabels))
		templateFiles[issueLabelsTemplateFile] = string(labelsJSON)
	}
	s.promoteMu.Lock()
	activeCount := s.countActiveClawsInGroup(groupName)
	isPending := groupLimit > 0 && activeCount >= groupLimit
	initialStatus := "provisioning"
	if isPending {
		initialStatus = "pending"
	}

	filesJSON, _ := json.Marshal(templateFiles)
	now := time.Now().UTC()
	_, err = s.db.Exec(`
		INSERT INTO claws(id, tenant_id, name, template, provider, default_model, template_files, github_repos, linear_workspace, nix, docker, tags, color, llm_key, status, created_at, factory_name, concurrency_group, workflow_volumes)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		clawID, tenantID, clawName, workspace.Name, provider, defaultModel, string(filesJSON),
		string(repositoriesJSON), linearWorkspace, nixEnabled, dockerEnabled, string(tagsJSON), workflow.Color, llmKey,
		initialStatus, now, "", groupName, string(workflowVolumesJSON),
	)
	s.promoteMu.Unlock()
	if err != nil {
		return "", false, fmt.Errorf("db insert: %w", err)
	}

	if opts.githubIssueID != "" {
		_, _ = s.db.Exec(`UPDATE claws SET github_issue_id=? WHERE id=?`, opts.githubIssueID, clawID)
	}
	if opts.linearIssueID != "" {
		_, _ = s.db.Exec(`UPDATE claws SET linear_issue_id=? WHERE id=?`, opts.linearIssueID, clawID)
	}
	if opts.shortcutStoryID != "" {
		_, _ = s.db.Exec(`UPDATE claws SET shortcut_story_id=? WHERE id=?`, opts.shortcutStoryID, clawID)
	}
	if opts.jiraIssueID != "" {
		_, _ = s.db.Exec(`UPDATE claws SET jira_issue_id=? WHERE id=?`, opts.jiraIssueID, clawID)
	}
	if opts.triggerActor != nil {
		actorJSON, err := json.Marshal(opts.triggerActor)
		if err != nil {
			log.Printf("[workflow] failed to marshal trigger actor for claw %s: %v", shortID(clawID), err)
		} else if _, err := s.db.Exec(`UPDATE claws SET trigger_actor_json=? WHERE id=?`, string(actorJSON), clawID); err != nil {
			log.Printf("[workflow] failed to persist trigger actor for claw %s: %v", shortID(clawID), err)
		}
	}

	analyticsEnabled, requiresPR, excludedReason := taskRunAnalyticsContractForWorkflow(workflow)
	if _, _, err := s.ensureTaskRunForClaw(clawID, TaskRunStart{
		RunKind:          taskRunKindForWorkflow(workflow),
		OwnerType:        taskRunOwnerWorkflow,
		OwnerName:        workspace.Name + "/" + workflow.Name,
		OwnerDisplayName: workflow.Name,
		WorkspaceName:    workspace.Name,
		WorkflowName:     workflow.Name,
		Integration:      workflow.Integration,
		IssueID:          firstNonEmpty(opts.githubIssueID, opts.linearIssueID, opts.shortcutStoryID, opts.jiraIssueID),
		IssueCreatedAt:   opts.issueCreatedAt,
		Model:            defaultModel,
		LLMKey:           llmKey,
		Source:           taskRunSourceWorkflow,
		AnalyticsEnabled: analyticsEnabled,
		RequiresPR:       requiresPR,
		ExcludedReason:   excludedReason,
		StartedAt:        now,
		EventKey:         "task_start:claw:" + clawID,
	}); err != nil {
		if _, cleanupErr := s.db.Exec(`DELETE FROM claws WHERE id=?`, clawID); cleanupErr != nil {
			log.Printf("[workflow] WARNING: failed to delete orphaned claw %s after task-run creation failure: %v", clawID[:8], cleanupErr)
		}
		go s.promotePendingClaws()
		return "", false, fmt.Errorf("task run analytics: %w", err)
	}
	if opts.beforeProvision != nil {
		callbackCtx := opts.ctx
		if callbackCtx == nil {
			callbackCtx = context.Background()
		}
		if err := opts.beforeProvision(callbackCtx, clawID, tenantID); err != nil {
			_, _ = s.finishClawTerminalTx(clawID, "deleted", "", "failed",
				"workflow v2 activation failed: "+err.Error(), terminalTxOpts{})
			go s.promotePendingClaws()
			return "", false, fmt.Errorf("activate workflow v2 run: %w", err)
		}
	}

	log.Printf("[workflow] created claw %s (%s) for workflow %s/%s (status=%s, reason=%s)", clawName, clawID[:8], workspace.Name, workflow.Name, initialStatus, opts.reason)
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type:    "claw_status",
		Payload: map[string]string{"claw_id": clawID, "status": initialStatus},
	})

	if isPending {
		return clawID, true, nil
	}

	providerTemplateFiles := workspaceTemplateFiles(templateFiles)
	req := types.CreateClawRequest{
		Name:         clawName,
		TemplateName: workspace.Name,
		Provider:     provider,
		DefaultModel: defaultModel,
		LLMKey:       llmKey,
		Files:        providerTemplateFiles,
		Env:          env,
		InstanceType: instanceType,
		ProviderName: "ec-" + clawID[:8],
	}
	fileBytes := make(map[string][]byte, len(providerTemplateFiles))
	for k, v := range providerTemplateFiles {
		fileBytes[k] = []byte(v)
	}

	go func() {
		var currentStatus string
		_ = s.db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&currentStatus)
		if currentStatus == "deleted" {
			return
		}
		ctx := context.Background()
		var provErr error
		switch provider {
		case "replicated":
			provErr = s.provisionReplicated(ctx, clawID, req, provCfg, env)
		case "daytona":
			provErr = s.provisionDaytona(ctx, clawID, req, provCfg, fileBytes, env)
		case "exedev":
			provErr = s.provisionExedev(ctx, clawID, req, provCfg, fileBytes, env)
		case "docker":
			provErr = s.provisionDocker(ctx, clawID, req, provCfg, fileBytes)
		case "lambda-microvms":
			provErr = s.provisionLambdaMicroVMs(ctx, clawID, req, provCfg, fileBytes)
		case "noop":
			if os.Getenv("ELASTICCLAW_NOOP_PROVIDER") == "" {
				provErr = fmt.Errorf("noop provider requires ELASTICCLAW_NOOP_PROVIDER=1 (test use only)")
			} else {
				providerID := "noop-vm-" + clawID[:8]
				_, _ = s.db.Exec(`UPDATE claws SET status='connected', provider='noop', provider_id=? WHERE id=? AND status NOT IN ('idle','deleted','error')`, providerID, clawID)
			}
		default:
			provErr = fmt.Errorf("unsupported provider: %s", provider)
		}
		if provErr != nil {
			log.Printf("[workflow] provision failed for %s: %v", clawID, provErr)
			s.stopAgentWithReason(clawID, fmt.Sprintf("Workflow provision failed: %v", provErr), false)
		}
	}()

	return clawID, false, nil
}

// resolveClawEnv reconstructs the environment supplied to a claw. Workspace
// secrets intentionally take precedence over hub secrets for all secret refs.
func (s *Server) resolveClawEnv(workspaceName string, workflow *types.WorkflowConfig, tmplCfg *types.TemplateConfig, clawID string) (map[string]string, map[string]string, error) {
	s.mu.RLock()
	clawToken := s.hubCfg.ClawToken
	hubSecrets := s.hubCfg.Secrets
	s.mu.RUnlock()
	workspaceSecrets, err := loadWorkspaceSecrets(workspaceName)
	if err != nil {
		return nil, nil, fmt.Errorf("load workspace secrets: %w", err)
	}
	resolveSecret := func(secretRef string) (string, bool) {
		if val, ok := workspaceSecrets[secretRef]; ok {
			return val, true
		}
		val, ok := hubSecrets[secretRef]
		return val, ok
	}
	env := map[string]string{
		"ELASTICCLAW_HUB_URL": s.clawHubURL(), "ELASTICCLAW_CLAW_ID": clawID, "ELASTICCLAW_CLAW_TOKEN": clawToken,
	}
	resolvedSecrets := map[string]string{}
	if workflow != nil && workflow.Integration == "linear" {
		if token := s.resolveLinearTokenForWorkflow(workspaceName, workflow); token != "" {
			env["LINEAR_API_KEY"] = token
			resolvedSecrets["LINEAR_API_KEY"] = "Linear integration token"
		}
	}
	if workflow != nil && workflow.Integration == "jira" {
		if tracker, ok := s.resolveJiraTrackerForWorkflow(workspaceName, workflow); ok && tracker.Token != "" {
			env["JIRA_API_KEY"] = tracker.Token
			resolvedSecrets["JIRA_API_KEY"] = "Jira integration token"
			if tracker.BaseURL != "" {
				env["JIRA_BASE_URL"] = tracker.BaseURL
			}
			if tracker.Username != "" {
				env["JIRA_USERNAME"] = tracker.Username
			}
		}
	}
	if tmplCfg != nil {
		for envName, envVar := range tmplCfg.Env {
			if envVar.Secret != "" {
				if val, ok := resolveSecret(envVar.Secret); ok {
					env[envName], resolvedSecrets[envName] = val, "workspace env secret"
				}
				continue
			}
			env[envName] = envVar.Value
		}
		for envName, secretRef := range tmplCfg.SecretRefs {
			if val, ok := resolveSecret(secretRef); ok {
				env[envName], resolvedSecrets[envName] = val, "workspace secret_ref"
			}
		}
	}
	if workflow != nil {
		for envName, secretRef := range workflow.SecretRefs {
			if val, ok := resolveSecret(secretRef); ok {
				env[envName], resolvedSecrets[envName] = val, "workflow secret_ref"
			}
		}
	}
	return env, resolvedSecrets, nil
}

func (s *Server) resolveWorkflowGroupLimit(workflow *types.WorkflowConfig) (string, int) {
	groupName := workflow.ConcurrencyGroup
	if groupName == "" {
		groupName = "global"
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, g := range s.hubCfg.ConcurrencyGroups {
		if g.Name == groupName {
			return groupName, g.Limit
		}
	}
	if groupName == "global" && s.hubCfg.MaxConcurrentClaws > 0 {
		return groupName, s.hubCfg.MaxConcurrentClaws
	}
	return groupName, 0
}

func buildWorkflowManualTriggerContext(workflow *types.WorkflowConfig, inputs map[string]string, workspaceContext string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("# Manual Workflow Trigger: %s\n\n", workflow.Name))
	b.WriteString("## Inputs\n\n")
	if len(workflow.Inputs) == 0 {
		b.WriteString("No manual inputs were provided.\n")
	} else {
		for _, in := range workflow.Inputs {
			val, ok := inputs[in.Name]
			if !ok {
				val = in.Default
			}
			b.WriteString(fmt.Sprintf("- **%s**: %s\n", in.Name, val))
			if in.Description != "" {
				b.WriteString(fmt.Sprintf("  - %s\n", in.Description))
			}
		}
	}
	if inject := workflowEntryInject(workflow); inject != "" {
		b.WriteString("\n## Workflow Task\n\n")
		b.WriteString(inject)
		if !strings.HasSuffix(inject, "\n") {
			b.WriteString("\n")
		}
	}
	if workspaceContext = strings.TrimSpace(workspaceContext); workspaceContext != "" {
		b.WriteString("\n## Workspace Context\n\n")
		b.WriteString(workspaceContext)
		b.WriteString("\n")
	}
	b.WriteString("\n## Instructions\n\n")
	if len(workflow.Inputs) > 0 {
		b.WriteString("1. Read the trigger inputs fully\n")
		b.WriteString("2. Follow the workflow task and workspace context\n")
		b.WriteString("3. Explore the codebase as needed\n")
		b.WriteString("4. Follow the PR Completion Policy below\n")
	} else {
		b.WriteString("1. Follow the workflow task and workspace context\n")
		b.WriteString("2. Explore the codebase as needed\n")
		b.WriteString("3. Follow the PR Completion Policy below\n")
	}
	appendDefaultFactoryPRPolicy(&b)
	return b.String()
}

func workflowEntryInject(workflow *types.WorkflowConfig) string {
	for _, stage := range workflow.Stages {
		if !stage.Entry {
			continue
		}
		if inject, ok := stage.OnEnter["inject"].(string); ok {
			return strings.TrimSpace(inject)
		}
		return ""
	}
	if len(workflow.Stages) == 0 {
		return ""
	}
	if inject, ok := workflow.Stages[0].OnEnter["inject"].(string); ok {
		return strings.TrimSpace(inject)
	}
	return ""
}

func buildSecretsFile(resolved map[string]string) string {
	var lines []string
	for envName, source := range resolved {
		lines = append(lines, fmt.Sprintf("- `%s` — %s", envName, source))
	}
	if len(lines) == 0 {
		lines = append(lines, "- No workflow secrets were injected.")
	}
	return "## Available Secrets\n\nThe following API keys are available as environment variables:\n\n" +
		strings.Join(lines, "\n") + "\n\nUse these with your tools as needed. Values are in the environment, not in files.\n"
}
