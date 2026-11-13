package hub

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

type clawBranch struct {
	Repo    string `json:"repo"`
	Branch  string `json:"branch"`
	HeadSHA string `json:"head_sha"`
}

var branchHeadSHA = regexp.MustCompile(`^(?:[a-fA-F0-9]{40}|[a-fA-F0-9]{64})$`)

func (s *Server) handleClawBranches(w http.ResponseWriter, r *http.Request) {
	clawToken := r.URL.Query().Get("claw_token")
	if clawToken == "" {
		clawToken = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	s.mu.RLock()
	hubClawToken := s.hubCfg.ClawToken
	s.mu.RUnlock()
	if clawToken == "" || subtle.ConstantTimeCompare([]byte(clawToken), []byte(hubClawToken)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	clawID := r.PathValue("id")
	var status, reposJSON string
	err := s.db.QueryRow(`SELECT status, COALESCE(github_repos,'[]') FROM claws WHERE id=?`, clawID).Scan(&status, &reposJSON)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "claw not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	var report struct {
		Branches []clawBranch `json:"branches"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256*1024))
	if err := decoder.Decode(&report); err != nil || decoder.Decode(new(any)) != io.EOF || len(report.Branches) > 256 {
		http.Error(w, "invalid branch report", http.StatusBadRequest)
		return
	}
	var repos []RepoAccess
	_ = json.Unmarshal([]byte(reposJSON), &repos)
	// Agents may clone extra repositories; skip those branches instead of
	// rejecting the whole report. sweepClawBranch re-checks access per branch.
	configured := make([]clawBranch, 0, len(report.Branches))
	var skipped []string
	for _, branch := range report.Branches {
		prURL := "https://github.com/" + branch.Repo + "/pull/1"
		match := prURLRegex.FindStringSubmatch(prURL)
		if len(match) == 0 || match[0] != prURL || len(branch.Branch) > 255 || branch.Branch == "" ||
			strings.ContainsAny(branch.Branch, " \t\r\n~^:?*[\\") || !branchHeadSHA.MatchString(branch.HeadSHA) {
			http.Error(w, "invalid branch", http.StatusBadRequest)
			return
		}
		if effectiveRepoAccess(branch.Repo, repos) == nil {
			skipped = append(skipped, branch.Repo)
			continue
		}
		configured = append(configured, branch)
	}
	if len(skipped) > 0 {
		log.Printf("[branch-sweep] claw %s: ignoring %d branch(es) in repos not configured on the claw: %s",
			shortID(clawID), len(skipped), strings.Join(skipped, ", "))
	}
	if status == "deleted" || status == "error" {
		w.WriteHeader(http.StatusOK)
		return
	}
	pending, err := s.storeClawBranches(clawID, configured)
	if err != nil {
		http.Error(w, "failed to store branches", http.StatusInternalServerError)
		return
	}
	if len(pending) > 0 {
		s.safeGo("claw branch sweep", func() { s.sweepClawBranches(clawID, pending) })
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int{"queued": len(pending)})
}

func (s *Server) storeClawBranches(clawID string, branches []clawBranch) ([]clawBranch, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var pending []clawBranch
	for _, branch := range branches {
		var prURL string
		// A new push re-opens the lookup: the branch may now carry a replacement
		// PR, and re-registering the same open PR is idempotent.
		err := tx.QueryRow(`INSERT INTO claw_branches(claw_id,repo,branch,head_sha) VALUES(?,?,?,?)
			ON CONFLICT(claw_id,repo,branch) DO UPDATE SET head_sha=excluded.head_sha, checked_at=NULL, pr_url=''
			WHERE claw_branches.head_sha<>excluded.head_sha RETURNING pr_url`,
			clawID, branch.Repo, branch.Branch, branch.HeadSHA).Scan(&prURL)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if prURL == "" {
			pending = append(pending, branch)
		}
	}
	return pending, tx.Commit()
}

func (s *Server) sweepClawBranches(clawID string, branches []clawBranch) {
	// Every claw PR is opened by the hub's own App; without its identity any
	// Bot (Dependabot, Renovate) would qualify, so fail closed.
	if len(branches) > 0 && !s.hasOwnAppIdentity() {
		log.Printf("[branch-sweep] claw %s: no GitHub App slug known to identify the hub's bot; skipping %d branch(es)",
			shortID(clawID), len(branches))
		return
	}
	for _, branch := range branches {
		if err := s.sweepClawBranch(clawID, branch); err != nil {
			log.Printf("[branch-sweep] claw %s %s:%s: %v", shortID(clawID), branch.Repo, branch.Branch, err)
		}
	}
}

func (s *Server) sweepClawBranch(clawID string, branch clawBranch) error {
	var tenantID, status, reposJSON, headSHA, prURL string
	err := s.db.QueryRow(`SELECT c.tenant_id,c.status,c.github_repos,b.head_sha,b.pr_url
		FROM claws c JOIN claw_branches b ON b.claw_id=c.id WHERE c.id=? AND b.repo=? AND b.branch=?`,
		clawID, branch.Repo, branch.Branch).Scan(&tenantID, &status, &reposJSON, &headSHA, &prURL)
	if err != nil {
		return err
	}
	var repos []RepoAccess
	if status == "deleted" || status == "error" || headSHA != branch.HeadSHA || prURL != "" ||
		json.Unmarshal([]byte(reposJSON), &repos) != nil || effectiveRepoAccess(branch.Repo, repos) == nil {
		return nil
	}
	// Stamp attempts too: transient failures must not starve other pending branches.
	if _, err := s.db.Exec(`UPDATE claw_branches SET checked_at=? WHERE claw_id=? AND repo=? AND branch=? AND head_sha=?`, now(), clawID, branch.Repo, branch.Branch, branch.HeadSHA); err != nil {
		return err
	}
	token := s.resolveGitHubTokenForRepo(branch.Repo)
	if token == "" {
		return fmt.Errorf("no GitHub token for repository")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	base := s.githubBaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	owner := strings.SplitN(branch.Repo, "/", 2)[0]
	// Only open PRs: a reused branch name must not claim an earlier, closed PR.
	query := url.Values{"head": {owner + ":" + branch.Branch}, "state": {"open"}, "per_page": {"100"}}
	resp, err := defaultGitHubClient.getContext(ctx, base+"/repos/"+branch.Repo+"/pulls?"+query.Encode(), token)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub returned %d", resp.StatusCode)
	}
	var prs []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		State   string `json:"state"`
		User    struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
		Head struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
	}
	if err := json.Unmarshal(resp.Body, &prs); err != nil {
		return err
	}
	newPR := false
	for _, pr := range prs {
		if pr.State != "open" || !s.isClawAppAuthor(pr.User.Login, pr.User.Type) || !strings.EqualFold(pr.Head.Repo.FullName, branch.Repo) || pr.Head.Ref != branch.Branch || pr.Number <= 0 ||
			// Branch names get reused; only a PR whose head is the commit this
			// sandbox last pushed is the claw's own.
			!strings.EqualFold(pr.Head.SHA, branch.HeadSHA) {
			continue
		}
		expectedURL := fmt.Sprintf("https://github.com/%s/pull/%d", branch.Repo, pr.Number)
		if !strings.EqualFold(pr.HTMLURL, expectedURL) {
			continue
		}
		accepted, isNew, err := s.registerSweptPR(ctx, clawID, tenantID, branch.Repo, pr.Number, expectedURL)
		if err != nil {
			return err
		}
		if !accepted {
			continue
		}
		prURL = expectedURL
		newPR = isNew
		break
	}
	_, err = s.db.Exec(`UPDATE claw_branches SET checked_at=?,pr_url=? WHERE claw_id=? AND repo=? AND branch=? AND head_sha=?`,
		now(), prURL, clawID, branch.Repo, branch.Branch, branch.HeadSHA)
	if newPR {
		s.nudgeSweptPRStageSignal(clawID, prURL)
	}
	return err
}

// isClawAppAuthor reports whether a PR author is the hub's own GitHub App,
// which opens every claw PR. Other Apps (Dependabot, Renovate) are Bots too,
// so the login must match a configured App slug.
func (s *Server) isClawAppAuthor(login, userType string) bool {
	return userType == "Bot" && s.isOwnAppBot(login)
}

// hasOwnAppIdentity reports whether any configured App's bot login is known.
func (s *Server) hasOwnAppIdentity() bool {
	return len(s.ownAppSlugs()) > 0
}

// Serialize ownership checks and registration, leaving branch lookups independent.
// accepted means the PR is the claw's; isNew that this call registered it.
func (s *Server) registerSweptPR(ctx context.Context, clawID, tenantID, repo string, number int, prURL string) (accepted, isNew bool, err error) {
	s.branchSweepMu.Lock()
	defer s.branchSweepMu.Unlock()
	var status string
	if err := s.db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&status); err != nil {
		return false, false, err
	}
	if status == "deleted" || status == "error" {
		return false, false, nil
	}
	// Ownership spans both stores: legacy delivered or created rows, and V2
	// delivered or registered PRs of runs whose current attempt is another claw.
	// An offline claw may reconnect, so only deleted/error claws release.
	var otherClaw string
	err = s.db.QueryRow(`SELECT cp.claw_id FROM claw_prs cp JOIN claws c ON c.id=cp.claw_id
		WHERE cp.repo=? COLLATE NOCASE AND cp.pr_number=? AND cp.claw_id<>?
		AND (cp.mention_only=0 OR cp.source IN ('gh_create','branch_sweep'))
		AND c.status NOT IN ('deleted','error')
		UNION ALL
		SELECT a.claw_id FROM workflow_v2_delivery_prs p JOIN workflow_v2_runs r ON r.id=p.run_id
		JOIN workflow_v2_attempts a ON a.id=r.current_attempt_id
		WHERE p.repository=? COLLATE NOCASE AND p.pr_number=? AND p.active=1 AND a.claw_id<>?
		AND r.status IN ('active','suspended') AND a.status='active'
		LIMIT 1`, repo, number, clawID, repo, number, clawID).Scan(&otherClaw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, false, err
	}
	if otherClaw != "" {
		log.Printf("[branch-sweep] skipping %s for claw %s: owned by active claw %s", prURL, shortID(clawID), shortID(otherClaw))
		return false, false, nil
	}
	isNew, err = s.registerClawPR(ctx, clawID, tenantID, repo, number, prURL, "branch_sweep")
	return err == nil, isNew, err
}

// nudgeSweptPRStageSignal tells an agent whose PR the sweep just found that
// its stage still waits for [DONE]. The PR stage advances only on that signal,
// and the sweep exists because it was lost (a transport error after the agent
// opened the PR), so without a nudge the claw idles in the stage. The agent may
// believe it already sent the signal; the message says it did not arrive.
// Workflow V2 registration is itself the delivery, so V2 claws are skipped.
func (s *Server) nudgeSweptPRStageSignal(clawID, prURL string) {
	s.mu.RLock()
	cc := s.claws[clawID]
	s.mu.RUnlock()
	if cc == nil {
		cc = &clawConn{id: clawID}
		_ = s.db.QueryRow(`SELECT tenant_id FROM claws WHERE id=?`, clawID).Scan(&cc.tenantID)
	}
	if s.workflowV2OwnsExecution(cc) || !s.pipelineAwaitsDoneSignal(clawID) {
		return
	}
	urls, _ := s.donePRURLs(clawID, "")
	if len(urls) == 0 {
		urls = []string{prURL}
	}
	text := fmt.Sprintf("[hub] The hub found these PRs on your branch:\n%s\n\n"+
		"The hub has NOT received your %s signal for this stage. Do not open another PR. "+
		"If these are your final PRs for this stage, reply with %s on its own line followed by the PR URLs, one per line, "+
		"even if you believe you already sent it.",
		strings.Join(urls, "\n"), doneSignalToken, doneSignalToken)
	// One nudge per PR, across sweeps and hub restarts.
	messageID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("swept-pr-signal\x00"+clawID+"\x00"+prURL)).String()
	log.Printf("[branch-sweep] claw %s: nudging for %s after registering %s", shortID(clawID), doneSignalToken, prURL)
	// External event: lift a no-progress or bridge-error pause, as
	// injectExternalHubMessageByID does, so the nudge is delivered.
	s.resumeNoProgressAfterUserInput(clawID)
	s.injectMessageWithID(clawID, text, "hub", messageID)
}

// pipelineAwaitsDoneSignal reports whether [DONE] would move the claw out of
// its current, non-terminal pipeline stage.
func (s *Server) pipelineAwaitsDoneSignal(clawID string) bool {
	ctx, ok := s.findPipelineContextForClaw(clawID)
	if !ok {
		return false
	}
	pl := parsePipelineForContext(ctx)
	if pl == nil {
		return false
	}
	current := s.getPipelineStage(clawID)
	if stage := pl.StageByID(current); stage == nil || stage.Terminal {
		return false
	}
	next := pl.StageForMessageContains(doneSignalToken, current)
	return next != nil && next.ID != current
}

// Retry only sandbox-reported branches. This also catches PR creation after a
// previous turn reported the same head, without requiring unchanged bridge reports.
func (s *Server) retryClawBranches() {
	if !s.branchRetryMu.TryLock() {
		return
	}
	defer s.branchRetryMu.Unlock()
	if !defaultGitHubClient.allowLowPriority() {
		return
	}
	rows, err := s.db.Query(`SELECT b.claw_id,b.repo,b.branch,b.head_sha FROM claw_branches b
		JOIN claws c ON c.id=b.claw_id WHERE b.pr_url='' AND c.status NOT IN ('deleted','error','offline')
		AND (b.checked_at IS NULL OR b.checked_at<?) ORDER BY b.checked_at LIMIT 8`, now().Add(-5*time.Minute))
	if err != nil {
		log.Printf("[branch-sweep] load pending branches: %v", err)
		return
	}
	type pendingBranch struct {
		clawID string
		branch clawBranch
	}
	var pending []pendingBranch
	for rows.Next() {
		var p pendingBranch
		if err := rows.Scan(&p.clawID, &p.branch.Repo, &p.branch.Branch, &p.branch.HeadSHA); err != nil {
			rows.Close()
			return
		}
		pending = append(pending, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(pending) == 0 {
		return
	}
	if !s.hasOwnAppIdentity() {
		log.Printf("[branch-sweep] no GitHub App slug known to identify the hub's bot; skipping %d pending branch(es)", len(pending))
		return
	}
	for _, p := range pending {
		s.sweepClawBranches(p.clawID, []clawBranch{p.branch})
	}
}
