package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
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
// inserted is true only when this call created the claw_prs row.
func (s *Server) storePRMention(clawID, repo string, prNumber int, prURL string) (inserted bool, err error) {
	var existing string
	_ = s.db.QueryRow(`SELECT id FROM claw_prs WHERE claw_id=? AND pr_url=?`, clawID, prURL).Scan(&existing)
	if existing != "" {
		return false, nil
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
		return false, err
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return false, nil // concurrent writer already registered this PR
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
	return true, nil
}

// scanMessageForPRs extracts and stores any PR URLs found in a message.
func (s *Server) scanMessageForPRs(clawID, content string) {
	for _, pr := range extractPRs(content) {
		if _, err := s.storePRMention(clawID, pr.repo, pr.number, pr.url); err != nil {
			log.Printf("[pr-watcher] failed to store PR mention: %v", err)
		}
	}
}

// prMentionCandidate is a PR URL pending claw_prs registration.
type prMentionCandidate struct {
	repo     string
	number   int
	url      string
	comment  int64
	review   int64
	commentAt string
	headSHA  string
}

// preparePRMention loads GitHub watermarks for a PR insert. alreadyTracked is
// true when claw_prs already has this URL (no write needed).
func (s *Server) preparePRMention(clawID, repo string, prNumber int, prURL string) (alreadyTracked bool, row prMentionCandidate, err error) {
	var existing string
	_ = s.db.QueryRow(`SELECT id FROM claw_prs WHERE claw_id=? AND pr_url=?`, clawID, prURL).Scan(&existing)
	if existing != "" {
		return true, prMentionCandidate{}, nil
	}

	row = prMentionCandidate{repo: repo, number: prNumber, url: prURL}
	token := s.resolveGitHubToken()
	if token == "" {
		return false, row, nil
	}
	var lastCommentTime time.Time
	commentsData, err := githubAPIList(fmt.Sprintf("repos/%s/issues/%d/comments", repo, prNumber), token)
	if err == nil {
		for _, c := range commentsData {
			comment, _ := c.(map[string]interface{})
			idF, _ := comment["id"].(float64)
			id := int64(idF)
			if id > row.comment {
				row.comment = id
			}
			createdAt, _ := comment["created_at"].(string)
			if createdAt == "" {
				continue
			}
			createdAtTime, err := time.Parse(time.RFC3339, createdAt)
			if err != nil {
				continue
			}
			if row.commentAt == "" || createdAtTime.After(lastCommentTime) {
				row.commentAt = createdAt
				lastCommentTime = createdAtTime
			}
		}
	}
	prData, err := githubAPI(fmt.Sprintf("repos/%s/pulls/%d", repo, prNumber), token)
	if err == nil {
		if headObj, ok := prData["head"].(map[string]interface{}); ok {
			row.headSHA, _ = headObj["sha"].(string)
		}
	}
	reviewsData, err := githubAPIList(fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, prNumber), token)
	if err == nil {
		row.review = maxPRReviewID(reviewsData, 0)
	}
	return false, row, nil
}

// insertClawPRsAtomic inserts zero or more new claw_prs rows in a single
// transaction. Either every new row is committed, or none are — so callers
// never leave the PR watcher partially armed when one URL fails.
// Returns the URL that failed, or "" on full success.
func (s *Server) insertClawPRsAtomic(clawID string, rows []prMentionCandidate) string {
	if len(rows) == 0 {
		return ""
	}
	tx, err := s.db.Begin()
	if err != nil {
		log.Printf("[pr-watcher] begin claw_prs tx for claw %s: %v", shortID(clawID), err)
		return rows[0].url
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var inserted []prMentionCandidate
	for _, row := range rows {
		// Re-check inside the transaction so a concurrent writer is handled
		// via INSERT OR IGNORE rather than a hard failure.
		var existing string
		_ = tx.QueryRow(`SELECT id FROM claw_prs WHERE claw_id=? AND pr_url=?`, clawID, row.url).Scan(&existing)
		if existing != "" {
			continue
		}
		prID := uuid.New().String()
		res, err := tx.Exec(
			`INSERT OR IGNORE INTO claw_prs(id,claw_id,repo,pr_number,pr_url,last_comment_id,last_comment_at,last_review_id,last_ci_sha,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			prID, clawID, row.repo, row.number, row.url, row.comment, row.commentAt, row.review, row.headSHA, now(),
		)
		if err != nil {
			log.Printf("[pr-watcher] failed to persist PR %s#%d for claw %s: %v", row.repo, row.number, shortID(clawID), err)
			return row.url
		}
		if affected, err := res.RowsAffected(); err == nil && affected == 0 {
			continue // concurrent insert won the race
		}
		inserted = append(inserted, row)
	}
	if err := tx.Commit(); err != nil {
		log.Printf("[pr-watcher] commit claw_prs tx for claw %s: %v", shortID(clawID), err)
		if len(inserted) > 0 {
			return inserted[0].url
		}
		return rows[0].url
	}
	committed = true

	// Side effects only after the rows are durably present.
	factory, issueID := s.findFactoryForClaw(clawID)
	for _, row := range inserted {
		if factory != nil {
			s.trackPROpened(factory.Name, issueID, clawID, row.repo, row.number)
		}
		if _, runID, _, ok, err := s.taskRunContextForClaw(clawID); err != nil {
			log.Printf("[task-run-analytics] failed to resolve task run for PR mention claw %s: %v", clawID, err)
		} else if ok {
			if err := s.associateTaskRunPR(TaskRunPR{
				RunID:        runID,
				Repo:         row.repo,
				PRNumber:     row.number,
				URL:          row.url,
				HeadSHA:      row.headSHA,
				AgentHeadSHA: true,
				State:        taskRunPRStateOpen,
			}); err != nil {
				log.Printf("[task-run-analytics] failed to associate PR %s#%d for claw %s: %v", row.repo, row.number, clawID, err)
			}
		}
		log.Printf("[pr-watcher] detected PR %s#%d for claw %s", row.repo, row.number, shortID(clawID))
	}
	return ""
}

const (
	// prWatcherBaseInterval is the fastest the poller ever runs. Comments,
	// reviews and merges are already delivered by webhooks, so a tighter loop
	// buys latency nobody consumes while it burns installation quota.
	prWatcherBaseInterval = 30 * time.Second
	// prWatcherMaxInterval caps the backoff so a big factory still reconciles.
	prWatcherMaxInterval = 5 * time.Minute
	// prWatcherCallsPerPR is the worst-case cost of one PR per poll: merge check
	// (1) + CI failures (pull + check-runs = 2) + issue comments (1) + review
	// comments (1) + reviews (1) + pr_conditions (pull + check-runs + reviews =
	// 3). ETag 304s make the billed number lower in steady state.
	prWatcherCallsPerPR = 9
	// prWatcherHourlyBudget is the watcher's share of the 5,000/hour GitHub App
	// installation quota. The rest is left for webhooks, pipelines and agents.
	prWatcherHourlyBudget = 2000
)

// prWatcherInterval scales the poll interval with the number of tracked PRs so
// the watcher's hourly cost stays inside prWatcherHourlyBudget. The guarantee
// holds until the interval hits prWatcherMaxInterval (around 18 tracked PRs);
// past that the cost grows linearly again and the backstop is the rate-limit
// reserve in githubClient, which degrades polling to merge-only rather than
// letting the watcher exhaust the installation quota.
func prWatcherInterval(trackedPRs int) time.Duration {
	if trackedPRs <= 0 {
		return prWatcherBaseInterval
	}
	callsPerPoll := float64(trackedPRs * prWatcherCallsPerPR)
	seconds := callsPerPoll * 3600 / float64(prWatcherHourlyBudget)
	interval := time.Duration(seconds * float64(time.Second))
	if interval < prWatcherBaseInterval {
		return prWatcherBaseInterval
	}
	if interval > prWatcherMaxInterval {
		return prWatcherMaxInterval
	}
	return interval
}

// nextPollDelay is the wait before the next poll: the quota reset when GitHub
// has cut us off, otherwise the PR-count-scaled interval.
func (s *Server) nextPollDelay() time.Duration {
	if until, blocked := defaultGitHubClient.blockedUntilTime(); blocked {
		wait := time.Until(until) + time.Second
		if wait > prWatcherMaxInterval {
			wait = prWatcherMaxInterval
		}
		if wait > 0 {
			return wait
		}
	}
	s.mu.RLock()
	tracked := s.trackedPRCount
	s.mu.RUnlock()
	return prWatcherInterval(tracked)
}

// startPRWatcher launches the background poller.
func (s *Server) startPRWatcher() {
	go func() {
		timer := time.NewTimer(prWatcherBaseInterval)
		defer timer.Stop()
		reconcileTicker := time.NewTicker(5 * time.Minute)
		defer reconcileTicker.Stop()
		for {
			select {
			case <-timer.C:
				s.pollAllPRs()
				timer.Reset(s.nextPollDelay())
			case <-reconcileTicker.C:
				s.reconcileDeadClawPRs()
			}
		}
	}()
}

// reconcileDeadClawPRs closes out task_run_prs rows left open because their
// backing claw died before the PR's merge/close was observed. It only queries
// and updates task-run state; it never touches claws or injects into agents.
// Terminal runs that do not require a PR can still have an open PR whose merge
// must be recorded, so this intentionally does not filter by run status.
func (s *Server) reconcileDeadClawPRs() {
	rows, err := s.db.Query(`
		SELECT trp.run_id, trp.repo, trp.pr_number, trp.pr_url
		FROM task_run_prs trp
		JOIN task_run_summaries trs ON trs.run_id = trp.run_id
		WHERE trp.state = 'open'
		  AND trs.analytics_enabled = 1
		  AND (trp.opened_at = 0 OR trp.opened_at > ?)
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
	globalToken := s.resolveGitHubToken()
	for _, p := range prs {
		repoToken := s.resolveGitHubTokenForRepo(p.repo)
		if repoToken == "" {
			repoToken = globalToken
		}
		if repoToken == "" {
			log.Printf("[pr-reconciler] no token available for %s, skipping run %s", p.repo, p.runID)
			continue
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
	lastCIConclusion    string
	lastCommentID       int64
	lastCommentAt       string
	lastReviewCommentID int64
	lastReviewID        int64
	prConditionsFired   bool
	createdAt           string
}

// loadClawPRsByNumber hydrates every tracked-PR row for a (repo, number) pair
// that belongs to a live claw — the same set pollAllPRs would visit, restricted
// to one PR. More than one claw can track the same PR, and each row carries its
// own CI watermark, so all of them have to be evaluated.
//
// checkCIStatus claims its verdict with a conditional UPDATE keyed on
// claw_prs.id and skips work using the stored watermark, so the value-literal
// clawPR built by the review-comment webhook handlers (which leaves id empty)
// is not sufficient here — the rows have to come from the database.
func (s *Server) loadClawPRsByNumber(repo string, prNumber int) []clawPR {
	rows, err := s.db.Query(`
		SELECT cp.id, cp.claw_id, cp.repo, cp.pr_number, cp.pr_url, cp.last_ci_sha, cp.last_ci_conclusion, cp.last_comment_id,
		       cp.last_comment_at, cp.last_review_comment_id, cp.last_review_id, cp.pr_conditions_fired, cp.created_at
		FROM claw_prs cp
		JOIN claws cl ON cl.id = cp.claw_id
		WHERE cp.repo = ? AND cp.pr_number = ? AND cl.status NOT IN ('deleted','error','offline')
		ORDER BY cp.created_at DESC
	`, repo, prNumber)
	if err != nil {
		log.Printf("[pr-watcher] failed to load tracked PR %s#%d: %v", repo, prNumber, err)
		return nil
	}
	defer rows.Close()

	var prs []clawPR
	for rows.Next() {
		var pr clawPR
		var prConditionsFiredInt int
		if err := rows.Scan(&pr.id, &pr.clawID, &pr.repo, &pr.prNumber, &pr.prURL,
			&pr.lastCISHA, &pr.lastCIConclusion, &pr.lastCommentID, &pr.lastCommentAt,
			&pr.lastReviewCommentID, &pr.lastReviewID, &prConditionsFiredInt, &pr.createdAt); err != nil {
			log.Printf("[pr-watcher] failed to scan tracked PR %s#%d: %v", repo, prNumber, err)
			return prs
		}
		pr.prConditionsFired = prConditionsFiredInt == 1
		prs = append(prs, pr)
	}
	if err := rows.Err(); err != nil {
		log.Printf("[pr-watcher] failed to iterate tracked PRs %s#%d: %v", repo, prNumber, err)
	}
	return prs
}

func (s *Server) pollAllPRs() {
	rows, err := s.db.Query(`
		SELECT cp.id, cp.claw_id, cp.repo, cp.pr_number, cp.pr_url, cp.last_ci_sha, cp.last_ci_conclusion, cp.last_comment_id,
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
			&r.pr.lastCISHA, &r.pr.lastCIConclusion, &r.pr.lastCommentID, &r.pr.lastCommentAt, &r.pr.lastReviewCommentID, &r.pr.lastReviewID, &prConditionsFiredInt, &r.pr.createdAt,
			&r.clawStatus); err != nil {
			continue
		}
		r.pr.prConditionsFired = prConditionsFiredInt == 1
		prs = append(prs, r)
	}
	rows.Close()

	s.mu.Lock()
	s.trackedPRCount = len(prs)
	s.mu.Unlock()

	// GitHub already told us the quota is gone: retrying now only adds error
	// lines to the log and delays the recovery for everyone else.
	if until, blocked := defaultGitHubClient.blockedUntilTime(); blocked {
		s.mu.Lock()
		shouldLog := time.Since(s.lastRateLimitSkipLog) >= time.Minute
		if shouldLog {
			s.lastRateLimitSkipLog = time.Now()
		}
		s.mu.Unlock()
		if shouldLog && len(prs) > 0 {
			log.Printf("[pr-watcher] GitHub rate limit exhausted; skipping poll of %d PR(s) until %s",
				len(prs), until.UTC().Format(time.RFC3339))
		}
		return
	}

	// Below the reserve the watcher drops everything except merge detection, so
	// interactive and agent-initiated calls keep their headroom.
	lowPriorityOK := defaultGitHubClient.allowLowPriority()
	s.logGitHubBudget(lowPriorityOK)

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
		if !lowPriorityOK {
			// Merge detection above is the only call worth the remaining budget.
			// NOTE: this also suppresses the green-CI wake-up below, so a claw
			// waiting on CI stays idle until the budget recovers. The webhook
			// path is the fix for that; polling alone cannot be both cheap and
			// prompt.
			continue
		}
		// Always check CI status (failures and, just as importantly, green)
		s.checkCIStatus(r.pr, token)

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

// logGitHubBudget periodically records the remaining installation quota so a
// depletion is visible before it turns into a wall of 403s.
func (s *Server) logGitHubBudget(lowPriorityOK bool) {
	limit, remaining, reset, ok := defaultGitHubClient.budget()
	if !ok {
		return
	}
	interval := 5 * time.Minute
	if !lowPriorityOK {
		interval = time.Minute
	}
	s.mu.Lock()
	if time.Since(s.lastQuotaLog) < interval {
		s.mu.Unlock()
		return
	}
	s.lastQuotaLog = time.Now()
	s.mu.Unlock()
	log.Printf("[pr-watcher] github quota: remaining=%d/%d reset=%s reserve=%d lowPriority=%v",
		remaining, limit, reset.UTC().Format(time.RFC3339), githubRateLimitReserve, lowPriorityOK)
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
	// Installation tokens live an hour. Minting one per call cost two extra
	// GitHub App requests on every poll of every PR, which was its own
	// rate-limit exhaustion path.
	cacheKey := githubTokenCacheKey(repoAccess)
	s.ghTokenMu.Lock()
	defer s.ghTokenMu.Unlock()
	if cached, ok := s.ghTokenCache[cacheKey]; ok && time.Now().Before(cached.expiresAt) {
		return cached.token
	}
	for _, appCfg := range cfg.GitHubApps {
		provider, err := NewGitHubTokenProvider(appCfg)
		if err != nil {
			log.Printf("[pr-watcher] CRITICAL: GitHub token provider setup failed: %v", err)
			continue
		}
		token, expiresAt, err := provider.InstallationToken(context.Background(), 0, repoAccess)
		if err != nil {
			log.Printf("[pr-watcher] CRITICAL: GitHub token provider failed: %v", err)
			continue
		}
		if s.ghTokenCache == nil {
			s.ghTokenCache = map[string]cachedGitHubToken{}
		}
		// Renew a few minutes early so no in-flight call uses an expiring token.
		if expiry := expiresAt.Add(-5 * time.Minute); expiry.After(time.Now()) {
			s.ghTokenCache[cacheKey] = cachedGitHubToken{token: token, expiresAt: expiry}
		}
		return token
	}
	return ""
}

// cachedGitHubToken is an installation token held until shortly before expiry.
type cachedGitHubToken struct {
	token     string
	expiresAt time.Time
}

func githubTokenCacheKey(repoAccess []RepoAccess) string {
	if len(repoAccess) == 0 {
		return ""
	}
	parts := make([]string, 0, len(repoAccess))
	for _, r := range repoAccess {
		parts = append(parts, r.Repo+":"+r.Permissions)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
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

// checkCIStatus polls PR check runs and injects a message when CI reaches a
// terminal verdict for the head SHA — failure *or* success.
//
// The success branch exists because a green CI run is otherwise not an event:
// an agent that pushed a fix and ended its turn waiting on CI would never be
// woken, and the run deadlocks with both sides waiting on each other.
func (s *Server) checkCIStatus(pr clawPR, token string) {
	ghBase := s.githubBaseURL
	if ghBase == "" {
		ghBase = "https://api.github.com"
	}

	// Get PR head SHA
	prData, err := githubAPIWithBase(ghBase, fmt.Sprintf("repos/%s/pulls/%d", pr.repo, pr.prNumber), token)
	if err != nil {
		return
	}
	headObj, ok := prData["head"].(map[string]interface{})
	if !ok {
		return
	}
	headSHA, ok := headObj["sha"].(string)
	if !ok || headSHA == "" {
		return
	}
	// Already delivered a terminal verdict for this SHA. A failure verdict stays
	// re-checkable so a re-run of the same commit can still report green.
	if headSHA == pr.lastCISHA && pr.lastCIConclusion == ciConclusionSuccess {
		return
	}

	// Get check runs for head SHA
	checksData, err := githubAPIWithBase(ghBase, fmt.Sprintf("repos/%s/commits/%s/check-runs", pr.repo, headSHA), token)
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

		// Anything terminal that is not green blocks: cancelled, action_required,
		// stale and startup_failure are "completed" too, and announcing them as
		// "all checks passed" would be an affirmative false claim.
		if conclusion != "" && !isGreenCheckConclusion(conclusion) {
			detailsURL, _ := run["details_url"].(string)
			label := name
			if conclusion != "failure" && conclusion != "timed_out" {
				label = fmt.Sprintf("%s (%s)", name, conclusion)
			}
			failures = append(failures, fmt.Sprintf("**%s** — [view logs](%s)", label, detailsURL))
		}
	}

	// No check runs at all: CI has not reported yet. Reporting "0 checks passed"
	// would be a spurious wake-up, and advancing the watermark would hide the
	// real verdict when it lands.
	if len(checkRuns) == 0 {
		return
	}
	// CI still running: nothing terminal to report, and the watermark must not
	// advance or the eventual verdict becomes unobservable.
	if len(failures) == 0 && !allCompleted {
		return
	}

	conclusion := ciConclusionSuccess
	if len(failures) > 0 {
		conclusion = ciConclusionFailure
	}

	// Conditional UPDATE = claim, same idiom as claimPipelineStageTransition.
	// Exactly one poll (and exactly one hub process) observes a given
	// (sha, conclusion) pair, so the injection below cannot double-fire.
	res, err := s.db.Exec(
		`UPDATE claw_prs SET last_ci_sha=?, last_ci_conclusion=? WHERE id=? AND NOT (last_ci_sha=? AND last_ci_conclusion=?)`,
		headSHA, conclusion, pr.id, headSHA, conclusion)
	if err != nil {
		log.Printf("[pr-watcher] failed to claim CI status for %s: %v", pr.prURL, err)
		return
	}
	if claimed, err := res.RowsAffected(); err != nil || claimed == 0 {
		return
	}

	if conclusion == ciConclusionFailure {
		msg := fmt.Sprintf("CI failed on PR #%d ([%s](%s)):\n\n%s\n\nPlease fix these failures on the same branch. Preserve completed task outputs and reviewer evidence; do not delete or weaken them merely to make CI pass. Diagnose the actual failing check.",
			pr.prNumber, pr.repo, pr.prURL, strings.Join(failures, "\n"))
		s.injectUserMessage(pr.clawID, msg)
		return
	}

	log.Printf("[pr-watcher] CI passed on %s@%s — notifying claw %s", pr.prURL, shortSHA(headSHA), shortID(pr.clawID))
	s.injectExternalHubMessageByID(pr.clawID, fmt.Sprintf(
		"[hub] All CI checks passed on PR #%d ([%s](%s)) at commit `%s` (%d check(s)).\n\n"+
			"If your workflow is waiting on CI, this is the signal to proceed: emit the stage's signal token, or explain what is still blocking you.",
		pr.prNumber, pr.repo, pr.prURL, shortSHA(headSHA), len(checkRuns)))
}

// Terminal CI verdicts recorded in claw_prs.last_ci_conclusion.
const (
	ciConclusionSuccess = "success"
	ciConclusionFailure = "failure"
)

// isGreenCheckConclusion reports whether a completed check run counts as green.
// GitHub's non-green terminal conclusions (failure, timed_out, cancelled,
// action_required, stale, startup_failure) and any conclusion we do not know
// are treated as blocking, so an unknown value can never be reported as green.
func isGreenCheckConclusion(conclusion string) bool {
	switch conclusion {
	case "success", "neutral", "skipped":
		return true
	}
	return false
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
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
	s.injectExternalHubMessageByID(pr.clawID, strings.Join(newComments, "\n\n"))
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
	s.injectExternalHubMessageByID(pr.clawID, formatHumanReviewCommentMessage(login, pr.prNumber, body, htmlURL, path, line))
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
	s.injectExternalHubMessageByID(pr.clawID, formatHumanRequestedChangesMessage(login, pr.prNumber, body, htmlURL))
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

// injectExternalHubMessageByID delivers an external event that is rendered as
// a hub message. Unlike workflow bookkeeping, new review or tracker activity
// is material progress and resumes a no-progress pause.
func (s *Server) injectExternalHubMessageByID(clawID, content string) {
	s.resumeNoProgressAfterUserInput(clawID)
	s.injectMessage(clawID, content, "hub")
}

func (s *Server) injectMessage(clawID, content, role string) {
	if role == "user" {
		s.resumeNoProgressAfterUserInput(clawID)
	}
	// Resolve tenant
	var tenantID string
	if err := s.db.QueryRow(`SELECT tenant_id FROM claws WHERE id=?`, clawID).Scan(&tenantID); err != nil {
		log.Printf("[pr-watcher] dropping %s message for claw %s: tenant lookup failed: %v", role, shortID(clawID), err)
		return
	}
	var pendingDupes int
	// Fail open on a dedup-check error: dropping the injection can strand
	// the agent (a lost watchdog nudge or restart-resume prompt), while
	// injecting blind at worst duplicates one pending message.
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM messages WHERE claw_id=? AND role=? AND content=? AND delivered_at IS NULL`, clawID, role, content).Scan(&pendingDupes); err != nil {
		log.Printf("[pr-watcher] dedup check for claw %s failed, injecting anyway: %v", shortID(clawID), err)
		pendingDupes = 0
	}
	if pendingDupes > 0 {
		log.Printf("[pr-watcher] skipping duplicate pending %s message for claw %s", role, shortID(clawID))
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
	resp, err := defaultGitHubClient.get(baseURL+"/"+path, token)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &githubAPIError{StatusCode: resp.StatusCode, Body: string(resp.Body), RateLimited: resp.rateLimited()}
	}
	var result map[string]interface{}
	if err := json.Unmarshal(resp.Body, &result); err != nil {
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
	draft, _ := data["draft"].(bool)
	mergedAtValue, _ := data["merged_at"].(string)
	createdAtValue, _ := data["created_at"].(string)
	mergedAt := parseRFC3339Timestamp(mergedAtValue)
	createdAt := parseRFC3339Timestamp(createdAtValue)
	// Detection time is only a sound approximation of ready_at while the PR is
	// still open. On the poll that first observes a merged or closed PR, now()
	// is already past the real merge time, which would push ready_at beyond
	// merged_at and silently drop the run from the ready→merge average.
	if !draft && state != "closed" && !merged {
		if _, runID, _, ok, err := s.taskRunContextForClaw(pr.clawID); err == nil && ok {
			var readyAt int64
			if err := s.db.QueryRow(`SELECT ready_at FROM task_run_prs WHERE run_id=? AND repo=? AND pr_number=?`, runID, pr.repo, pr.prNumber).Scan(&readyAt); err == nil && readyAt == 0 {
				if err := s.associateTaskRunPR(TaskRunPR{RunID: runID, Repo: pr.repo, PRNumber: pr.prNumber, URL: pr.prURL, State: taskRunPRStateOpen, ReadyAt: epochMillis(now())}); err != nil {
					log.Printf("[pr-watcher] failed to detect ready_at for run %s: %v", runID, err)
				}
			}
		}
	}

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
	resp, err := defaultGitHubClient.get(baseURL+"/"+path+separator+"per_page=100", token)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		// GitHub returns 404 for auth failures to avoid leaking repo existence.
		// Surface a clearer error when the token is likely the problem.
		msg := string(resp.Body)
		if resp.StatusCode == http.StatusNotFound && strings.Contains(msg, "Not Found") {
			return nil, &githubAPIError{StatusCode: resp.StatusCode, Body: fmt.Sprintf("404 for %s/%s (token may be invalid or repo may not exist): %s", baseURL, path, msg), RateLimited: resp.rateLimited()}
		}
		return nil, &githubAPIError{StatusCode: resp.StatusCode, Body: msg, RateLimited: resp.rateLimited()}
	}
	var result []interface{}
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return nil, fmt.Errorf("github API list parse error: %w", err)
	}
	return result, nil
}
