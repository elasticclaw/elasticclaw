package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
	"nhooyr.io/websocket/wsjson"
)

var prURLRegex = regexp.MustCompile(`https://github\.com/([a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+)/pull/(\d+)`)

// extractPRs finds GitHub PR URLs in a message body.
func extractPRs(content string) []struct {
	repo   string
	number int
	url    string
} {
	var results []struct {
		repo   string
		number int
		url    string
	}
	seen := map[string]bool{}
	for _, m := range prURLRegex.FindAllStringSubmatch(content, -1) {
		url := m[0]
		if seen[url] {
			continue
		}
		seen[url] = true
		num, _ := strconv.Atoi(m[2])
		results = append(results, struct {
			repo   string
			number int
			url    string
		}{m[1], num, url})
	}
	return results
}

// storePRMention persists a detected PR reference for a claw (idempotent by URL).
func (s *Server) storePRMention(clawID, repo string, prNumber int, prURL string) {
	var existing string
	_ = s.db.QueryRow(`SELECT id FROM claw_prs WHERE claw_id=? AND pr_url=?`, clawID, prURL).Scan(&existing)
	if existing != "" {
		return
	}

	// Get the current max comment ID and head SHA to avoid flooding with historical data
	token := s.resolveGitHubToken()
	var maxCommentID int64
	var headSHA string
	if token != "" {
		commentsData, err := githubAPIList(fmt.Sprintf("repos/%s/issues/%d/comments", repo, prNumber), token)
		if err == nil {
			for _, c := range commentsData {
				comment, _ := c.(map[string]interface{})
				idF, _ := comment["id"].(float64)
				id := int64(idF)
				if id > maxCommentID {
					maxCommentID = id
				}
			}
		}

		prData, err := githubAPI(fmt.Sprintf("repos/%s/pulls/%d", repo, prNumber), token)
		if err == nil {
			if headObj, ok := prData["head"].(map[string]interface{}); ok {
				headSHA, _ = headObj["sha"].(string)
			}
		}
	}

	_, _ = s.db.Exec(
		`INSERT INTO claw_prs(id,claw_id,repo,pr_number,pr_url,last_comment_id,last_ci_sha,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		uuid.New().String(), clawID, repo, prNumber, prURL, maxCommentID, headSHA, now(),
	)
	log.Printf("[pr-watcher] detected PR %s#%d for claw %s", repo, prNumber, clawID[:8])
}

// scanMessageForPRs extracts and stores any PR URLs found in a message.
func (s *Server) scanMessageForPRs(clawID, content string) {
	for _, pr := range extractPRs(content) {
		s.storePRMention(clawID, pr.repo, pr.number, pr.url)
	}
}

// startPRWatcher launches the background poller.
func (s *Server) startPRWatcher() {
	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			s.pollAllPRs()
		}
	}()
}

type clawPR struct {
	id            string
	clawID        string
	repo          string
	prNumber      int
	prURL         string
	lastCISHA     string
	lastCommentID int64
}

func (s *Server) pollAllPRs() {
	rows, err := s.db.Query(`
		SELECT cp.id, cp.claw_id, cp.repo, cp.pr_number, cp.pr_url, cp.last_ci_sha, cp.last_comment_id,
		       cl.auto_fix_ci, cl.auto_fix_bugbot, cl.status
		FROM claw_prs cp
		JOIN claws cl ON cl.id = cp.claw_id
		WHERE cl.status NOT IN ('deleted','error','offline')
	`)
	if err != nil {
		return
	}
	defer rows.Close()

	type row struct {
		pr            clawPR
		autoFixCI     bool
		autoFixBugbot bool
		clawStatus    string
	}
	var prs []row
	for rows.Next() {
		var r row
		var ciInt, bugbotInt int
		if err := rows.Scan(&r.pr.id, &r.pr.clawID, &r.pr.repo, &r.pr.prNumber, &r.pr.prURL,
			&r.pr.lastCISHA, &r.pr.lastCommentID, &ciInt, &bugbotInt, &r.clawStatus); err != nil {
			continue
		}
		r.autoFixCI = ciInt == 1
		r.autoFixBugbot = bugbotInt == 1
		prs = append(prs, r)
	}
	rows.Close()

	token := s.resolveGitHubToken()
	if token == "" {
		return
	}

	terminatedClaws := map[string]bool{}
	for _, r := range prs {
		// Skip PRs for claws that were already terminated in this poll
		if terminatedClaws[r.pr.clawID] {
			continue
		}

		// Check if PR is merged/closed for idle claws or pipeline-driven connected claws.
		if (r.clawStatus == "idle" || r.clawStatus == "connected") && s.checkPRMerged(r.pr, token) {
			terminatedClaws[r.pr.clawID] = true
			continue // claw is being terminated, skip other checks
		}
		if r.clawStatus == "idle" {
			var prCount int
			if err := s.db.QueryRow(`SELECT COUNT(1) FROM claw_prs WHERE id=?`, r.pr.id).Scan(&prCount); err == nil && prCount == 0 {
				// PR was removed while handling close/merge state, so skip follow-up checks.
				continue
			}
		}
		if r.autoFixCI {
			s.checkCIFailures(r.pr, token)
		}
		factory := s.findFactoryForClaw(r.pr.clawID)
		if r.autoFixBugbot || factory != nil {
			commentsData, err := githubAPIList(fmt.Sprintf("repos/%s/issues/%d/comments", r.pr.repo, r.pr.prNumber), token)
			if err != nil {
				continue
			}
			if r.autoFixBugbot {
				s.checkBugbotComments(r.pr, commentsData)
			}
			// For pipeline-driven claws, forward all new PR comments (from any human reviewer)
			if factory != nil {
				// When auto-fix bugbot is enabled, suppress bugbot-like comments here
				// so the same comment is not injected twice with different templates.
				s.checkPRComments(r.pr, commentsData, r.autoFixBugbot)
			}
			s.updatePRCommentWatermark(r.pr, commentsData)
		}
	}
}

// resolveGitHubTokenWithRepos is a shared helper that resolves a GitHub App installation token
// with optional repo-scoped access.
func (s *Server) resolveGitHubTokenWithRepos(repoAccess []RepoAccess) string {
	s.mu.RLock()
	cfg := s.hubCfg
	s.mu.RUnlock()
	if len(cfg.GitHubApps) == 0 {
		return ""
	}
	for _, appCfg := range cfg.GitHubApps {
		provider, err := NewGitHubTokenProvider(appCfg)
		if err != nil {
			continue
		}
		token, _, err := provider.InstallationToken(context.Background(), 0, repoAccess)
		if err != nil {
			continue
		}
		return token
	}
	return ""
}

// resolveGitHubTokenForRepo returns a GitHub App installation token scoped to the given repo.
// Use this for private repos — an unscoped token won't have read access.
func (s *Server) resolveGitHubTokenForRepo(repo string) string {
	return s.resolveGitHubTokenWithRepos([]RepoAccess{{Repo: repo, Permissions: "read"}})
}

// resolveGitHubToken returns a GitHub App installation token for PR polling.
func (s *Server) resolveGitHubToken() string {
	return s.resolveGitHubTokenWithRepos(nil)
}

// checkCIFailures polls PR check runs and injects a message on new failures.
func (s *Server) checkCIFailures(pr clawPR, token string) {
	// Get PR head SHA
	prData, err := githubAPI(fmt.Sprintf("repos/%s/pulls/%d", pr.repo, pr.prNumber), token)
	if err != nil {
		return
	}
	headObj, ok := prData["head"].(map[string]interface{})
	if !ok {
		return
	}
	headSHA, ok := headObj["sha"].(string)
	if !ok || headSHA == "" || headSHA == pr.lastCISHA {
		return // no new commits
	}

	// Get check runs for head SHA
	checksData, err := githubAPI(fmt.Sprintf("repos/%s/commits/%s/check-runs", pr.repo, headSHA), token)
	if err != nil {
		return
	}

	checkRuns, _ := checksData["check_runs"].([]interface{})
	var failures []string
	allCompleted := true
	for _, cr := range checkRuns {
		run, _ := cr.(map[string]interface{})
		status, _ := run["status"].(string)
		conclusion, _ := run["conclusion"].(string)
		name, _ := run["name"].(string)

		if status != "completed" || conclusion == "" {
			allCompleted = false
		}

		if conclusion == "failure" || conclusion == "timed_out" {
			detailsURL, _ := run["details_url"].(string)
			failures = append(failures, fmt.Sprintf("**%s** — [view logs](%s)", name, detailsURL))
		}
	}

	// Only update SHA if all checks have completed or if we found failures
	if len(failures) > 0 || allCompleted {
		_, _ = s.db.Exec(`UPDATE claw_prs SET last_ci_sha=? WHERE id=?`, headSHA, pr.id)
	}

	if len(failures) == 0 {
		return
	}

	msg := fmt.Sprintf("CI failed on PR #%d ([%s](%s)):\n\n%s\n\nPlease fix these failures on the same branch.",
		pr.prNumber, pr.repo, pr.prURL, strings.Join(failures, "\n"))

	s.injectUserMessage(pr.clawID, msg)
}

// checkPRComments forwards new comments from any human reviewer to the claw.
// Used for pipeline-driven claws that need to react to review feedback.
func (s *Server) checkPRComments(pr clawPR, commentsData []interface{}, skipBugbot bool) {
	var newComments []string

	for _, c := range commentsData {
		comment, _ := c.(map[string]interface{})
		idF, _ := comment["id"].(float64)
		id := int64(idF)
		if id <= pr.lastCommentID {
			continue
		}
		user, _ := comment["user"].(map[string]interface{})
		login, _ := user["login"].(string)
		body, _ := comment["body"].(string)
		htmlURL, _ := comment["html_url"].(string)

		// Skip bots
		userType, _ := user["type"].(string)
		if strings.EqualFold(userType, "bot") || strings.HasSuffix(login, "[bot]") {
			continue
		}
		if skipBugbot && isBugbotComment(login, body) {
			continue
		}

		newComments = append(newComments, fmt.Sprintf("**@%s** commented on PR #%d:\n> %s\n[View](%s)",
			login, pr.prNumber, strings.TrimSpace(body), htmlURL))
	}

	if len(newComments) == 0 {
		return
	}

	s.injectUserMessage(pr.clawID, strings.Join(newComments, "\n\n"))
}

// checkBugbotComments polls PR review comments for new bugbot entries.
func (s *Server) checkBugbotComments(pr clawPR, commentsData []interface{}) {
	var newComments []string

	for _, c := range commentsData {
		comment, _ := c.(map[string]interface{})
		idF, _ := comment["id"].(float64)
		id := int64(idF)
		if id <= pr.lastCommentID {
			continue
		}
		user, _ := comment["user"].(map[string]interface{})
		login, _ := user["login"].(string)
		body, _ := comment["body"].(string)
		htmlURL, _ := comment["html_url"].(string)
		if isBugbotComment(login, body) {
			newComments = append(newComments, fmt.Sprintf("> %s\n\n[View comment](%s)",
				strings.TrimSpace(body), htmlURL))
		}
	}

	if len(newComments) == 0 {
		return
	}

	msg := fmt.Sprintf("New bugbot comment on PR #%d ([%s](%s)):\n\n%s\n\nPlease address this in the same branch.",
		pr.prNumber, pr.repo, pr.prURL, strings.Join(newComments, "\n\n---\n\n"))

	s.injectUserMessage(pr.clawID, msg)
}

func isBugbotComment(login, body string) bool {
	return strings.Contains(strings.ToLower(login), "cursor") ||
		strings.Contains(strings.ToLower(body), "cursor bot") ||
		strings.Contains(strings.ToLower(body), "bugbot")
}

func (s *Server) updatePRCommentWatermark(pr clawPR, commentsData []interface{}) {
	maxID := pr.lastCommentID
	for _, c := range commentsData {
		comment, _ := c.(map[string]interface{})
		idF, _ := comment["id"].(float64)
		id := int64(idF)
		if id > maxID {
			maxID = id
		}
	}
	if maxID > pr.lastCommentID {
		_, _ = s.db.Exec(`UPDATE claw_prs SET last_comment_id=? WHERE id=?`, maxID, pr.id)
	}
}

// injectUserMessage inserts a user-role message into the claw's conversation
// and forwards it over the WS connection (if connected) so the agent sees it.
// Skips injection if the claw is currently streaming a response.
func (s *Server) injectUserMessage(clawID, content string) {
	s.injectUserMessageWithRetry(clawID, content, 0)
}

func (s *Server) injectUserMessageWithRetry(clawID, content string, retryCount int) {
	// Don't interrupt a response in progress
	s.mu.RLock()
	cc, connected := s.claws[clawID]
	streaming := connected && cc.streamingBuf.Len() > 0
	s.mu.RUnlock()
	if streaming {
		if retryCount < 1 {
			log.Printf("[pr-watcher] claw %s is streaming, delaying injection", clawID[:8])
			// Retry once after 30s
			go func() {
				time.Sleep(30 * time.Second)
				s.injectUserMessageWithRetry(clawID, content, retryCount+1)
			}()
		} else {
			log.Printf("[pr-watcher] claw %s still streaming after retry, dropping message", clawID[:8])
		}
		return
	}

	// Resolve tenant
	var tenantID string
	if err := s.db.QueryRow(`SELECT tenant_id FROM claws WHERE id=?`, clawID).Scan(&tenantID); err != nil {
		return
	}

	msgID := uuid.New().String()
	_, err := s.db.Exec(
		`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)`,
		msgID, clawID, tenantID, "user", content, now(),
	)
	if err != nil {
		log.Printf("[pr-watcher] failed to insert message: %v", err)
		return
	}

	// Forward to agent over WS
	s.mu.RLock()
	cc, ok := s.claws[clawID]
	s.mu.RUnlock()
	if ok {
		_ = wsjson.Write(context.Background(), cc.conn, types.WSMessage{
			Type:    "message",
			Payload: map[string]string{"role": "user", "content": content},
		})
	}

	// Broadcast to dashboard
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type: "message",
		Payload: map[string]interface{}{
			"id":         msgID,
			"claw_id":    clawID,
			"tenant_id":  tenantID,
			"role":       "user",
			"content":    content,
			"created_at": now(),
		},
	})
	log.Printf("[pr-watcher] injected message into claw %s", clawID[:8])
}

// githubAPI makes a GET request to the GitHub API and returns parsed JSON.
func githubAPI(path, token string) (map[string]interface{}, error) {
	return githubAPIWithBase("https://api.github.com", path, token)
}

// githubAPIWithBase is like githubAPI but against a custom base URL (for testing).
func githubAPIWithBase(baseURL, path, token string) (map[string]interface{}, error) {
	req, _ := http.NewRequest("GET", baseURL+"/"+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("github API parse error: %w", err)
	}
	return result, nil
}

// githubAPIList makes a GET request expecting a JSON array.
func githubAPIList(path, token string) ([]interface{}, error) {
	req, _ := http.NewRequest("GET", "https://api.github.com/"+path+"?per_page=100&sort=created&direction=desc", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result []interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("github API list parse error: %w", err)
	}
	return result, nil
}

// handleClawSubresource routes /api/claws/:id/prs and /api/claws/:id/settings
func (s *Server) handleClawSubresource(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/claws/"), "/")
	if len(parts) < 2 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	clawID := parts[0]
	sub := parts[1]

	switch sub {
	case "prs":
		s.handleClawPRs(w, r, clawID)
	case "settings":
		s.handleClawSettings(w, r, clawID)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// handleClawPRs returns the list of PRs detected for a claw.
func (s *Server) handleClawPRs(w http.ResponseWriter, r *http.Request, clawID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tenantID := tenantFromCtx(r)

	// Verify claw belongs to tenant
	var exists int
	if err := s.db.QueryRow(`SELECT 1 FROM claws WHERE id=? AND tenant_id=?`, clawID, tenantID).Scan(&exists); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	rows, err := s.db.Query(
		`SELECT id, repo, pr_number, pr_url, created_at FROM claw_prs WHERE claw_id=? ORDER BY created_at ASC`,
		clawID,
	)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type PR struct {
		ID        string `json:"id"`
		Repo      string `json:"repo"`
		PRNumber  int    `json:"prNumber"`
		URL       string `json:"url"`
		CreatedAt string `json:"createdAt"`
	}
	var prs []PR
	for rows.Next() {
		var p PR
		if err := rows.Scan(&p.ID, &p.Repo, &p.PRNumber, &p.URL, &p.CreatedAt); err != nil {
			continue
		}
		prs = append(prs, p)
	}
	if prs == nil {
		prs = []PR{}
	}
	jsonOK(w, prs)
}

// handleClawSettings reads/writes per-claw settings (auto_fix_ci, auto_fix_bugbot).
func (s *Server) handleClawSettings(w http.ResponseWriter, r *http.Request, clawID string) {
	tenantID := tenantFromCtx(r)

	if r.Method == http.MethodGet {
		var autoCI, autoBugbot int
		if err := s.db.QueryRow(`SELECT auto_fix_ci, auto_fix_bugbot FROM claws WHERE id=? AND tenant_id=?`, clawID, tenantID).
			Scan(&autoCI, &autoBugbot); err != nil {
			http.Error(w, "claw not found", http.StatusNotFound)
			return
		}
		jsonOK(w, map[string]bool{"autoFixCI": autoCI == 1, "autoFixBugbot": autoBugbot == 1})
		return
	}
	if r.Method == http.MethodPatch {
		var body struct {
			AutoFixCI     *bool `json:"autoFixCI"`
			AutoFixBugbot *bool `json:"autoFixBugbot"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if body.AutoFixCI != nil {
			v := 0
			if *body.AutoFixCI {
				v = 1
			}
			_, _ = s.db.Exec(`UPDATE claws SET auto_fix_ci=? WHERE id=? AND tenant_id=?`, v, clawID, tenantID)
		}
		if body.AutoFixBugbot != nil {
			v := 0
			if *body.AutoFixBugbot {
				v = 1
			}
			_, _ = s.db.Exec(`UPDATE claws SET auto_fix_bugbot=? WHERE id=? AND tenant_id=?`, v, clawID, tenantID)
		}
		jsonOK(w, map[string]bool{"ok": true})
		return
	}
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

// checkPRMerged checks if a tracked PR is merged or closed.
// It terminates the claw only when merged, and returns true only in that case.
func (s *Server) checkPRMerged(pr clawPR, token string) bool {
	tokenForPR := s.resolveGitHubTokenForRepo(pr.repo)
	if tokenForPR == "" {
		tokenForPR = token
	}
	ghBase := s.githubBaseURL
	if ghBase == "" {
		ghBase = "https://api.github.com"
	}
	data, err := githubAPIWithBase(ghBase, fmt.Sprintf("repos/%s/pulls/%d", pr.repo, pr.prNumber), tokenForPR)
	if err != nil {
		return false
	}
	state, _ := data["state"].(string)
	merged, _ := data["merged"].(bool)

	if state != "closed" && !merged {
		return false // still open
	}

	clawID := pr.clawID
	var tenantID string
	if err := s.db.QueryRow(`SELECT tenant_id FROM claws WHERE id=?`, clawID).Scan(&tenantID); err != nil {
		return false
	}

	// If the PR was closed without merging, notify the claw and let it decide — don't terminate.
	if state == "closed" && !merged {
		log.Printf("[pr-watcher] PR %s#%d closed without merge — notifying claw %s", pr.repo, pr.prNumber, clawID[:8])
		// Stop polling this closed PR so we only notify once.
		_, _ = s.db.Exec(`DELETE FROM claw_prs WHERE id=?`, pr.id)

		// Check if the pipeline handles pr_closed
		factory, issueID := s.findFactoryForClaw(clawID)
		pipelineHandled := false
		if factory != nil {
			if pl := parsePipelineForFactory(factory); pl != nil {
				if stage := pl.StageForPRClosed(); stage != nil {
					s.transitionPipelineStage(clawID, *stage, factory, issueID)
					pipelineHandled = true
				}
			}
		}
		if !pipelineHandled {
			s.injectUserMessage(clawID, fmt.Sprintf("PR %s was closed without being merged. Decide what to do: reopen it, open a new PR, or let the user know.", pr.prURL))
		}
		return false
	}

	// PR was merged — run pipeline on_enter if applicable, then terminate the claw.
	log.Printf("[pr-watcher] PR %s#%d merged — terminating claw %s", pr.repo, pr.prNumber, clawID[:8])

	// Check if the pipeline handles pr_merged (run on_enter before terminating)
	mergeFactory, mergeIssueID := s.findFactoryForClaw(clawID)
	if mergeFactory != nil {
		if pl := parsePipelineForFactory(mergeFactory); pl != nil {
			if stage := pl.StageForPRMerged(); stage != nil {
				s.transitionPipelineStage(clawID, *stage, mergeFactory, mergeIssueID)
			}
		}
	}

	var providerID, provider string
	_ = s.db.QueryRow(`SELECT COALESCE(provider_id,''), COALESCE(provider,'') FROM claws WHERE id=?`, clawID).Scan(&providerID, &provider)

	_, _ = s.db.Exec(`DELETE FROM claw_prs WHERE claw_id=?`, clawID)
	_, _ = s.db.Exec(`UPDATE claws SET status='deleted' WHERE id=?`, clawID)

	s.mu.Lock()
	if cc, ok := s.claws[clawID]; ok {
		cc.conn.Close(1000, "factory: PR merged")
		delete(s.claws, clawID)
	}
	s.mu.Unlock()

	s.broadcastToUsers(tenantID, types.WSMessage{
		Type:    "claw_status",
		Payload: map[string]string{"claw_id": clawID, "status": "deleted"},
	})

	if providerID != "" {
		go s.terminateVM(provider, providerID)
	}

	return true
}
