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
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
)

// githubPRPayload holds the relevant fields from a GitHub pull_request webhook event.
type githubPRPayload struct {
	Action      string `json:"action"` // "opened", "synchronize", "reopened", "closed"
	Number      int    `json:"number"`
	PullRequest struct {
		HTMLURL string `json:"html_url"`
		Title   string `json:"title"`
		User    struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// handleGitHubWebhook processes incoming GitHub webhook events.
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	// Verify HMAC-SHA256 signature against any matching factory webhook secret.
	sig := r.Header.Get("X-Hub-Signature-256")
	if !s.validateGitHubSignature(body, sig) {
		log.Printf("[github-webhook] signature validation failed for event=%q — check webhook secret", event)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	if event == "" {
		http.Error(w, "missing X-GitHub-Event", http.StatusBadRequest)
		return
	}

	switch event {
	case "pull_request":
		var payload githubPRPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			log.Printf("[github-webhook] failed to parse pull_request payload: %v", err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		log.Printf("[github-webhook] pull_request action=%q repo=%q pr=#%d author=%q base=%q",
			payload.Action, payload.Repository.FullName, payload.Number,
			payload.PullRequest.User.Login, payload.PullRequest.Base.Ref)
		go s.processGitHubPREvent(payload)
	case "ping":
		log.Printf("[github-webhook] ping received — webhook configured correctly")
	default:
		log.Printf("[github-webhook] unsupported event type %q — ignored", event)
	}

	w.WriteHeader(http.StatusOK)
}

// validateGitHubSignature verifies the HMAC-SHA256 signature from GitHub against all
// configured GitHub factory webhook secrets. Returns true if any matches.
func (s *Server) validateGitHubSignature(body []byte, sig string) bool {
	// Strip sha256= prefix
	sig = strings.TrimPrefix(sig, "sha256=")
	if sig == "" {
		return false
	}

	s.mu.RLock()
	factories := s.hubCfg.Factories
	secrets := s.hubCfg.Secrets
	s.mu.RUnlock()

	for _, factory := range factories {
		if factory.Integration != "github" {
			continue
		}
		// Resolve secret: inline or via named ref
		secret := factory.WebhookSecret
		if secret == "" && factory.WebhookSecretRef != "" && secrets != nil {
			secret = secrets[factory.WebhookSecretRef]
		}
		if secret == "" {
			continue
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		expected := hex.EncodeToString(mac.Sum(nil))
		if hmac.Equal([]byte(sig), []byte(expected)) {
			return true
		}
	}

	// Reject if no secrets are configured or if none matched.
	return false
}

// processGitHubPREvent finds matching factories and creates claws for a PR event.
func (s *Server) processGitHubPREvent(payload githubPRPayload) {
	s.mu.RLock()
	factories := s.hubCfg.Factories
	s.mu.RUnlock()

	repoFullName := payload.Repository.FullName
	log.Printf("[github-webhook] processing PR event: repo=%q action=%q — checking %d factories", repoFullName, payload.Action, len(factories))

	for _, factory := range factories {
		if factory.Integration != "github" {
			continue
		}
		if factory.Enabled != nil && !*factory.Enabled {
			log.Printf("[github-webhook] factory %q: skipped (disabled)", factory.Name)
			continue
		}
		if factory.Trigger == nil {
			log.Printf("[github-webhook] factory %q: skipped (no trigger configured)", factory.Name)
			continue
		}
		if factory.Trigger.On != "pull_request" {
			log.Printf("[github-webhook] factory %q: skipped (trigger.on=%q, not pull_request)", factory.Name, factory.Trigger.On)
			continue
		}
		// Check repo match
		if !githubRepoMatches(repoFullName, factory.Repos) {
			log.Printf("[github-webhook] factory %q: skipped (repo %q not in %v)", factory.Name, repoFullName, factory.Repos)
			continue
		}
		// Check filter
		if factory.Trigger.Filter != nil {
			f := factory.Trigger.Filter
			if f.Author != "" && !strings.EqualFold(f.Author, payload.PullRequest.User.Login) {
				log.Printf("[github-webhook] factory %q: skipped (author mismatch: want %q, got %q)", factory.Name, f.Author, payload.PullRequest.User.Login)
				continue
			}
			if f.BaseBranch != "" && !strings.EqualFold(f.BaseBranch, payload.PullRequest.Base.Ref) {
				log.Printf("[github-webhook] factory %q: skipped (base_branch mismatch: want %q, got %q)", factory.Name, f.BaseBranch, payload.PullRequest.Base.Ref)
				continue
			}
		}

		// Check if a claw already exists for this PR
		existingClawID := s.findClawForGitHubPR(payload.PullRequest.HTMLURL)
		if existingClawID != "" {
			switch payload.Action {
			case "closed":
				// Let the pr_watcher handle merged/closed — don't double-inject
			case "synchronize", "reopened":
				// Only inject if the claw is connected and ready — otherwise the
				// claw hasn't started yet and the update is redundant/lost.
				s.mu.RLock()
				cc, connected := s.claws[existingClawID]
				ready := connected && cc.gatewayReady
				s.mu.RUnlock()
				if !ready {
					log.Printf("[factory:%s] github PR #%d action=%s — claw %s not ready yet, skipping inject", factory.Name, payload.Number, payload.Action, existingClawID[:8])
				} else {
					var msg string
					if payload.Action == "synchronize" {
						msg = fmt.Sprintf("PR #%d was updated with new commits. Review the latest changes.", payload.Number)
					} else {
						msg = fmt.Sprintf("PR #%d was reopened.", payload.Number)
					}
					log.Printf("[factory:%s] github PR #%d action=%s — injecting into claw %s", factory.Name, payload.Number, payload.Action, existingClawID[:8])
					s.injectHubMessageByID(existingClawID, msg)
				}
			default:
				log.Printf("[factory:%s] github PR #%d action=%s — claw exists, ignoring", factory.Name, payload.Number, payload.Action)
			}
			continue
		}

		// No existing claw — create one (regardless of action, first event wins)
		log.Printf("[factory:%s] github PR #%d in %s (action=%s) — creating claw", factory.Name, payload.Number, repoFullName, payload.Action)
		if err := s.createClawForGitHubPR(factory, payload); err != nil {
			log.Printf("[factory:%s] failed to create claw for PR #%d: %v", factory.Name, payload.Number, err)
			s.logFactoryEvent(factory.Name, fmt.Sprintf("%s#%d", repoFullName, payload.Number),
				payload.PullRequest.Title, "", payload.Action, "error", "", err.Error())
		}
	}
}

// githubRepoMatches returns true if fullName matches any entry in repos.
// Each entry is either an exact "owner/repo" or a glob "owner/*".
func githubRepoMatches(fullName string, repos []string) bool {
	if len(repos) == 0 {
		return true // no restriction
	}
	parts := strings.SplitN(fullName, "/", 2)
	if len(parts) != 2 {
		return false
	}
	owner := parts[0]
	for _, pattern := range repos {
		if strings.EqualFold(pattern, fullName) {
			return true
		}
		// Glob: owner/*
		if strings.HasSuffix(pattern, "/*") {
			patternOwner := strings.TrimSuffix(pattern, "/*")
			if strings.EqualFold(patternOwner, owner) {
				return true
			}
		}
	}
	return false
}

// createClawForGitHubPR provisions a new claw for a GitHub PR event.
// findClawForGitHubPR returns the claw ID that is already tracking this PR URL, or "".
func (s *Server) findClawForGitHubPR(prURL string) string {
	var clawID string
	_ = s.db.QueryRow(
		`SELECT cp.claw_id FROM claw_prs cp
		 JOIN claws cl ON cl.id = cp.claw_id
		 WHERE cp.pr_url = ? AND cl.status NOT IN ('deleted','error','offline')
		 LIMIT 1`,
		prURL,
	).Scan(&clawID)
	return clawID
}

func (s *Server) createClawForGitHubPR(factory *types.FactoryConfig, pr githubPRPayload) error {
	repoFullName := pr.Repository.FullName
	prNumber := pr.Number
	prURL := pr.PullRequest.HTMLURL

	// Enforce 1:1 — check if a claw already exists for this PR URL.
	var existingID string
	_ = s.db.QueryRow(
		`SELECT c.id FROM claws c
		 JOIN claw_prs cp ON cp.claw_id = c.id
		 WHERE cp.pr_url=? AND c.status NOT IN ('error','deleted') LIMIT 1`,
		prURL,
	).Scan(&existingID)
	if existingID != "" {
		return fmt.Errorf("claw %s already exists for PR %s", existingID[:8], prURL)
	}

	// Resolve template files
	templateFiles, err := s.resolveTemplateFiles(factory.Template)
	if err != nil {
		return fmt.Errorf("template %q not found: %w", factory.Template, err)
	}

	// Parse elasticclaw-config.yaml from the template if present
	var tmplCfg *types.TemplateConfig
	if cfgContent, ok := templateFiles["elasticclaw-config.yaml"]; ok {
		if parsed, parseErr := config.ParseTemplateConfig([]byte(cfgContent)); parseErr == nil {
			tmplCfg = parsed
		}
	}

	// Build BOOTSTRAP context
	prCtx := buildGitHubPRContext(pr)
	if existing, ok := templateFiles["BOOTSTRAP.md"]; ok && existing != "" {
		prCtx = prCtx + "\n\n---\n\n" + existing
	}
	templateFiles["BOOTSTRAP.md"] = prCtx
	templateFiles["CONTEXT.md"] = prCtx

	// Determine claw name: "repo#123" pattern
	repoShort := repoFullName
	if idx := strings.LastIndex(repoFullName, "/"); idx >= 0 {
		repoShort = repoFullName[idx+1:]
	}
	clawName := fmt.Sprintf("%s#%d", repoShort, prNumber)
	if factory.NamePattern != "" {
		clawName = strings.ReplaceAll(factory.NamePattern, "{pr_number}", fmt.Sprintf("%d", prNumber))
		clawName = strings.ReplaceAll(clawName, "{repo}", repoShort)
	}

	// Find tenant
	var tenantID string
	if err := s.db.QueryRow(`SELECT id FROM tenants LIMIT 1`).Scan(&tenantID); err != nil {
		return fmt.Errorf("no tenant: %w", err)
	}

	// Resolve provider
	provider := factory.Provider
	if provider == "" && tmplCfg != nil && tmplCfg.Provider != "" {
		provider = tmplCfg.Provider
	}
	if provider == "" {
		provider = s.defaultProvider()
	}
	if provider == "" {
		return fmt.Errorf("no provider configured")
	}

	// Build env vars
	env := map[string]string{
		"ELASTICCLAW_HUB_URL":    s.clawHubURL(),
		"ELASTICCLAW_CLAW_TOKEN": s.hubCfg.ClawToken,
	}

	// Resolve template config fields
	var (
		instanceType    string
		defaultModel    string
		llmKey          string
		nixEnabled      int
		githubRepos     []types.GitHubRepoAccess
		linearWorkspace string
		autoFixCI       int = 1
		autoFixBugbot   int = 1
	)
	if tmplCfg != nil {
		instanceType = tmplCfg.InstanceType
		defaultModel = tmplCfg.DefaultModel
		llmKey = tmplCfg.LLMKey
		if tmplCfg.Nix {
			nixEnabled = 1
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
	}
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

	// Build tags — include template:<name>, factory:<name>, and github_pr:<url>
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
	// Tag with GitHub PR URL for identification
	tags = append(tags, "github_pr:"+prURL)
	tagsJSON, _ := json.Marshal(tags)

	clawColor := factory.Color
	if clawColor == "" && tmplCfg != nil {
		clawColor = tmplCfg.Color
	}

	githubReposJSON, _ := json.Marshal(githubRepos)

	// Insert claw record (linear_issue_id = "" for GitHub-sourced claws)
	clawID := uuid.New().String()
	filesJSON, _ := json.Marshal(templateFiles)
	createdAt := now()

	_, err = s.db.Exec(`
		INSERT INTO claws(id, tenant_id, name, template, provider, default_model, template_files, github_repos, linear_workspace, nix, tags, color, llm_key, auto_fix_ci, auto_fix_bugbot, linear_issue_id, status, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'provisioning',?)`,
		clawID, tenantID, clawName, factory.Template, provider, defaultModel, string(filesJSON),
		string(githubReposJSON), linearWorkspace, nixEnabled, string(tagsJSON), clawColor, llmKey, autoFixCI, autoFixBugbot, "", createdAt,
	)
	if err != nil {
		return fmt.Errorf("db insert: %w", err)
	}

	// Store the PR immediately so the watcher picks it up
	s.storePRMention(clawID, repoFullName, prNumber, prURL)

	// Log factory event
	s.logFactoryEvent(factory.Name,
		fmt.Sprintf("%s#%d", repoFullName, prNumber),
		pr.PullRequest.Title, "", pr.Action, "claw_created", clawID, "")

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
			_, _ = s.db.Exec(`UPDATE claws SET status='error' WHERE id=? AND status != 'deleted'`, clawID)
		}
	}()

	log.Printf("[factory] created claw %s (%s) for GitHub PR %s#%d", clawName, clawID[:8], repoFullName, prNumber)
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type:    "claw_status",
		Payload: map[string]string{"claw_id": clawID, "status": "provisioning"},
	})

	return nil
}

// buildGitHubPRContext builds the BOOTSTRAP.md context for a GitHub PR assignment.
func buildGitHubPRContext(pr githubPRPayload) string {
	var b strings.Builder
	b.WriteString("## GitHub PR Assignment\n\n")
	b.WriteString(fmt.Sprintf("PR: %s\n", pr.PullRequest.HTMLURL))
	b.WriteString(fmt.Sprintf("Author: %s\n", pr.PullRequest.User.Login))
	b.WriteString(fmt.Sprintf("Branch: %s → %s\n", pr.PullRequest.Head.Ref, pr.PullRequest.Base.Ref))
	b.WriteString(fmt.Sprintf("Title: %s\n", pr.PullRequest.Title))
	b.WriteString(fmt.Sprintf("Repo: %s\n", pr.Repository.FullName))
	return b.String()
}
