package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
)

var prURLRegex = regexp.MustCompile(`https://github\.com/([a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+)/pull/(\d+)`)

const prMergedPermanentFailureLimit = 5

type githubAPIError struct {
	StatusCode  int
	Body        string
	RateLimited bool
}

func (e *githubAPIError) Error() string {
	return fmt.Sprintf("github API returned status %d: %s", e.StatusCode, e.Body)
}
func isPermanentGitHubAPIError(err error) bool {
	var apiErr *githubAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.StatusCode == 404 || apiErr.StatusCode == 410 || apiErr.StatusCode == 401 {
		return true
	}
	return apiErr.StatusCode == 403 && !apiErr.RateLimited && !strings.Contains(strings.ToLower(apiErr.Body), "rate limit")
}

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
// Also tracks analytics for the first detection of a PR open.
func (s *Server) storePRMention(clawID, repo string, prNumber int, prURL string) error {
	var existing string
	_ = s.db.QueryRow(`SELECT id FROM claw_prs WHERE claw_id=? AND pr_url=?`, clawID, prURL).Scan(&existing)
	if existing != "" {
		return nil
	}

	// Track analytics: PR was opened (detected for the first time)
	factory, issueID := s.findFactoryForClaw(clawID)
	if factory != nil {
		s.trackPROpened(factory.Name, issueID, clawID, repo, prNumber)
	}

	// Get the current max comment ID and head SHA to avoid flooding with historical data
	token := s.resolveGitHubToken()
	var maxCommentID int64
	var maxReviewID int64
	var lastCommentAt string
	var lastCommentTime time.Time
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
				createdAt, _ := comment["created_at"].(string)
				if createdAt == "" {
					continue
				}
				createdAtTime, err := time.Parse(time.RFC3339, createdAt)
				if err != nil {
					continue
				}
				if lastCommentAt == "" || createdAtTime.After(lastCommentTime) {
					lastCommentAt = createdAt
					lastCommentTime = createdAtTime
				}
			}
		}

		prData, err := githubAPI(fmt.Sprintf("repos/%s/pulls/%d", repo, prNumber), token)
		if err == nil {
			if headObj, ok := prData["head"].(map[string]interface{}); ok {
				headSHA, _ = headObj["sha"].(string)
			}
		}
		reviewsData, err := githubAPIList(fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, prNumber), token)
		if err == nil {
			maxReviewID = maxPRReviewID(reviewsData, 0)
		}
	}

	prID := uuid.New().String()
	// INSERT OR IGNORE: the [DONE] handler and the message scanner can race to
	// register the same PR (both saw no row in the pre-check above). The loser
	// hitting the (claw_id, pr_url) unique index means the PR is already
	// tracked — idempotent success, not a persistence failure.
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO claw_prs(id,claw_id,repo,pr_number,pr_url,last_comment_id,last_comment_at,last_review_id,last_ci_sha,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		prID, clawID, repo, prNumber, prURL, maxCommentID, lastCommentAt, maxReviewID, headSHA, now(),
	)
	if err != nil {
		log.Printf("[pr-watcher] failed to persist PR %s#%d for claw %s: %v", repo, prNumber, clawID[:8], err)
		return err
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return nil // concurrent writer already registered this PR
	}
	if _, runID, _, ok, err := s.taskRunContextForClaw(clawID); err != nil {
		log.Printf("[task-run-analytics] failed to resolve task run for PR mention claw %s: %v", clawID, err)
	} else if ok {
		// OccurredAt is intentionally left zero: this is a detection time, so
		// associateTaskRunPR treats it as a write-once fallback rather than an
		// authoritative provider timestamp.
		if err := s.associateTaskRunPR(TaskRunPR{
			RunID:        runID,
			Repo:         repo,
			PRNumber:     prNumber,
			URL:          prURL,
			HeadSHA:      headSHA,
			AgentHeadSHA: true,
			State:        taskRunPRStateOpen,
		}); err != nil {
			log.Printf("[task-run-analytics] failed to associate PR %s#%d for claw %s: %v", repo, prNumber, clawID, err)
		}
	}
	log.Printf("[pr-watcher] detected PR %s#%d for claw %s", repo, prNumber, clawID[:8])
	return nil
}

// scanMessageForPRs extracts and stores any PR URLs found in a message.
func (s *Server) scanMessageForPRs(clawID, content string) {
	for _, pr := range extractPRs(content) {
		if err := s.storePRMention(clawID, pr.repo, pr.number, pr.url); err != nil {
			log.Printf("[pr-watcher] failed to store PR mention: %v", err)
		}
	}
}

// startPRWatcher launches the background poller.
func (s *Server) startPRWatcher() {
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		reconcileTicker := time.NewTicker(5 * time.Minute)
		defer reconcileTicker.Stop()
		for {
			select {
			case <-ticker.C:
				s.pollAllPRs()
			case <-reconcileTicker.C:
				s.reconcileDeadClawPRs()
			}
		}
	}()
}

// reconcileDeadClawPRs closes out task_run_prs rows left open because their
// backing claw died before the PR's merge/close was observed. It only queries
// and updates task-run state; it never touches claws or injects into agents.
func (s *Server) reconcileDeadClawPRs() {
	rows, err := s.db.Query(`
		SELECT trp.run_id, trp.repo, trp.pr_number, trp.pr_url
		FROM task_run_prs trp
		JOIN task_run_summaries trs ON trs.run_id = trp.run_id
		WHERE trp.state = 'open'
		  AND trs.status = 'running'
		  AND trs.analytics_enabled = 1
		  AND trp.opened_at > ?
		  AND NOT EXISTS (
			SELECT 1 FROM claw_prs cp JOIN claws cl ON cl.id = cp.claw_id
			WHERE cp.repo = trp.repo AND cp.pr_number = trp.pr_number
			  AND cl.status NOT IN ('deleted','error','offline')
		  )`, epochMillis(now().Add(-90*24*time.Hour)))
	if err != nil {
		if strings.Contains(err.Error(), "database is closed") {
			return
		}
		log.Printf("[pr-reconciler] query error: %v", err)
		return
	}
	type openPR struct {
		runID, repo, url string
		prNumber         int
	}
	var prs []openPR
	for rows.Next() {
		var p openPR
		if err := rows.Scan(&p.runID, &p.repo, &p.prNumber, &p.url); err != nil {
			continue
		}
		prs = append(prs, p)
	}
	rows.Close()
	if len(prs) == 0 {
		return
	}
	token := s.resolveGitHubToken()
	if token == "" {
		return
	}
	for _, p := range prs {
		repoToken := s.resolveGitHubTokenForRepo(p.repo)
		if repoToken == "" {
			repoToken = token
		}
		ghBase := s.githubBaseURL
		if ghBase == "" {
			ghBase = "https://api.github.com"
		}
		data, err := githubAPIWithBase(ghBase, fmt.Sprintf("repos/%s/pulls/%d", p.repo, p.prNumber), repoToken)
		if err != nil {
			log.Printf("[pr-reconciler] fetch PR %s#%d failed: %v", p.repo, p.prNumber, err)
			continue
		}
		state, _ := data["state"].(string)
		merged, _ := data["merged"].(bool)
		if state != "closed" && !merged {
			continue
		}
		atValue, _ := data["merged_at"].(string)
		if !merged {
			atValue, _ = data["closed_at"].(string)
		}
		at := parseRFC3339Timestamp(atValue)
		if err := s.associateTaskRunPR(TaskRunPR{
			RunID:      p.runID,
			Repo:       p.repo,
			PRNumber:   p.prNumber,
			URL:        p.url,
			State:      taskRunPRStateClosed,
			Merged:     merged,
			OccurredAt: at,
		}); err != nil {
			log.Printf("[pr-reconciler] failed to reconcile run %s PR %s#%d: %v", p.runID, p.repo, p.prNumber, err)
		} else {
			log.Printf("[pr-reconciler] reconciled dead-claw PR %s#%d for run %s (merged=%v)", p.repo, p.prNumber, p.runID, merged)
		}
	}
}

type clawPR struct {
	id                  string
	clawID              string
	repo                string
	prNumber            int
	prURL               string
	lastCISHA           string
	lastCommentID       int64
	lastCommentAt       string
	lastReviewCommentID int64
	lastReviewID        int64
	prConditionsFired   bool
	createdAt           string
}

func (s *Server) pollAllPRs() {
	rows, err := s.db.Query(`
		SELECT cp.id, cp.claw_id, cp.repo, cp.pr_number, cp.pr_url, cp.last_ci_sha, cp.last_comment_id,
		       cp.last_comment_at, cp.last_review_comment_id, cp.last_review_id, cp.pr_conditions_fired, cp.created_at,
		       cl.status
		FROM claw_prs cp
		JOIN claws cl ON cl.id = cp.claw_id
		WHERE cl.status NOT IN ('deleted','error','offline')
	`)
	if err != nil {
		if strings.Contains(err.Error(), "database is closed") {
			return
		}
		log.Printf("[pr-watcher] poll query error: %v", err)
		return
	}
	defer rows.Close()

	type row struct {
		pr         clawPR
		clawStatus string
	}
	var prs []row
	for rows.Next() {
		var r row
		var prConditionsFiredInt int
		if err := rows.Scan(&r.pr.id, &r.pr.clawID, &r.pr.repo, &r.pr.prNumber, &r.pr.prURL,
			&r.pr.lastCISHA, &r.pr.lastCommentID, &r.pr.lastCommentAt, &r.pr.lastReviewCommentID, &r.pr.lastReviewID, &prConditionsFiredInt, &r.pr.createdAt,
			&r.clawStatus); err != nil {
			continue
		}
		r.pr.prConditionsFired = prConditionsFiredInt == 1
		prs = append(prs, r)
	}
	rows.Close()

	token := s.resolveGitHubToken()
	if token == "" {
		if len(prs) > 0 {
			s.mu.Lock()
			if time.Since(s.lastTokenFailureLog) >= time.Minute {
				log.Printf("[pr-watcher] CRITICAL: GitHub token resolution failed; PR watcher is effectively disabled for %d tracked PR(s)", len(prs))
				s.lastTokenFailureLog = time.Now()
			}
			s.mu.Unlock()
		}
		return
	}

	if len(prs) > 0 {
		log.Printf("[pr-watcher] poll: checking %d tracked PR(s)", len(prs))
	}

	terminatedClaws := map[string]bool{}
	for _, r := range prs {
		log.Printf("[pr-watcher] poll: claw=%s status=%s pr=%s", r.pr.clawID[:8], r.clawStatus, r.pr.prURL)
		// Skip PRs for claws that were already terminated in this poll
		if terminatedClaws[r.pr.clawID] {
			continue
		}

		pipelineCtx, hasPipelineCtx := s.findPipelineContextForClaw(r.pr.clawID)
		isPipelineDriven := hasPipelineCtx && parsePipelineForContext(pipelineCtx) != nil
		log.Printf("[pr-watcher] claw=%s pipeline=%s pipelineDriven=%v", r.pr.clawID[:8], pipelineCtx.Name(), isPipelineDriven)

		// Check if PR is merged/closed for any non-terminal claw status.
		// checkPRMerged also runs human code push detection off the same PR
		// fetch, before any termination handling.
		if s.checkPRMerged(r.pr, token) {
			terminatedClaws[r.pr.clawID] = true
			continue // claw is being terminated, skip other checks
		}
		// Always check CI failures
		s.checkCIFailures(r.pr, token)

		repoToken := s.resolveGitHubTokenForRepo(r.pr.repo)
		if repoToken == "" {
			repoToken = token
		}
		commentsData, err := githubAPIList(fmt.Sprintf("repos/%s/issues/%d/comments", r.pr.repo, r.pr.prNumber), repoToken)
		if err != nil {
			log.Printf("[pr-watcher] error fetching comments for %s: %v", r.pr.prURL, err)
			continue
		}
		// Always check bugbot and greptile comments
		s.checkBugbotComments(r.pr, commentsData)
		s.checkGreptileComments(r.pr, commentsData)
		log.Printf("[pr-watcher] checking %d comment(s) for claw %s (watermark=%d, forward=%v)", len(commentsData), r.pr.clawID[:8], r.pr.lastCommentID, isPipelineDriven)
		s.checkPRComments(r.pr, commentsData, prCommentOptions{
			skipBugbot:   true,
			skipGreptile: true,
			forward:      isPipelineDriven,
		})
		s.updatePRCommentWatermark(r.pr, commentsData)

		// Greptile posts inline review comments via the pulls/{n}/comments API.
		// Fetch and track them separately from issue comments.
		reviewCommentsData, err := githubAPIList(fmt.Sprintf("repos/%s/pulls/%d/comments", r.pr.repo, r.pr.prNumber), repoToken)
		if err != nil {
			log.Printf("[pr-watcher] error fetching review comments for %s: %v", r.pr.prURL, err)
		} else {
			s.checkGreptileReviewComments(r.pr, reviewCommentsData)
			s.updateReviewCommentWatermark(r.pr, reviewCommentsData)
		}

		reviewsData, err := githubAPIList(fmt.Sprintf("repos/%s/pulls/%d/reviews", r.pr.repo, r.pr.prNumber), repoToken)
		if err != nil {
			log.Printf("[pr-watcher] error fetching reviews for %s: %v", r.pr.prURL, err)
		} else {
			s.checkPRReviews(r.pr, reviewsData)
			s.updatePRReviewWatermark(r.pr, reviewsData)
		}

		// For pipeline-driven claws, evaluate pr_conditions trigger.
		if isPipelineDriven && !r.pr.prConditionsFired {
			stage, status := s.checkPRConditions(r.pr, token, pipelineCtx)
			if stage != nil {
				s.firePRConditions(r.pr, *stage, pipelineCtx)
			} else if status == prConditionsStuck {
				// Only a genuine CI stall (no check runs ever appeared) is eligible
				// for the max-wait timeout. Healthy progress (CI running, changes
				// being addressed, quiet_for still elapsing) and transient API
				// errors keep the run alive.
				maxWait := s.livenessSettings().prConditionsMaxWait
				if created, err := time.Parse(time.RFC3339, r.pr.createdAt); err == nil && time.Since(created) > maxWait {
					go s.stopAgentWithReason(r.pr.clawID, fmt.Sprintf("pr_conditions on PR %s not satisfied within %s — CI stuck or no check runs", r.pr.prURL, maxWait), false)
					terminatedClaws[r.pr.clawID] = true
				}
			}
		}
	}
}

// firePRConditions consumes the one-shot trigger only after its transition
// claims the stage. A failed claim remains eligible for the next poll.
func (s *Server) firePRConditions(pr clawPR, stage pipeline.Stage, ctx pipelineContext) {
	if !s.transitionPipelineStageWithContext(pr.clawID, stage, ctx) {
		return
	}
	if _, err := s.db.Exec(`UPDATE claw_prs SET pr_conditions_fired=1 WHERE id=?`, pr.id); err != nil {
		log.Printf("[pr-watcher] marking pr conditions fired for %s: %v", pr.prURL, err)
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
			log.Printf("[pr-watcher] CRITICAL: GitHub token provider setup failed: %v", err)
			continue
		}
		token, _, err := provider.InstallationToken(context.Background(), 0, repoAccess)
		if err != nil {
			log.Printf("[pr-watcher] CRITICAL: GitHub token provider failed: %v", err)
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

type prCommentOptions struct {
	skipBugbot   bool
	skipGreptile bool
	forward      bool
}

// checkPRComments forwards new comments from any human reviewer to the claw.
// Used for pipeline-driven claws that need to react to review feedback.
func (s *Server) checkPRComments(pr clawPR, commentsData []interface{}, opts prCommentOptions) {
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
		if opts.skipBugbot && isBugbotComment(login, body) {
			continue
		}
		if opts.skipGreptile && isGreptileComment(login, body) {
			continue
		}

		s.recordTaskRunHumanEventForClaw(pr.clawID, taskRunEventHumanPRComment, fmt.Sprintf("human_pr_comment:%d", id), login, htmlURL, map[string]any{
			"repo":       pr.repo,
			"pr_number":  pr.prNumber,
			"comment_id": id,
		})
		if opts.forward {
			newComments = append(newComments, fmt.Sprintf("**@%s** commented on PR #%d:\n> %s\n[View](%s)",
				login, pr.prNumber, strings.TrimSpace(body), htmlURL))
		}
	}

	if len(newComments) == 0 {
		return
	}

	log.Printf("[pr-watcher] forwarding %d new comment(s) to claw %s", len(newComments), pr.clawID[:8])
	s.injectHubMessageByID(pr.clawID, strings.Join(newComments, "\n\n"))
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

// checkGreptileComments polls PR review comments for new greptile entries.
func (s *Server) checkGreptileComments(pr clawPR, commentsData []interface{}) {
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
		if isGreptileComment(login, body) {
			newComments = append(newComments, fmt.Sprintf("> %s\n\n[View comment](%s)",
				strings.TrimSpace(body), htmlURL))
		}
	}

	if len(newComments) == 0 {
		return
	}

	msg := fmt.Sprintf("New greptile review comment on PR #%d ([%s](%s)):\n\n%s\n\nPlease address this in the same branch.",
		pr.prNumber, pr.repo, pr.prURL, strings.Join(newComments, "\n\n---\n\n"))

	s.injectUserMessage(pr.clawID, msg)
}

func isGreptileComment(login, body string) bool {
	// Only match by bot login to avoid false positives from reviewers
	// mentioning "greptile" in casual discussion.
	return strings.Contains(strings.ToLower(login), "greptile")
}

func isBugbotComment(login, body string) bool {
	return strings.Contains(strings.ToLower(login), "cursor") ||
		strings.Contains(strings.ToLower(body), "cursor bot") ||
		strings.Contains(strings.ToLower(body), "bugbot")
}

func (s *Server) updatePRCommentWatermark(pr clawPR, commentsData []interface{}) {
	maxID := pr.lastCommentID
	latestCommentAt := ""
	var latestCommentTime time.Time
	for _, c := range commentsData {
		comment, _ := c.(map[string]interface{})
		idF, _ := comment["id"].(float64)
		id := int64(idF)
		if id > maxID {
			maxID = id
		}
		if id <= pr.lastCommentID {
			continue
		}
		createdAt, _ := comment["created_at"].(string)
		if createdAt == "" {
			continue
		}
		createdAtTime, err := time.Parse(time.RFC3339, createdAt)
		if err != nil {
			continue
		}
		if latestCommentAt == "" || createdAtTime.After(latestCommentTime) {
			latestCommentAt = createdAt
			latestCommentTime = createdAtTime
		}
	}
	if maxID > pr.lastCommentID {
		if latestCommentAt != "" {
			_, _ = s.db.Exec(`UPDATE claw_prs SET last_comment_id=?, last_comment_at=? WHERE id=?`,
				maxID, latestCommentAt, pr.id)
		} else {
			_, _ = s.db.Exec(`UPDATE claw_prs SET last_comment_id=? WHERE id=?`, maxID, pr.id)
		}
	}
}

// checkGreptileReviewComments polls PR review comments (pulls/{n}/comments) for
// new greptile entries. These are inline code-review comments, distinct from
// top-level issue comments.
func (s *Server) checkGreptileReviewComments(pr clawPR, reviewCommentsData []interface{}) {
	var newComments []string

	for _, c := range reviewCommentsData {
		comment, _ := c.(map[string]interface{})
		idF, _ := comment["id"].(float64)
		id := int64(idF)
		if id <= pr.lastReviewCommentID {
			continue
		}
		user, _ := comment["user"].(map[string]interface{})
		login, _ := user["login"].(string)
		body, _ := comment["body"].(string)
		htmlURL, _ := comment["html_url"].(string)
		path, _ := comment["path"].(string)
		line := githubReviewCommentLine(comment)
		userType, _ := user["type"].(string)
		if !isGreptileComment(login, body) && s.isHumanGitHubActor(login, userType) {
			s.forwardHumanReviewComment(pr, id, login, body, htmlURL, path, line)
			continue
		}
		if isGreptileComment(login, body) {
			loc := ""
			if path != "" {
				loc = fmt.Sprintf("`%s", path)
				if line > 0 {
					loc += fmt.Sprintf(":%d", line)
				}
				loc += "`"
			}
			entry := fmt.Sprintf("> %s", strings.TrimSpace(body))
			if loc != "" {
				entry += fmt.Sprintf("\n\n[View comment on %s](%s)", loc, htmlURL)
			} else {
				entry += fmt.Sprintf("\n\n[View comment](%s)", htmlURL)
			}
			newComments = append(newComments, entry)
		}
	}

	if len(newComments) == 0 {
		return
	}

	msg := fmt.Sprintf("New greptile review comment on PR #%d ([%s](%s)):\n\n%s\n\nPlease address this in the same branch.",
		pr.prNumber, pr.repo, pr.prURL, strings.Join(newComments, "\n\n---\n\n"))

	s.injectUserMessage(pr.clawID, msg)
}

func (s *Server) checkPRReviews(pr clawPR, reviewsData []interface{}) {
	for _, rv := range reviewsData {
		review, _ := rv.(map[string]interface{})
		state, _ := review["state"].(string)
		if state != "CHANGES_REQUESTED" {
			continue
		}
		user, _ := review["user"].(map[string]interface{})
		login, _ := user["login"].(string)
		if login == "" {
			continue
		}
		userType, _ := user["type"].(string)
		if strings.EqualFold(userType, "bot") || strings.HasSuffix(login, "[bot]") {
			continue
		}
		idF, _ := review["id"].(float64)
		reviewID := int64(idF)
		if reviewID <= pr.lastReviewID {
			continue
		}
		htmlURL, _ := review["html_url"].(string)
		if htmlURL == "" {
			htmlURL = pr.prURL
		}
		body, _ := review["body"].(string)
		s.forwardHumanRequestedChangesReview(pr, reviewID, login, body, htmlURL)
	}
}

func (s *Server) forwardHumanReviewComment(pr clawPR, id int64, login, body, htmlURL, path string, line int) {
	if !s.claimPRReviewComment(pr.clawID, id) {
		return
	}
	s.recordTaskRunHumanEventForClaw(pr.clawID, taskRunEventHumanReviewComment, fmt.Sprintf("human_review_comment:%d", id), login, htmlURL, map[string]any{
		"repo":       pr.repo,
		"pr_number":  pr.prNumber,
		"comment_id": id,
		"path":       path,
	})
	s.injectHubMessageByID(pr.clawID, formatHumanReviewCommentMessage(login, pr.prNumber, body, htmlURL, path, line))
}

func (s *Server) forwardHumanRequestedChangesReview(pr clawPR, id int64, login, body, htmlURL string) {
	if !s.claimPRReview(pr.clawID, id) {
		return
	}
	s.recordTaskRunHumanEventForClaw(pr.clawID, taskRunEventHumanRequestedChanges, fmt.Sprintf("human_requested_changes:%d", id), login, htmlURL, map[string]any{
		"repo":      pr.repo,
		"pr_number": pr.prNumber,
		"review_id": id,
	})
	s.injectHubMessageByID(pr.clawID, formatHumanRequestedChangesMessage(login, pr.prNumber, body, htmlURL))
}

// detectHumanCodePush compares a tracked PR's head SHA against the last
// agent-authored head SHA in task_run_prs. A head commit linked to a human
// GitHub account is recorded as a human_manual_code_push event; agent (or
// unattributable) pushes and base merges advance the baseline instead, so the
// agent's own work never counts as a human interaction. headSHA may be empty,
// in which case the PR's current head is fetched from GitHub. The event key
// format is shared, so the poller and webhook paths dedupe against each other.
func (s *Server) detectHumanCodePush(clawID, runID, repo string, prNumber int, prURL, headSHA, token string) {
	var lastAgentSHA string
	if err := s.db.QueryRow(`SELECT last_agent_head_sha FROM task_run_prs WHERE run_id=? AND repo=? AND pr_number=?`,
		runID, repo, prNumber).Scan(&lastAgentSHA); err != nil {
		return // PR not tracked for analytics
	}

	repoToken := s.resolveGitHubTokenForRepo(repo)
	if repoToken == "" {
		repoToken = token
	}
	ghBase := s.githubBaseURL
	if ghBase == "" {
		ghBase = "https://api.github.com"
	}
	if headSHA == "" {
		prData, err := githubAPIWithBase(ghBase, fmt.Sprintf("repos/%s/pulls/%d", repo, prNumber), repoToken)
		if err != nil {
			return
		}
		headObj, _ := prData["head"].(map[string]interface{})
		headSHA, _ = headObj["sha"].(string)
	}
	if headSHA == "" || headSHA == lastAgentSHA {
		return
	}
	if lastAgentSHA == "" {
		// No agent baseline yet (rows that predate this feature): adopt the
		// current head as the agent's SHA rather than risk a false positive.
		s.setTaskRunAgentHeadSHA(runID, repo, prNumber, headSHA)
		return
	}
	eventKey := fmt.Sprintf("human_manual_code_push:%s#%d:%s", repo, prNumber, headSHA)
	var seen int
	_ = s.db.QueryRow(`SELECT 1 FROM task_run_events WHERE run_id=? AND event_key=?`, runID, eventKey).Scan(&seen)
	if seen == 1 {
		return // this head SHA was already recorded (poller or webhook)
	}
	commitData, err := githubAPIWithBase(ghBase, fmt.Sprintf("repos/%s/commits/%s", repo, headSHA), repoToken)
	if err != nil {
		return
	}
	if parents := githubCommitParents(commitData); len(parents) >= 2 && parents[lastAgentSHA] {
		// Merge commit whose parent is the agent's head: a base merge (e.g.
		// GitHub's "Update branch" button) brings no human code even though the
		// clicking human is the commit author. Advance the baseline instead.
		s.setTaskRunAgentHeadSHA(runID, repo, prNumber, headSHA)
		return
	}
	if login, userType := githubCommitActor(commitData); login != "" && s.isHumanGitHubActor(login, userType) {
		s.recordTaskRunHumanEventForClaw(clawID, taskRunWarningHumanManualCodePush, eventKey,
			login, prURL, map[string]any{"repo": repo, "pr_number": prNumber, "head_sha": headSHA})
		return
	}
	// Agent's own push (or a commit with no linked GitHub account): advance the
	// baseline so the next human push is compared against the agent's latest work.
	s.setTaskRunAgentHeadSHA(runID, repo, prNumber, headSHA)
}

// setTaskRunAgentHeadSHA records the given SHA as the agent's latest head for a
// tracked PR, resetting the baseline used by human code push detection.
func (s *Server) setTaskRunAgentHeadSHA(runID, repo string, prNumber int, sha string) {
	if _, err := s.db.Exec(`UPDATE task_run_prs SET last_agent_head_sha=? WHERE run_id=? AND repo=? AND pr_number=?`,
		sha, runID, repo, prNumber); err != nil {
		log.Printf("[pr-watcher] failed to update agent head SHA for run %s %s#%d: %v", runID, repo, prNumber, err)
	}
}

// githubCommitParents returns the parent SHAs of a commit as a set.
func githubCommitParents(commitData map[string]interface{}) map[string]bool {
	raw, _ := commitData["parents"].([]interface{})
	parents := make(map[string]bool, len(raw))
	for _, p := range raw {
		parent, _ := p.(map[string]interface{})
		if sha, _ := parent["sha"].(string); sha != "" {
			parents[sha] = true
		}
	}
	return parents
}

// githubCommitActor returns the GitHub account linked to a commit, preferring
// the author over the committer (a web UI edit has the human as author and
// GitHub's web-flow as committer). Empty when no account is linked.
func githubCommitActor(commitData map[string]interface{}) (login, userType string) {
	for _, key := range []string{"author", "committer"} {
		user, _ := commitData[key].(map[string]interface{})
		if l, _ := user["login"].(string); l != "" {
			t, _ := user["type"].(string)
			return l, t
		}
	}
	return "", ""
}

func (s *Server) isHumanGitHubActor(login, userType string) bool {
	if login == "" {
		return false
	}
	if strings.EqualFold(userType, "bot") || strings.HasSuffix(login, "[bot]") {
		return false
	}
	if s.isOwnAppBot(login) {
		return false
	}
	return true
}

func formatHumanReviewCommentMessage(login string, prNumber int, body, htmlURL, path string, line int) string {
	location := ""
	if path != "" {
		location = fmt.Sprintf(" at `%s", path)
		if line > 0 {
			location += fmt.Sprintf(":%d", line)
		}
		location += "`"
	}
	msg := fmt.Sprintf("**@%s** left an inline review comment on PR #%d%s:\n> %s", login, prNumber, location, strings.TrimSpace(body))
	if htmlURL != "" {
		msg += fmt.Sprintf("\n[View](%s)", htmlURL)
	}
	return msg
}

func formatHumanRequestedChangesMessage(login string, prNumber int, body, htmlURL string) string {
	body = strings.TrimSpace(body)
	msg := fmt.Sprintf("**@%s** requested changes on PR #%d:", login, prNumber)
	if body != "" {
		msg += fmt.Sprintf("\n> %s", body)
	}
	if htmlURL != "" {
		msg += fmt.Sprintf("\n[View review](%s)", htmlURL)
	}
	return msg
}

func githubReviewCommentLine(comment map[string]interface{}) int {
	if lineF, ok := comment["line"].(float64); ok && lineF > 0 {
		return int(lineF)
	}
	if lineF, ok := comment["original_line"].(float64); ok && lineF > 0 {
		return int(lineF)
	}
	return 0
}

func (s *Server) claimPRReviewComment(clawID string, id int64) bool {
	return s.claimPRFeedbackDelivery(clawID, "review_comment", id)
}

func (s *Server) claimPRReview(clawID string, id int64) bool {
	return s.claimPRFeedbackDelivery(clawID, "review", id)
}

func (s *Server) claimPRFeedbackDelivery(clawID, feedbackType string, id int64) bool {
	if id <= 0 {
		return false
	}
	result, err := s.db.Exec(
		`INSERT OR IGNORE INTO claw_pr_feedback_deliveries(claw_id, feedback_type, github_id, created_at) VALUES(?,?,?,?)`,
		clawID, feedbackType, id, now(),
	)
	if err != nil {
		log.Printf("[pr-watcher] failed to claim %s %d for claw %s: %v", feedbackType, id, clawID, err)
		return true
	}
	affected, err := result.RowsAffected()
	if err != nil {
		log.Printf("[pr-watcher] failed to inspect %s claim %d for claw %s: %v", feedbackType, id, clawID, err)
		return true
	}
	return affected > 0
}

func (s *Server) updatePRReviewWatermark(pr clawPR, reviewsData []interface{}) {
	maxID := maxPRReviewID(reviewsData, pr.lastReviewID)
	if maxID > pr.lastReviewID {
		_, _ = s.db.Exec(`UPDATE claw_prs SET last_review_id=? WHERE id=?`, maxID, pr.id)
	}
}

func maxPRReviewID(reviewsData []interface{}, initial int64) int64 {
	maxID := initial
	for _, rv := range reviewsData {
		review, _ := rv.(map[string]interface{})
		idF, _ := review["id"].(float64)
		if id := int64(idF); id > maxID {
			maxID = id
		}
	}
	return maxID
}

// updateReviewCommentWatermark updates the last_review_comment_id watermark
// after polling PR review comments.
func (s *Server) updateReviewCommentWatermark(pr clawPR, reviewCommentsData []interface{}) {
	maxID := pr.lastReviewCommentID
	for _, c := range reviewCommentsData {
		comment, _ := c.(map[string]interface{})
		idF, _ := comment["id"].(float64)
		id := int64(idF)
		if id > maxID {
			maxID = id
		}
	}
	if maxID > pr.lastReviewCommentID {
		_, _ = s.db.Exec(`UPDATE claw_prs SET last_review_comment_id=? WHERE id=?`, maxID, pr.id)
	}
}

// injectUserMessage inserts a user-role message into the claw's conversation
// and forwards it over the WS connection (if connected) so the agent sees it.
func (s *Server) injectUserMessage(clawID, content string) {
	s.injectMessage(clawID, content, "user")
}

// injectHubMessageByID inserts a hub-role message into the claw's conversation.
// Hub messages are system-injected and rendered distinctly in the UI.
func (s *Server) injectHubMessageByID(clawID, content string) {
	s.injectMessage(clawID, content, "hub")
}

func (s *Server) injectMessage(clawID, content, role string) {
	// Resolve tenant
	var tenantID string
	if err := s.db.QueryRow(`SELECT tenant_id FROM claws WHERE id=?`, clawID).Scan(&tenantID); err != nil {
		return
	}

	format := ""
	if role == "hub" {
		format = "pre"
	}
	msg := types.HubMessage{
		ID:        uuid.New().String(),
		ClawID:    clawID,
		TenantID:  tenantID,
		Role:      role,
		Content:   content,
		Format:    format,
		CreatedAt: now(),
	}
	_, err := s.db.Exec(
		`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at,delivered_at) VALUES(?,?,?,?,?,?,?,NULL)`,
		msg.ID, msg.ClawID, msg.TenantID, msg.Role, msg.Content, msg.Format, msg.CreatedAt,
	)
	if err != nil {
		log.Printf("[pr-watcher] failed to insert message: %v", err)
		return
	}

	// Broadcast to dashboard immediately, even if delivery to the agent is queued.
	s.broadcastToUsers(tenantID, types.WSMessage{Type: "message", Payload: msg})

	// Deliver oldest pending message when the agent is idle.
	s.mu.RLock()
	cc, ok := s.claws[clawID]
	s.mu.RUnlock()
	acceptedForAgent := false
	if ok {
		cc.mu.Lock()
		cc.lastUserMessageAt = time.Now()
		busy := cc.isBusyLocked()
		cc.mu.Unlock()
		acceptedForAgent = true
		if !busy {
			s.sendNextQueuedMessage(cc)
		}
	}
	if acceptedForAgent {
		log.Printf("[pr-watcher] injected message accepted for claw %s", shortID(clawID))
	}

	// If this is a user/hub message injection (not a claw response), move the issue
	// to WorkingStatus if the claw was idle (watching for PR events).
	// Only move if the message was delivered or queued for the agent.
	if acceptedForAgent && (role == "user" || role == "hub") {
		var currentStatus string
		_ = s.db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&currentStatus)
		if currentStatus == "idle" {
			// Find the factory and issue for this claw
			factory, issueID := s.findFactoryForClaw(clawID)
			if factory != nil && factory.WorkingStatus != "" && issueID != "" {
				if strings.HasPrefix(issueID, "sc-") {
					scToken := s.resolveShortcutToken(factory.Workspace)
					if scToken != "" {
						if err := moveShortcutStory(s.resolveShortcutBaseURL(), scToken, issueID, factory.WorkingStatus); err != nil {
							log.Printf("[factory] failed to move story %s to working status '%s' on resume: %v", issueID, factory.WorkingStatus, err)
						} else {
							log.Printf("[factory] moved story %s to working status '%s' on resume", issueID, factory.WorkingStatus)
						}
					}
				} else if strings.HasPrefix(issueID, "gh-") {
					ghToken := s.resolveGitHubIssuesTokenForFactory(factory)
					if ghToken != "" {
						rest := strings.TrimPrefix(issueID, "gh-")
						lastSlash := strings.LastIndex(rest, "/")
						if lastSlash > 0 {
							repo := rest[:lastSlash]
							issueNumStr := rest[lastSlash+1:]
							var issueNum int
							if _, err := fmt.Sscanf(issueNumStr, "%d", &issueNum); err == nil {
								base := s.githubBaseURL
								if base == "" {
									base = "https://api.github.com"
								}
								if err := moveGitHubIssue(ghToken, repo, issueNum, factory.WorkingStatus, base); err != nil {
									log.Printf("[factory] failed to move GitHub issue %s to working status '%s' on resume: %v", issueID, factory.WorkingStatus, err)
								} else {
									log.Printf("[factory] moved GitHub issue %s to working status '%s' on resume", issueID, factory.WorkingStatus)
								}
							}
						}
					}
				} else {
					// Linear issue
					linearToken := s.resolveLinearTokenForFactory(factory)
					if linearToken != "" {
						if err := s.moveLinearIssueOnServer(linearToken, issueID, factory.WorkingStatus); err != nil {
							log.Printf("[factory] failed to move issue %s to working status '%s' on resume: %v", issueID, factory.WorkingStatus, err)
						} else {
							log.Printf("[factory] moved issue %s to working status '%s' on resume", issueID, factory.WorkingStatus)
						}
					}
				}
			}
		}
	}

	log.Printf("[pr-watcher] injected message into claw %s", shortID(clawID))
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
	if resp.StatusCode >= 400 {
		return nil, &githubAPIError{StatusCode: resp.StatusCode, Body: string(body), RateLimited: resp.Header.Get("X-RateLimit-Remaining") == "0"}
	}
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("github API parse error: %w", err)
	}
	return result, nil
}

// githubAPIList makes a GET request expecting a JSON array.
func githubAPIList(path, token string) ([]interface{}, error) {
	return githubAPIListWithBase("https://api.github.com", path+"?sort=created&direction=desc", token)
}

// handleClawSubresource routes /api/claws/:id/prs and /api/claws/:id/checkpoints.
func (s *Server) handleClawSubresource(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/claws/"), "/")
	if len(parts) < 2 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	clawID := parts[0]
	sub := parts[1]

	if sub != "prs" && sub != "checkpoints" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	ghLogin := githubLoginFromContext(r.Context())
	if ghLogin != "" {
		s.mu.RLock()
		var accessCfg *types.AccessConfig
		if s.hubCfg.Auth != nil {
			accessCfg = s.hubCfg.Auth.Access
		}
		s.mu.RUnlock()

		var tagsJSON string
		if err := s.db.QueryRow(`SELECT COALESCE(tags,'[]') FROM claws WHERE id=? AND tenant_id=?`, clawID, tenantFromCtx(r)).Scan(&tagsJSON); err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var clawTags []string
		_ = json.Unmarshal([]byte(tagsJSON), &clawTags)
		if !canViewClaw(accessCfg, ghLogin, clawTags) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}

	if sub == "prs" {
		s.handleClawPRs(w, r, clawID)
		return
	}
	if sub == "checkpoints" {
		s.handleClawCheckpoints(w, r, clawID)
		return
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

// checkPRMerged checks if a tracked PR is merged or closed.
// It returns true when it terminates the claw, which happens in two cases:
// the PR was merged, or the PR has been inaccessible (permanent API error:
// 404/410/401/non-rate-limit 403) for prMergedPermanentFailureLimit
// consecutive polls — repo/PR deleted or GitHub App uninstalled.
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
		if isPermanentGitHubAPIError(err) {
			_, _ = s.db.Exec(`UPDATE claw_prs SET permanent_failure_count=permanent_failure_count+1 WHERE id=?`, pr.id)
			var failures int
			_ = s.db.QueryRow(`SELECT permanent_failure_count FROM claw_prs WHERE id=?`, pr.id).Scan(&failures)
			if failures >= prMergedPermanentFailureLimit {
				var apiErr *githubAPIError
				_ = errors.As(err, &apiErr)
				go s.stopAgentWithReason(pr.clawID, fmt.Sprintf("PR %s has been inaccessible (HTTP %d) for %d consecutive polls — repo or PR deleted, or GitHub App uninstalled", pr.prURL, apiErr.StatusCode, prMergedPermanentFailureLimit), false)
				return true
			}
			return false
		}
		// Transient error (5xx, network): reset the counter so only genuinely
		// consecutive permanent failures accumulate toward the limit.
		_, _ = s.db.Exec(`UPDATE claw_prs SET permanent_failure_count=0 WHERE id=? AND permanent_failure_count != 0`, pr.id)
		return false
	}
	_, _ = s.db.Exec(`UPDATE claw_prs SET permanent_failure_count=0 WHERE id=? AND permanent_failure_count != 0`, pr.id)
	// Detect human pushes off this same PR fetch, before any merge handling,
	// so a human commit present at merge time is still recorded before the
	// claw is terminated.
	headObj, _ := data["head"].(map[string]interface{})
	headSHA, _ := headObj["sha"].(string)
	if _, runID, _, ok, err := s.taskRunContextForClaw(pr.clawID); err == nil && ok {
		s.detectHumanCodePush(pr.clawID, runID, pr.repo, pr.prNumber, pr.prURL, headSHA, token)
	}
	state, _ := data["state"].(string)
	merged, _ := data["merged"].(bool)
	mergedAtValue, _ := data["merged_at"].(string)
	createdAtValue, _ := data["created_at"].(string)
	mergedAt := parseRFC3339Timestamp(mergedAtValue)
	createdAt := parseRFC3339Timestamp(createdAtValue)

	log.Printf("[pr-watcher] checkPRMerged: claw=%s pr=%s state=%s merged=%v", pr.clawID[:8], pr.prURL, state, merged)

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
		log.Printf("[pr-watcher] PR %s#%d closed without merge — stopping claw %s", pr.repo, pr.prNumber, clawID[:8])
		_, _ = s.db.Exec(`DELETE FROM claw_prs WHERE id=?`, pr.id)

		pipelineCtx, hasPipelineCtx := s.findPipelineContextForClaw(clawID)
		if hasPipelineCtx {
			s.trackPRClosed(pipelineCtx.Name(), pipelineCtx.IssueID, clawID, pr.repo, pr.prNumber)
		}

		// Check if the pipeline handles pr_closed (run on_enter before stopping)
		pipelineHandled := false
		if hasPipelineCtx {
			if pl := parsePipelineForContext(pipelineCtx); pl != nil {
				if stage := pl.StageForPRClosed(); stage != nil {
					s.transitionPipelineStageWithContext(clawID, *stage, pipelineCtx)
					if stage.Terminal {
						_, _ = s.db.Exec(`DELETE FROM claw_prs WHERE claw_id=?`, clawID)
						pipelineHandled = true
					}
				}
			}
		}
		if !pipelineHandled {
			go s.stopAgentWithReason(clawID, fmt.Sprintf("PR %s was closed without being merged", pr.prURL), false)
		}
		return false
	}

	// PR was merged — run pipeline on_enter if applicable, then terminate the claw.
	log.Printf("[pr-watcher] PR %s#%d merged — terminating claw %s", pr.repo, pr.prNumber, clawID[:8])

	// Track analytics for PR merge
	mergeCtx, hasMergeCtx := s.findPipelineContextForClaw(clawID)
	if hasMergeCtx {
		s.trackPRMergedAt(mergeCtx.Name(), mergeCtx.IssueID, clawID, pr.repo, pr.prNumber, firstNonZeroTime(mergedAt, now()))
	} else {
		s.trackPRMergedAt("", "", clawID, pr.repo, pr.prNumber, firstNonZeroTime(mergedAt, now()))
	}
	if _, runID, _, ok, err := s.taskRunContextForClaw(clawID); err == nil && ok {
		if !createdAt.IsZero() {
			if err := s.associateTaskRunPR(TaskRunPR{RunID: runID, Repo: pr.repo, PRNumber: pr.prNumber, URL: pr.prURL, State: taskRunPRStateOpen, OccurredAt: createdAt}); err != nil {
				log.Printf("[pr-watcher] failed to backfill opened_at for run %s: %v", runID, err)
			}
		}
		if err := s.associateTaskRunPR(TaskRunPR{RunID: runID, Repo: pr.repo, PRNumber: pr.prNumber, URL: pr.prURL, State: taskRunPRStateClosed, Merged: true, OccurredAt: firstNonZeroTime(mergedAt, now())}); err != nil {
			log.Printf("[pr-watcher] failed to record merge for run %s: %v", runID, err)
		}
	}

	// Check if the pipeline handles pr_merged (run on_enter before terminating)
	pipelineHandled := false
	if hasMergeCtx {
		if pl := parsePipelineForContext(mergeCtx); pl != nil {
			if stage := pl.StageForPRMerged(); stage != nil {
				s.transitionPipelineStageWithContext(clawID, *stage, mergeCtx)
				if stage.Terminal {
					pipelineHandled = true
				}
			}
		}
	}

	mergeFactory, mergeIssueID := s.findFactoryForClaw(clawID)
	// Move the issue to DoneStatus if configured (final status after PR merge)
	// Skip if pipeline already handled it (mirrors handleClawDoneSignal pattern)
	if !pipelineHandled && mergeFactory != nil && mergeFactory.DoneStatus != "" {
		if strings.HasPrefix(mergeIssueID, "sc-") {
			scToken := s.resolveShortcutToken(mergeFactory.Workspace)
			if scToken != "" {
				if err := moveShortcutStory(s.resolveShortcutBaseURL(), scToken, mergeIssueID, mergeFactory.DoneStatus); err != nil {
					log.Printf("[factory] failed to move story %s to done status '%s': %v", mergeIssueID, mergeFactory.DoneStatus, err)
				} else {
					log.Printf("[factory] moved story %s to done status '%s'", mergeIssueID, mergeFactory.DoneStatus)
				}
			}
		} else if strings.HasPrefix(mergeIssueID, "gh-") {
			ghToken := s.resolveGitHubIssuesTokenForFactory(mergeFactory)
			if ghToken != "" {
				rest := strings.TrimPrefix(mergeIssueID, "gh-")
				lastSlash := strings.LastIndex(rest, "/")
				if lastSlash > 0 {
					repo := rest[:lastSlash]
					issueNumStr := rest[lastSlash+1:]
					var issueNum int
					if _, err := fmt.Sscanf(issueNumStr, "%d", &issueNum); err == nil {
						base := s.githubBaseURL
						if base == "" {
							base = "https://api.github.com"
						}
						if err := moveGitHubIssue(ghToken, repo, issueNum, mergeFactory.DoneStatus, base); err != nil {
							log.Printf("[factory] failed to move GitHub issue %s to done status '%s': %v", mergeIssueID, mergeFactory.DoneStatus, err)
						} else {
							log.Printf("[factory] moved GitHub issue %s to done status '%s'", mergeIssueID, mergeFactory.DoneStatus)
						}
					}
				}
			}
		} else {
			// Linear issue
			linearToken := s.resolveLinearTokenForFactory(mergeFactory)
			if linearToken != "" {
				if err := s.moveLinearIssueOnServer(linearToken, mergeIssueID, mergeFactory.DoneStatus); err != nil {
					log.Printf("[factory] failed to move issue %s to done status '%s': %v", mergeIssueID, mergeFactory.DoneStatus, err)
				} else {
					log.Printf("[factory] moved issue %s to done status '%s'", mergeIssueID, mergeFactory.DoneStatus)
				}
			}
		}
	}

	// If the pipeline handled termination (terminal stage), we're done.
	if pipelineHandled {
		_, _ = s.db.Exec(`DELETE FROM claw_prs WHERE claw_id=?`, clawID)
		return true
	}

	var providerID, provider string
	_ = s.db.QueryRow(`SELECT COALESCE(provider_id,''), COALESCE(provider,'') FROM claws WHERE id=?`, clawID).Scan(&providerID, &provider)

	s.checkpointBeforeTermination(clawID, "pr-merged")

	_, _ = s.db.Exec(`DELETE FROM claw_prs WHERE claw_id=?`, clawID)
	applied, err := s.finishClawTerminalTx(clawID, "deleted", "", "completed", "PR merged", terminalTxOpts{})
	if err != nil || !applied {
		return false
	}
	if s.cronScheduler != nil {
		s.cronScheduler.releaseClawWorkflowSlot(clawID)
	}

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

	// Promote any pending claws now that a slot is free
	go s.promotePendingClaws()

	return true
}

// prConditionsStatus explains why checkPRConditions did or didn't fire, so the
// poller can distinguish a genuine CI stall (eligible for the max-wait timeout)
// from healthy progress or transient API errors that must never time out a run.
type prConditionsStatus int

const (
	// prConditionsSatisfied: all conditions passed and the stage should fire.
	prConditionsSatisfied prConditionsStatus = iota
	// prConditionsWaiting: conditions evaluated cleanly but aren't met yet
	// (CI still running, checks failing, changes requested, quiet_for not
	// elapsed). Healthy — the claw is expected to keep working, so this never
	// triggers the timeout.
	prConditionsWaiting
	// prConditionsTransientError: a GitHub API call failed, so conditions can't
	// be evaluated. Must not be treated as a stall.
	prConditionsTransientError
	// prConditionsStuck: ci:passing is required but no check runs have appeared
	// (or the PR has no head SHA). This is the "CI stuck / no check runs" stall
	// the max-wait guards against — the only status eligible for the timeout.
	prConditionsStuck
)

// checkPRConditions evaluates the pr_conditions trigger for a given PR.
// Returns the matching stage if ALL conditions pass (with prConditionsSatisfied),
// otherwise nil and a status explaining why so the caller can tell a genuine
// stall apart from healthy progress or transient errors.
func (s *Server) checkPRConditions(pr clawPR, token string, ctx pipelineContext) (*pipeline.Stage, prConditionsStatus) {
	pl := parsePipelineForContext(ctx)
	if pl == nil {
		return nil, prConditionsWaiting
	}
	stage := pl.StageForPRConditions()
	if stage == nil {
		return nil, prConditionsWaiting
	}
	// Find the pr_conditions trigger on this stage
	var cond *pipeline.PRConditionsTrigger
	for _, t := range stage.Triggers {
		if t.PRConditions != nil {
			cond = t.PRConditions
			break
		}
	}
	if cond == nil {
		return nil, prConditionsWaiting
	}

	repoToken := s.resolveGitHubTokenForRepo(pr.repo)
	if repoToken == "" {
		repoToken = token
	}
	ghBase := s.githubBaseURL
	if ghBase == "" {
		ghBase = "https://api.github.com"
	}

	// Evaluate ci: passing
	if cond.CI == "passing" {
		prData, err := githubAPIWithBase(ghBase, fmt.Sprintf("repos/%s/pulls/%d", pr.repo, pr.prNumber), repoToken)
		if err != nil {
			log.Printf("[pr-conditions] claw %s: failed to get PR data: %v", pr.clawID[:8], err)
			return nil, prConditionsTransientError
		}
		headObj, _ := prData["head"].(map[string]interface{})
		sha, _ := headObj["sha"].(string)
		if sha == "" {
			return nil, prConditionsStuck // no head SHA yet, CI can't run
		}
		checksData, err := githubAPIWithBase(ghBase, fmt.Sprintf("repos/%s/commits/%s/check-runs", pr.repo, sha), repoToken)
		if err != nil {
			log.Printf("[pr-conditions] claw %s: failed to get check-runs: %v", pr.clawID[:8], err)
			return nil, prConditionsTransientError
		}
		checkRuns, _ := checksData["check_runs"].([]interface{})
		if len(checkRuns) == 0 {
			return nil, prConditionsStuck // no checks ever appeared — the stall the max-wait targets
		}
		for _, cr := range checkRuns {
			run, _ := cr.(map[string]interface{})
			conclusion, _ := run["conclusion"].(string)
			status, _ := run["status"].(string)
			if status != "completed" {
				return nil, prConditionsWaiting // CI still running — healthy progress
			}
			if conclusion != "success" && conclusion != "skipped" && conclusion != "neutral" {
				return nil, prConditionsWaiting // a check failed — claw is expected to fix it
			}
		}
	}

	// Evaluate reviews: clean
	if cond.Reviews == "clean" {
		reviewsData, err := githubAPIListWithBase(ghBase, fmt.Sprintf("repos/%s/pulls/%d/reviews", pr.repo, pr.prNumber), repoToken)
		if err != nil {
			log.Printf("[pr-conditions] claw %s: failed to get reviews: %v", pr.clawID[:8], err)
			return nil, prConditionsTransientError
		}
		latestReviewStateByUser := make(map[string]struct {
			id    int64
			state string
		})
		for _, rv := range reviewsData {
			review, _ := rv.(map[string]interface{})
			userObj, _ := review["user"].(map[string]interface{})
			login, _ := userObj["login"].(string)
			if login == "" {
				continue
			}
			idF, _ := review["id"].(float64)
			reviewID := int64(idF)
			state, _ := review["state"].(string)
			prev, seen := latestReviewStateByUser[login]
			if seen && reviewID <= prev.id {
				continue
			}
			latestReviewStateByUser[login] = struct {
				id    int64
				state string
			}{id: reviewID, state: state}
		}
		for _, latest := range latestReviewStateByUser {
			if latest.state == "CHANGES_REQUESTED" {
				return nil, prConditionsWaiting // claw is expected to address feedback
			}
		}
	}

	// Evaluate quiet_for
	if cond.QuietFor != "" {
		dur, err := time.ParseDuration(cond.QuietFor)
		if err != nil {
			log.Printf("[pr-conditions] claw %s: invalid quiet_for %q: %v", pr.clawID[:8], cond.QuietFor, err)
			return nil, prConditionsWaiting
		}
		// If no comments yet, use PR creation time — a PR with no comments
		// has been quiet since it was created, so quiet_for should still fire.
		quietSince := pr.lastCommentAt
		if quietSince == "" {
			quietSince = pr.createdAt
		}
		if quietSince == "" {
			return nil, prConditionsWaiting
		}
		lastComment, err := time.Parse(time.RFC3339, quietSince)
		if err != nil {
			return nil, prConditionsWaiting
		}
		if time.Since(lastComment) < dur {
			return nil, prConditionsWaiting // not quiet enough yet — healthy, keep waiting
		}
	}

	return stage, prConditionsSatisfied
}

// githubAPIListWithBase makes a GET request against a custom base URL expecting a JSON array.
func githubAPIListWithBase(baseURL, path, token string) ([]interface{}, error) {
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	req, err := http.NewRequest("GET", baseURL+"/"+path+separator+"per_page=100", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("github API response read error: %w", err)
	}
	if resp.StatusCode >= 400 {
		// GitHub returns 404 for auth failures to avoid leaking repo existence.
		// Surface a clearer error when the token is likely the problem.
		msg := string(body)
		if resp.StatusCode == http.StatusNotFound && strings.Contains(msg, "Not Found") {
			return nil, &githubAPIError{StatusCode: resp.StatusCode, Body: fmt.Sprintf("404 for %s/%s (token may be invalid or repo may not exist): %s", baseURL, path, msg), RateLimited: resp.Header.Get("X-RateLimit-Remaining") == "0"}
		}
		return nil, &githubAPIError{StatusCode: resp.StatusCode, Body: msg, RateLimited: resp.Header.Get("X-RateLimit-Remaining") == "0"}
	}
	var result []interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("github API list parse error: %w", err)
	}
	return result, nil
}
