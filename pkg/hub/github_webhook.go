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
	"os"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
)

// githubPRPayload holds the relevant fields from a GitHub pull_request webhook event.
type githubPRPayload struct {
	Action string `json:"action"` // "opened", "synchronize", "reopened", "closed"
	Number int    `json:"number"`
	Sender struct {
		Login string `json:"login"`
		Type  string `json:"type"` // "Bot", "User", "Organization"
	} `json:"sender"`
	PullRequest struct {
		HTMLURL   string `json:"html_url"`
		Title     string `json:"title"`
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
		MergedAt  string `json:"merged_at"`
		ClosedAt  string `json:"closed_at"`
		Merged    bool   `json:"merged"`
		Draft     bool   `json:"draft"`
		User      struct {
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
	case "issues":
		var payload githubIssuesWebhookPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			log.Printf("[github-webhook] failed to parse issues payload: %v", err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		log.Printf("[github-webhook] issues action=%q repo=%q issue=#%d author=%q",
			payload.Action, payload.Repository.FullName, payload.Issue.Number, payload.Issue.User.Login)
		s.safeGo("github issue webhook", func() { s.processGitHubIssueEvent(payload) })
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
		s.safeGo("github PR webhook", func() { s.processGitHubPREvent(payload) })
	case "pull_request_review_comment":
		var payload githubPRReviewCommentPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			log.Printf("[github-webhook] failed to parse pull_request_review_comment payload: %v", err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		log.Printf("[github-webhook] pull_request_review_comment action=%q repo=%q pr=#%d author=%q",
			payload.Action, payload.Repository.FullName, payload.PullRequest.Number, payload.Comment.User.Login)
		s.safeGo("github PR review comment webhook", func() { s.processGitHubPRReviewCommentEvent(payload) })
	case "pull_request_review":
		var payload githubPRReviewPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			log.Printf("[github-webhook] failed to parse pull_request_review payload: %v", err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		log.Printf("[github-webhook] pull_request_review action=%q state=%q repo=%q pr=#%d author=%q",
			payload.Action, payload.Review.State, payload.Repository.FullName, payload.PullRequest.Number, payload.Review.User.Login)
		s.safeGo("github PR review webhook", func() { s.processGitHubPRReviewEvent(payload) })
	case "issue_comment":
		var payload githubIssueCommentPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			log.Printf("[github-webhook] failed to parse issue_comment payload: %v", err)
			break
		}
		if payload.Issue.PullRequest.URL != "" {
			// Comment on a pull request
			log.Printf("[github-webhook] issue_comment action=%q repo=%q pr=#%d author=%q",
				payload.Action, payload.Repository.FullName, payload.Issue.Number, payload.Comment.User.Login)
			s.safeGo("github issue comment webhook", func() { s.processGitHubIssueCommentEvent(payload) })
		} else {
			// Comment on a plain issue (not PR)
			log.Printf("[github-webhook] issue_comment action=%q repo=%q issue=#%d author=%q",
				payload.Action, payload.Repository.FullName, payload.Issue.Number, payload.Comment.User.Login)
			s.safeGo("github issue comment webhook", func() { s.processGitHubIssueComment(payload) })
		}
	case "check_run", "check_suite":
		var payload githubCheckPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			log.Printf("[github-webhook] failed to parse %s payload: %v", event, err)
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		headSHA, prNumbers, conclusion := payload.checkEventSummary(event)
		log.Printf("[github-webhook] %s action=%q repo=%q sha=%q conclusion=%q prs=%d",
			event, payload.Action, payload.Repository.FullName, headSHA, conclusion, len(prNumbers))
		s.safeGo("github check webhook", func() { s.processGitHubCheckEvent(event, payload) })
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
	secrets := s.hubCfg.Secrets
	s.mu.RUnlock()

	factories := s.resolveFactories()
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
	s.processWorkflowV2GitHubPREvent(payload)
	factories := s.resolveFactories()

	repoFullName := payload.Repository.FullName
	log.Printf("[github-webhook] processing PR event: repo=%q action=%q — checking %d factories", repoFullName, payload.Action, len(factories))

	// ready_at is a property of the task run, not of any factory trigger: a hub
	// whose factories are all issue-triggered still needs the authoritative
	// timestamp for its own draft PRs. Record it before the factory loop.
	if payload.Action == "ready_for_review" {
		s.recordGitHubPRReadyForReview(payload)
	}

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
		currentLabels, labelsOK := s.githubFactoryPRLabels(factory, repoFullName, payload.Number)
		if !labelsOK {
			continue
		}
		if !labelsAllowed(currentLabels, factory.Labels, factory.ExcludeLabels) {
			log.Printf("[github-webhook] factory %q: skipped (PR backing issue labels did not match)", factory.Name)
			continue
		}

		// Check if a claw already exists for this PR
		existingClawID := s.findClawForGitHubPR(payload.PullRequest.HTMLURL)
		if existingClawID != "" {
			if payload.Action == "synchronize" {
				// Run the same detection as the poller (SHA comparison against
				// the agent baseline plus commit attribution) rather than
				// trusting the webhook sender: "Update branch" base merges and
				// agent pushes must advance the baseline, not count as human.
				if _, runID, _, ok, err := s.taskRunContextForClaw(existingClawID); err == nil && ok {
					s.detectHumanCodePush(existingClawID, runID, repoFullName, payload.Number,
						payload.PullRequest.HTMLURL, payload.PullRequest.Head.SHA, s.resolveGitHubIssuesTokenForFactory(factory))
				}
			}
			if payload.Action == "closed" {
				if _, runID, _, ok, err := s.taskRunContextForClaw(existingClawID); err == nil && ok {
					at := parseRFC3339Timestamp(payload.PullRequest.MergedAt)
					if !payload.PullRequest.Merged {
						at = parseRFC3339Timestamp(payload.PullRequest.ClosedAt)
					}
					_ = s.associateTaskRunPR(TaskRunPR{RunID: runID, Repo: repoFullName, PRNumber: payload.Number, URL: payload.PullRequest.HTMLURL, State: taskRunPRStateClosed, Merged: payload.PullRequest.Merged, OccurredAt: at})
				}
			}
			switch payload.Action {
			case "closed":
				// Let the pr_watcher handle merged/closed — don't double-inject
			case "synchronize", "reopened", "review_requested":
				// Skip synchronize events triggered by the claw itself pushing commits.
				// Only skip if the sender is our own GitHub App bot, not other bots (e.g. dependabot).
				if payload.Action == "synchronize" && s.isOwnAppBot(payload.Sender.Login) {
					log.Printf("[factory:%s] github PR #%d synchronize from own app bot %q — skipping", factory.Name, payload.Number, payload.Sender.Login)
					continue
				}
				// Only inject if the claw is connected and ready — otherwise the
				// claw hasn't started yet and the update is redundant (claw gets
				// full context from BOOTSTRAP on startup).
				s.mu.RLock()
				cc, connected := s.claws[existingClawID]
				ready := connected && cc.gatewayReady
				s.mu.RUnlock()
				if !ready {
					log.Printf("[factory:%s] github PR #%d action=%s — claw %s not ready yet, skipping inject", factory.Name, payload.Number, payload.Action, existingClawID[:8])
				} else {
					msg := fmt.Sprintf("PR #%d update: %s.", payload.Number, payload.Action)
					if payload.Action == "synchronize" {
						msg = fmt.Sprintf("PR #%d was updated with new commits from a human. Review the latest changes.", payload.Number)
					} else if payload.Action == "reopened" {
						msg = fmt.Sprintf("PR #%d was reopened.", payload.Number)
					}
					log.Printf("[factory:%s] github PR #%d action=%s — injecting into claw %s", factory.Name, payload.Number, payload.Action, existingClawID[:8])
					s.injectExternalHubMessageByID(existingClawID, msg)
				}
			default:
				log.Printf("[factory:%s] github PR #%d action=%s — claw exists, no action taken", factory.Name, payload.Number, payload.Action)
			}
			continue
		}

		// A closed PR may belong to a claw that has already been deleted,
		// errored, or gone offline. Such claws are intentionally excluded by
		// findClawForGitHubPR, but its task-run PR outcome still needs recording.
		if payload.Action == "closed" {
			if runID, ok := s.findOpenTaskRunPR(repoFullName, payload.Number); ok {
				at := parseRFC3339Timestamp(payload.PullRequest.MergedAt)
				if !payload.PullRequest.Merged {
					at = parseRFC3339Timestamp(payload.PullRequest.ClosedAt)
				}
				if err := s.associateTaskRunPR(TaskRunPR{RunID: runID, Repo: repoFullName, PRNumber: payload.Number, URL: payload.PullRequest.HTMLURL, State: taskRunPRStateClosed, Merged: payload.PullRequest.Merged, OccurredAt: at}); err != nil {
					log.Printf("[github-webhook] failed to record dead-claw PR outcome for run %s, %s#%d: %v", runID, repoFullName, payload.Number, err)
				} else {
					log.Printf("[github-webhook] recorded closed PR outcome for dead-claw run %s, %s#%d (merged=%v)", runID, repoFullName, payload.Number, payload.PullRequest.Merged)
				}
				continue
			}
		}

		// No existing claw — create one (regardless of action, first event wins)
		log.Printf("[factory:%s] github PR #%d in %s (action=%s) — creating claw", factory.Name, payload.Number, repoFullName, payload.Action)
		if err := s.createClawForGitHubPR(factory, payload, "github-pr webhook"); err != nil {
			if isFactoryTriggerAlreadyClaimed(err) {
				continue
			}
			log.Printf("[factory:%s] failed to create claw for PR #%d: %v", factory.Name, payload.Number, err)
			s.logFactoryEvent(factory.Name, fmt.Sprintf("%s#%d", repoFullName, payload.Number),
				payload.PullRequest.Title, "", payload.Action, "error", "", err.Error())
		}
	}
}

// recordGitHubPRReadyForReview stores the authoritative ready-for-review
// timestamp for the task run behind a PR. It runs independently of factory
// trigger configuration and of whether the claw is still alive, because the
// ready→merge metric must be recorded for every hub-authored PR.
func (s *Server) recordGitHubPRReadyForReview(payload githubPRPayload) {
	at := parseRFC3339Timestamp(payload.PullRequest.UpdatedAt)
	if at.IsZero() {
		return
	}
	repoFullName := payload.Repository.FullName
	runID := ""
	if clawID := s.findClawForGitHubPR(payload.PullRequest.HTMLURL); clawID != "" {
		if _, id, _, ok, err := s.taskRunContextForClaw(clawID); err == nil && ok {
			runID = id
		}
	}
	if runID == "" {
		id, ok := s.findOpenTaskRunPR(repoFullName, payload.Number)
		if !ok {
			return
		}
		runID = id
	}
	if err := s.associateTaskRunPR(TaskRunPR{RunID: runID, Repo: repoFullName, PRNumber: payload.Number, URL: payload.PullRequest.HTMLURL, State: taskRunPRStateOpen, ReadyAt: epochMillis(at), AuthoritativeReadyAt: true}); err != nil {
		log.Printf("[github-webhook] failed to record PR ready time for run %s, %s#%d: %v", runID, repoFullName, payload.Number, err)
	}
}

// findOpenTaskRunPR returns the run ID of an open task-run PR regardless of
// whether its backing claw is still alive. This lets closed PR webhooks update
// task runs after the claw has been deleted, errored, or gone offline.
func (s *Server) findOpenTaskRunPR(repo string, prNumber int) (runID string, ok bool) {
	err := s.db.QueryRow(
		`SELECT run_id FROM task_run_prs WHERE repo = ? AND pr_number = ? AND state = 'open' ORDER BY opened_at DESC LIMIT 1`,
		repo, prNumber,
	).Scan(&runID)
	if err != nil {
		return "", false
	}
	return runID, runID != ""
}

// githubPRReviewCommentPayload holds fields from a pull_request_review_comment webhook event.
type githubPRReviewCommentPayload struct {
	Action  string `json:"action"` // "created", "edited", "deleted"
	Comment struct {
		ID      int64  `json:"id"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		Path    string `json:"path"`
		Line    int    `json:"line"`
		User    struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
	} `json:"comment"`
	PullRequest struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// githubPRReviewPayload holds fields from a pull_request_review webhook event.
type githubPRReviewPayload struct {
	Action string `json:"action"` // "submitted", "edited", "dismissed"
	Review struct {
		ID      int64  `json:"id"`
		State   string `json:"state"` // "changes_requested", "approved", "commented"
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		User    struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
	} `json:"review"`
	PullRequest struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// githubCheckPayload holds the fields we need from check_run and check_suite
// webhook events. One struct serves both: the handler reads the sub-object that
// matches the event name.
type githubCheckPayload struct {
	Action   string `json:"action"` // "completed", "created", "rerequested", "requested"
	CheckRun struct {
		Name         string `json:"name"`
		Status       string `json:"status"`
		Conclusion   string `json:"conclusion"`
		HeadSHA      string `json:"head_sha"`
		PullRequests []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	} `json:"check_run"`
	CheckSuite struct {
		Status       string `json:"status"`
		Conclusion   string `json:"conclusion"`
		HeadSHA      string `json:"head_sha"`
		PullRequests []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	} `json:"check_suite"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// checkEventSummary extracts the head SHA, PR numbers and conclusion for the
// sub-object matching the event name.
func (p githubCheckPayload) checkEventSummary(event string) (headSHA string, prNumbers []int, conclusion string) {
	if event == "check_suite" {
		headSHA, conclusion = p.CheckSuite.HeadSHA, p.CheckSuite.Conclusion
		for _, pr := range p.CheckSuite.PullRequests {
			prNumbers = append(prNumbers, pr.Number)
		}
		return headSHA, prNumbers, conclusion
	}
	headSHA, conclusion = p.CheckRun.HeadSHA, p.CheckRun.Conclusion
	for _, pr := range p.CheckRun.PullRequests {
		prNumbers = append(prNumbers, pr.Number)
	}
	return headSHA, prNumbers, conclusion
}

// processGitHubCheckEvent turns a completed check_run/check_suite delivery into
// an immediate CI re-evaluation for the tracked PR, so a claw waiting on CI is
// woken in seconds instead of on the next poller tick (30s–5min).
//
// The event is only a *trigger*: the verdict itself always comes from
// checkCIStatus, which re-reads the full check-run set for the head SHA. A
// single green check_run says nothing about the sibling checks still running,
// and the payload conclusion must never be used to announce green.
//
// Because both paths end in the same conditional UPDATE on claw_prs.id, the
// webhook and the poller cannot double-notify the same (sha, conclusion): the
// loser sees RowsAffected()==0 and returns silently. The poller therefore stays
// as the fallback for missed or unsubscribed deliveries.
func (s *Server) processGitHubCheckEvent(event string, payload githubCheckPayload) {
	// Only terminal events can change the verdict; created/rerequested/requested
	// would spend two GitHub API calls on a guaranteed no-op.
	if payload.Action != "completed" {
		return
	}
	s.processWorkflowV2GitHubCheckEvent(event, payload)

	headSHA, prNumbers, _ := payload.checkEventSummary(event)
	repo := payload.Repository.FullName
	// GitHub omits pull_requests when the head repository differs from the
	// check's repository (notably fork PRs). Nothing to resolve then — the
	// poller remains the fallback for those PRs.
	if headSHA == "" || len(prNumbers) == 0 {
		log.Printf("[github-webhook] %s completed repo=%q sha=%q — no pull_requests in payload, leaving it to the poller", event, repo, headSHA)
		return
	}

	for _, number := range prNumbers {
		// A PR can be tracked by more than one live claw; the poller evaluates
		// every row, so the webhook must too or the extra claws would only ever
		// be woken by the (possibly paused) poller.
		prs := s.loadClawPRsByNumber(repo, number)
		if len(prs) == 0 {
			log.Printf("[github-webhook] %s completed on %s#%d — not an agent-tracked PR, ignored", event, repo, number)
			continue
		}
		for _, pr := range prs {
			// Repo-scoped only — do not fall back to an unscoped token from a
			// different workspace app (wrong-org 404s look like deleted PRs).
			token := s.tokenForRepo(pr.repo)
			if token == "" {
				log.Printf("[github-webhook] CRITICAL: GitHub token resolution failed; cannot check CI for %s#%d", repo, number)
				continue
			}
			s.checkCIStatus(pr, token)
		}
	}
}

func (s *Server) processGitHubPRReviewCommentEvent(payload githubPRReviewCommentPayload) {
	s.processWorkflowV2GitHubReviewCommentEvent(payload)
	if payload.Action != "created" {
		return
	}
	if !s.isHumanGitHubActor(payload.Comment.User.Login, payload.Comment.User.Type) {
		return
	}
	prURL := payload.PullRequest.HTMLURL
	clawID := s.findClawForGitHubPR(prURL)
	if clawID == "" {
		log.Printf("[github-webhook] review_comment on PR #%d — no claw found for %s", payload.PullRequest.Number, prURL)
		return
	}
	pr := clawPR{
		clawID:   clawID,
		repo:     payload.Repository.FullName,
		prNumber: payload.PullRequest.Number,
		prURL:    prURL,
	}
	s.forwardHumanReviewComment(pr, payload.Comment.ID, payload.Comment.User.Login, payload.Comment.Body, payload.Comment.HTMLURL, payload.Comment.Path, payload.Comment.Line)
}

func (s *Server) processGitHubPRReviewEvent(payload githubPRReviewPayload) {
	s.processWorkflowV2GitHubReviewEvent(payload)
	if payload.Action != "submitted" || !strings.EqualFold(payload.Review.State, "changes_requested") {
		return
	}
	if !s.isHumanGitHubActor(payload.Review.User.Login, payload.Review.User.Type) {
		return
	}
	prURL := payload.PullRequest.HTMLURL
	clawID := s.findClawForGitHubPR(prURL)
	if clawID == "" {
		log.Printf("[github-webhook] review on PR #%d — no claw found for %s", payload.PullRequest.Number, prURL)
		return
	}
	pr := clawPR{
		clawID:   clawID,
		repo:     payload.Repository.FullName,
		prNumber: payload.PullRequest.Number,
		prURL:    prURL,
	}
	s.forwardHumanRequestedChangesReview(pr, payload.Review.ID, payload.Review.User.Login, payload.Review.Body, payload.Review.HTMLURL)
}

// githubIssueCommentPayload holds fields from an issue_comment webhook event.
type githubIssueCommentPayload struct {
	Action string `json:"action"` // "created", "edited", "deleted"
	Issue  struct {
		Number      int    `json:"number"`
		HTMLURL     string `json:"html_url"` // web URL for the issue
		PullRequest struct {
			URL     string `json:"url"`      // non-empty only for PR comments
			HTMLURL string `json:"html_url"` // web URL for the PR
		} `json:"pull_request"`
	} `json:"issue"`
	Comment struct {
		ID      int64  `json:"id"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		User    struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
	} `json:"comment"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Sender struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"sender"`
}

// processGitHubIssueComment handles issue_comment events on plain issues (not PRs).
// If a claw exists for the issue, it injects the comment.
func (s *Server) processGitHubIssueComment(payload githubIssueCommentPayload) {
	if payload.Action != "created" {
		return
	}
	// Skip bot comments to avoid loops
	if strings.EqualFold(payload.Comment.User.Type, "bot") {
		return
	}

	issueURL := payload.Issue.HTMLURL
	commentMsg := fmt.Sprintf("**@%s** commented on issue #%d:\n> %s\n[View](%s)",
		payload.Comment.User.Login, payload.Issue.Number,
		strings.TrimSpace(payload.Comment.Body), payload.Comment.HTMLURL)

	existingClawID := s.findClawForGitHubIssue(issueURL)
	if existingClawID != "" {
		log.Printf("[github-webhook] issue_comment on issue #%d — injecting into existing claw %s", payload.Issue.Number, existingClawID[:8])
		s.injectExternalHubMessageByID(existingClawID, commentMsg)
		return
	}

	log.Printf("[github-webhook] issue_comment on issue #%d — no claw found for %s", payload.Issue.Number, issueURL)
}

// processGitHubIssueCommentEvent handles issue_comment events on PRs.
// If a claw exists for the PR, it injects the comment. If not, it tries to create a claw
// (useful when a PR was opened before the webhook was configured or the opened event was missed).
func (s *Server) processGitHubIssueCommentEvent(payload githubIssueCommentPayload) {
	s.processWorkflowV2GitHubIssueCommentEvent(payload)
	if payload.Action != "created" {
		return // only care about new comments
	}
	// Skip bot comments to avoid loops
	if strings.EqualFold(payload.Comment.User.Type, "bot") {
		return
	}

	factories := s.resolveFactories()

	repoFullName := payload.Repository.FullName
	prNumber := payload.Issue.Number

	prURL := payload.Issue.PullRequest.HTMLURL
	commentMsg := fmt.Sprintf("**@%s** commented on PR #%d:\n> %s\n[View](%s)",
		payload.Comment.User.Login, prNumber,
		strings.TrimSpace(payload.Comment.Body), payload.Comment.HTMLURL)

	// Check if a claw already exists for this PR
	existingClawID := s.findClawForGitHubPR(prURL)
	if existingClawID != "" {
		// Inject the comment into the existing claw
		log.Printf("[github-webhook] issue_comment on PR #%d — injecting into existing claw %s", prNumber, existingClawID[:8])
		s.injectExternalHubMessageByID(existingClawID, commentMsg)
		return
	}

	// No claw yet — try to create one if a factory matches this repo
	for _, factory := range factories {
		if factory.Integration != "github" || factory.Trigger == nil {
			continue
		}
		if factory.Trigger.On != "pull_request" {
			continue
		}
		if factory.Enabled != nil && !*factory.Enabled {
			continue
		}
		if !githubRepoMatches(repoFullName, factory.Repos) {
			continue
		}
		// Fetch PR details — we need them before we can apply author/base_branch filters.
		token := s.resolveGitHubTokenForRepo(repoFullName)
		if token == "" {
			continue
		}
		ghBase := s.githubBaseURL
		if ghBase == "" {
			ghBase = "https://api.github.com"
		}
		data, err := githubAPIWithBase(ghBase, fmt.Sprintf("repos/%s/pulls/%d", repoFullName, prNumber), token)
		if err != nil {
			log.Printf("[github-webhook] issue_comment: failed to fetch PR %s#%d: %v", repoFullName, prNumber, err)
			continue
		}
		var prPayload githubPRPayload
		prPayload.Action = "comment_triggered"
		prPayload.Number = prNumber
		prPayload.Repository.FullName = repoFullName
		prPayload.PullRequest.HTMLURL, _ = data["html_url"].(string)
		prPayload.PullRequest.Title, _ = data["title"].(string)
		if user, ok := data["user"].(map[string]interface{}); ok {
			prPayload.PullRequest.User.Login, _ = user["login"].(string)
		}
		if head, ok := data["head"].(map[string]interface{}); ok {
			prPayload.PullRequest.Head.Ref, _ = head["ref"].(string)
			prPayload.PullRequest.Head.SHA, _ = head["sha"].(string)
		}
		if base, ok := data["base"].(map[string]interface{}); ok {
			prPayload.PullRequest.Base.Ref, _ = base["ref"].(string)
		}
		if prPayload.PullRequest.HTMLURL == "" {
			continue
		}
		// Apply filters on the PR itself (not the commenter)
		if factory.Trigger.Filter != nil {
			f := factory.Trigger.Filter
			// Author filter: check PR author, not commenter
			if f.Author != "" && !strings.EqualFold(f.Author, prPayload.PullRequest.User.Login) {
				log.Printf("[github-webhook] factory %q: issue_comment skipped (PR author mismatch: want %q, got %q)",
					factory.Name, f.Author, prPayload.PullRequest.User.Login)
				continue
			}
			if f.BaseBranch != "" && !strings.EqualFold(f.BaseBranch, prPayload.PullRequest.Base.Ref) {
				log.Printf("[github-webhook] factory %q: issue_comment skipped (base_branch mismatch: want %q, got %q)",
					factory.Name, f.BaseBranch, prPayload.PullRequest.Base.Ref)
				continue
			}
		}
		currentLabels, labelsOK := s.githubFactoryPRLabels(factory, repoFullName, prNumber)
		if !labelsOK {
			continue
		}
		if !labelsAllowed(currentLabels, factory.Labels, factory.ExcludeLabels) {
			log.Printf("[github-webhook] factory %q: issue_comment skipped (PR backing issue labels did not match)", factory.Name)
			continue
		}
		log.Printf("[factory:%s] issue_comment triggered claw creation for PR %s#%d", factory.Name, repoFullName, prNumber)
		if err := s.createClawForGitHubPR(factory, prPayload, "github-pr webhook (issue_comment)"); err != nil {
			if isFactoryTriggerAlreadyClaimed(err) {
				continue
			}
			log.Printf("[factory:%s] failed to create claw for PR #%d: %v", factory.Name, prNumber, err)
			continue // let next matching factory try
		}
		createdClawID := s.findClawForGitHubPR(prPayload.PullRequest.HTMLURL)
		if createdClawID != "" {
			log.Printf("[github-webhook] issue_comment on PR #%d — injecting into newly created claw %s", prNumber, createdClawID[:8])
			s.injectHubMessageByID(createdClawID, commentMsg)
		}
		break // success — one factory match is enough
	}
}

func (s *Server) processGitHubIssueEvent(payload githubIssuesWebhookPayload) {
	if payload.Issue.Number == 0 {
		return
	}
	factories := s.resolveFactories()
	repoFullName := payload.Repository.FullName
	issueID := fmt.Sprintf("%s/%d", repoFullName, payload.Issue.Number)
	currentLabels := make([]string, 0, len(payload.Issue.Labels))
	for _, label := range payload.Issue.Labels {
		currentLabels = append(currentLabels, label.Name)
	}

	for _, factory := range factories {
		if factory.Integration != "github" {
			continue
		}
		if factory.Enabled != nil && !*factory.Enabled {
			continue
		}
		if factory.Trigger == nil || factory.Trigger.On != "issue" {
			continue
		}
		if factory.Trigger.Action != "" && !strings.EqualFold(factory.Trigger.Action, payload.Action) {
			continue
		}
		if !githubRepoMatches(repoFullName, factory.Repos) {
			continue
		}
		if !labelsAllowed(currentLabels, factory.Labels, factory.ExcludeLabels) {
			log.Printf("[github-webhook] factory %q: skipped issue %s (labels did not match)", factory.Name, issueID)
			continue
		}
		log.Printf("[factory:%s] github issue %s (action=%s) — creating claw", factory.Name, issueID, payload.Action)
		if err := s.createClawForGitHubIssue(factory, payload, "github-issue webhook"); err != nil {
			if isFactoryTriggerAlreadyClaimed(err) {
				continue
			}
			log.Printf("[factory:%s] failed to create claw for issue %s: %v", factory.Name, issueID, err)
			s.logFactoryEvent(factory.Name, issueID, payload.Issue.Title, "", payload.Action, "error", "", err.Error())
		} else {
			s.logFactoryEvent(factory.Name, issueID, payload.Issue.Title, "", payload.Action, "claw_created", "", "github-issue webhook")
		}
	}
}

func (s *Server) githubFactoryPRLabels(factory *types.FactoryConfig, repo string, prNumber int) ([]string, bool) {
	if len(factory.Labels) == 0 && len(factory.ExcludeLabels) == 0 {
		return nil, true
	}
	labels, err := s.fetchGitHubIssueLabelsForFactory(factory, repo, prNumber)
	if err != nil {
		log.Printf("[github-webhook] factory %q: skipping PR %s#%d because backing issue labels could not be fetched: %v", factory.Name, repo, prNumber, err)
		return nil, false
	}
	return labels, true
}

func (s *Server) fetchGitHubIssueLabelsForFactory(factory *types.FactoryConfig, repo string, issueNumber int) ([]string, error) {
	token := s.resolveGitHubTokenForRepo(repo)
	if token == "" {
		token = s.resolveGitHubIssuesTokenForFactory(factory)
	}
	if token == "" {
		return nil, fmt.Errorf("no GitHub token available")
	}
	base := s.githubBaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	data, err := githubAPIWithBase(base, fmt.Sprintf("repos/%s/issues/%d", repo, issueNumber), token)
	if err != nil {
		return nil, err
	}
	rawLabels, _ := data["labels"].([]interface{})
	labels := make([]string, 0, len(rawLabels))
	for _, raw := range rawLabels {
		label, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := label["name"].(string)
		labels = append(labels, name)
	}
	return labels, nil
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
// isOwnAppBot returns true if the given GitHub login is the bot account for one of
// the configured GitHub Apps (hub-global or workspace-scoped). App bots have the
// login "<app-slug>[bot]". The app slug is derived from the App URL field
// (e.g. "https://github.com/apps/my-app" → "my-app").
func (s *Server) isOwnAppBot(login string) bool {
	if !strings.HasSuffix(login, "[bot]") {
		return false
	}
	for _, app := range s.githubAppConfigsForTokens() {
		if app.URL == "" {
			continue
		}
		// Extract slug from URL: https://github.com/apps/<slug> or https://github.com/settings/apps/<slug>
		// Also accept org settings URLs: .../organizations/.../settings/apps/<slug>
		u := strings.TrimSuffix(app.URL, "/")
		slug := u[strings.LastIndex(u, "/")+1:]
		if strings.EqualFold(login, slug+"[bot]") {
			return true
		}
	}
	return false
}

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

func (s *Server) createClawForGitHubPR(factory *types.FactoryConfig, pr githubPRPayload, reason string) error {
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

	triggerKey := factoryTriggerKey("github-pr", prURL)
	claimed, err := s.claimFactoryTrigger(factory.Name, "github-pr", triggerKey, reason, map[string]interface{}{
		"repository": repoFullName,
		"pr_number":  prNumber,
		"source":     reason,
	})
	if err != nil {
		return fmt.Errorf("claim factory trigger: %w", err)
	}
	if !claimed {
		return fmt.Errorf("%w: claw already being created for PR %s (trigger claimed)", errFactoryTriggerAlreadyClaimed, prURL)
	}
	claimOpen := true
	defer func() {
		if claimOpen {
			s.failFactoryTrigger(factory.Name, "github-pr", triggerKey)
		}
	}()

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

	// Write PR context to CONTEXT.md only — NOT BOOTSTRAP.md.
	// BOOTSTRAP.md is read by OpenClaw on every session start (AGENTS.md convention),
	// which causes the claw to re-read it as a checklist on every reconnect.
	// CONTEXT.md is a one-time reference the pipeline inject can point to.
	prCtx := buildGitHubPRContext(pr)
	templateFiles["CONTEXT.md"] = prCtx
	// Keep BOOTSTRAP.md from the template if it exists (it won't for most github factories)
	// but don't overwrite it with PR context.

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
	// Tag with compact GitHub PR ref: owner/repo#number
	tags = append(tags, fmt.Sprintf("github_pr:%s#%d", repoFullName, prNumber))
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
		log.Printf("[factory] concurrency limit reached for group %q (active=%d, limit=%d) — queueing claw for GitHub PR %s#%d as pending", groupName, activeCount, groupLimit, repoFullName, prNumber)
	}

	initialStatus := "provisioning"
	if isPending {
		initialStatus = "pending"
	}

	_, err = s.db.Exec(`
		INSERT INTO claws(id, tenant_id, name, template, provider, default_model, template_files, github_repos, linear_workspace, nix, docker, tags, color, llm_key, linear_issue_id, github_issue_id, shortcut_story_id, status, created_at, factory_name, concurrency_group)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		clawID, tenantID, clawName, factory.Template, provider, defaultModel, string(filesJSON),
		string(githubReposJSON), linearWorkspace, nixEnabled, dockerEnabled, string(tagsJSON), clawColor, llmKey, "", "", "", initialStatus, createdAt, factory.Name, groupName,
	)

	// Release promoteMu immediately after INSERT so we don't hold it across
	// the potentially slow async provisioning below.
	s.promoteMu.Unlock()

	if err != nil {
		return fmt.Errorf("db insert: %w", err)
	}
	analyticsEnabled, requiresPR, excludedReason := taskRunAnalyticsContractForFactory(factory)
	if _, _, err := s.ensureTaskRunForClaw(clawID, TaskRunStart{
		RunKind: taskRunKindForFactory(factory), OwnerType: taskRunOwnerFactory, OwnerName: factory.Name,
		OwnerDisplayName: factory.Name, WorkspaceName: factory.Workspace, FactoryName: factory.Name,
		Integration: factory.Integration, Model: defaultModel, LLMKey: llmKey, Source: taskRunSourceFactory,
		AnalyticsEnabled: analyticsEnabled, RequiresPR: requiresPR, ExcludedReason: excludedReason, StartedAt: createdAt,
		EventKey: "task_start:claw:" + clawID,
	}); err != nil {
		// Tear the claw down so the orphaned 'provisioning' row doesn't linger;
		// the deferred failFactoryTrigger releases the claim so poll/webhook can retry.
		_, _ = s.db.Exec(`UPDATE claws SET status='deleted' WHERE id=?`, clawID)
		return fmt.Errorf("task run analytics: %w", err)
	}

	// Store the PR immediately so the watcher picks it up. This claw_prs row is a
	// hard prerequisite for completing the claim: the PR watcher and the existence
	// checks both JOIN claw_prs, so a claw without it is invisible yet would leave
	// the trigger claim active — permanently blocking re-creation for this PR. If
	// the association fails, tear the claw down and release the claim so poll/webhook
	// can retry.
	if _, err := s.storePRMention(clawID, repoFullName, prNumber, prURL); err != nil {
		_, _ = s.db.Exec(`UPDATE claws SET status='deleted' WHERE id=?`, clawID)
		if s.cronScheduler != nil {
			s.cronScheduler.finishRunByClawID(clawID, "failed", err.Error())
		}
		return fmt.Errorf("associate PR with claw: %w", err)
	}
	if _, runID, _, ok, err := s.taskRunContextForClaw(clawID); err == nil && ok {
		occurredAt := parseRFC3339Timestamp(pr.PullRequest.CreatedAt)
		// AgentHeadSHA: the triggering PR's head may be human-authored (a
		// factory reacting to an external PR), but the run only measures human
		// touch after the agent takes over — so the head at claw creation is
		// the detection baseline, exactly what empty-baseline adoption in
		// detectHumanCodePush would otherwise record on the first pass.
		readyAt := int64(0)
		if !pr.PullRequest.Draft {
			readyAt = epochMillis(occurredAt)
		}
		if err := s.associateTaskRunPR(TaskRunPR{RunID: runID, Repo: repoFullName, PRNumber: prNumber, URL: prURL, HeadSHA: pr.PullRequest.Head.SHA, HeadBranch: pr.PullRequest.Head.Ref, BaseBranch: pr.PullRequest.Base.Ref, AgentHeadSHA: true, State: taskRunPRStateOpen, OccurredAt: occurredAt, ReadyAt: readyAt, AuthoritativeReadyAt: readyAt != 0}); err != nil {
			log.Printf("[task-run-analytics] failed to record GitHub PR: %v", err)
		}
	}
	if err := s.completeFactoryTrigger(factory.Name, "github-pr", triggerKey, clawID); err != nil {
		_, _ = s.db.Exec(`UPDATE claws SET status='deleted' WHERE id=?`, clawID)
		if s.cronScheduler != nil {
			s.cronScheduler.finishRunByClawID(clawID, "failed", err.Error())
		}
		return fmt.Errorf("complete factory trigger: %w", err)
	}
	claimOpen = false

	// Log factory event
	s.logFactoryEvent(factory.Name,
		fmt.Sprintf("%s#%d", repoFullName, prNumber),
		pr.PullRequest.Title, "", pr.Action, "claw_created", clawID, "")

	log.Printf("[factory] created claw %s (%s) for GitHub PR %s#%d (status=%s, reason=%s)", clawName, clawID[:8], repoFullName, prNumber, initialStatus, reason)
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type:    "claw_status",
		Payload: map[string]string{"claw_id": clawID, "status": initialStatus},
	})

	if isPending {
		return nil
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
		case "exedev":
			provErr = s.provisionExedev(ctx, clawID, req, provCfg, fileBytes, env)
		case "lambda-microvms":
			provErr = s.provisionLambdaMicroVMs(ctx, clawID, req, provCfg, fileBytes)
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

	return nil
}

// parseRFC3339Timestamp parses an RFC3339 timestamp (as sent by GitHub and
// Linear), returning the zero time when the value is absent or malformed.
func parseRFC3339Timestamp(value string) time.Time {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return t
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
	b.WriteString(`
## Git & GitHub Auth

A git credential helper is pre-configured. You do NOT need to run gh auth login or set GH_TOKEN.
Just use git and gh commands directly — authentication is handled automatically.

To check out this PR:
`)
	b.WriteString(fmt.Sprintf("\tgh pr checkout %s\n", pr.PullRequest.HTMLURL))
	return b.String()
}

// mergePRForClaw finds the tracked PR for a claw and merges it via the GitHub API.
func (s *Server) mergePRForClaw(clawID string) {
	var prURL, repo string
	var prNumber int
	err := s.db.QueryRow(
		`SELECT pr_url, repo, pr_number FROM claw_prs WHERE claw_id=? ORDER BY created_at DESC LIMIT 1`,
		clawID,
	).Scan(&prURL, &repo, &prNumber)
	if err != nil || prURL == "" {
		log.Printf("[pipeline] merge_pr: no tracked PR for claw %s", clawID[:8])
		return
	}

	token := s.resolveGitHubTokenForRepo(repo)
	if token == "" {
		log.Printf("[pipeline] merge_pr: no GitHub token for repo %s", repo)
		s.injectHubMessageByID(clawID, fmt.Sprintf("[hub] merge_pr: no GitHub token available for %s — cannot auto-merge.", repo))
		return
	}

	body, _ := json.Marshal(map[string]string{
		"merge_method": "squash",
	})
	req, err := http.NewRequest(
		http.MethodPut,
		fmt.Sprintf("%s/repos/%s/pulls/%d/merge", s.ghBaseURL(), repo, prNumber),
		bytes.NewReader(body),
	)
	if err != nil {
		log.Printf("[pipeline] merge_pr: failed to build request for %s#%d: %v", repo, prNumber, err)
		s.injectHubMessageByID(clawID, fmt.Sprintf("[hub] merge_pr: failed to prepare merge request for PR #%d: %v", prNumber, err))
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[pipeline] merge_pr: request failed for %s#%d: %v", repo, prNumber, err)
		s.injectHubMessageByID(clawID, fmt.Sprintf("[hub] merge_pr: failed to merge PR #%d: %v", prNumber, err))
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		log.Printf("[pipeline] merge_pr: merged %s#%d successfully", repo, prNumber)
		s.injectHubMessageByID(clawID, fmt.Sprintf("[hub] PR #%d merged successfully.", prNumber))
	} else {
		log.Printf("[pipeline] merge_pr: failed to merge %s#%d: HTTP %d: %s", repo, prNumber, resp.StatusCode, string(respBody))
		s.injectHubMessageByID(clawID, fmt.Sprintf("[hub] merge_pr: failed to merge PR #%d (HTTP %d). Check CI status and review requirements.", prNumber, resp.StatusCode))
	}
}
