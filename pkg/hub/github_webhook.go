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

	// Verify HMAC-SHA256 signature against any matching factory webhook secret.
	sig := r.Header.Get("X-Hub-Signature-256")
	if !s.validateGitHubSignature(body, sig) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	if event == "" {
		http.Error(w, "missing X-GitHub-Event", http.StatusBadRequest)
		return
	}

	switch event {
	case "pull_request":
		var payload githubPRPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		go s.processGitHubPREvent(payload)
	case "ping":
		// GitHub sends a ping on webhook creation — acknowledge it.
	default:
		// Unsupported event type; ignore gracefully.
	}

	w.WriteHeader(http.StatusOK)
}

// validateGitHubSignature verifies the HMAC-SHA256 signature from GitHub against all
// configured GitHub factory webhook secrets. Returns true if any matches (or no secrets configured).
func (s *Server) validateGitHubSignature(body []byte, sig string) bool {
	// Strip sha256= prefix
	sig = strings.TrimPrefix(sig, "sha256=")

	s.mu.RLock()
	factories := s.hubCfg.Factories
	secrets := s.hubCfg.Secrets
	s.mu.RUnlock()

	hasAnySecret := false

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
		hasAnySecret = true
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		expected := hex.EncodeToString(mac.Sum(nil))
		if hmac.Equal([]byte(sig), []byte(expected)) {
			return true
		}
	}

	// If any secrets are configured but none matched, reject.
	if hasAnySecret {
		return false
	}
	return true // no secrets configured
}

// processGitHubPREvent finds matching factories and creates claws for a PR event.
func (s *Server) processGitHubPREvent(payload githubPRPayload) {
	s.mu.RLock()
	factories := s.hubCfg.Factories
	s.mu.RUnlock()

	repoFullName := payload.Repository.FullName

	for _, factory := range factories {
		if factory.Integration != "github" {
			continue
		}
		if factory.Enabled != nil && !*factory.Enabled {
			continue
		}
		if factory.Trigger == nil {
			continue
		}
		if factory.Trigger.On != "pull_request" {
			continue
		}
		// Check action match
		if factory.Trigger.Action != "" && factory.Trigger.Action != payload.Action {
			continue
		}
		// Check repo match
		if !githubRepoMatches(repoFullName, factory.Repos) {
			continue
		}
		// Check filter
		if factory.Trigger.Filter != nil {
			f := factory.Trigger.Filter
			if f.Author != "" && !strings.EqualFold(f.Author, payload.PullRequest.User.Login) {
				continue
			}
			if f.BaseBranch != "" && !strings.EqualFold(f.BaseBranch, payload.PullRequest.Base.Ref) {
				continue
			}
		}

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
