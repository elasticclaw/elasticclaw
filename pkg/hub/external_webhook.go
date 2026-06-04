package hub

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
)

// externalWebhookPayload is a generic payload for external triggers.
// It supports GitHub release events and generic webhooks.
type externalWebhookPayload struct {
	// Common fields
	EventType string `json:"event_type,omitempty"` // e.g., "released", "published"
	Action    string `json:"action,omitempty"`     // e.g., "released", "created"

	// GitHub release fields
	Release *struct {
		TagName     string `json:"tag_name"`
		Name        string `json:"name"`
		Body        string `json:"body"`
		HTMLURL     string `json:"html_url"`
		Prerelease  bool   `json:"prerelease"`
		Draft       bool   `json:"draft"`
		CreatedAt   string `json:"created_at"`
		PublishedAt string `json:"published_at"`
		Author      struct {
			Login string `json:"login"`
		} `json:"author"`
	} `json:"release,omitempty"`

	// Repository info (GitHub-style)
	Repository struct {
		FullName string `json:"full_name"`
		Name     string `json:"name"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
		HTMLURL string `json:"html_url"`
	} `json:"repository,omitempty"`

	// Sender info
	Sender struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"sender,omitempty"`

	// Generic payload for other webhook sources
	Payload map[string]interface{} `json:"payload,omitempty"`
}

// handleExternalWebhook processes incoming external webhook events.
// This endpoint accepts generic webhooks and GitHub release events.
func (s *Server) handleExternalWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	// Validate signature using X-Hub-Signature-256 header (GitHub-style)
	// or X-External-Signature for generic webhooks
	sig := r.Header.Get("X-Hub-Signature-256")
	if sig == "" {
		sig = r.Header.Get("X-External-Signature")
	}

	// Determine event type from headers
	event := r.Header.Get("X-GitHub-Event")
	if event == "" {
		event = r.Header.Get("X-External-Event")
	}

	// Parse the payload based on event type
	var payload externalWebhookPayload

	switch event {
	case "release":
		var ghReleasePayload struct {
			Action  string `json:"action"`
			Release struct {
				TagName     string `json:"tag_name"`
				Name        string `json:"name"`
				Body        string `json:"body"`
				HTMLURL     string `json:"html_url"`
				Prerelease  bool   `json:"prerelease"`
				Draft       bool   `json:"draft"`
				CreatedAt   string `json:"created_at"`
				PublishedAt string `json:"published_at"`
				Author      struct {
					Login string `json:"login"`
				} `json:"author"`
			} `json:"release"`
			Repository struct {
				FullName string `json:"full_name"`
				Name     string `json:"name"`
				Owner    struct {
					Login string `json:"login"`
				} `json:"owner"`
				HTMLURL string `json:"html_url"`
			} `json:"repository"`
			Sender struct {
				Login string `json:"login"`
				Type  string `json:"type"`
			} `json:"sender"`
		}
		if err := json.Unmarshal(body, &ghReleasePayload); err != nil {
			log.Printf("[external-webhook] failed to parse release payload: %v", err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		payload.EventType = ghReleasePayload.Action
		payload.Action = ghReleasePayload.Action
		payload.Release = &ghReleasePayload.Release
		payload.Repository = ghReleasePayload.Repository
		payload.Sender = ghReleasePayload.Sender
		log.Printf("[external-webhook] release event: action=%q repo=%q tag=%q",
			ghReleasePayload.Action, ghReleasePayload.Repository.FullName, ghReleasePayload.Release.TagName)

	case "ping":
		log.Printf("[external-webhook] ping received — webhook configured correctly")
		w.WriteHeader(http.StatusOK)
		return

	case "":
		// Generic webhook - try to parse as our standard payload format
		if err := json.Unmarshal(body, &payload); err != nil {
			log.Printf("[external-webhook] failed to parse generic payload: %v", err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		log.Printf("[external-webhook] generic event: type=%q repo=%q",
			payload.EventType, payload.Repository.FullName)

	default:
		log.Printf("[external-webhook] unsupported event type %q — ignored", event)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Dedup: ignore duplicate webhook deliveries using a fingerprint
	deliveryID := r.Header.Get("X-GitHub-Delivery")
	if deliveryID == "" {
		deliveryID = r.Header.Get("X-External-Delivery")
	}
	if deliveryID != "" {
		fingerprint := fmt.Sprintf("external:%s:%s:%s", deliveryID, payload.Repository.FullName, payload.EventType)
		if s.isDuplicateWebhook(fingerprint) {
			log.Printf("[external-webhook] dedup: skipping duplicate delivery %s", deliveryID)
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	go s.processExternalEvent(payload, body, sig)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) validateExternalSignatureForFactory(factory *types.FactoryConfig, body []byte, sig string) bool {
	sig = strings.TrimPrefix(sig, "sha256=")

	s.mu.RLock()
	secrets := s.hubCfg.Secrets
	s.mu.RUnlock()

	secret := factory.WebhookSecret
	if secret == "" && factory.WebhookSecretRef != "" && secrets != nil {
		secret = secrets[factory.WebhookSecretRef]
	}
	if secret == "" {
		return sig == ""
	}
	if sig == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(sig), []byte(expected))
}

// processExternalEvent finds matching factories and creates claws for external events.
func (s *Server) processExternalEvent(payload externalWebhookPayload, body []byte, sig string) {
	factories := s.resolveFactories()
	if len(factories) == 0 {
		return
	}

	repoFullName := payload.Repository.FullName
	eventType := payload.EventType
	if eventType == "" {
		eventType = payload.Action
	}

	log.Printf("[external-webhook] processing event: type=%q repo=%q — checking %d factories",
		eventType, repoFullName, len(factories))

	for _, factory := range factories {
		if factory.Integration != "external" {
			continue
		}
		if factory.Enabled != nil && !*factory.Enabled {
			log.Printf("[external-webhook] factory %q: skipped (disabled)", factory.Name)
			continue
		}
		if factory.ExternalTrigger == nil {
			log.Printf("[external-webhook] factory %q: skipped (no external_trigger configured)", factory.Name)
			continue
		}
		if !s.validateExternalSignatureForFactory(factory, body, sig) {
			log.Printf("[external-webhook] factory %q: skipped (signature mismatch)", factory.Name)
			continue
		}

		trigger := factory.ExternalTrigger

		// Check source filter
		if trigger.Source != "" {
			switch trigger.Source {
			case "github-release":
				// Only trigger on actual releases, not drafts
				// "created" fires when a draft is saved - exclude it to avoid premature triggers
				if eventType != "released" && eventType != "published" {
					log.Printf("[external-webhook] factory %q: skipped (event type %q not a published release)",
						factory.Name, eventType)
					continue
				}
			case "generic-webhook":
				// Accept any event type for generic webhooks
			default:
				// Unknown source type - skip
				log.Printf("[external-webhook] factory %q: skipped (unknown source %q)",
					factory.Name, trigger.Source)
				continue
			}
		}

		// Check filter criteria
		if trigger.Filter != nil {
			f := trigger.Filter

			// Repository filter
			if f.Repository != "" && !strings.EqualFold(f.Repository, repoFullName) {
				log.Printf("[external-webhook] factory %q: skipped (repo mismatch: want %q, got %q)",
					factory.Name, f.Repository, repoFullName)
				continue
			}

			// Event type filter
			if f.EventType != "" && !strings.EqualFold(f.EventType, eventType) {
				log.Printf("[external-webhook] factory %q: skipped (event type mismatch: want %q, got %q)",
					factory.Name, f.EventType, eventType)
				continue
			}

			// Tag pattern filter (for releases)
			if f.TagPattern != "" && payload.Release != nil {
				if !matchGlob(f.TagPattern, payload.Release.TagName) {
					log.Printf("[external-webhook] factory %q: skipped (tag pattern mismatch: want %q, got %q)",
						factory.Name, f.TagPattern, payload.Release.TagName)
					continue
				}
			}
		}

		// Build trigger key for atomic claim
		triggerKey := fmt.Sprintf("%s:%s@%s", factory.Name, repoFullName, eventType)
		if payload.Release != nil {
			triggerKey = fmt.Sprintf("%s:%s@%s", factory.Name, repoFullName, payload.Release.TagName)
		}

		s.processExternalFactoryTrigger(factory, payload, triggerKey, repoFullName, eventType)
	}
}

func (s *Server) processExternalFactoryTrigger(factory *types.FactoryConfig, payload externalWebhookPayload, triggerKey, repoFullName, eventType string) {
	// Atomically claim the trigger via factory_triggers (prevents concurrent duplicate creation).
	claimed, err := s.claimFactoryTrigger(factory.Name, "external", triggerKey, "webhook", map[string]string{
		"repository": repoFullName,
		"event_type": eventType,
	})
	if err != nil {
		log.Printf("[external-webhook] factory %q: failed to claim trigger for %s: %v",
			factory.Name, triggerKey, err)
		s.logFactoryEvent(factory.Name, triggerKey, fmt.Sprintf("External event: %s", eventType),
			"", eventType, "error", "", err.Error())
		return
	}
	if !claimed {
		log.Printf("[external-webhook] factory %q: trigger %s already claimed — treating as idempotent success",
			factory.Name, triggerKey)
		return
	}
	claimOpen := true
	defer func() {
		if claimOpen {
			s.failFactoryTrigger(factory.Name, "external", triggerKey)
		}
	}()

	// Create claw
	log.Printf("[external-webhook] factory %q: creating claw for %s (event=%s)",
		factory.Name, triggerKey, eventType)
	clawID, err := s.createClawForExternalEvent(factory, payload, triggerKey)
	if err != nil {
		log.Printf("[external-webhook] factory %q: failed to create claw for %s: %v",
			factory.Name, triggerKey, err)
		s.logFactoryEvent(factory.Name, triggerKey, fmt.Sprintf("External event: %s", eventType),
			"", eventType, "error", "", err.Error())
		return
	}

	if err := s.completeFactoryTrigger(factory.Name, "external", triggerKey, clawID); err != nil {
		_, _ = s.db.Exec(`UPDATE claws SET status='deleted' WHERE id=?`, clawID)
		log.Printf("[external-webhook] factory %q: failed to complete trigger for %s: %v",
			factory.Name, triggerKey, err)
		s.logFactoryEvent(factory.Name, triggerKey, fmt.Sprintf("External event: %s", eventType),
			"", eventType, "error", "", err.Error())
		return
	}
	claimOpen = false

	s.logFactoryEvent(factory.Name, triggerKey, fmt.Sprintf("External event: %s", eventType),
		"", eventType, "claw_created", clawID, "")
}

// matchGlob performs simple glob matching (only * wildcard supported).
func matchGlob(pattern, s string) bool {
	// Convert glob pattern to a simple matcher
	// * matches any sequence of characters
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}

	// Pattern has wildcards
	pos := 0
	for i, part := range parts {
		if i == 0 && part != "" {
			// First part must match at start
			if !strings.HasPrefix(s, part) {
				return false
			}
			pos = len(part)
		} else if i == len(parts)-1 && part != "" {
			// Last part must match at end, but not overlap with consumed prefix
			if len(s)-len(part) < pos {
				return false
			}
			if !strings.HasSuffix(s, part) {
				return false
			}
		} else if part != "" {
			// Middle parts must be found in sequence
			idx := strings.Index(s[pos:], part)
			if idx == -1 {
				return false
			}
			pos += idx + len(part)
		}
	}
	return true
}

// createClawForExternalEvent provisions a new claw for an external event.
// The caller must have already claimed the factory trigger atomically.
func (s *Server) createClawForExternalEvent(factory *types.FactoryConfig, payload externalWebhookPayload, triggerID string) (string, error) {
	repoFullName := payload.Repository.FullName
	eventType := payload.EventType
	if eventType == "" {
		eventType = payload.Action
	}

	// Resolve template files
	templateFiles, err := s.resolveTemplateFiles(factory.Template)
	if err != nil {
		return "", fmt.Errorf("template %q not found: %w", factory.Template, err)
	}

	// Parse elasticclaw-config.yaml from the template if present
	var tmplCfg *types.TemplateConfig
	if cfgContent, ok := templateFiles["elasticclaw-config.yaml"]; ok {
		if parsed, parseErr := config.ParseTemplateConfig([]byte(cfgContent)); parseErr == nil {
			tmplCfg = parsed
		}
	}

	// Build context for the external event
	ctxContent := buildExternalEventContext(payload, factory)
	templateFiles["CONTEXT.md"] = ctxContent

	// Store trigger inputs as JSON
	inputs := map[string]string{
		"event_type": eventType,
		"repository": repoFullName,
		"trigger_id": triggerID,
	}
	if payload.Release != nil {
		inputs["release_tag"] = payload.Release.TagName
		inputs["release_name"] = payload.Release.Name
		inputs["release_url"] = payload.Release.HTMLURL
	}
	inputsJSON, _ := json.Marshal(inputs)
	templateFiles["TRIGGER_INPUTS.json"] = string(inputsJSON)

	// Determine claw name
	clawName := factory.Name
	if factory.NamePattern != "" {
		clawName = factory.NamePattern
		clawName = strings.ReplaceAll(clawName, "{repo}", path.Base(repoFullName))
		clawName = strings.ReplaceAll(clawName, "{event_type}", eventType)
		if payload.Release != nil {
			clawName = strings.ReplaceAll(clawName, "{tag}", payload.Release.TagName)
			clawName = strings.ReplaceAll(clawName, "{release_name}", payload.Release.Name)
		}
	} else if payload.Release != nil {
		clawName = fmt.Sprintf("%s-%s", path.Base(repoFullName), payload.Release.TagName)
	}

	// Find tenant
	var tenantID string
	if err := s.db.QueryRow(`SELECT id FROM tenants LIMIT 1`).Scan(&tenantID); err != nil {
		return "", fmt.Errorf("no tenant: %w", err)
	}

	// Read all hub config values under lock to avoid data races
	s.mu.RLock()
	clawToken := s.hubCfg.ClawToken
	secrets := s.hubCfg.Secrets
	providers := s.hubCfg.Providers
	llmKeys := s.hubCfg.LLMKeys
	defaultModelHub := s.hubCfg.DefaultModel
	integrations := s.hubCfg.Integrations

	// Resolve provider (must be done under lock as it accesses s.hubCfg.Providers)
	provider := factory.Provider
	if provider == "" && tmplCfg != nil && tmplCfg.Provider != "" {
		provider = tmplCfg.Provider
	}
	if provider == "" {
		// Inline defaultProvider logic to avoid calling method outside lock
		for name, p := range providers {
			if p.Token != "" || p.APIKey != "" || p.AccessToken != "" {
				provider = name
				break
			}
		}
	}
	s.mu.RUnlock()

	if provider == "" {
		return "", fmt.Errorf("no provider configured")
	}

	// Build env vars
	env := map[string]string{
		"ELASTICCLAW_HUB_URL":    s.clawHubURL(),
		"ELASTICCLAW_CLAW_TOKEN": clawToken,
		"EXTERNAL_EVENT_TYPE":    eventType,
		"EXTERNAL_REPOSITORY":    repoFullName,
		"EXTERNAL_TRIGGER_ID":    triggerID,
	}
	if payload.Release != nil {
		env["EXTERNAL_RELEASE_TAG"] = payload.Release.TagName
		env["EXTERNAL_RELEASE_NAME"] = payload.Release.Name
		env["EXTERNAL_RELEASE_URL"] = payload.Release.HTMLURL
	}

	// Resolve and inject template-requested secrets
	if tmplCfg != nil && len(tmplCfg.Secrets) > 0 {
		log.Printf("[factory:%s] DEPRECATED: template %q uses 'secrets:' list — migrate to 'secret_refs:' map",
			factory.Name, factory.Template)
		for _, ref := range tmplCfg.Secrets {
			val, envName, ok := resolveSecretRefLocal(integrations, ref, factory)
			if ok {
				env[envName] = val
				log.Printf("[factory:%s] injected template secret %s as %s into claw env",
					factory.Name, ref.Type, envName)
			}
		}
	}

	// Resolve and inject template-level secret_refs
	if tmplCfg != nil && len(tmplCfg.SecretRefs) > 0 {
		for envName, secretRef := range tmplCfg.SecretRefs {
			if val, ok := secrets[secretRef]; ok {
				env[envName] = val
				log.Printf("[factory:%s] injected template secret_ref %s as %s into claw env",
					factory.Name, secretRef, envName)
			}
		}
	}

	// Resolve and inject factory-level secret_refs
	if len(factory.SecretRefs) > 0 {
		for envName, secretRef := range factory.SecretRefs {
			if val, ok := secrets[secretRef]; ok {
				env[envName] = val
				log.Printf("[factory:%s] injected factory secret_ref %s as %s into claw env",
					factory.Name, secretRef, envName)
			}
		}
	}
	templateFiles = injectFigmaAPIDocs(templateFiles, env)

	// Resolve template config fields
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

	// Resolve default model
	if defaultModel == "" && llmKey != "" {
		for _, k := range llmKeys {
			if k.Name == llmKey {
				defaultModel = resolveDefaultModelForKeyLocal(defaultModelHub, k)
				break
			}
		}
	}
	if defaultModel == "" {
		defaultModel = defaultModelHub
	}

	// Build tags
	tags := mergeTags(factory.Template, factory.Tags, nil)
	hasFactory := false
	for _, t := range tags {
		if t == "factory:"+factory.Name {
			hasFactory = true
			break
		}
	}
	if !hasFactory {
		tags = append(tags, "factory:"+factory.Name)
	}
	// Add external trigger tag
	tags = append(tags, fmt.Sprintf("external_trigger:%s", triggerID))
	tagsJSON, _ := json.Marshal(tags)

	clawColor := factory.Color
	if clawColor == "" && tmplCfg != nil {
		clawColor = tmplCfg.Color
	}

	githubReposJSON, _ := json.Marshal(githubRepos)

	// Insert claw record
	clawID := uuid.New().String()
	filesJSON, _ := json.Marshal(templateFiles)
	createdAt := now()

	// Check concurrency limit
	s.promoteMu.Lock()

	s.mu.RLock()
	groupName, groupLimit := s.resolveGroupLimit(factory)
	s.mu.RUnlock()

	activeCount := s.countActiveClawsInGroup(groupName)
	isPending := false
	if groupLimit > 0 && activeCount >= groupLimit {
		isPending = true
		log.Printf("[factory] concurrency limit reached for group %q (active=%d, limit=%d) — queueing claw for external trigger %s as pending",
			groupName, activeCount, groupLimit, triggerID)
	}

	initialStatus := "provisioning"
	if isPending {
		initialStatus = "pending"
	}

	_, err = s.db.Exec(`
		INSERT INTO claws(id, tenant_id, name, template, provider, default_model, template_files, github_repos, linear_workspace, nix, docker, tags, color, llm_key, external_trigger_id, status, created_at, factory_name, concurrency_group)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		clawID, tenantID, clawName, factory.Template, provider, defaultModel, string(filesJSON),
		string(githubReposJSON), linearWorkspace, nixEnabled, dockerEnabled, string(tagsJSON), clawColor, llmKey, triggerID, initialStatus, createdAt, factory.Name, groupName,
	)

	s.promoteMu.Unlock()

	if err != nil {
		return "", fmt.Errorf("db insert: %w", err)
	}

	log.Printf("[factory] created claw %s (%s) for external trigger %s (status=%s)",
		clawName, clawID[:8], triggerID, initialStatus)
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type:    "claw_status",
		Payload: map[string]string{"claw_id": clawID, "status": initialStatus},
	})

	if isPending {
		return clawID, nil
	}

	// Provision asynchronously
	provCfg, _ := providers[provider]
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
		case "exedev":
			provErr = s.provisionExedev(ctx, clawID, req, provCfg, fileBytes, env)
		case "noop":
			if os.Getenv("ELASTICCLAW_NOOP_PROVIDER") == "" {
				provErr = fmt.Errorf("noop provider requires ELASTICCLAW_NOOP_PROVIDER=1")
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

	return clawID, nil
}

// buildExternalEventContext builds the CONTEXT.md content for an external event.
func buildExternalEventContext(payload externalWebhookPayload, factory *types.FactoryConfig) string {
	var b strings.Builder
	b.WriteString("# External Event Context\n\n")
	b.WriteString("This agent was automatically created by a factory in response to an external event.\n\n")

	if payload.Release != nil {
		b.WriteString("## Release Event\n\n")
		b.WriteString(fmt.Sprintf("**Repository:** %s\n", payload.Repository.FullName))
		b.WriteString(fmt.Sprintf("**Tag:** %s\n", payload.Release.TagName))
		b.WriteString(fmt.Sprintf("**Release Name:** %s\n", payload.Release.Name))
		b.WriteString(fmt.Sprintf("**Release URL:** %s\n", payload.Release.HTMLURL))
		b.WriteString(fmt.Sprintf("**Author:** %s\n", payload.Release.Author.Login))
		if payload.Release.Prerelease {
			b.WriteString("**Type:** Pre-release\n")
		} else if payload.Release.Draft {
			b.WriteString("**Type:** Draft\n")
		} else {
			b.WriteString("**Type:** Stable Release\n")
		}
		if payload.Release.Body != "" {
			b.WriteString("\n## Release Notes\n\n")
			b.WriteString(payload.Release.Body)
			b.WriteString("\n")
		}
	} else {
		b.WriteString("## External Event\n\n")
		b.WriteString(fmt.Sprintf("**Repository:** %s\n", payload.Repository.FullName))
		b.WriteString(fmt.Sprintf("**Event Type:** %s\n", payload.EventType))
		if payload.Sender.Login != "" {
			b.WriteString(fmt.Sprintf("**Triggered by:** %s\n", payload.Sender.Login))
		}
	}

	b.WriteString("\n## Factory Configuration\n\n")
	b.WriteString(fmt.Sprintf("**Factory:** %s\n", factory.Name))
	if factory.ExternalTrigger != nil {
		b.WriteString(fmt.Sprintf("**Trigger Source:** %s\n", factory.ExternalTrigger.Source))
		if factory.ExternalTrigger.Filter != nil {
			if factory.ExternalTrigger.Filter.Repository != "" {
				b.WriteString(fmt.Sprintf("**Filter Repository:** %s\n", factory.ExternalTrigger.Filter.Repository))
			}
			if factory.ExternalTrigger.Filter.EventType != "" {
				b.WriteString(fmt.Sprintf("**Filter Event Type:** %s\n", factory.ExternalTrigger.Filter.EventType))
			}
			if factory.ExternalTrigger.Filter.TagPattern != "" {
				b.WriteString(fmt.Sprintf("**Filter Tag Pattern:** %s\n", factory.ExternalTrigger.Filter.TagPattern))
			}
		}
	}

	b.WriteString("\n## Instructions\n\n")
	b.WriteString("1. Read this file fully to understand the external event context\n")
	b.WriteString("2. Explore the codebase and identify what needs to be updated\n")
	b.WriteString("3. Implement the necessary changes to handle this external event\n")
	b.WriteString("4. When complete, send exactly: `[DONE] https://github.com/org/repo/pull/N` (with your PR URL)\n")

	return b.String()
}

// resolveDefaultModelForKeyLocal returns the effective model for a given LLM key.
// This is a lock-free version that takes the hub's default model as a parameter.
func resolveDefaultModelForKeyLocal(hubDefaultModel string, key *types.LLMKeyConfig) string {
	if key == nil {
		return hubDefaultModel
	}

	// Use per-key default model if set; normalize to include provider prefix
	if key.DefaultModel != "" {
		prefix := key.Provider + "/"
		if !strings.HasPrefix(key.DefaultModel, prefix) {
			return prefix + key.DefaultModel
		}
		return key.DefaultModel
	}

	// Check if hub's DefaultModel matches this key's provider
	if hubDefaultModel != "" && strings.HasPrefix(hubDefaultModel, key.Provider+"/") {
		return hubDefaultModel
	}

	// Construct a provider-specific default model
	switch key.Provider {
	case "anthropic":
		return "anthropic/claude-sonnet-4-6"
	case "fireworks":
		return "fireworks/accounts/fireworks/models/kimi-k2p6"
	case "moonshot":
		return "moonshot/moonshot-v1-8k"
	default:
		return hubDefaultModel
	}
}

// resolveSecretRefLocal resolves a typed SecretRef to its value and env var name.
// This is a lock-free version that takes integrations as a parameter.
func resolveSecretRefLocal(integrations *types.IntegrationsConfig, ref types.SecretRef, factory *types.FactoryConfig) (string, string, bool) {
	envName := ref.EnvVarName()
	if envName == "" {
		return "", "", false
	}
	switch ref.Type {
	case "linear":
		if integrations == nil {
			return "", envName, false
		}
		ws := ref.Workspace
		if ws == "" && factory != nil {
			ws = factory.Workspace
		}
		for _, li := range integrations.Linear {
			if ws == "" || strings.EqualFold(li.Workspace, ws) {
				return li.Token, envName, true
			}
		}
		return "", envName, false
	case "shortcut":
		if integrations == nil {
			return "", envName, false
		}
		ws := ref.Workspace
		if ws == "" && factory != nil {
			ws = factory.Workspace
		}
		for _, si := range integrations.Shortcut {
			if ws == "" || strings.EqualFold(si.Workspace, ws) {
				return si.Token, envName, true
			}
		}
		return "", envName, false
	case "github-issues":
		if integrations == nil {
			return "", envName, false
		}
		ws := ref.Workspace
		if ws == "" && factory != nil {
			ws = factory.Workspace
		}
		for _, gi := range integrations.GitHubIssues {
			if ws == "" || strings.EqualFold(gi.Workspace, ws) {
				return gi.Token, envName, true
			}
		}
		return "", envName, false
	case "github":
		// GitHub tokens are handled via GitHub App, not integrations
		return "", envName, false
	case "custom":
		// Custom secrets are resolved from the secrets map, not here
		return "", envName, false
	default:
		return "", envName, false
	}
}
