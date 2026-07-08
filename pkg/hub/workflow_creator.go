package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
)

// workflowCreateOptions moved to pkg/hub/integrations (aliased in
// integrations_bridge.go).

const hubInternalTemplateFilePrefix = "__hub__/"

func workspaceTemplateFiles(files map[string]string) map[string]string {
	filtered := make(map[string]string, len(files))
	for name, content := range files {
		if strings.HasPrefix(name, hubInternalTemplateFilePrefix) {
			continue
		}
		filtered[name] = content
	}
	return filtered
}

func (s *Server) createClawFromWorkflow(workspace *types.WorkspaceConfig, workflow *types.WorkflowConfig, inputs map[string]string, reason string) (string, bool, error) {
	return s.createClawFromWorkflowWithOptions(workspace, workflow, workflowCreateOptions{Inputs: inputs, Reason: reason})
}

func (s *Server) createClawFromWorkflowWithOptions(workspace *types.WorkspaceConfig, workflow *types.WorkflowConfig, opts workflowCreateOptions) (string, bool, error) {
	templateFiles := cloneStringMap(workspace.Files)
	if opts.WorkspaceFiles != nil {
		templateFiles = cloneStringMap(opts.WorkspaceFiles)
	}

	var tmplCfg *types.TemplateConfig
	if cfgContent, ok := templateFiles["elasticclaw-config.yaml"]; ok {
		var err error
		tmplCfg, err = config.ParseTemplateConfig([]byte(cfgContent))
		if err != nil {
			logf("[workflow:%s/%s] warning: could not parse workspace elasticclaw-config.yaml: %v", workspace.Name, workflow.Name, err)
			tmplCfg = nil
		}
	}

	if opts.Inputs != nil {
		templateFiles["CONTEXT.md"] = buildWorkflowManualTriggerContext(workflow, opts.Inputs, templateFiles["CONTEXT.md"])
		inputsJSON, _ := json.Marshal(opts.Inputs)
		templateFiles["TRIGGER_INPUTS.json"] = string(inputsJSON)
	}

	clawName := workflow.Name
	if opts.ClawName != "" {
		clawName = opts.ClawName
	} else if workflow.NamePattern != "" {
		clawName = workflow.NamePattern
		for k, v := range opts.Inputs {
			clawName = strings.ReplaceAll(clawName, "{"+k+"}", v)
		}
	}
	if clawName == workflow.Name && opts.Inputs != nil && len(workflow.Inputs) > 0 {
		if firstVal := opts.Inputs[workflow.Inputs[0].Name]; firstVal != "" {
			clawName = firstVal
		}
	}

	var tenantID string
	if err := s.db.QueryRow(`SELECT id FROM tenants LIMIT 1`).Scan(&tenantID); err != nil {
		return "", false, fmt.Errorf("no tenant: %w", err)
	}

	provider := workflow.Provider
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
	clawToken := s.hubCfg.ClawToken
	hubSecrets := s.hubCfg.Secrets
	defaultModel := s.hubCfg.DefaultModel
	provCfg, ok := s.hubCfg.Providers[provider]
	s.mu.RUnlock()
	if !ok {
		return "", false, fmt.Errorf("provider %q is not configured on this hub", provider)
	}
	workspaceSecrets, err := loadWorkspaceSecrets(workspace.Name)
	if err != nil {
		return "", false, fmt.Errorf("load workspace secrets: %w", err)
	}
	resolveSecret := func(secretRef string) (string, bool) {
		if val, ok := workspaceSecrets[secretRef]; ok {
			return val, true
		}
		if val, ok := hubSecrets[secretRef]; ok {
			return val, true
		}
		return "", false
	}

	clawID := uuid.New().String()
	env := map[string]string{
		"ELASTICCLAW_HUB_URL":    s.clawHubURL(),
		"ELASTICCLAW_CLAW_ID":    clawID,
		"ELASTICCLAW_CLAW_TOKEN": clawToken,
	}

	resolvedSecrets := map[string]string{}
	if workflow.Integration == "linear" {
		if token := s.resolveLinearTokenForWorkflow(workspace.Name, workflow); token != "" {
			env["LINEAR_API_KEY"] = token
			resolvedSecrets["LINEAR_API_KEY"] = "Linear integration token"
		}
	}
	if workflow.Integration == "jira" {
		if tracker, ok := s.resolveJiraTrackerForWorkflow(workspace.Name, workflow); ok && tracker.Token != "" {
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
					env[envName] = val
					resolvedSecrets[envName] = "workspace env secret"
				}
				continue
			}
			env[envName] = envVar.Value
		}
		for envName, secretRef := range tmplCfg.SecretRefs {
			if val, ok := resolveSecret(secretRef); ok {
				env[envName] = val
				resolvedSecrets[envName] = "workspace secret_ref"
			}
		}
	}
	for envName, secretRef := range workflow.SecretRefs {
		if val, ok := resolveSecret(secretRef); ok {
			env[envName] = val
			resolvedSecrets[envName] = "workflow secret_ref"
		}
	}
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
	s.mu.RLock()
	defaultModel, llmKey = resolveModelAndLLMKey(s.hubCfg, llmKey, defaultModel)
	s.mu.RUnlock()

	tags := mergeTags(workspace.Name, workflow.Tags, nil)
	tags = append(tags, "workspace:"+workspace.Name, "workflow:"+workflow.Name)
	if opts.Inputs != nil {
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
	if opts.IssueLabelsAvailable {
		labelsJSON, _ := json.Marshal(normalizeIssueLabels(opts.IssueLabels))
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

	if opts.GitHubIssueID != "" {
		_, _ = s.db.Exec(`UPDATE claws SET github_issue_id=? WHERE id=?`, opts.GitHubIssueID, clawID)
	}
	if opts.LinearIssueID != "" {
		_, _ = s.db.Exec(`UPDATE claws SET linear_issue_id=? WHERE id=?`, opts.LinearIssueID, clawID)
	}
	if opts.ShortcutStoryID != "" {
		_, _ = s.db.Exec(`UPDATE claws SET shortcut_story_id=? WHERE id=?`, opts.ShortcutStoryID, clawID)
	}
	if opts.JiraIssueID != "" {
		_, _ = s.db.Exec(`UPDATE claws SET jira_issue_id=? WHERE id=?`, opts.JiraIssueID, clawID)
	}
	if opts.TriggerActor != nil {
		actorJSON, err := json.Marshal(opts.TriggerActor)
		if err != nil {
			logf("[workflow] failed to marshal trigger actor for claw %s: %v", shortID(clawID), err)
		} else if _, err := s.db.Exec(`UPDATE claws SET trigger_actor_json=? WHERE id=?`, string(actorJSON), clawID); err != nil {
			logf("[workflow] failed to persist trigger actor for claw %s: %v", shortID(clawID), err)
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
		IssueID:          firstNonEmpty(opts.GitHubIssueID, opts.LinearIssueID, opts.ShortcutStoryID, opts.JiraIssueID),
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
			logf("[workflow] WARNING: failed to delete orphaned claw %s after task-run creation failure: %v", clawID[:8], cleanupErr)
		}
		go s.promotePendingClaws()
		return "", false, fmt.Errorf("task run analytics: %w", err)
	}

	logf("[workflow] created claw %s (%s) for workflow %s/%s (status=%s, reason=%s)", clawName, clawID[:8], workspace.Name, workflow.Name, initialStatus, opts.Reason)
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
			logf("[workflow] provision failed for %s: %v", clawID, provErr)
			s.stopAgentWithReason(clawID, fmt.Sprintf("Workflow provision failed: %v", provErr), false)
		}
	}()

	return clawID, false, nil
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
