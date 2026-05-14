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

// createClawFromFactory creates a claw from a factory configuration.
// issueID is empty for manual triggers (not tied to a Linear/GitHub issue).
// inputs is the validated map of user inputs for manual triggers; nil for webhooks.
// prebuiltTemplateFiles is optional — when provided (e.g. from linear.go with CONTEXT.md
// already injected), it skips template resolution. When nil, templates are resolved here.
// reason is a human-readable string describing why the claw is being created
// (e.g. "linear webhook", "manual trigger", "poll"). It is logged for debugging.
// Returns the claw ID, whether it was queued as pending (concurrency limit), and any error.
func (s *Server) createClawFromFactory(factory *types.FactoryConfig, issueID string, inputs map[string]string, prebuiltTemplateFiles map[string]string, reason string) (string, bool, error) {
	// Resolve template files (or use pre-built ones from caller)
	var templateFiles map[string]string
	var err error
	if prebuiltTemplateFiles != nil {
		templateFiles = prebuiltTemplateFiles
	} else {
		templateFiles, err = s.resolveTemplateFiles(factory.Template)
		if err != nil {
			return "", false, fmt.Errorf("template %q not found: %w", factory.Template, err)
		}
	}

	// Parse elasticclaw-config.yaml from template files
	var tmplCfg *types.TemplateConfig
	if cfgContent, ok := templateFiles["elasticclaw-config.yaml"]; ok {
		var parseErr error
		tmplCfg, parseErr = config.ParseTemplateConfig([]byte(cfgContent))
		if parseErr != nil {
			log.Printf("[factory:%s] warning: could not parse elasticclaw-config.yaml from template %q: %v", factory.Name, factory.Template, parseErr)
			tmplCfg = nil
		}
	}

	// Build context file for manual triggers
	if inputs != nil && len(inputs) > 0 {
		ctxContent := buildManualTriggerContext(factory, inputs)
		templateFiles["CONTEXT.md"] = ctxContent
		// Also store raw inputs as JSON for reliable parsing (avoids fragile markdown splitting)
		inputsJSON, _ := json.Marshal(inputs)
		templateFiles["TRIGGER_INPUTS.json"] = string(inputsJSON)
	} else if issueID != "" && prebuiltTemplateFiles == nil {
		// Webhook-triggered without pre-built files: integration-specific context
		// should be injected by caller. If prebuiltTemplateFiles was passed,
		// the caller already injected it (e.g. buildLinearContext in linear.go).
	}

	// Determine claw name
	clawName := factory.Name
	if factory.NamePattern != "" {
		clawName = factory.NamePattern
		if issueID != "" {
			clawName = strings.ReplaceAll(clawName, "{issue_id}", issueID)
		}
		// Replace input placeholders for manual triggers
		if inputs != nil {
			for k, v := range inputs {
				clawName = strings.ReplaceAll(clawName, "{"+k+"}", v)
			}
		}
	}
	if clawName == factory.Name && issueID != "" {
		clawName = issueID
	}
	// For manual triggers without a name_pattern, generate a descriptive name
	// using the first input value (e.g., "fix login bug")
	if clawName == factory.Name && inputs != nil && len(factory.Inputs) > 0 {
		// Use the first defined input's value as the descriptive part.
		// factory.Inputs is an ordered slice, so this is deterministic even
		// when inputs contains multiple keys (Go map iteration order is random).
		firstKey := factory.Inputs[0].Name
		if firstVal := inputs[firstKey]; firstVal != "" {
			clawName = firstVal
		}
	}

	// Find tenant
	var tenantID string
	if err := s.db.QueryRow(`SELECT id FROM tenants LIMIT 1`).Scan(&tenantID); err != nil {
		return "", false, fmt.Errorf("no tenant: %w", err)
	}

	// Find provider: factory override > template config > hub default
	provider := factory.Provider
	if provider == "" {
		if tmplCfg != nil && tmplCfg.Provider != "" {
			provider = tmplCfg.Provider
		}
	}
	if provider == "" {
		provider = s.defaultProvider()
	}
	if provider == "" {
		return "", false, fmt.Errorf("no provider configured")
	}

	// Build env vars
	env := map[string]string{
		"ELASTICCLAW_HUB_URL":    s.clawHubURL(),
		"ELASTICCLAW_CLAW_TOKEN": s.hubCfg.ClawToken,
	}

	// Resolve integration tokens based on factory type
	var linearToken string
	if factory.Integration == "linear" {
		linearToken = s.resolveLinearTokenForFactory(factory)
		if linearToken != "" {
			env["LINEAR_API_KEY"] = linearToken
		}
	}

	// Resolve and inject template-requested secrets (deprecated list format)
	resolvedSecrets := make(map[string]string)
	if tmplCfg != nil && len(tmplCfg.Secrets) > 0 {
		log.Printf("[factory:%s] DEPRECATED: template %q uses 'secrets:' list — migrate to 'secret_refs:' map", factory.Name, factory.Template)
		for _, ref := range tmplCfg.Secrets {
			val, envName, ok := s.resolveSecretRef(ref, factory)
			if ok {
				env[envName] = val
				resolvedSecrets[envName] = val
				log.Printf("[factory:%s] injected template secret %s as %s into claw env", factory.Name, ref.Type, envName)
			}
		}
	}

	// Resolve and inject template-level secret_refs (env var → hub secret name)
	// Template-level refs go first; factory-level refs override them for the same env var.
	if tmplCfg != nil && len(tmplCfg.SecretRefs) > 0 {
		for envName, secretRef := range tmplCfg.SecretRefs {
			if val, ok := s.hubCfg.Secrets[secretRef]; ok {
				env[envName] = val
				resolvedSecrets[envName] = val
				log.Printf("[factory:%s] injected template secret_ref %s as %s into claw env", factory.Name, secretRef, envName)
			} else {
				log.Printf("[factory:%s] WARNING: template secret_ref %q not found in hub secrets", factory.Name, secretRef)
			}
		}
	}

	// Resolve and inject factory-level secret_refs (env var → hub secret name)
	// Factory-level refs win over template-level refs for the same env var.
	if len(factory.SecretRefs) > 0 {
		for envName, secretRef := range factory.SecretRefs {
			if val, ok := s.hubCfg.Secrets[secretRef]; ok {
				env[envName] = val
				resolvedSecrets[envName] = val
				log.Printf("[factory:%s] injected factory secret_ref %s as %s into claw env", factory.Name, secretRef, envName)
			} else {
				log.Printf("[factory:%s] WARNING: secret_ref %q not found in hub secrets", factory.Name, secretRef)
			}
		}
	}

	// Resolve template config fields
	var (
		instanceType    string
		defaultModel    string
		llmKey          string
		nixEnabled      int
		dockerEnabled   int
		githubRepos     []types.GitHubRepoAccess
		linearWorkspace string
		autoFixCI         int = 1
		autoFixBugbot     int = 1
		autoFixGreptile   int = 0
	)
	if tmplCfg != nil {
		instanceType = tmplCfg.InstanceType
		defaultModel = tmplCfg.DefaultModel
		llmKey = tmplCfg.LLMKey
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
		if tmplCfg.AutoWatchCI != nil && !*tmplCfg.AutoWatchCI {
			autoFixCI = 0
		}
		if tmplCfg.AutoWatchBugbot != nil && !*tmplCfg.AutoWatchBugbot {
			autoFixBugbot = 0
		}
		if tmplCfg.AutoWatchGreptile != nil && *tmplCfg.AutoWatchGreptile {
			autoFixGreptile = 1
		}
	}

	// Build SECRETS.md
	var secretList []string
	if linearToken != "" {
		secretList = append(secretList, "- `LINEAR_API_KEY` — Linear API token")
	}
	for envName, val := range resolvedSecrets {
		var refType string
		// Check template secrets (deprecated list format)
		if tmplCfg != nil {
			for _, ref := range tmplCfg.Secrets {
				if ref.EnvVarName() == envName {
					refType = ref.Type
					break
				}
			}
		}
		// Check template secret_refs
		if refType == "" && tmplCfg != nil {
			if _, ok := tmplCfg.SecretRefs[envName]; ok {
				refType = "template secret_ref"
			}
		}
		// Check factory secret_refs
		if refType == "" {
			if _, ok := factory.SecretRefs[envName]; ok {
				refType = "factory secret_ref"
			}
		}
		if refType == "" {
			refType = "custom"
		}
		secretList = append(secretList, fmt.Sprintf("- `%s` — %s secret", envName, refType))
		_ = val
	}

	secretsContent := "## Available Secrets\n\nThe following API keys are available as environment variables:\n\n" +
		strings.Join(secretList, "\n") + "\n\nUse these with your tools as needed. Values are in the environment, not in files.\n"

	// Add integration-specific instructions
	if factory.Integration == "linear" {
		secretsContent += "\n## Linear Tools (REQUIRED)\n\n" +
			"You are working on a Linear issue. You MUST use the built-in Linear CLI to interact with Linear.\n\n" +
			"**Never** use the browser, web_search, or raw HTTP/GraphQL to access Linear.\n\n" +
			"The CLI is pre-installed in this environment:\n\n" +
			"```bash\n" +
			"# Get issue details (replace ELA-123 with the actual issue ID)\n" +
			"claw-bridge linear issue get ELA-123\n\n" +
			"# Update issue state\n" +
			"claw-bridge linear issue update ELA-123 --state=\"In Progress\"\n\n" +
			"# Add a comment\n" +
			"claw-bridge linear issue update ELA-123 --comment=\"your comment here\"\n\n" +
			"# Search for related issues\n" +
			"claw-bridge linear issue search \"some query\"\n\n" +
			"# List teams\n" +
			"claw-bridge linear teams\n" +
			"```\n\n" +
			"Always use these commands for any Linear interaction.\n"
	}

	templateFiles["SECRETS.md"] = secretsContent

	// Resolve default model
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
	if defaultModel == "" {
		s.mu.RLock()
		defaultModel = s.hubCfg.DefaultModel
		s.mu.RUnlock()
	}

	// Build tags
	tags := mergeTags(factory.Template, factory.Tags, nil)
	hasfactory := false
	for _, t := range tags {
		if t == "factory:"+factory.Name {
			hasfactory = true
			break
		}
	}
	if !hasfactory {
		tags = append(tags, "factory:"+factory.Name)
	}
	// Add manual-trigger tag for UI filtering
	if inputs != nil {
		tags = append(tags, "manual-trigger")
	}
	tagsJSON, _ := json.Marshal(tags)

	// Color
	clawColor := factory.Color
	if clawColor == "" && tmplCfg != nil {
		clawColor = tmplCfg.Color
	}

	githubReposJSON, _ := json.Marshal(githubRepos)

	// Check concurrency limit (group-aware)
	s.promoteMu.Lock()

	s.mu.RLock()
	groupName, groupLimit := s.resolveGroupLimit(factory)
	s.mu.RUnlock()

	activeCount := s.countActiveClawsInGroup(groupName)
	isPending := false
	if groupLimit > 0 && activeCount >= groupLimit {
		isPending = true
		log.Printf("[factory] concurrency limit reached for group %q (active=%d, limit=%d) — queueing claw for factory %s as pending", groupName, activeCount, groupLimit, factory.Name)
	}

	// Insert claw record
	clawID := uuid.New().String()
	filesJSON, _ := json.Marshal(templateFiles)
	now := time.Now().UTC()

	initialStatus := "provisioning"
	if isPending {
		initialStatus = "pending"
	}

	var linearIssueID, githubIssueID, shortcutStoryID string
	if factory.Integration == "linear" && issueID != "" {
		linearIssueID = issueID
	}
	if factory.Integration == "github-issues" && issueID != "" {
		githubIssueID = issueID
	}
	if factory.Integration == "shortcut" && issueID != "" {
		shortcutStoryID = issueID
	}

	_, err = s.db.Exec(`
		INSERT INTO claws(id, tenant_id, name, template, provider, default_model, template_files, github_repos, linear_workspace, nix, docker, tags, color, llm_key, auto_fix_ci, auto_fix_bugbot, auto_fix_greptile, linear_issue_id, github_issue_id, shortcut_story_id, status, created_at, factory_name, concurrency_group)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		clawID, tenantID, clawName, factory.Template, provider, defaultModel, string(filesJSON),
		string(githubReposJSON), linearWorkspace, nixEnabled, dockerEnabled, string(tagsJSON), clawColor, llmKey, autoFixCI, autoFixBugbot, autoFixGreptile,
		linearIssueID, githubIssueID, shortcutStoryID, initialStatus, now, factory.Name, groupName,
	)

	s.promoteMu.Unlock()

	if err != nil {
		return "", false, fmt.Errorf("db insert: %w", err)
	}

	log.Printf("[factory] created claw %s (%s) for factory %s (status=%s, reason=%s)", clawName, clawID[:8], factory.Name, initialStatus, reason)
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type:    "claw_status",
		Payload: map[string]string{"claw_id": clawID, "status": initialStatus},
	})

	if isPending {
		return clawID, true, nil
	}

	// Provision asynchronously
	provCfg, _ := s.hubCfg.Providers[provider]
	go func() {
		var currentStatus string
		_ = s.db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&currentStatus)
		if currentStatus == "deleted" {
			log.Printf("[factory] claw %s already deleted before provisioning, aborting", clawID[:8])
			return
		}
		ctx := context.Background()
		req := types.CreateClawRequest{
			Name:         clawName,
			TemplateName: factory.Template,
			Provider:     provider,
			Files:        templateFiles,
			Env:          env,
			InstanceType: instanceType,
			ProviderName: "ec-" + clawID[:8],
		}
		fileBytes := make(map[string][]byte, len(templateFiles))
		for k, v := range templateFiles {
			fileBytes[k] = []byte(v)
		}

		var provErr error
		switch provider {
		case "replicated":
			provErr = s.provisionReplicated(ctx, clawID, req, provCfg, env)
		case "daytona":
			provErr = s.provisionDaytona(ctx, clawID, req, provCfg, fileBytes, env)
		case "vercel":
			provErr = s.provisionVercel(ctx, clawID, req, provCfg, fileBytes, env)
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
			log.Printf("[factory] provision failed for %s: %v", clawID, provErr)
			s.stopAgentWithReason(clawID, fmt.Sprintf("Factory provision failed: %v", provErr), false)
		}
	}()

	return clawID, false, nil
}

// buildManualTriggerContext creates a CONTEXT.md for manually triggered claws.
func buildManualTriggerContext(factory *types.FactoryConfig, inputs map[string]string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("# Manual Trigger: %s\n\n", factory.Name))
	b.WriteString("## Inputs\n\n")
	for _, in := range factory.Inputs {
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
