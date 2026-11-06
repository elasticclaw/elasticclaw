package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

const prDiscoveryRunLimit = 25

type prDiscoveryRun struct {
	id, clawID, issue, workspace, workflow string
	started, finished                      int64
	repos                                  string
}

func issueTokenMatches(text, issue string) bool {
	if strings.TrimSpace(issue) == "" {
		return false
	}
	return regexp.MustCompile("(?i)(^|[^a-z0-9])" + regexp.QuoteMeta(issue) + "($|[^a-z0-9])").MatchString(text)
}

func (s *Server) prDiscoveryRun(runID string) (prDiscoveryRun, error) {
	var r prDiscoveryRun
	err := s.db.QueryRow(`
		SELECT tr.id, tr.claw_id, tr.issue_id, tr.workspace_name, tr.workflow_name,
		       trs.started_at, trs.finished_at, COALESCE(c.github_repos,'')
		FROM task_runs tr JOIN task_run_summaries trs ON trs.run_id=tr.id
		LEFT JOIN claws c ON c.id=tr.claw_id WHERE tr.id=?`, runID).
		Scan(&r.id, &r.clawID, &r.issue, &r.workspace, &r.workflow, &r.started, &r.finished, &r.repos)
	return r, err
}

type prDiscoveryPR struct {
	TaskRunPR
	mention prMentionCandidate
}

// Discovery only searches the run's repository access. Pages and runs are bounded.
func (s *Server) discoverRunPRs(r prDiscoveryRun) ([]prDiscoveryPR, error) {
	prs, complete, err := s.discoverRunPRsWithCompletion(r)
	if err == nil && !complete {
		return nil, fmt.Errorf("incomplete PR discovery")
	}
	return prs, err
}

func (s *Server) discoverRunPRsWithCompletion(r prDiscoveryRun) ([]prDiscoveryPR, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if strings.TrimSpace(r.issue) == "" {
		return nil, true, nil
	}
	if r.started == 0 {
		return nil, false, nil
	}
	var repos []types.GitHubRepoAccess
	if r.repos != "" {
		if err := json.Unmarshal([]byte(r.repos), &repos); err != nil {
			return nil, false, err
		}
	} else if ws, _, ok := loadWorkflowPipelineContext(r.workspace, r.workflow); ok {
		repos = append(repos, ws.Repositories...)
	}
	if hasRepositoryGlob(repos) {
		// clone:false patterns are kept verbatim for claw creation; discovery
		// queries concrete repositories, so expand every pattern.
		clone := true
		for i := range repos {
			if isRepositoryPattern(repos[i].Repo) {
				repos[i].Clone = &clone
			}
		}
		var err error
		repos, err = s.expandWorkspaceRepositories(ctx, r.workspace, repos)
		if err != nil {
			return nil, false, err
		}
	}
	if len(repos) == 0 {
		return nil, true, nil
	}
	base := s.githubBaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	var result []prDiscoveryPR
	seen := map[string]bool{}
	queried := false
	complete := true
	for _, repo := range repos {
		if !defaultGitHubClient.allowLowPriority() {
			return nil, false, fmt.Errorf("discovery quota unavailable")
		}
		if seen[strings.ToLower(repo.Repo)] {
			continue
		}
		seen[strings.ToLower(repo.Repo)] = true
		token := s.resolveGitHubTokenWithReposContext(ctx, []RepoAccess{{Repo: repo.Repo, Permissions: "read"}})
		if token == "" {
			complete = false
			log.Printf("[pr-discovery] no repository token for %s", repo.Repo)
			continue
		}
		for page := 1; page <= 10; page++ {
			if !defaultGitHubClient.allowLowPriority() {
				return nil, false, fmt.Errorf("discovery quota unavailable")
			}
			items, err := githubAPIListWithBaseContext(ctx, base, fmt.Sprintf("repos/%s/pulls?state=all&sort=created&direction=desc&page=%d", repo.Repo, page), token)
			if err != nil {
				return nil, false, err
			}
			queried = true
			older := false
			for _, item := range items {
				data, _ := item.(map[string]interface{})
				created, _ := data["created_at"].(string)
				at := parseRFC3339Timestamp(created)
				if at.IsZero() {
					continue
				}
				if at.UnixMilli() < r.started {
					older = true
					continue
				}
				if r.finished != 0 && at.UnixMilli() > r.finished {
					continue
				}
				title, _ := data["title"].(string)
				head, _ := data["head"].(map[string]interface{})
				branch, _ := head["ref"].(string)
				if !issueTokenMatches(title, r.issue) && !issueTokenMatches(branch, r.issue) {
					continue
				}
				user, _ := data["user"].(map[string]interface{})
				login, _ := user["login"].(string)
				if !s.isDiscoveryAppBot(r.workspace, login) {
					continue
				}
				var overlapping int
				if err := s.db.QueryRow(`SELECT COUNT(*) FROM task_runs tr JOIN task_run_summaries ts ON ts.run_id=tr.id
 WHERE tr.workspace_name=? AND lower(tr.issue_id)=lower(?) AND ts.started_at<=? AND (ts.finished_at=0 OR ts.finished_at>=?)`, r.workspace, r.issue, at.UnixMilli(), at.UnixMilli()).Scan(&overlapping); err != nil {
					return nil, false, err
				}
				if overlapping > 1 {
					continue
				}
				number, _ := data["number"].(float64)
				var owned int
				if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM task_run_prs WHERE lower(repo)=lower(?) AND pr_number=? AND run_id<>?)
			OR EXISTS(SELECT 1 FROM claw_prs p JOIN claws c ON c.id=p.claw_id
				WHERE lower(p.repo)=lower(?) AND p.pr_number=? AND c.task_run_id<>? AND c.task_run_id<>'')`, repo.Repo, int(number), r.id, repo.Repo, int(number), r.id).Scan(&owned); err != nil {
					return nil, false, err
				}
				if owned != 0 || number <= 0 {
					continue
				}
				// Pull-list responses do not include the authoritative merged flag.
				if !defaultGitHubClient.allowLowPriority() {
					return nil, false, fmt.Errorf("discovery quota unavailable")
				}
				pr, err := githubAPIWithBaseContext(ctx, base, fmt.Sprintf("repos/%s/pulls/%d", repo.Repo, int(number)), token)
				if err != nil {
					return nil, false, err
				}
				state, _ := pr["state"].(string)
				if state != taskRunPRStateOpen && state != taskRunPRStateClosed {
					return nil, false, fmt.Errorf("invalid PR state for %s#%d", repo.Repo, int(number))
				}
				merged, _ := pr["merged"].(bool)
				mergedAt, _ := pr["merged_at"].(string)
				closedAt, _ := pr["closed_at"].(string)
				sha, _ := head["sha"].(string)
				p := TaskRunPR{RunID: r.id, Repo: repo.Repo, PRNumber: int(number),
					URL:        fmt.Sprintf("https://github.com/%s/pull/%d", repo.Repo, int(number)),
					HeadBranch: branch, HeadSHA: sha, State: state, Merged: merged,
					OccurredAt: at, Exclusive: true}
				// Keep provider creation and terminal timestamps separate.
				p.OpenedAt = at
				if merged {
					p.OccurredAt = parseRFC3339Timestamp(mergedAt)
				} else if state == taskRunPRStateClosed {
					p.OccurredAt = parseRFC3339Timestamp(closedAt)
				}
				if (merged || state == taskRunPRStateClosed) && p.OccurredAt.IsZero() {
					continue
				}
				candidate := prDiscoveryPR{TaskRunPR: p}
				if r.finished == 0 && (state == taskRunPRStateOpen || merged) {
					// Fetch every watermark before publishing any watcher row.
					row := prMentionCandidate{repo: p.Repo, number: p.PRNumber, url: p.URL, headSHA: p.HeadSHA, inferred: true}
					row.title, _ = pr["title"].(string)
					row.mergeableState, _ = pr["mergeable_state"].(string)
					comments, err := githubAPIListWithBaseContext(ctx, base, fmt.Sprintf("repos/%s/issues/%d/comments", repo.Repo, p.PRNumber), token)
					if err != nil {
						return nil, false, err
					}
					for _, item := range comments {
						comment, _ := item.(map[string]interface{})
						id, _ := comment["id"].(float64)
						if int64(id) > row.comment {
							row.comment = int64(id)
						}
						created, _ := comment["created_at"].(string)
						if parseRFC3339Timestamp(created).After(parseRFC3339Timestamp(row.commentAt)) {
							row.commentAt = created
						}
					}
					reviews, err := githubAPIListWithBaseContext(ctx, base, fmt.Sprintf("repos/%s/pulls/%d/reviews", repo.Repo, p.PRNumber), token)
					if err != nil {
						return nil, false, err
					}
					row.review = maxPRReviewID(reviews, 0)
					candidate.mention = row
				}
				result = append(result, candidate)
			}
			if older || len(items) < 100 {
				break
			}
			if page == 10 {
				complete = false
				log.Printf("[pr-discovery] page limit for %s", repo.Repo)
			}
		}
	}
	if !queried {
		return nil, false, fmt.Errorf("no repository could be queried")
	}
	return result, complete, nil
}

func (s *Server) discoverClawPRs(clawID, runID string) {
	var count int
	if !s.taskRunRequiresPRForClaw(clawID) || s.db.QueryRow("SELECT COUNT(*) FROM claw_prs WHERE claw_id=? AND mention_only=0", clawID).Scan(&count) != nil || count != 0 {
		return
	}
	r, err := s.prDiscoveryRun(runID)
	if err != nil {
		return
	}
	r.finished = 0
	prs, err := s.discoverRunPRs(r)
	if err != nil {
		log.Printf("[pr-discovery] discover claw %s: %v", shortID(clawID), err)
		return
	}
	var rows []prMentionCandidate
	for _, p := range prs {
		if p.State == taskRunPRStateClosed && !p.Merged {
			continue
		}
		// Reserve ownership before atomically arming the watcher for the full set.
		p.State, p.Merged, p.OccurredAt = taskRunPRStateOpen, false, p.OpenedAt
		if err := s.associateTaskRunPR(p.TaskRunPR); err != nil {
			log.Printf("[pr-discovery] reserve claw %s: %v", shortID(clawID), err)
			return
		}
		rows = append(rows, p.mention)
	}
	if failed := s.insertClawPRsAtomic(clawID, rows, false, "discovery"); failed != "" {
		log.Printf("[pr-discovery] register claw %s failed at %s", shortID(clawID), failed)
	}
}

// The cursor rotates through unsuccessful candidates so a busy factory cannot
// permanently starve older runs behind the per-pass cap.
func (s *Server) reconcileUnreportedPRs(after string) string {
	rows, err := s.db.Query(`SELECT tr.id FROM task_runs tr
		JOIN task_run_summaries trs ON trs.run_id=tr.id
		WHERE trs.phase='terminal' AND tr.requires_pr=1 AND trs.finished_at>=?
		  AND tr.pr_discovery_scanned=0 AND tr.id>?
		  AND NOT EXISTS(SELECT 1 FROM task_run_prs p WHERE p.run_id=tr.id AND p.inferred=0)
		ORDER BY tr.id LIMIT ?`, time.Now().Add(-14*24*time.Hour).UnixMilli(), after, prDiscoveryRunLimit)
	if err != nil {
		log.Printf("[pr-discovery] query terminal runs: %v", err)
		return ""
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	iterErr := rows.Err()
	rows.Close()
	if iterErr != nil {
		log.Printf("[pr-discovery] iterate terminal runs: %v", iterErr)
		return after
	}
	for _, id := range ids {
		if !defaultGitHubClient.allowLowPriority() {
			return after
		}
		after = id
		r, err := s.prDiscoveryRun(id)
		if err != nil {
			continue
		}
		prs, complete, err := s.discoverRunPRsWithCompletion(r)
		if err != nil {
			log.Printf("[pr-discovery] discover run %s: %v", id, err)
			continue
		}
		if !complete {
			continue
		}
		// All-or-nothing: an open PR moves the run out of the terminal phase,
		// so a partial backfill would never be selected for retry.
		if err := s.backfillRunPRs(id, prs); err != nil {
			log.Printf("[pr-discovery] backfill run %s: %v", id, err)
			continue
		}
		for _, p := range prs {
			log.Printf("[pr-discovery] backfilled PR %s#%d for run %s (issue %s)", p.Repo, p.PRNumber, id, r.issue)
		}
	}
	if len(ids) < prDiscoveryRunLimit {
		return ""
	}
	return after
}

func (s *Server) backfillRunPRs(runID string, prs []prDiscoveryPR) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range prs {
		if err := associateTaskRunPRTx(tx, p.TaskRunPR); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE task_runs SET pr_discovery_scanned=1 WHERE id=?`, runID); err != nil {
		return err
	}
	return tx.Commit()
}

// Scope bot identity to the run's workspace; legacy runs use hub app settings.
func (s *Server) isDiscoveryAppBot(workspace, login string) bool {
	if workspace == "" {
		return s.isOwnAppBot(login)
	}
	apps, err := loadWorkspaceGitHubAppConfigs(workspace)
	if err != nil {
		return false
	}
	if len(apps) == 0 {
		s.mu.RLock()
		apps = append(apps, s.hubCfg.GitHubApps...)
		s.mu.RUnlock()
	}
	for _, app := range apps {
		if app == nil {
			continue
		}
		slug := strings.TrimSuffix(app.URL, "/")
		slug = slug[strings.LastIndex(slug, "/")+1:]
		if slug != "" && strings.EqualFold(login, slug+"[bot]") {
			return true
		}
	}
	return false
}

func githubAPIListWithBaseContext(ctx context.Context, base, path, token string) ([]interface{}, error) {
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	resp, err := defaultGitHubClient.getContext(ctx, base+"/"+path+separator+"per_page=100", token)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, &githubAPIError{StatusCode: resp.StatusCode, Body: string(resp.Body), RateLimited: resp.rateLimited()}
	}
	var items []interface{}
	err = json.Unmarshal(resp.Body, &items)
	return items, err
}

const prDiscoveryRetryInterval = 10 * time.Minute

// Independent of auto-resume settings, latches, caps and no-progress pauses.
func (s *Server) scheduleIdlePRDiscovery(at time.Time) {
	s.prDiscoveryMu.Lock()
	for id, attempted := range s.prDiscoveryAttempts {
		if at.Sub(attempted) >= prDiscoveryRetryInterval {
			delete(s.prDiscoveryAttempts, id)
		}
	}
	s.prDiscoveryMu.Unlock()

	s.mu.RLock()
	connections := make(map[string]*clawConn, len(s.claws))
	for id, cc := range s.claws {
		connections[id] = cc
	}
	s.mu.RUnlock()
	for id, cc := range connections {
		snap := agentIdleSnapshotOf(cc)
		if snap.busy || snap.stretchStartAt.IsZero() || at.Sub(snap.stretchStartAt) < s.livenessSettings().idleResumeAfter {
			continue
		}
		var run string
		if err := s.db.QueryRow(`SELECT c.task_run_id FROM claws c JOIN task_runs tr ON tr.id=c.task_run_id
 WHERE c.id=? AND c.status='connected' AND tr.requires_pr=1 AND NOT EXISTS(SELECT 1 FROM claw_prs p WHERE p.claw_id=c.id AND p.mention_only=0)`, id).Scan(&run); err != nil {
			continue
		}
		s.prDiscoveryMu.Lock()
		if s.prDiscoveryInFlight[id] || at.Sub(s.prDiscoveryAttempts[id]) < prDiscoveryRetryInterval {
			s.prDiscoveryMu.Unlock()
			continue
		}
		if s.prDiscoveryAttempts == nil {
			s.prDiscoveryAttempts = map[string]time.Time{}
			s.prDiscoveryInFlight = map[string]bool{}
		}
		s.prDiscoveryAttempts[id], s.prDiscoveryInFlight[id] = at, true
		s.prDiscoveryMu.Unlock()
		go func(id, run string) {
			defer func() {
				s.prDiscoveryMu.Lock()
				delete(s.prDiscoveryInFlight, id)
				s.prDiscoveryMu.Unlock()
			}()
			s.discoverClawPRs(id, run)
		}(id, run)
	}
}
