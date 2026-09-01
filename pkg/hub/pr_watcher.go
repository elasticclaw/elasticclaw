package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
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
//
// mentionOnly marks rows created from PR URLs the agent merely mentioned in a
// message; only delivered rows (mentionOnly=false) gate claw finalization. A
// delivered call for an already-tracked URL upgrades a mention-only row to
// delivered, so a PR mentioned mid-work and then listed in [DONE] blocks.
func (s *Server) storePRMention(clawID, repo string, prNumber int, prURL string, mentionOnly bool) (inserted bool, err error) {
	var existing string
	_ = s.db.QueryRow(`SELECT id FROM claw_prs WHERE claw_id=? AND pr_url=?`, clawID, prURL).Scan(&existing)
	if existing != "" {
		if !mentionOnly {
			if err := s.upgradeMentionOnlyPR(clawID, prURL); err != nil {
				return false, err
			}
		}
		return false, nil
	}

	// Track analytics: PR was opened (detected for the first time)
	factory, issueID := s.findFactoryForClaw(clawID)
	if factory != nil {
		s.trackPROpened(factory.Name, issueID, clawID, repo, prNumber)
	}

	// Get the current max comment ID and head SHA to avoid flooding with historical data.
	// Prefer a repo-scoped installation token so multi-org workspace apps work.
	token := s.tokenForRepo(repo)
	var maxCommentID int64
	var maxReviewID int64
	var lastCommentAt string
	var lastCommentTime time.Time
	var headSHA string
	var title string
	var mergeableState string
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
			title, _ = prData["title"].(string)
			mergeableState, _ = prData["mergeable_state"].(string)
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
		`INSERT OR IGNORE INTO claw_prs(id,claw_id,repo,pr_number,pr_url,title,last_comment_id,last_comment_at,last_review_id,last_ci_sha,mention_only,last_mergeable_state,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		prID, clawID, repo, prNumber, prURL, title, maxCommentID, lastCommentAt, maxReviewID, headSHA, boolInt(mentionOnly), mergeableState, now(),
	)
	if err != nil {
		log.Printf("[pr-watcher] failed to persist PR %s#%d for claw %s: %v", repo, prNumber, clawID[:8], err)
		return false, err
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		// A concurrent writer already registered this PR. If it was the message
		// scanner, its row may be mention-only while this call is a delivery.
		if !mentionOnly {
			if err := s.upgradeMentionOnlyPR(clawID, prURL); err != nil {
				return false, err
			}
		}
		return false, nil
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
//
// mentionOnly must reflect what the content IS, not where the URLs end up:
//   - true for arbitrary agent turn text: the agent can mention any PR in
//     passing ("depends on .../pull/12"), so those rows keep being polled
//     (CI, comments and reviews are still forwarded) but never gate claw
//     finalization and are never action targets.
//   - false for content that is a delivery channel — e.g. the stdout of a
//     pipeline gate script such as verify-github-pr-links, whose whole job is
//     to emit the claw's OWN delivered PR URLs. Those rows must block
//     finalization exactly like PRs registered via [DONE].
func (s *Server) scanMessageForPRs(clawID, content string, mentionOnly bool) {
	for _, pr := range extractPRs(content) {
		if _, err := s.storePRMention(clawID, pr.repo, pr.number, pr.url, mentionOnly); err != nil {
			log.Printf("[pr-watcher] failed to store PR mention: %v", err)
		}
	}
}

// upgradeMentionOnlyPR promotes a mention-only claw_prs row to delivered, so a
// PR the agent mentioned mid-work and then delivered via [DONE] starts gating
// finalization. No-op when the row is already delivered or does not exist.
func (s *Server) upgradeMentionOnlyPR(clawID, prURL string) error {
	res, err := s.db.Exec(`UPDATE claw_prs SET mention_only=0 WHERE claw_id=? AND pr_url=? AND mention_only=1`, clawID, prURL)
	if err != nil {
		log.Printf("[pr-watcher] failed to upgrade mention-only PR %s for claw %s: %v", prURL, shortID(clawID), err)
		return err
	}
	if affected, err := res.RowsAffected(); err == nil && affected > 0 {
		log.Printf("[pr-watcher] PR %s upgraded from mention-only to delivered for claw %s", prURL, shortID(clawID))
	}
	return nil
}

// prMentionCandidate is a PR URL pending claw_prs registration.
type prMentionCandidate struct {
	repo             string
	number           int
	url              string
	comment          int64
	review           int64
	commentAt        string
	headSHA          string
	title            string
	mergeableState   string
}

// preparePRMention loads GitHub watermarks for a PR insert. alreadyTracked is
// true when claw_prs already has this URL (no insert needed) — though a
// delivered (mentionOnly=false) call still upgrades a mention-only row so the
// PR starts gating finalization.
func (s *Server) preparePRMention(clawID, repo string, prNumber int, prURL string, mentionOnly bool) (alreadyTracked bool, row prMentionCandidate, err error) {
	var existing string
	_ = s.db.QueryRow(`SELECT id FROM claw_prs WHERE claw_id=? AND pr_url=?`, clawID, prURL).Scan(&existing)
	if existing != "" {
		if !mentionOnly {
			if err := s.upgradeMentionOnlyPR(clawID, prURL); err != nil {
				return true, prMentionCandidate{}, err
			}
		}
		return true, prMentionCandidate{}, nil
	}

	row = prMentionCandidate{repo: repo, number: prNumber, url: prURL}
	token := s.tokenForRepo(repo)
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
		row.title, _ = prData["title"].(string)
		row.mergeableState, _ = prData["mergeable_state"].(string)
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
// mentionOnly is stamped onto every inserted row; see storePRMention.
func (s *Server) insertClawPRsAtomic(clawID string, rows []prMentionCandidate, mentionOnly bool) string {
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
			// A concurrent writer (possibly the message scanner) got here first.
			// A delivered call must still upgrade a mention-only row.
			if !mentionOnly {
				if _, err := tx.Exec(`UPDATE claw_prs SET mention_only=0 WHERE claw_id=? AND pr_url=? AND mention_only=1`, clawID, row.url); err != nil {
					log.Printf("[pr-watcher] failed to upgrade mention-only PR %s for claw %s: %v", row.url, shortID(clawID), err)
					return row.url
				}
			}
			continue
		}
		prID := uuid.New().String()
		res, err := tx.Exec(
			`INSERT OR IGNORE INTO claw_prs(id,claw_id,repo,pr_number,pr_url,title,last_comment_id,last_comment_at,last_review_id,last_ci_sha,mention_only,last_mergeable_state,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			prID, clawID, row.repo, row.number, row.url, row.title, row.comment, row.commentAt, row.review, row.headSHA, boolInt(mentionOnly), row.mergeableState, now(),
		)
		if err != nil {
			log.Printf("[pr-watcher] failed to persist PR %s#%d for claw %s: %v", row.repo, row.number, shortID(clawID), err)
			return row.url
		}
		if affected, err := res.RowsAffected(); err == nil && affected == 0 {
			if !mentionOnly {
				if _, err := tx.Exec(`UPDATE claw_prs SET mention_only=0 WHERE claw_id=? AND pr_url=? AND mention_only=1`, clawID, row.url); err != nil {
					log.Printf("[pr-watcher] failed to upgrade mention-only PR %s for claw %s: %v", row.url, shortID(clawID), err)
					return row.url
				}
			}
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
	for _, p := range prs {
		repoToken := s.tokenForRepo(p.repo)
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
	state               string
	merged              bool
	mergedAt            *string
	lastMergeableState  string
	// mentionOnly mirrors claw_prs.mention_only at load time. A mention-only
	// row is a POLLING target (CI, comments and reviews are still forwarded)
	// but never an ACTION target and never a TRIGGER: it must not block
	// finalization, drive a pipeline transition, move a tracker issue, be
	// merged by the hub, or terminate a claw. Decision-time reads should still
	// prefer clawPRIsMentionOnly (the flag can be upgraded mid-poll).
	mentionOnly bool
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
		       cp.last_comment_at, cp.last_review_comment_id, cp.last_review_id, cp.pr_conditions_fired, cp.created_at,
		       cp.last_mergeable_state
		FROM claw_prs cp
		JOIN claws cl ON cl.id = cp.claw_id
		WHERE cp.repo = ? AND cp.pr_number = ? AND cl.status NOT IN ('deleted','error','offline')
		  AND cp.state NOT IN ('merged','closed')
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
			&pr.lastReviewCommentID, &pr.lastReviewID, &prConditionsFiredInt, &pr.createdAt,
			&pr.lastMergeableState); err != nil {
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

// rearmTokenMissClosedPRs reopens rows the token-miss bound closed once their
// repo's installation token resolves again. The bound closes a row after
// prMergedPermanentFailureLimit consecutive polls without a token so an
// unpollable repo cannot pin the claw, but the cause is usually transient
// (mint 5xx, network, JWT clock skew) — and a closed row is excluded from
// polling, so without this sweep the row would stay closed forever after the
// outage ends: the PR still open on GitHub, the tracker issue never moved, the
// workflow slot never released, the VM still running.
//
// A closed unmerged row with token_miss_count at the bound is exactly "closed
// by the token-miss bound": the closing path deliberately does NOT zero the
// counter, and every other closing path (checkPRMerged, closeUnreachablePR) is
// only reachable after a successful token resolve already reset it to 0.
//
// Kept cheap: one DB query; when nothing matches, no token resolution is
// attempted at all. Token resolution per distinct repo is the only external
// work — no PR fetches happen here.
func (s *Server) rearmTokenMissClosedPRs() {
	// Mirror pollAllPRs's own gates. While GitHub reports the quota exhausted,
	// or no GitHub App can mint installation tokens, the per-repo token
	// resolution below is exactly the spend those gates exist to prevent —
	// and any row re-armed now would not be polled in this pass anyway, so
	// deferring the sweep to the first healthy pass loses nothing.
	if _, blocked := defaultGitHubClient.blockedUntilTime(); blocked {
		return
	}
	if len(s.githubAppConfigsForTokens()) == 0 {
		return
	}
	rows, err := s.db.Query(`
		SELECT DISTINCT cp.repo
		FROM claw_prs cp
		JOIN claws cl ON cl.id = cp.claw_id
		WHERE cl.status NOT IN ('deleted','error','offline')
		  AND cp.state='closed' AND cp.merged=0
		  AND cp.token_miss_count >= ?
	`, prMergedPermanentFailureLimit)
	if err != nil {
		if !strings.Contains(err.Error(), "database is closed") {
			log.Printf("[pr-watcher] token-miss re-arm query error: %v", err)
		}
		return
	}
	defer rows.Close()
	var repos []string
	for rows.Next() {
		var repo string
		if err := rows.Scan(&repo); err != nil {
			continue
		}
		repos = append(repos, repo)
	}
	rows.Close()
	for _, repo := range repos {
		if s.tokenForRepo(repo) == "" {
			continue // outage still ongoing for this repo
		}
		res, err := s.db.Exec(`
			UPDATE claw_prs SET state='open', token_miss_count=0
			WHERE repo=? AND state='closed' AND merged=0 AND token_miss_count >= ?
			  AND claw_id IN (SELECT id FROM claws WHERE status NOT IN ('deleted','error','offline'))
		`, repo, prMergedPermanentFailureLimit)
		if err != nil {
			log.Printf("[pr-watcher] failed to re-arm token-miss-closed PR rows for %s: %v", repo, err)
			continue
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			log.Printf("[pr-watcher] token for %s resolvable again — re-armed %d PR row(s) closed by the token-miss bound", repo, n)
		}
	}
}

func (s *Server) pollAllPRs() {
	// Re-arm rows the token-miss bound closed, before the main query, so a
	// recovered row is polled again in this same pass.
	s.rearmTokenMissClosedPRs()

	rows, err := s.db.Query(`
		SELECT cp.id, cp.claw_id, cp.repo, cp.pr_number, cp.pr_url, cp.last_ci_sha, cp.last_ci_conclusion, cp.last_comment_id,
		       cp.last_comment_at, cp.last_review_comment_id, cp.last_review_id, cp.pr_conditions_fired, cp.created_at,
		       cp.mention_only, cp.last_mergeable_state, cl.status
		FROM claw_prs cp
		JOIN claws cl ON cl.id = cp.claw_id
		WHERE cl.status NOT IN ('deleted','error','offline')
		  AND cp.state NOT IN ('merged','closed')
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
		var prConditionsFiredInt, mentionOnlyInt int
		if err := rows.Scan(&r.pr.id, &r.pr.clawID, &r.pr.repo, &r.pr.prNumber, &r.pr.prURL,
			&r.pr.lastCISHA, &r.pr.lastCIConclusion, &r.pr.lastCommentID, &r.pr.lastCommentAt, &r.pr.lastReviewCommentID, &r.pr.lastReviewID, &prConditionsFiredInt, &r.pr.createdAt,
			&mentionOnlyInt, &r.pr.lastMergeableState, &r.clawStatus); err != nil {
			continue
		}
		r.pr.prConditionsFired = prConditionsFiredInt == 1
		r.pr.mentionOnly = mentionOnlyInt == 1
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

	// Prefer per-repo installation tokens. An unscoped mint from the first app
	// cannot be reused across orgs when multiple workspace GitHub Apps exist
	// (Greptile P1): merge/CI checks for later apps would 404/403 silently.
	if len(s.githubAppConfigsForTokens()) == 0 {
		if len(prs) > 0 {
			s.mu.Lock()
			if time.Since(s.lastTokenFailureLog) >= time.Minute {
				log.Printf("[pr-watcher] CRITICAL: no GitHub Apps configured (hub or workspace); PR watcher disabled for %d tracked PR(s)", len(prs))
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
	tokenMisses := 0
	for _, r := range prs {
		log.Printf("[pr-watcher] poll: claw=%s status=%s pr=%s", r.pr.clawID[:8], r.clawStatus, r.pr.prURL)
		// Skip PRs for claws that were already terminated in this poll
		if terminatedClaws[r.pr.clawID] {
			continue
		}

		token := s.tokenForRepo(r.pr.repo)
		if token == "" {
			tokenMisses++
			log.Printf("[pr-watcher] no token for %s (claw %s); skipping this PR", r.pr.repo, shortID(r.pr.clawID))
			// A row skipped here never reaches checkPRMerged, so nothing else
			// can ever move it to a resolved state — bound the misses so an
			// unreachable repo cannot pin the claw forever. Tracked in
			// token_miss_count, NOT permanent_failure_count: that counter
			// belongs to checkPRMerged's permanent-API-error handling and
			// resetting one from the other's path would break both bounds.
			if _, err := s.db.Exec(`UPDATE claw_prs SET token_miss_count=token_miss_count+1 WHERE id=?`, r.pr.id); err != nil {
				log.Printf("[pr-watcher] failed to count token miss for PR %s: %v", r.pr.prURL, err)
				continue
			}
			var misses int
			if err := s.db.QueryRow(`SELECT token_miss_count FROM claw_prs WHERE id=?`, r.pr.id).Scan(&misses); err != nil {
				log.Printf("[pr-watcher] failed to read token miss count for PR %s: %v", r.pr.prURL, err)
				continue
			}
			if misses >= prMergedPermanentFailureLimit {
				// Close ONLY the row — never the claw. A token miss has
				// transient causes (installation-token mint 5xx, network
				// errors, JWT clock skew), and a mid-work agent may have
				// delivered nothing yet, so escalating here would kill a
				// healthy claw during a token outage. The teardown decision
				// stays with checkPRMerged observing a real delivered row.
				// token_miss_count is deliberately NOT zeroed here: a closed
				// unmerged row with the counter at the bound is how
				// rearmTokenMissClosedPRs recognises (and reopens) these rows
				// once the token resolves again.
				log.Printf("[pr-watcher] WARN: PR %s (%s#%d) unpollable for %d consecutive polls (no GitHub token resolvable for %s) — marking the row closed so it stops blocking finalization; claw %s left untouched",
					r.pr.prURL, r.pr.repo, r.pr.prNumber, prMergedPermanentFailureLimit, r.pr.repo, shortID(r.pr.clawID))
				if _, err := s.db.Exec(`UPDATE claw_prs SET state='closed' WHERE id=?`, r.pr.id); err != nil {
					log.Printf("[pr-watcher] failed to mark unpollable PR %s closed for claw %s: %v", r.pr.prURL, shortID(r.pr.clawID), err)
				}
			}
			continue
		}
		// The token resolved: only genuinely consecutive misses may accumulate
		// toward the bound above.
		if _, err := s.db.Exec(`UPDATE claw_prs SET token_miss_count=0 WHERE id=? AND token_miss_count != 0`, r.pr.id); err != nil {
			log.Printf("[pr-watcher] failed to reset token miss count for PR %s: %v", r.pr.prURL, err)
		}

		pipelineCtx, hasPipelineCtx := s.findPipelineContextForClaw(r.pr.clawID)
		isPipelineDriven := hasPipelineCtx && parsePipelineForContext(pipelineCtx) != nil
		log.Printf("[pr-watcher] claw=%s pipeline=%s pipelineDriven=%v", r.pr.clawID[:8], pipelineCtx.Name(), isPipelineDriven)

		// Check if PR is merged/closed for any non-terminal claw status.
		// checkPRMerged also runs human code push detection off the same PR
		// fetch, before any termination handling.
		resolved, terminated := s.checkPRMerged(r.pr, token)
		if lowPriorityOK || terminated {
			// Record a terminal CI result even when merge handling removed the PR row
			// earlier in this poll. The bypass is gated on terminated ("the claw
			// is being torn down — one last CI record on the way out"), NOT on
			// resolved: a row can resolve while the claw survives (a PR closed
			// with others open, or administratively closed as unreachable), and
			// firing below the budget reserve there would burn the headroom the
			// reserve protects and inject a misleading CI verdict for a PR that
			// was just closed.
			s.checkCIStatus(r.pr, token)
		}
		if terminated {
			terminatedClaws[r.pr.clawID] = true
			continue // claw is being terminated, skip other checks
		}
		if resolved {
			// The row reached a terminal state (merged/closed) but the claw
			// stays alive for its other PRs. Skip the low-priority pipeline —
			// comments, reviews and pr_conditions must not fire off a PR that
			// is already resolved.
			continue
		}
		if !lowPriorityOK {
			// Merge detection above is the only call worth the remaining budget.
			continue
		}
		commentsData, err := githubAPIList(fmt.Sprintf("repos/%s/issues/%d/comments", r.pr.repo, r.pr.prNumber), token)
		if err != nil {
			log.Printf("[pr-watcher] error fetching comments for %s: %v", r.pr.prURL, err)
			continue
		}
		// Always check bugbot and greptile comments
		s.checkBugbotComments(r.pr, commentsData)
		s.checkGreptileComments(r.pr, commentsData)
		log.Printf("[pr-watcher] checking %d comment(s) for claw %s (watermark=%d, forward=%v)", len(commentsData), r.pr.clawID[:8], r.pr.lastCommentID, isPipelineDriven)
		if !isPipelineDriven && hasNewComments(commentsData, r.pr.lastCommentID) {
			log.Printf("[pr-watcher] claw=%s pr #%d has new comment(s) above watermark but forward=false (no pipeline context) — not delivering", r.pr.clawID[:8], r.pr.prNumber)
		}
		s.checkPRComments(r.pr, commentsData, prCommentOptions{
			skipBugbot:   true,
			skipGreptile: true,
			forward:      isPipelineDriven,
		})
		s.updatePRCommentWatermark(r.pr, commentsData)

		// Greptile posts inline review comments via the pulls/{n}/comments API.
		// Fetch and track them separately from issue comments.
		reviewCommentsData, err := githubAPIList(fmt.Sprintf("repos/%s/pulls/%d/comments", r.pr.repo, r.pr.prNumber), token)
		if err != nil {
			log.Printf("[pr-watcher] error fetching review comments for %s: %v", r.pr.prURL, err)
		} else {
			s.checkGreptileReviewComments(r.pr, reviewCommentsData)
			s.updateReviewCommentWatermark(r.pr, reviewCommentsData)
		}

		reviewsData, err := githubAPIList(fmt.Sprintf("repos/%s/pulls/%d/reviews", r.pr.repo, r.pr.prNumber), token)
		if err != nil {
			log.Printf("[pr-watcher] error fetching reviews for %s: %v", r.pr.prURL, err)
		} else {
			s.checkPRReviews(r.pr, reviewsData)
			s.updatePRReviewWatermark(r.pr, reviewsData)
		}

		// For pipeline-driven claws, evaluate pr_conditions trigger — but only
		// off a DELIVERED row. A mention-only row is a polling target, never a
		// TRIGGER: a stranger's green CI must not advance the claw's pipeline
		// (and finalize it on a terminal stage), and a stale mentioned PR with
		// no check runs must not trip the max-wait stop and destroy a sandbox
		// mid-work.
		if isPipelineDriven && !r.pr.prConditionsFired && !r.pr.mentionOnly {
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
	if tokenMisses > 0 && tokenMisses == len(prs) {
		s.mu.Lock()
		if time.Since(s.lastTokenFailureLog) >= time.Minute {
			log.Printf("[pr-watcher] CRITICAL: GitHub token resolution failed for all %d tracked PR(s)", len(prs))
			s.lastTokenFailureLog = time.Now()
		}
		s.mu.Unlock()
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

// githubAppConfigsForTokens returns GitHub Apps for hub-side API minting
// (PR watcher, reconciler, issue poller). Factories often configure apps only
// on the workspace (no hub-global github_apps); include those so token
// resolution matches agent credential helpers.
//
// Order: workspace apps first (primary for factory deploys), then hub apps.
// Same AppID may appear more than once (e.g. workspace key unusable, hub key
// valid). resolveGitHubTokenWithRepos tries each entry in order and skips
// setup/mint failures, so we must not drop later credentials by AppID.
func (s *Server) githubAppConfigsForTokens() []*types.GitHubAppConfig {
	var apps []*types.GitHubAppConfig
	add := func(list []*types.GitHubAppConfig) {
		for _, app := range list {
			if app == nil || app.AppID == 0 || app.PrivateKeyPEM == "" {
				continue
			}
			apps = append(apps, app)
		}
	}
	// Workspace-scoped apps (adversaries, etc.)
	if entries, err := os.ReadDir(workspacesDir()); err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			wsApps, err := loadWorkspaceGitHubAppConfigs(name)
			if err != nil {
				log.Printf("[github] load workspace %q github apps: %v", name, err)
				continue
			}
			add(wsApps)
		}
	}
	s.mu.RLock()
	hubApps := append([]*types.GitHubAppConfig(nil), s.hubCfg.GitHubApps...)
	s.mu.RUnlock()
	add(hubApps)
	return apps
}

// resolveGitHubTokenWithRepos is a shared helper that resolves a GitHub App installation token
// with optional repo-scoped access.
func (s *Server) resolveGitHubTokenWithRepos(repoAccess []RepoAccess) string {
	appCfgs := s.githubAppConfigsForTokens()
	if len(appCfgs) == 0 {
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
	// If the shared client is already rate-limit blocked, don't mint: a
	// mid-pass 429 on one repo must stop token mints for the rest of the
	// pass, not just the repo that hit it.
	if blockedUntil, blocked := defaultGitHubClient.blockedUntilTime(); blocked {
		if s.ghTokenCache == nil {
			s.ghTokenCache = map[string]cachedGitHubToken{}
		}
		s.ghTokenCache[cacheKey] = cachedGitHubToken{expiresAt: blockedUntil}
		return ""
	}
	var lastErr error
	for _, appCfg := range appCfgs {
		provider, err := NewGitHubTokenProvider(appCfg)
		if err != nil {
			log.Printf("[pr-watcher] CRITICAL: GitHub token provider setup failed (app_id=%d): %v", appCfg.AppID, err)
			lastErr = err
			continue
		}
		token, expiresAt, err := provider.InstallationToken(context.Background(), 0, repoAccess)
		if err != nil {
			log.Printf("[pr-watcher] GitHub token provider failed (app_id=%d): %v", appCfg.AppID, err)
			lastErr = err
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
	if lastErr != nil {
		log.Printf("[pr-watcher] CRITICAL: GitHub token resolution failed after trying %d app(s): %v", len(appCfgs), lastErr)
	}
	// Only negatively cache rate-limit failures: those are the ones that
	// recur on every poll of every PR until the window passes, and the
	// shared client's blockedUntil already tells us when that is. Other
	// mint failures (outages, bad config) must be retried on the very next
	// poll — token_miss_count and the re-arm sweep depend on that.
	if apiErr, ok := lastErr.(*githubAPIError); ok && apiErr.RateLimited {
		if s.ghTokenCache == nil {
			s.ghTokenCache = map[string]cachedGitHubToken{}
		}
		until := time.Now().Add(time.Minute)
		if blockedUntil, blocked := defaultGitHubClient.blockedUntilTime(); blocked && blockedUntil.After(until) {
			until = blockedUntil
		}
		s.ghTokenCache[cacheKey] = cachedGitHubToken{expiresAt: until}
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
// When multiple workspace apps cover different orgs, this tries each app until
// one can mint for owner/repo (cached per repo).
func (s *Server) resolveGitHubTokenForRepo(repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return ""
	}
	return s.resolveGitHubTokenWithRepos([]RepoAccess{{Repo: repo, Permissions: "read"}})
}

// tokenForRepo returns a repo-scoped installation token for owner/repo.
// It does not fall back to an unscoped mint: the first configured app's
// unscoped token can 404/403 for repos only another workspace app can see,
// and repeated "permanent" 404s can wrongly terminate the claw.
func (s *Server) tokenForRepo(repo string) string {
	return s.resolveGitHubTokenForRepo(repo)
}

// resolveGitHubToken returns an unscoped GitHub App installation token.
// Prefer tokenForRepo when a repository is known — unscoped tokens from the
// first configured app may not reach other orgs.
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

	// Avoid BEGIN/ROLLBACK on settled polls. The conditional claim below remains
	// authoritative because another watcher can update the watermark after this
	// pre-read.
	var alreadyClaimed int
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM claw_prs WHERE id=? AND last_ci_sha=? AND last_ci_conclusion=?)`, pr.id, headSHA, conclusion).Scan(&alreadyClaimed); err != nil {
		log.Printf("[pr-watcher] read CI watermark for %s: %v", pr.prURL, err)
		return
	}
	if alreadyClaimed != 0 {
		return
	}

	// Intentionally keep task-run lookup, watermark claim, and event write in one
	// transaction: a rolled-back event write must not permanently consume the CI
	// watermark. The pre-read above only avoids this cost for settled polls.
	tx, err := s.db.Begin()
	if err != nil {
		log.Printf("[pr-watcher] begin CI event for %s: %v", pr.prURL, err)
		return
	}
	defer tx.Rollback()
	tenantID, runID, attemptID, hasRun, err := s.taskRunContextForClawTx(tx, pr.clawID)
	if err != nil {
		log.Printf("[pr-watcher] find CI task run for %s: %v", pr.prURL, err)
		return
	}

	// Conditional UPDATE = claim, same idiom as claimPipelineStageTransition.
	// A merged PR may have removed its row earlier in this poll; in that case
	// the task-run event key becomes the durable claim instead.
	res, err := tx.Exec(
		`UPDATE claw_prs SET last_ci_sha=?, last_ci_conclusion=? WHERE id=? AND NOT (last_ci_sha=? AND last_ci_conclusion=?)`,
		headSHA, conclusion, pr.id, headSHA, conclusion)
	if err != nil {
		log.Printf("[pr-watcher] failed to claim CI status for %s: %v", pr.prURL, err)
		return
	}
	claimed, err := res.RowsAffected()
	if err != nil {
		return
	}
	rowRemoved := false
	if claimed == 0 {
		var rowExists int
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM claw_prs WHERE id=?)`, pr.id).Scan(&rowExists); err != nil {
			log.Printf("[pr-watcher] check CI claim for %s: %v", pr.prURL, err)
			return
		}
		if rowExists != 0 {
			return
		}
		rowRemoved = true
		if !hasRun {
			return
		}
	}

	ciEventType := taskRunEventCISucceeded
	if conclusion == ciConclusionFailure {
		ciEventType = taskRunEventCIFailed
	}
	recordedEvent := !hasRun
	if hasRun {
		event := TaskRunEvent{
			EventKey: "ci:" + pr.id + ":" + headSHA + ":" + conclusion,
			TenantID: tenantID, RunID: runID, AttemptID: attemptID, Source: taskRunSourcePRWatcher, EventType: ciEventType, ActorType: taskRunActorSystem,
			TargetType: "pull_request", TargetURL: pr.prURL,
			Detail: map[string]any{"repo": pr.repo, "prNumber": pr.prNumber, "headSha": headSHA, "conclusion": conclusion}, OccurredAt: now(),
		}
		var err error
		recordedEvent, err = recordTaskRunEventIfNewTx(tx, event)
		if err != nil {
			log.Printf("[pr-watcher] record CI event for %s: %v", pr.prURL, err)
			return
		}
		if recordedEvent {
			if err := materializeTaskRunTx(tx, runID); err != nil {
				log.Printf("[pr-watcher] materialize CI event for %s: %v", pr.prURL, err)
				return
			}
		}
	}
	if err := tx.Commit(); err != nil {
		log.Printf("[pr-watcher] commit CI claim for %s: %v", pr.prURL, err)
		return
	}
	if !recordedEvent {
		return
	}
	if rowRemoved {
		return
	}

	if conclusion == ciConclusionFailure {
		msg := fmt.Sprintf("CI failed on PR #%d ([%s](%s)):\n\n%s\n\nPlease fix these failures on the same branch.",
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
			s.updateWatermarkGuarded(`UPDATE claw_prs SET last_comment_id=?, last_comment_at=? WHERE id=? AND last_comment_id < ?`,
				[]interface{}{maxID, latestCommentAt, pr.id, maxID}, pr.clawID, pr.prNumber)
		} else {
			s.updateWatermarkGuarded(`UPDATE claw_prs SET last_comment_id=? WHERE id=? AND last_comment_id < ?`,
				[]interface{}{maxID, pr.id, maxID}, pr.clawID, pr.prNumber)
		}
	}
}

// hasNewComments reports whether commentsData has an ID above watermark.
func hasNewComments(commentsData []interface{}, watermark int64) bool {
	for _, c := range commentsData {
		comment, _ := c.(map[string]interface{})
		idF, _ := comment["id"].(float64)
		if int64(idF) > watermark {
			return true
		}
	}
	return false
}

// updateWatermarkGuarded advances a monotonic watermark, retrying once on a
// SQLite busy error. Zero rows affected means another writer already advanced
// the watermark at or past the requested value, which is not an error.
func (s *Server) updateWatermarkGuarded(query string, args []interface{}, clawID string, prNumber int) error {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		_, err = s.db.Exec(query, args...)
		if err == nil || !isSQLiteBusy(err) {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
	if err != nil {
		log.Printf("[pr-watcher] watermark update failed for claw %s pr #%d: %v", clawID[:8], prNumber, err)
	}
	return err
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
		s.updateWatermarkGuarded(`UPDATE claw_prs SET last_review_id=? WHERE id=? AND last_review_id < ?`,
			[]interface{}{maxID, pr.id, maxID}, pr.clawID, pr.prNumber)
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
		s.updateWatermarkGuarded(`UPDATE claw_prs SET last_review_comment_id=? WHERE id=? AND last_review_comment_id < ?`,
			[]interface{}{maxID, pr.id, maxID}, pr.clawID, pr.prNumber)
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
		workflowV2Controlled := s.workflowV2OwnsExecution(cc)
		acceptedForAgent = !workflowV2Controlled
		if !busy {
			// V1 delivers the row; V2 settles it as display-only.
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

// handleClawSubresource routes /api/claws/:id/prs, /api/claws/:id/checkpoints
// and /api/claws/:id/llm-limit.
func (s *Server) handleClawSubresource(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/claws/"), "/")
	if len(parts) < 2 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	clawID := parts[0]
	sub := parts[1]

	if sub != "prs" && sub != "checkpoints" && sub != "llm-limit" {
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
	if sub == "llm-limit" {
		s.handleClawLLMLimit(w, r, clawID)
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

	// Every row is returned — resolved and mention-only included — with the
	// state and the mention_only flag exposed, so the UI can distinguish
	// delivered work from merely-mentioned PRs the same way the finalization
	// gate (clawOpenPRCount) does.
	rows, err := s.db.Query(
		`SELECT id, repo, pr_number, pr_url, title, state, merged, merged_at, mention_only, created_at FROM claw_prs WHERE claw_id=? ORDER BY created_at ASC`,
		clawID,
	)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type PR struct {
		ID          string  `json:"id"`
		Repo        string  `json:"repo"`
		PRNumber    int     `json:"prNumber"`
		URL         string  `json:"url"`
		Title       string  `json:"title"`
		State       string  `json:"state"`
		Merged      bool    `json:"merged"`
		MergedAt    *string `json:"mergedAt,omitempty"`
		MentionOnly bool    `json:"mentionOnly"`
		CreatedAt   string  `json:"createdAt"`
	}
	var prs []PR
	for rows.Next() {
		var p PR
		if err := rows.Scan(&p.ID, &p.Repo, &p.PRNumber, &p.URL, &p.Title, &p.State, &p.Merged, &p.MergedAt, &p.MentionOnly, &p.CreatedAt); err != nil {
			continue
		}
		prs = append(prs, p)
	}
	if prs == nil {
		prs = []PR{}
	}
	jsonOK(w, prs)
}

// clawPRStoredState folds GitHub's two-way `state` (open|closed) and its
// separate `merged` flag into the three-way open|merged|closed value the
// dashboard's claw_prs.state column and /api/claws/:id/prs expose. GitHub
// reports state=="closed" for merged PRs too, so `merged` must be checked
// first or a merged PR would be indistinguishable from a rejected one.
func clawPRStoredState(githubState string, merged bool) string {
	if merged {
		return "merged"
	}
	return githubState
}

// clawOpenPRCount returns how many DELIVERED PRs tracked for the claw are
// still unresolved — neither merged nor closed. Teardown is gated on this
// being zero so an agent that delivered several PRs keeps watching the rest.
// Mention-only rows (PR URLs the agent merely mentioned in a message) are
// excluded: they keep being polled — CI, comments and reviews are still
// forwarded — but they never block finalization.
func (s *Server) clawOpenPRCount(clawID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id=? AND state NOT IN ('merged','closed') AND mention_only=0`, clawID).Scan(&n)
	return n, err
}

// clawPRIsMentionOnly reports whether a claw_prs row is mention-only. The read
// happens at decision time (not from a possibly stale clawPR loaded at the top
// of a poll) because a mention row can be upgraded to delivered mid-poll by a
// concurrent [DONE] registration. ok is false when the row cannot be read —
// callers must then never run terminal handling off the row.
func (s *Server) clawPRIsMentionOnly(prID string) (mentionOnly, ok bool) {
	var v int
	if err := s.db.QueryRow(`SELECT mention_only FROM claw_prs WHERE id=?`, prID).Scan(&v); err != nil {
		log.Printf("[pr-watcher] failed to read mention_only for PR row %s: %v", prID, err)
		return false, false
	}
	return v == 1, true
}

// clawHasDeliveredPR reports whether the claw tracks at least one DELIVERED
// (mention_only=0) row, in any state. The PR watcher may finalize a claw only
// when this is true: a claw whose rows are all mention-only has delivered
// nothing, so nothing it is watching can mean "the claw's work is done" — a
// stranger merging or closing a merely-mentioned PR must never destroy the
// sandbox or move the tracker issue. This is an explicit guard, not an
// emergent property of the mention-only early returns, so a future refactor
// of those returns cannot silently reintroduce the teardown.
func (s *Server) clawHasDeliveredPR(clawID string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id=? AND mention_only=0`, clawID).Scan(&n)
	return n > 0, err
}

// closeUnreachablePR marks a tracked PR row closed after checkPRMerged saw
// permanent API errors for prMergedPermanentFailureLimit consecutive polls,
// so an unreachable repo stops blocking claw finalization instead of pinning
// the claw forever. (Token-resolution misses are bounded separately in
// pollAllPRs and only ever close the row — they must never reach this
// escalating path.)
//
// The claw is stopped only for a delivered row, and only when no unresolved
// delivered PR remains and the claw has delivered at least one PR; otherwise
// it keeps watching the rest. Returns (resolved, terminated) with checkPRMerged
// semantics: resolved reports whether this row reached a terminal state.
func (s *Server) closeUnreachablePR(pr clawPR, cause string) (resolved, terminated bool) {
	log.Printf("[pr-watcher] WARN: PR %s (%s#%d) %s — marking it closed so it stops blocking finalization of claw %s", pr.prURL, pr.repo, pr.prNumber, cause, shortID(pr.clawID))
	if _, err := s.db.Exec(`UPDATE claw_prs SET state='closed' WHERE id=?`, pr.id); err != nil {
		log.Printf("[pr-watcher] failed to mark unreachable PR %s closed for claw %s: %v", pr.prURL, shortID(pr.clawID), err)
		return false, false
	}
	// A mention-only row must never TRIGGER finalization: it is a polling
	// target only, and its unreachability says nothing about the claw's own
	// delivered work. (An unreadable flag also never triggers — fail alive.)
	if mentionOnly, ok := s.clawPRIsMentionOnly(pr.id); !ok || mentionOnly {
		return true, false
	}
	remaining, err := s.clawOpenPRCount(pr.clawID)
	if err != nil {
		// Never tear a claw down on an unknown PR count — the next poll retries.
		log.Printf("[pr-watcher] failed to count open PRs for claw %s: %v", shortID(pr.clawID), err)
		return true, false
	}
	if remaining > 0 {
		log.Printf("[pr-watcher] claw %s still has %d unresolved delivered PR(s) — keeping it alive", shortID(pr.clawID), remaining)
		return true, false
	}
	if delivered, err := s.clawHasDeliveredPR(pr.clawID); err != nil || !delivered {
		// See clawHasDeliveredPR: zero delivered rows means the watcher has no
		// authority to stop this claw, whatever happened to mentioned PRs.
		if err != nil {
			log.Printf("[pr-watcher] failed to count delivered PRs for claw %s: %v", shortID(pr.clawID), err)
		}
		return true, false
	}
	go s.stopAgentWithReason(pr.clawID, fmt.Sprintf("PR %s has been %s", pr.prURL, cause), false)
	return true, true
}

// checkPRMerged checks if a tracked PR is merged or closed.
//
// resolved is true when THIS row reached a terminal state (merged, closed, or
// closed administratively after prMergedPermanentFailureLimit consecutive
// permanent API errors: 404/410/401/non-rate-limit 403 — repo/PR deleted or
// GitHub App uninstalled). Callers must skip further polling work for a
// resolved row.
//
// terminated is true when the claw itself is being torn down, which happens
// only once every delivered tracked PR is resolved. While at least one
// delivered PR is still open the claw stays alive and keeps watching.
//
// A mention-only row resolves silently: its state is persisted and nothing
// else runs — no per-PR analytics, no pipeline transition, no tracker move,
// no teardown. A claw with zero delivered rows is never finalized here at all
// (see clawHasDeliveredPR).
func (s *Server) checkPRMerged(pr clawPR, token string) (resolved, terminated bool) {
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
				// One inaccessible PR must not kill a claw whose other PRs are
				// fine: close this row so it stops blocking finalization, and
				// stop the claw only when no delivered PR remains unresolved.
				return s.closeUnreachablePR(pr,
					fmt.Sprintf("inaccessible (HTTP %d) for %d consecutive polls — repo or PR deleted, or GitHub App uninstalled", apiErr.StatusCode, prMergedPermanentFailureLimit))
			}
			return false, false
		}
		// Transient error (5xx, network): reset the counter so only genuinely
		// consecutive permanent failures accumulate toward the limit.
		_, _ = s.db.Exec(`UPDATE claw_prs SET permanent_failure_count=0 WHERE id=? AND permanent_failure_count != 0`, pr.id)
		return false, false
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
	title, _ := data["title"].(string)
	merged, _ := data["merged"].(bool)
	draft, _ := data["draft"].(bool)
	mergedAtValue, _ := data["merged_at"].(string)
	createdAtValue, _ := data["created_at"].(string)
	mergedAt := parseRFC3339Timestamp(mergedAtValue)
	var mergedAtDB any
	if mergedAtValue != "" {
		mergedAtDB = mergedAtValue
	}
	storedState := clawPRStoredState(state, merged)
	if _, err := s.db.Exec(`UPDATE claw_prs SET title=?, state=?, merged=?, merged_at=? WHERE id=? AND NOT (title=? AND state=? AND merged=? AND merged_at IS ?)`, title, storedState, merged, mergedAtDB, pr.id, title, storedState, merged, mergedAtDB); err != nil {
		log.Printf("[pr-watcher] update PR state for %s: %v", pr.prURL, err)
	}
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
		// While the PR is still open, surface merge conflicts once per episode.
		s.checkPRMergeConflict(pr, data)
		return false, false // still open
	}

	clawID := pr.clawID
	var tenantID string
	if err := s.db.QueryRow(`SELECT tenant_id FROM claws WHERE id=?`, clawID).Scan(&tenantID); err != nil {
		return false, false
	}

	// If the PR was closed without merging, mark the row resolved and decide
	// whether the claw is finished: while other tracked PRs are still open the
	// claw stays alive and keeps watching them.
	if state == "closed" && !merged {
		// The row must survive (not be deleted) so the all-PRs-resolved
		// bookkeeping stays correct; the poll queries exclude resolved states.
		if _, err := s.db.Exec(`UPDATE claw_prs SET state='closed' WHERE id=?`, pr.id); err != nil {
			log.Printf("[pr-watcher] failed to mark PR %s closed for claw %s: %v", pr.prURL, shortID(clawID), err)
			return false, false
		}

		// A mention-only row resolves silently: persist the state (done above)
		// and stop. A stranger closing a PR the agent merely linked must not
		// fire per-PR analytics, pipeline transitions, or any stop path.
		// (Unreadable flag: fail alive, run no terminal handling.)
		if mentionOnly, ok := s.clawPRIsMentionOnly(pr.id); !ok || mentionOnly {
			return true, false
		}

		pipelineCtx, hasPipelineCtx := s.findPipelineContextForClaw(clawID)
		if hasPipelineCtx {
			s.trackPRClosed(pipelineCtx.Name(), pipelineCtx.IssueID, clawID, pr.repo, pr.prNumber)
		}

		remaining, err := s.clawOpenPRCount(clawID)
		if err != nil {
			// Never tear a claw down on an unknown PR count — the next poll retries.
			log.Printf("[pr-watcher] failed to count open PRs for claw %s: %v", shortID(clawID), err)
			return true, false
		}
		if remaining > 0 {
			log.Printf("[pr-watcher] PR %s#%d closed without merge — claw %s still has %d open PR(s), not stopping", pr.repo, pr.prNumber, clawID[:8], remaining)
			// External inject: a paused (no-progress) claw must be woken so it
			// can react to the close instead of sitting on the remaining PRs.
			s.injectExternalHubMessageByID(clawID, fmt.Sprintf("[hub] PR %s was closed without being merged. Still watching %d other open PR(s).", pr.prURL, remaining))
			return true, false
		}

		if delivered, err := s.clawHasDeliveredPR(clawID); err != nil || !delivered {
			// See clawHasDeliveredPR: never finalize a claw with zero
			// delivered rows, whatever happened to mentioned PRs.
			if err != nil {
				log.Printf("[pr-watcher] failed to count delivered PRs for claw %s: %v", shortID(clawID), err)
			}
			return true, false
		}

		log.Printf("[pr-watcher] PR %s#%d closed without merge — stopping claw %s", pr.repo, pr.prNumber, clawID[:8])

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
			// Mirror the merged teardown: stopAgentWithReason can end in a claw
			// retry that reuses this claw id, and surviving resolved rows would
			// permanently suppress the agent-idle stuck alert for the retried
			// claw (its consumers read "row exists" as "awaiting humans").
			_, _ = s.db.Exec(`DELETE FROM claw_prs WHERE claw_id=?`, clawID)
			go s.stopAgentWithReason(clawID, fmt.Sprintf("PR %s was closed without being merged", pr.prURL), false)
		}
		return true, true
	}

	// PR was merged — make the resolved state durable before gating on it. The
	// general UPDATE earlier in this function is conditional and could be
	// skipped; the all-PRs-resolved gate must not depend on it.
	if _, err := s.db.Exec(`UPDATE claw_prs SET state='merged', merged=1 WHERE id=?`, pr.id); err != nil {
		log.Printf("[pr-watcher] failed to mark PR %s merged for claw %s: %v", pr.prURL, shortID(clawID), err)
		return false, false
	}

	// A mention-only row resolves silently: persist the state (done above) and
	// stop. A stranger merging a PR the agent merely linked must not fire
	// per-PR analytics, the pipeline stage transition, the DoneStatus tracker
	// move, or any teardown. (Unreadable flag: fail alive, run no terminal
	// handling.)
	if mentionOnly, ok := s.clawPRIsMentionOnly(pr.id); !ok || mentionOnly {
		return true, false
	}

	// Track analytics for PR merge. These are per-PR facts and fire on every
	// merge, regardless of whether the claw is finished yet.
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

	// Finalize only when every delivered tracked PR is resolved (merged or closed).
	remaining, err := s.clawOpenPRCount(clawID)
	if err != nil {
		// Never tear a claw down on an unknown PR count — the next poll retries.
		log.Printf("[pr-watcher] failed to count open PRs for claw %s: %v", shortID(clawID), err)
		return true, false
	}
	if remaining > 0 {
		log.Printf("[pr-watcher] PR %s#%d merged — claw %s still has %d open PR(s), not terminating", pr.repo, pr.prNumber, clawID[:8], remaining)
		// External inject: a paused (no-progress) claw must be woken so it can
		// act on the partial merge instead of staying paused with PRs open.
		s.injectExternalHubMessageByID(clawID, fmt.Sprintf("[hub] PR %s merged. Still watching %d other open PR(s) — will finish when they are all merged or closed.", pr.prURL, remaining))
		return true, false
	}

	if delivered, err := s.clawHasDeliveredPR(clawID); err != nil || !delivered {
		// See clawHasDeliveredPR: never finalize a claw with zero delivered
		// rows, whatever happened to mentioned PRs.
		if err != nil {
			log.Printf("[pr-watcher] failed to count delivered PRs for claw %s: %v", shortID(clawID), err)
		}
		return true, false
	}

	log.Printf("[pr-watcher] PR %s#%d merged — terminating claw %s", pr.repo, pr.prNumber, clawID[:8])

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
		return true, true
	}

	var providerID, provider string
	_ = s.db.QueryRow(`SELECT COALESCE(provider_id,''), COALESCE(provider,'') FROM claws WHERE id=?`, clawID).Scan(&providerID, &provider)

	s.checkpointBeforeTermination(clawID, "pr-merged")

	_, _ = s.db.Exec(`DELETE FROM claw_prs WHERE claw_id=?`, clawID)
	applied, err := s.finishClawTerminalTx(clawID, "deleted", "", "completed", "PR merged", terminalTxOpts{})
	if err != nil || !applied {
		return true, false
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

	return true, true
}

// checkPRMergeConflict watches the mergeable_state field on an open PR and
// notifies the agent once when the PR becomes "dirty" (merge conflict). It is
// called from checkPRMerged while the PR is still open, so it never runs for
// closed or merged PRs. Mention-only rows are ignored: a conflict on a PR the
// agent merely mentioned must not interrupt the agent's own work.
func (s *Server) checkPRMergeConflict(pr clawPR, data map[string]interface{}) {
	if pr.mentionOnly {
		return
	}
	mergeableState, _ := data["mergeable_state"].(string)
	if mergeableState == "" {
		return
	}

	// Read the persisted watermark so a stale in-memory clawPR or a concurrent
	// poll cannot cause duplicate notifications.
	var oldState string
	if err := s.db.QueryRow(`SELECT last_mergeable_state FROM claw_prs WHERE id=?`, pr.id).Scan(&oldState); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("[pr-watcher] failed to read mergeable state for %s: %v", pr.prURL, err)
		}
		return
	}
	if mergeableState == oldState {
		return
	}

	// Notify only on a genuine transition to dirty from a known non-dirty state.
	shouldNotify := mergeableState == "dirty" && oldState != "dirty" && oldState != ""

	// Use a conditional update so only the poll that actually changes the
	// watermark delivers the notification.
	res, err := s.db.Exec(`UPDATE claw_prs SET last_mergeable_state=? WHERE id=? AND last_mergeable_state != ?`, mergeableState, pr.id, mergeableState)
	if err != nil {
		log.Printf("[pr-watcher] failed to update mergeable state for %s: %v", pr.prURL, err)
		return
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return
	}
	if shouldNotify {
		msg := fmt.Sprintf("PR #%d ([%s](%s)) is now in a merge conflict. Please resolve the conflicts on the same branch.",
			pr.prNumber, pr.repo, pr.prURL)
		s.injectUserMessage(pr.clawID, msg)
	}
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
