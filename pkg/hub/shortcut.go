package hub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
)

// ── Webhook payload types ─────────────────────────────────────────────────────

type shortcutWebhookPayload struct {
	ID        string           `json:"id"`
	Actions   []shortcutAction `json:"actions"`
	ChangedAt string           `json:"changed_at"`
}

type shortcutAction struct {
	ID          int64                     `json:"id"`
	EntityType  string                    `json:"entity_type"` // "story"
	Action      string                    `json:"action"`      // "update", "create"
	Name        string                    `json:"name"`
	AppURL      string                    `json:"app_url"`
	Description string                    `json:"description"`
	Changes     map[string]shortcutChange `json:"changes"`
}

type shortcutStoryFilterData struct {
	labels    map[string]bool
	assignees map[string]bool
	hasOwner  bool
	loadErr   error
}

type shortcutChange struct {
	New interface{} `json:"new"`
	Old interface{} `json:"old"`
}

// ── Handler ───────────────────────────────────────────────────────────────────

// validateShortcutSignature checks HMAC-SHA256 against factory webhook secrets.
func (s *Server) validateShortcutSignature(workspaceName string, body []byte, sig string) bool {
	// Strip sha256= prefix if present
	sig = strings.TrimPrefix(sig, "sha256=")
	s.mu.RLock()
	secrets := s.hubCfg.Secrets
	s.mu.RUnlock()
	factories := s.resolveFactories()
	hasSecrets := false
	for _, secret := range workspaceIssueTrackerWebhookSecrets(workspaceName, "shortcut") {
		hasSecrets = true
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		expected := hex.EncodeToString(mac.Sum(nil))
		if hmac.Equal([]byte(sig), []byte(expected)) {
			return true
		}
	}
	if workspaceName != "" {
		return !hasSecrets
	}
	for _, f := range factories {
		if f.Integration != "shortcut" {
			continue
		}
		secret := f.WebhookSecret
		if secret == "" && f.WebhookSecretRef != "" && secrets != nil {
			secret = secrets[f.WebhookSecretRef]
		}
		if secret == "" {
			continue
		}
		hasSecrets = true
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		expected := hex.EncodeToString(mac.Sum(nil))
		if hmac.Equal([]byte(sig), []byte(expected)) {
			return true
		}
	}
	// If no secrets configured, accept all (open webhook)
	return !hasSecrets
}

func (s *Server) validateShortcutWebhook() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.hubCfg.Integrations == nil {
		return false
	}

	// Validate that at least one Shortcut integration with a token is configured
	for _, sc := range s.hubCfg.Integrations.Shortcut {
		if sc.Token != "" {
			return true
		}
	}

	return false
}

func (s *Server) handleShortcutWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Validate that at least one Shortcut integration is configured
	if !s.validateShortcutWebhook() {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	// Validate HMAC signature if any factory has a webhook_secret configured.
	// Shortcut sends: Payload-Signature: sha256=<hex>
	sig := r.Header.Get("Payload-Signature")
	if !s.validateShortcutSignature(r.PathValue("workspace"), body, sig) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	var payload shortcutWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	go s.processShortcutEvent(payload)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) processShortcutEvent(payload shortcutWebhookPayload) {
	factories := s.resolveFactories()

	// Build a per-token state-name cache so we only fetch the workflow list once
	// per Shortcut workspace across all actions in this payload. A webhook with K
	// actions and N factories sharing one token produces a single API call.
	stateNameCache := map[string]map[int64]string{}

	for _, action := range payload.Actions {
		if action.EntityType != "story" || action.Action != "update" {
			continue
		}

		storyFiltersByToken := map[string]*shortcutStoryFilterData{}

		// Check if workflow_state_id changed
		stateChange, ok := action.Changes["workflow_state_id"]
		if !ok {
			continue
		}

		// Resolve state names via API
		newStateID := toInt64(stateChange.New)
		oldStateID := toInt64(stateChange.Old)
		storyID := fmt.Sprintf("sc-%d", action.ID)

		for _, factory := range factories {
			if factory.Integration != "shortcut" {
				continue
			}
			if factory.Enabled != nil && !*factory.Enabled {
				continue
			}

			token := s.resolveShortcutToken(factory.Workspace)
			if token == "" {
				continue
			}

			if len(factory.Labels) > 0 || factory.AssignedTo != "" {
				filterData, ok := storyFiltersByToken[token]
				if !ok {
					filterData = s.loadShortcutStoryFilterData(token, action.ID)
					storyFiltersByToken[token] = filterData
				}
				if filterData.loadErr != nil {
					log.Printf("[factory:%s] skipping story %s: failed to load Shortcut story filters: %v", factory.Name, storyID, filterData.loadErr)
					continue
				}

				// Labels filter: all configured labels must be present on the story (AND)
				if len(factory.Labels) > 0 {
					allMatch := true
					for _, required := range factory.Labels {
						required = strings.ToLower(strings.TrimSpace(required))
						if required == "" {
							continue
						}
						if !filterData.labels[required] {
							allMatch = false
							break
						}
					}
					if !allMatch {
						continue
					}
				}

				// AssignedTo filter
				if factory.AssignedTo != "" {
					wanted := strings.ToLower(strings.TrimSpace(factory.AssignedTo))
					switch {
					case wanted == "any":
						if !filterData.hasOwner {
							continue
						}
					case wanted == "none":
						if filterData.hasOwner {
							continue
						}
					case strings.HasPrefix(wanted, "!"):
						excluded := strings.TrimPrefix(strings.TrimPrefix(wanted, "!"), "@")
						if excluded == "" {
							continue
						}
						if filterData.assignees[excluded] {
							continue
						}
					default:
						target := strings.TrimPrefix(wanted, "@")
						if target == "" {
							continue
						}
						if !filterData.assignees[target] {
							continue
						}
					}
				}
			}

			stateMap, ok := stateNameCache[token]
			if !ok {
				stateMap = buildShortcutStateMap(s.resolveShortcutBaseURL(), token)
				if len(stateMap) > 0 {
					stateNameCache[token] = stateMap
				} else {
					log.Printf("[shortcut-webhook] failed to load workflow states for workspace %q (empty response) — not caching", factory.Workspace)
					continue
				}
			}
			newStateName := stateMap[newStateID]
			oldStateName := stateMap[oldStateID] // only fetched when needed for logging

			// Issue entering trigger status → create claw
			if strings.EqualFold(newStateName, factory.TriggerStatus) && !strings.EqualFold(oldStateName, factory.TriggerStatus) {
				log.Printf("[factory:%s] story %s entered '%s' — creating claw", factory.Name, storyID, factory.TriggerStatus)
				if err := s.createClawForShortcutStory(factory, action, storyID, token, "shortcut webhook"); err != nil {
					if isFactoryTriggerAlreadyClaimed(err) {
						continue
					}
					log.Printf("[factory:%s] failed to create claw for %s: %v", factory.Name, storyID, err)
					s.logFactoryEvent(factory.Name, storyID, action.Name, oldStateName, newStateName, "error", "", err.Error())
					s.trackFactoryCreationFailure(factory.Name, storyID, err.Error())
				} else {
					var clawID string
					_ = s.db.QueryRow(`SELECT id FROM claws WHERE shortcut_story_id=? AND factory_name=? ORDER BY created_at DESC LIMIT 1`, storyID, factory.Name).Scan(&clawID)
					s.logFactoryEvent(factory.Name, storyID, action.Name, oldStateName, newStateName, "claw_created", clawID, "")
					if clawID != "" {
						s.trackFactoryCreationSuccess(factory.Name, storyID, clawID)
					}
				}
			}

			// Issue leaving trigger status → terminate claw
			if factory.TerminateOnLeave && !strings.EqualFold(newStateName, factory.TriggerStatus) {
				var activeClaw string
				_ = s.db.QueryRow(
					`SELECT id FROM claws WHERE shortcut_story_id = ? AND status NOT IN ('error','deleted') LIMIT 1`,
					storyID,
				).Scan(&activeClaw)
				if activeClaw != "" {
					log.Printf("[factory:%s] story %s left trigger — terminating claw", factory.Name, storyID)
					s.terminateClawForIssue(storyID)
					s.logFactoryEvent(factory.Name, storyID, action.Name, oldStateName, newStateName, "claw_terminated", activeClaw, "")
				} else {
					s.logFactoryEvent(factory.Name, storyID, action.Name, oldStateName, newStateName, "not_actionable",
						"", fmt.Sprintf("status '%s'→'%s' did not match trigger '%s'", oldStateName, newStateName, factory.TriggerStatus))
				}
			}
		}
	}
}

func (s *Server) loadShortcutStoryFilterData(token string, storyID int64) *shortcutStoryFilterData {
	data := &shortcutStoryFilterData{
		labels:    map[string]bool{},
		assignees: map[string]bool{},
	}
	story, err := shortcutAPI(s.resolveShortcutBaseURL(), fmt.Sprintf("stories/%d", storyID), token)
	if err != nil {
		data.loadErr = err
		return data
	}

	if rawLabels, ok := story["labels"].([]interface{}); ok {
		for _, item := range rawLabels {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			labelName, _ := m["name"].(string)
			labelName = strings.ToLower(strings.TrimSpace(labelName))
			if labelName != "" {
				data.labels[labelName] = true
			}
		}
	}

	var ownerIDs []string
	if rawOwners, ok := story["owner_ids"].([]interface{}); ok {
		for _, raw := range rawOwners {
			ownerID, ok := raw.(string)
			if !ok {
				continue
			}
			ownerID = strings.TrimSpace(ownerID)
			if ownerID == "" {
				continue
			}
			ownerIDs = append(ownerIDs, ownerID)
			data.assignees[strings.ToLower(ownerID)] = true
		}
	}
	data.hasOwner = len(ownerIDs) > 0

	for _, ownerID := range ownerIDs {
		member, err := shortcutAPI(s.resolveShortcutBaseURL(), "members/"+ownerID, token)
		if err != nil {
			continue
		}
		collectShortcutAssigneeIdentifiers(data.assignees, member)
	}

	return data
}

func collectShortcutAssigneeIdentifiers(set map[string]bool, member map[string]interface{}) {
	add := func(value string) {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			return
		}
		set[value] = true
		if strings.HasPrefix(value, "@") {
			set[strings.TrimPrefix(value, "@")] = true
		}
	}

	if mentionName, ok := member["mention_name"].(string); ok {
		add(mentionName)
	}
	if name, ok := member["name"].(string); ok {
		add(name)
	}
	if profile, ok := member["profile"].(map[string]interface{}); ok {
		if mentionName, ok := profile["mention_name"].(string); ok {
			add(mentionName)
		}
		if name, ok := profile["name"].(string); ok {
			add(name)
		}
	}
}

// resolveShortcutToken finds the API token for the given workspace label.
func (s *Server) resolveShortcutToken(workspace string) string {
	s.mu.RLock()
	cfg := s.hubCfg
	s.mu.RUnlock()
	if cfg.Integrations == nil {
		return ""
	}
	for _, sc := range cfg.Integrations.Shortcut {
		if workspace == "" || strings.EqualFold(sc.Workspace, workspace) {
			return sc.Token
		}
	}
	return ""
}

// buildShortcutStateMap fetches the full workflow list once and returns a map of
// state ID → state name. Callers cache the result per token so a single webhook
// event with N factories only hits the Shortcut API once per workspace.
func buildShortcutStateMap(baseURL, token string) map[int64]string {
	m := map[int64]string{}
	resp, err := shortcutAPIList(baseURL, "workflows", token)
	if err != nil {
		return m
	}
	for _, wf := range resp {
		workflow, _ := wf.(map[string]interface{})
		states, _ := workflow["states"].([]interface{})
		for _, st := range states {
			state, _ := st.(map[string]interface{})
			idF, _ := state["id"].(float64)
			name, _ := state["name"].(string)
			if name != "" {
				m[int64(idF)] = name
			}
		}
	}
	return m
}

// createClawForShortcutStory provisions a claw for a Shortcut story.
func (s *Server) createClawForShortcutStory(factory *types.FactoryConfig, action shortcutAction, storyID, token string, reason string) error {
	triggerKey := factoryTriggerKey("shortcut", storyID)

	claimed, err := s.claimFactoryTrigger(factory.Name, "shortcut", triggerKey, reason, map[string]string{
		"story_id": storyID,
		"source":   reason,
	})
	if err != nil {
		return fmt.Errorf("claim factory trigger: %w", err)
	}
	if !claimed {
		if reason != "poll" {
			log.Printf("[factory:%s] Shortcut story %s already has an active trigger claim — treating as idempotent success", factory.Name, storyID)
		}
		return errFactoryTriggerAlreadyClaimed
	}
	claimOpen := true
	defer func() {
		if claimOpen {
			s.failFactoryTrigger(factory.Name, "shortcut", triggerKey)
		}
	}()

	// Verify we can read the story before spending money on a sandbox.
	// Non-negotiable: if the story is unreadable, we can't do any work.
	if _, err := shortcutAPI(s.resolveShortcutBaseURL(), fmt.Sprintf("stories/%s", storyID), token); err != nil {
		return fmt.Errorf("cannot read story %s from Shortcut (check token/workspace access): %w", storyID, err)
	}
	log.Printf("[factory:%s] verified story %s is readable", factory.Name, storyID)

	templateFiles, err := s.resolveTemplateFiles(factory.Template)
	if err != nil {
		return fmt.Errorf("template %q not found: %w", factory.Template, err)
	}

	// Parse elasticclaw-config.yaml from the template files (if present) so we
	// honour provider settings like nix, github repos, default_model, instance_type, etc.
	var tmplCfg *types.TemplateConfig
	if cfgContent, ok := templateFiles["elasticclaw-config.yaml"]; ok {
		var parseErr error
		tmplCfg, parseErr = config.ParseTemplateConfig([]byte(cfgContent))
		if parseErr != nil {
			log.Printf("[factory:%s] warning: could not parse elasticclaw-config.yaml from template %q: %v", factory.Name, factory.Template, parseErr)
			// Non-fatal — continue with defaults
			tmplCfg = nil
		}
	}

	// Inject BOOTSTRAP.md and CONTEXT.md
	ctx := buildShortcutContext(action, storyID)
	templateFiles["BOOTSTRAP.md"] = ctx
	templateFiles["CONTEXT.md"] = ctx

	clawName := storyID
	if factory.NamePattern != "" {
		clawName = strings.ReplaceAll(factory.NamePattern, "{issue_id}", storyID)
	}

	var tenantID string
	if err := s.db.QueryRow(`SELECT id FROM tenants LIMIT 1`).Scan(&tenantID); err != nil {
		return fmt.Errorf("no tenant: %w", err)
	}

	// Find provider: factory override > template config > hub default
	provider, err := resolveProvider(factory, tmplCfg, s.defaultProvider())
	if err != nil {
		return err
	}

	s.mu.RLock()
	provCfg, _ := s.hubCfg.Providers[provider]
	clawToken := s.hubCfg.ClawToken
	s.mu.RUnlock()

	env := map[string]string{
		"ELASTICCLAW_HUB_URL":    s.clawHubURL(),
		"ELASTICCLAW_CLAW_TOKEN": clawToken,
	}
	if token != "" {
		env["SHORTCUT_API_KEY"] = token
	}

	// Resolve and inject template-requested secrets (typed refs + legacy)
	if tmplCfg != nil && len(tmplCfg.Secrets) > 0 {
		log.Printf("[factory:%s] DEPRECATED: template %q uses 'secrets:' list — migrate to 'secret_refs:' map", factory.Name, factory.Template)
		for _, ref := range tmplCfg.Secrets {
			val, envName, ok := s.resolveSecretRef(ref, factory)
			if ok {
				env[envName] = val
				log.Printf("[factory:%s] injected template secret %s as %s into claw env", factory.Name, ref.Type, envName)
			} else {
				log.Printf("[factory:%s] warning: requested secret (type=%s name=%s workspace=%s) not found", factory.Name, ref.Type, ref.Name, ref.Workspace)
			}
		}
	}

	// Resolve and inject template-level secret_refs
	if tmplCfg != nil && len(tmplCfg.SecretRefs) > 0 {
		for envName, secretRef := range tmplCfg.SecretRefs {
			if val, ok := s.hubCfg.Secrets[secretRef]; ok {
				env[envName] = val
				log.Printf("[factory:%s] injected template secret_ref %s as %s into claw env", factory.Name, secretRef, envName)
			} else {
				log.Printf("[factory:%s] WARNING: template secret_ref %q not found in hub secrets", factory.Name, secretRef)
			}
		}
	}

	// Resolve and inject factory-level secret_refs (factory overrides template)
	if len(factory.SecretRefs) > 0 {
		for envName, secretRef := range factory.SecretRefs {
			if val, ok := s.hubCfg.Secrets[secretRef]; ok {
				env[envName] = val
				log.Printf("[factory:%s] injected factory secret_ref %s as %s into claw env", factory.Name, secretRef, envName)
			} else {
				log.Printf("[factory:%s] WARNING: secret_ref %q not found in hub secrets", factory.Name, secretRef)
			}
		}
	}
	templateFiles = injectFigmaAPIDocs(templateFiles, env)

	// Resolve template config fields (from elasticclaw-config.yaml if present).
	// Factory-level overrides (color, tags) take precedence over template config.
	var (
		instanceType    string
		defaultModel    string
		llmKey          string
		nixEnabled      int
		dockerEnabled   int
		githubRepos     []types.GitHubRepoAccess
		linearWorkspace string
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
	}
	// Resolve default model: template > llm_key lookup > hub default
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
	tagsJSON, _ := json.Marshal(tags)

	// Color: factory overrides template config
	clawColor := factory.Color
	if clawColor == "" && tmplCfg != nil {
		clawColor = tmplCfg.Color
	}

	githubReposJSON, _ := json.Marshal(githubRepos)

	// Check concurrency limit — serialize with promoteMu to prevent TOCTOU
	// race where concurrent factory webhooks both read active < max and both
	// insert as provisioning, exceeding the limit.
	s.promoteMu.Lock()

	s.mu.RLock()
	groupName, groupLimit := s.resolveGroupLimit(factory)
	s.mu.RUnlock()

	activeCount := s.countActiveClawsInGroup(groupName)
	isPending := false
	if groupLimit > 0 && activeCount >= groupLimit {
		isPending = true
		log.Printf("[factory] concurrency limit reached for group %q (active=%d, limit=%d) — queueing claw for Shortcut story %s as pending", groupName, activeCount, groupLimit, storyID)
	}

	clawID := uuid.New().String()
	filesJSON, _ := json.Marshal(templateFiles)
	now := now()

	initialStatus := "provisioning"
	if isPending {
		initialStatus = "pending"
	}

	_, err = s.db.Exec(`
		INSERT INTO claws(id, tenant_id, name, template, provider, default_model, template_files, github_repos, linear_workspace, nix, docker, tags, color, llm_key, shortcut_story_id, status, created_at, factory_name, concurrency_group)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		clawID, tenantID, clawName, factory.Template, provider, defaultModel, string(filesJSON),
		string(githubReposJSON), linearWorkspace, nixEnabled, dockerEnabled, string(tagsJSON), clawColor, llmKey, storyID, initialStatus, now, factory.Name, groupName,
	)

	// Release promoteMu immediately after INSERT so we don't hold it across
	// the potentially slow async provisioning below.
	s.promoteMu.Unlock()

	if err != nil {
		return fmt.Errorf("db insert: %w", err)
	}
	if err := s.completeFactoryTrigger(factory.Name, "shortcut", triggerKey, clawID); err != nil {
		_, _ = s.db.Exec(`UPDATE claws SET status='deleted' WHERE id=?`, clawID)
		return fmt.Errorf("complete factory trigger: %w", err)
	}
	claimOpen = false

	log.Printf("[factory] created claw %s (%s) for Shortcut story %s (status=%s, reason=%s)", clawName, clawID[:8], storyID, initialStatus, reason)
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type:    "claw_status",
		Payload: map[string]string{"claw_id": clawID, "status": initialStatus},
	})

	// Move the story to WorkingStatus if configured (only if not pending —
	// a queued claw hasn't actually started working yet)
	if !isPending && factory.WorkingStatus != "" {
		if token != "" {
			if err := moveShortcutStory(s.resolveShortcutBaseURL(), token, storyID, factory.WorkingStatus); err != nil {
				log.Printf("[factory] failed to move story %s to working status '%s': %v", storyID, factory.WorkingStatus, err)
			} else {
				log.Printf("[factory] moved story %s to working status '%s'", storyID, factory.WorkingStatus)
			}
		}
	}

	if isPending {
		return nil
	}

	req := types.CreateClawRequest{
		Name: clawName, TemplateName: factory.Template,
		Provider: provider, Files: templateFiles, Env: env,
		InstanceType: instanceType,
		ProviderName: "ec-" + clawID[:8],
	}

	go func() {
		var currentStatus string
		_ = s.db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&currentStatus)
		if currentStatus == "deleted" {
			return
		}
		fileBytes := make(map[string][]byte, len(templateFiles))
		for k, v := range templateFiles {
			fileBytes[k] = []byte(v)
		}
		var provErr error
		switch provider {
		case "replicated":
			provErr = s.provisionReplicated(context.Background(), clawID, req, provCfg, env)
		case "daytona":
			provErr = s.provisionDaytona(context.Background(), clawID, req, provCfg, fileBytes, env)
		case "exedev":
			provErr = s.provisionExedev(context.Background(), clawID, req, provCfg, fileBytes, env)
		default:
			provErr = fmt.Errorf("unsupported provider: %s", provider)
		}
		if provErr != nil {
			log.Printf("[factory:%s] provision failed for %s: %v", factory.Name, clawID[:8], provErr)
			s.stopAgentWithReason(clawID, fmt.Sprintf("Factory provision failed: %v", provErr), false)
		}
	}()

	return nil
}

func (s *Server) createClawForShortcutWorkflow(workspace *types.WorkspaceConfig, workflow *types.WorkflowConfig, action shortcutAction, storyID, reason string) error {
	var existing string
	_ = s.db.QueryRow(`SELECT id FROM claws WHERE shortcut_story_id=? AND status NOT IN ('error','deleted') LIMIT 1`, storyID).Scan(&existing)
	if existing != "" {
		log.Printf("[workflow:%s/%s] Shortcut story %s already has active claw %s", workspace.Name, workflow.Name, storyID, existing[:8])
		return nil
	}

	triggerKey := factoryTriggerKey("shortcut", storyID)
	triggerOwner := fmt.Sprintf("workflow:%s/%s", workspace.Name, workflow.Name)
	claimed, err := s.claimFactoryTrigger(triggerOwner, "shortcut", triggerKey, reason, map[string]string{
		"story_id":  storyID,
		"source":    reason,
		"workspace": workspace.Name,
		"workflow":  workflow.Name,
	})
	if err != nil {
		return fmt.Errorf("claim workflow trigger: %w", err)
	}
	if !claimed {
		log.Printf("[workflow:%s/%s] Shortcut story %s already has an active trigger claim", workspace.Name, workflow.Name, storyID)
		return nil
	}
	claimOpen := true
	defer func() {
		if claimOpen {
			s.failFactoryTrigger(triggerOwner, "shortcut", triggerKey)
		}
	}()

	templateFiles := cloneStringMap(workspace.Files)
	templateFiles["CONTEXT.md"] = buildShortcutContext(action, storyID)

	clawName := storyID
	if workflow.NamePattern != "" {
		clawName = strings.ReplaceAll(workflow.NamePattern, "{issue_id}", storyID)
	}

	clawID, _, err := s.createClawFromWorkflowWithOptions(workspace, workflow, workflowCreateOptions{
		workspaceFiles:  templateFiles,
		clawName:        clawName,
		shortcutStoryID: storyID,
		reason:          reason,
	})
	if err != nil {
		return err
	}
	if err := s.completeFactoryTrigger(triggerOwner, "shortcut", triggerKey, clawID); err != nil {
		_, _ = s.db.Exec(`UPDATE claws SET status='deleted' WHERE id=?`, clawID)
		return fmt.Errorf("complete workflow trigger: %w", err)
	}
	claimOpen = false
	return nil
}

func buildShortcutContext(action shortcutAction, storyID string) string {
	var b strings.Builder
	b.WriteString("# Issue Context\n\n")
	b.WriteString("This agent was automatically created by a factory to work on a Shortcut story.\n\n")
	b.WriteString(fmt.Sprintf("## Story: %s\n\n", storyID))
	b.WriteString(fmt.Sprintf("**Title:** %s\n\n", action.Name))
	if action.AppURL != "" {
		b.WriteString(fmt.Sprintf("**URL:** %s\n\n", action.AppURL))
	}
	if action.Description != "" {
		b.WriteString("## Description\n\n")
		b.WriteString(action.Description)
		b.WriteString("\n")
	}
	b.WriteString("\n---\n\n## Instructions\n\n")
	b.WriteString("1. Read this file fully\n")
	b.WriteString("2. Explore the codebase\n")
	b.WriteString("3. Implement the feature/fix described above\n")
	b.WriteString("4. When complete, send exactly: `[DONE] https://github.com/org/repo/pull/N` (with your PR URL)\n")
	return b.String()
}

// resolveShortcutBaseURL returns the base URL for Shortcut API calls.
// Uses the server's override if set (for testing), otherwise the production endpoint.
func (s *Server) resolveShortcutBaseURL() string {
	if s.shortcutBaseURL != "" {
		return s.shortcutBaseURL
	}
	return "https://api.app.shortcut.com"
}

// shortcutAPI makes a GET request to the Shortcut API.
func shortcutAPI(baseURL, path, token string) (map[string]interface{}, error) {
	req, _ := http.NewRequest("GET", baseURL+"/api/v3/"+path, nil)
	req.Header.Set("Shortcut-Token", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// moveShortcutStory updates a story's workflow state.
func moveShortcutStory(baseURL, token, storyIDStr, stateName string) error {
	// First find the state ID by name
	resp, err := shortcutAPIList(baseURL, "workflows", token)
	if err != nil {
		return fmt.Errorf("list workflows: %w", err)
	}
	var stateID int64
	for _, wf := range resp {
		workflow, _ := wf.(map[string]interface{})
		states, _ := workflow["states"].([]interface{})
		for _, st := range states {
			state, _ := st.(map[string]interface{})
			name, _ := state["name"].(string)
			if strings.EqualFold(name, stateName) {
				idF, _ := state["id"].(float64)
				stateID = int64(idF)
				break
			}
		}
		if stateID != 0 {
			break
		}
	}
	if stateID == 0 {
		return fmt.Errorf("state %q not found", stateName)
	}

	// Parse story ID (sc-123 → 123)
	numStr := strings.TrimPrefix(storyIDStr, "sc-")
	storyNum, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid story id %s", storyIDStr)
	}

	body := fmt.Sprintf(`{"workflow_state_id":%d}`, stateID)
	req, _ := http.NewRequest("PUT",
		fmt.Sprintf("%s/api/v3/stories/%d", baseURL, storyNum),
		strings.NewReader(body))
	req.Header.Set("Shortcut-Token", token)
	req.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp2.Body.Close()
	if resp2.StatusCode >= 400 {
		return fmt.Errorf("shortcut API error %d", resp2.StatusCode)
	}
	return nil
}

// commentShortcutIssue adds a comment to a Shortcut story.
func commentShortcutIssue(baseURL, token, storyIDStr, body string) error {
	// Parse story ID (sc-123 → 123)
	numStr := strings.TrimPrefix(storyIDStr, "sc-")
	storyNum, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid story id %s", storyIDStr)
	}

	commentBody := map[string]interface{}{"text": body}
	b, _ := json.Marshal(commentBody)
	req, _ := http.NewRequest("POST",
		fmt.Sprintf("%s/api/v3/stories/%d/comments", baseURL, storyNum),
		bytes.NewReader(b))
	req.Header.Set("Shortcut-Token", token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("shortcut API error %d", resp.StatusCode)
	}
	return nil
}

func shortcutAPIList(baseURL, path, token string) ([]interface{}, error) {
	req, _ := http.NewRequest("GET", baseURL+"/api/v3/"+path, nil)
	req.Header.Set("Shortcut-Token", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result []interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func toInt64(v interface{}) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	}
	return 0
}
