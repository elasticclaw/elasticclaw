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

	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
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
// Also tracks analytics for the first detection of a PR open.
func (s *Server) storePRMention(clawID, repo string, prNumber int, prURL string) {
	var existing string
	_ = s.db.QueryRow(`SELECT id FROM claw_prs WHERE claw_id=? AND pr_url=?`, clawID, prURL).Scan(&existing)
	if existing != "" {
		return
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
	if _, err := s.db.Exec(
		`INSERT INTO claw_prs(id,claw_id,repo,pr_number,pr_url,last_comment_id,last_comment_at,last_review_id,last_ci_sha,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		prID, clawID, repo, prNumber, prURL, maxCommentID, lastCommentAt, maxReviewID, headSHA, now(),
	); err != nil {
		log.Printf("[pr-watcher] failed to persist PR %s#%d for claw %s: %v", repo, prNumber, clawID[:8], err)
		return
	}
	if _, runID, _, ok, err := s.taskRunContextForClaw(clawID); err != nil {
		log.Printf("[task-run-analytics] failed to resolve task run for PR mention claw %s: %v", clawID, err)
	} else if ok {
		if err := s.associateTaskRunPR(TaskRunPR{
			RunID:      runID,
			Repo:       repo,
			PRNumber:   prNumber,
			URL:        prURL,
			HeadSHA:    headSHA,
			State:      taskRunPRStateOpen,
			OccurredAt: now(),
		}); err != nil {
			log.Printf("[task-run-analytics] failed to associate PR %s#%d for claw %s: %v", repo, prNumber, clawID, err)
		}
	}
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
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			s.pollAllPRs()
		}
	}()
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
			if stage := s.checkPRConditions(r.pr, token, pipelineCtx); stage != nil {
				_, _ = s.db.Exec(`UPDATE claw_prs SET pr_conditions_fired=1 WHERE id=?`, r.pr.id)
				s.transitionPipelineStageWithContext(r.pr.clawID, *stage, pipelineCtx)
			}
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
		`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at) VALUES(?,?,?,?,?,?,?)`,
		msg.ID, msg.ClawID, msg.TenantID, msg.Role, msg.Content, msg.Format, msg.CreatedAt,
	)
	if err != nil {
		log.Printf("[pr-watcher] failed to insert message: %v", err)
		return
	}

	// Broadcast to dashboard immediately, even if delivery to the agent is queued.
	s.broadcastToUsers(tenantID, types.WSMessage{Type: "message", Payload: msg})

	// Forward to agent over WS, or queue behind the current turn.
	s.mu.RLock()
	cc, ok := s.claws[clawID]
	s.mu.RUnlock()
	acceptedForAgent := false
	if ok {
		cc.mu.Lock()
		cc.lastUserMessageAt = time.Now()
		if cc.isBusyLocked() {
			cc.messageQueue = append(cc.messageQueue, msg)
			queueLen := len(cc.messageQueue)
			cc.mu.Unlock()
			acceptedForAgent = true
			log.Printf("[pr-watcher] queued injected message for claw %s (queue length: %d)", shortID(clawID), queueLen)
		} else {
			conn := cc.conn
			cc.mu.Unlock()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := wsjson.Write(ctx, conn, types.WSMessage{Type: "message", Payload: msg})
			cancel()
			if err != nil {
				log.Printf("[pr-watcher] failed to send injected message to claw %s: %v, queueing", shortID(clawID), err)
				s.mu.RLock()
				currentCC := s.claws[clawID]
				s.mu.RUnlock()
				if currentCC != nil {
					currentCC.mu.Lock()
					currentCC.messageQueue = append([]types.HubMessage{msg}, currentCC.messageQueue...)
					queueLen := len(currentCC.messageQueue)
					currentCC.mu.Unlock()
					acceptedForAgent = true
					log.Printf("[pr-watcher] queued injected message for claw %s after send failure (queue length: %d)", shortID(clawID), queueLen)
					go s.sendNextQueuedMessage(currentCC)
				}
			} else {
				acceptedForAgent = true
				log.Printf("[pr-watcher] delivered injected message to claw %s", shortID(clawID))
			}
		}
	}
	if acceptedForAgent {
		// Signal to UI that agent is working on the injected message
		s.broadcastToUsers(tenantID, types.WSMessage{
			Type: "agent_typing",
			Payload: map[string]string{
				"claw_id": clawID,
				"status":  "typing",
			},
		})
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
		writeErr(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	clawID := parts[0]
	sub := parts[1]

	if sub != "prs" && sub != "checkpoints" {
		writeErr(w, http.StatusNotFound, "not_found", "not found")
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
			writeErr(w, http.StatusNotFound, "not_found", "not found")
			return
		}
		var clawTags []string
		_ = json.Unmarshal([]byte(tagsJSON), &clawTags)
		if !canViewClaw(accessCfg, ghLogin, clawTags) {
			writeErr(w, http.StatusForbidden, "forbidden", "forbidden")
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
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	tenantID := tenantFromCtx(r)

	// Verify claw belongs to tenant
	var exists int
	if err := s.db.QueryRow(`SELECT 1 FROM claws WHERE id=? AND tenant_id=?`, clawID, tenantID).Scan(&exists); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "not found")
		return
	}

	rows, err := s.db.Query(
		`SELECT id, repo, pr_number, pr_url, created_at FROM claw_prs WHERE claw_id=? ORDER BY created_at ASC`,
		clawID,
	)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
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
		s.trackPRMerged(mergeCtx.Name(), mergeCtx.IssueID, clawID, pr.repo, pr.prNumber)
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
	_, _ = s.db.Exec(`UPDATE claws SET status='deleted' WHERE id=?`, clawID)
	if s.cronScheduler != nil {
		s.cronScheduler.finishRunByClawID(clawID, "completed", "PR merged")
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

// checkPRConditions evaluates the pr_conditions trigger for a given PR.
// Returns the matching stage if ALL conditions pass, nil otherwise.
func (s *Server) checkPRConditions(pr clawPR, token string, ctx pipelineContext) *pipeline.Stage {
	pl := parsePipelineForContext(ctx)
	if pl == nil {
		return nil
	}
	stage := pl.StageForPRConditions()
	if stage == nil {
		return nil
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
		return nil
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
			return nil
		}
		headObj, _ := prData["head"].(map[string]interface{})
		sha, _ := headObj["sha"].(string)
		if sha == "" {
			return nil // no head SHA yet, can't check
		}
		checksData, err := githubAPIWithBase(ghBase, fmt.Sprintf("repos/%s/commits/%s/check-runs", pr.repo, sha), repoToken)
		if err != nil {
			log.Printf("[pr-conditions] claw %s: failed to get check-runs: %v", pr.clawID[:8], err)
			return nil
		}
		checkRuns, _ := checksData["check_runs"].([]interface{})
		if len(checkRuns) == 0 {
			return nil // no checks yet
		}
		for _, cr := range checkRuns {
			run, _ := cr.(map[string]interface{})
			conclusion, _ := run["conclusion"].(string)
			status, _ := run["status"].(string)
			if status != "completed" {
				return nil // not all done
			}
			if conclusion != "success" && conclusion != "skipped" && conclusion != "neutral" {
				return nil // a check failed or is pending
			}
		}
	}

	// Evaluate reviews: clean
	if cond.Reviews == "clean" {
		reviewsData, err := githubAPIListWithBase(ghBase, fmt.Sprintf("repos/%s/pulls/%d/reviews", pr.repo, pr.prNumber), repoToken)
		if err != nil {
			log.Printf("[pr-conditions] claw %s: failed to get reviews: %v", pr.clawID[:8], err)
			return nil
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
				return nil
			}
		}
	}

	// Evaluate quiet_for
	if cond.QuietFor != "" {
		dur, err := time.ParseDuration(cond.QuietFor)
		if err != nil {
			log.Printf("[pr-conditions] claw %s: invalid quiet_for %q: %v", pr.clawID[:8], cond.QuietFor, err)
			return nil
		}
		// If no comments yet, use PR creation time — a PR with no comments
		// has been quiet since it was created, so quiet_for should still fire.
		quietSince := pr.lastCommentAt
		if quietSince == "" {
			quietSince = pr.createdAt
		}
		if quietSince == "" {
			return nil
		}
		lastComment, err := time.Parse(time.RFC3339, quietSince)
		if err != nil {
			return nil
		}
		if time.Since(lastComment) < dur {
			return nil // not quiet enough
		}
	}

	return stage
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
			return nil, fmt.Errorf("github API returned 404 for %s/%s (token may be invalid or repo may not exist): %s", baseURL, path, msg)
		}
		return nil, fmt.Errorf("github API returned status %d: %s", resp.StatusCode, msg)
	}
	var result []interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("github API list parse error: %w", err)
	}
	return result, nil
}
