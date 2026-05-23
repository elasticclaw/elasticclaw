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
	"github.com/google/uuid"
)

type workflowCreateOptions struct {
	inputs        map[string]string
	templateFiles map[string]string
	clawName      string
	githubIssueID string
	linearIssueID string
	reason        string
}

func (s *Server) createClawFromWorkflow(workspace *types.WorkspaceConfig, workflow *types.WorkflowConfig, inputs map[string]string, reason string) (string, bool, error) {
	return s.createClawFromWorkflowWithOptions(workspace, workflow, workflowCreateOptions{inputs: inputs, reason: reason})
}

func (s *Server) createClawFromWorkflowWithOptions(workspace *types.WorkspaceConfig, workflow *types.WorkflowConfig, opts workflowCreateOptions) (string, bool, error) {
	templateFiles := opts.templateFiles
	var err error
	if templateFiles == nil {
		templateFiles, err = s.resolveTemplateFiles(workflow.Template)
		if err != nil {
			return "", false, fmt.Errorf("template %q not found: %w", workflow.Template, err)
		}
	}

	var tmplCfg *types.TemplateConfig
	if cfgContent, ok := templateFiles["elasticclaw-config.yaml"]; ok {
		tmplCfg, err = config.ParseTemplateConfig([]byte(cfgContent))
		if err != nil {
			log.Printf("[workflow:%s/%s] warning: could not parse elasticclaw-config.yaml from template %q: %v", workspace.Name, workflow.Name, workflow.Template, err)
			tmplCfg = nil
		}
	}

	if opts.inputs != nil {
		templateFiles["CONTEXT.md"] = buildWorkflowManualTriggerContext(workflow, opts.inputs)
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

	clawID := uuid.New().String()
	env := map[string]string{
		"ELASTICCLAW_HUB_URL":    s.clawHubURL(),
		"ELASTICCLAW_CLAW_ID":    clawID,
		"ELASTICCLAW_CLAW_TOKEN": clawToken,
	}

	resolvedSecrets := map[string]string{}
	if workflow.Integration == "linear" {
		if token := s.resolveLinearTokenForWorkflow(workflow); token != "" {
			env["LINEAR_API_KEY"] = token
			resolvedSecrets["LINEAR_API_KEY"] = "Linear integration token"
		}
	}
	if tmplCfg != nil {
		for envName, secretRef := range tmplCfg.SecretRefs {
			if val, ok := hubSecrets[secretRef]; ok {
				env[envName] = val
				resolvedSecrets[envName] = "template secret_ref"
			}
		}
	}
	for envName, secretRef := range workflow.SecretRefs {
		if val, ok := hubSecrets[secretRef]; ok {
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
		githubRepos     []types.GitHubRepoAccess
		linearWorkspace string
	)
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
		if tmplCfg.GitHub != nil {
			githubRepos = tmplCfg.GitHub.Repos
		}
		if tmplCfg.Linear != nil {
			linearWorkspace = tmplCfg.Linear.Workspace
		}
	}
	if defaultModel == "" && llmKey != "" {
		s.mu.RLock()
		for _, k := range s.hubCfg.LLMKeys {
			if k.Name == llmKey {
				defaultModel = resolveDefaultModelForKey(s.hubCfg, k)
				break
			}
		}
		s.mu.RUnlock()
	}

	tags := mergeTags(workflow.Template, workflow.Tags, nil)
	tags = append(tags, "workspace:"+workspace.Name, "workflow:"+workflow.Name)
	if opts.inputs != nil {
		tags = append(tags, "manual-trigger")
	}
	tagsJSON, _ := json.Marshal(tags)
	githubReposJSON, _ := json.Marshal(githubRepos)

	groupName, groupLimit := s.resolveWorkflowGroupLimit(workflow)
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
		INSERT INTO claws(id, tenant_id, name, template, provider, default_model, template_files, github_repos, linear_workspace, nix, docker, tags, color, llm_key, status, created_at, factory_name, concurrency_group)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		clawID, tenantID, clawName, workflow.Template, provider, defaultModel, string(filesJSON),
		string(githubReposJSON), linearWorkspace, nixEnabled, dockerEnabled, string(tagsJSON), workflow.Color, llmKey,
		initialStatus, now, "", groupName,
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

	log.Printf("[workflow] created claw %s (%s) for workflow %s/%s (status=%s, reason=%s)", clawName, clawID[:8], workspace.Name, workflow.Name, initialStatus, opts.reason)
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type:    "claw_status",
		Payload: map[string]string{"claw_id": clawID, "status": initialStatus},
	})

	if isPending {
		return clawID, true, nil
	}

	req := types.CreateClawRequest{
		Name:         clawName,
		TemplateName: workflow.Template,
		Provider:     provider,
		DefaultModel: defaultModel,
		LLMKey:       llmKey,
		Files:        templateFiles,
		Env:          env,
		InstanceType: instanceType,
		ProviderName: "ec-" + clawID[:8],
	}
	fileBytes := make(map[string][]byte, len(templateFiles))
	for k, v := range templateFiles {
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

func buildWorkflowManualTriggerContext(workflow *types.WorkflowConfig, inputs map[string]string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("# Manual Trigger: %s\n\n", workflow.Name))
	b.WriteString("## Inputs\n\n")
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
	b.WriteString("\n")
	return b.String()
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
