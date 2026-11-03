package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
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

// Discovery only searches the run's repository access, never all repositories
// reachable by the installation token. Pages and runs are bounded per pass.
func (s *Server) discoverRunPRs(r prDiscoveryRun) ([]TaskRunPR, error) {
	if r.issue == "" || r.started == 0 || !defaultGitHubClient.allowLowPriority() {
		return nil, nil
	}
	var repos []types.GitHubRepoAccess
	if r.repos != "" {
		if err := json.Unmarshal([]byte(r.repos), &repos); err != nil {
			return nil, err
		}
	} else if ws, _, ok := loadWorkflowPipelineContext(r.workspace, r.workflow); ok {
		repos = append(repos, ws.Repositories...)
	}
	if hasRepositoryGlob(repos) {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		var err error
		repos, err = s.expandWorkspaceRepositories(ctx, r.workspace, repos)
		if err != nil {
			return nil, err
		}
	}
	base := s.githubBaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	var result []TaskRunPR
	seen := map[string]bool{}
	for _, repo := range repos {
		if !defaultGitHubClient.allowLowPriority() {
			return nil, nil
		}
		if seen[strings.ToLower(repo.Repo)] {
			continue
		}
		seen[strings.ToLower(repo.Repo)] = true
		token := s.resolveGitHubTokenForRepo(repo.Repo)
		if token == "" {
			return nil, fmt.Errorf("no repository token for %s", repo.Repo)
		}
		for page := 1; page <= 10; page++ {
			if !defaultGitHubClient.allowLowPriority() {
				return nil, nil
			}
			items, err := githubAPIListWithBase(base, fmt.Sprintf("repos/%s/pulls?state=all&sort=created&direction=desc&page=%d", repo.Repo, page), token)
			if err != nil {
				return nil, err
			}
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
				number, _ := data["number"].(float64)
				var owned int
				if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM task_run_prs WHERE lower(repo)=lower(?) AND pr_number=? AND run_id<>?)
			OR EXISTS(SELECT 1 FROM claw_prs p JOIN claws c ON c.id=p.claw_id
				WHERE lower(p.repo)=lower(?) AND p.pr_number=? AND c.task_run_id<>? AND c.task_run_id<>'')`, repo.Repo, int(number), r.id, repo.Repo, int(number), r.id).Scan(&owned); err != nil {
					return nil, err
				}
				if owned != 0 || number <= 0 {
					continue
				}
				// Pull-list responses do not include the authoritative merged flag.
				if !defaultGitHubClient.allowLowPriority() {
					return nil, nil
				}
				pr, err := githubAPIWithBase(base, fmt.Sprintf("repos/%s/pulls/%d", repo.Repo, int(number)), token)
				if err != nil {
					return nil, err
				}
				state, _ := pr["state"].(string)
				if state != taskRunPRStateOpen && state != taskRunPRStateClosed {
					return nil, fmt.Errorf("invalid PR state for %s#%d", repo.Repo, int(number))
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
				result = append(result, p)
			}
			if older || len(items) < 100 {
				break
			}
			if page == 10 {
				return nil, fmt.Errorf("discovery page limit for %s", repo.Repo)
			}
		}
	}
	return result, nil
}

func (s *Server) discoverClawPRs(clawID, runID string) {
	var count int
	if !s.taskRunRequiresPRForClaw(clawID) || s.db.QueryRow("SELECT COUNT(*) FROM claw_prs WHERE claw_id=?", clawID).Scan(&count) != nil || count != 0 {
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
	for _, p := range prs {
		// Reserve ownership atomically before arming the watcher. Leave state open:
		// checkPRMerged must observe the terminal provider state and run teardown.
		p.State, p.Merged, p.OccurredAt = taskRunPRStateOpen, false, p.OpenedAt
		if err := s.associateTaskRunPR(p); err != nil {
			log.Printf("[pr-discovery] associate claw %s: %v", shortID(clawID), err)
			continue
		}
		if inserted, err := s.storePRMention(clawID, p.Repo, p.PRNumber, p.URL, false); err == nil && inserted {
			log.Printf("[pr-discovery] registered unreported PR %s#%d for claw %s (issue %s)", p.Repo, p.PRNumber, shortID(clawID), r.issue)
		}
	}
}

// The cursor rotates through unsuccessful candidates so a busy factory cannot
// permanently starve older runs behind the per-pass cap.
func (s *Server) reconcileUnreportedPRs(after string) string {
	rows, err := s.db.Query(`SELECT tr.id FROM task_runs tr
		JOIN task_run_summaries trs ON trs.run_id=tr.id
		WHERE trs.phase='terminal' AND tr.requires_pr=1 AND trs.finished_at>=?
		  AND tr.id>? AND NOT EXISTS(SELECT 1 FROM task_run_prs p WHERE p.run_id=tr.id)
		ORDER BY tr.id LIMIT ?`, time.Now().Add(-14*24*time.Hour).UnixMilli(), after, prDiscoveryRunLimit)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Printf("[pr-discovery] query terminal runs: %v", err)
		}
		return ""
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if !defaultGitHubClient.allowLowPriority() {
			return after
		}
		after = id
		r, err := s.prDiscoveryRun(id)
		if err != nil {
			continue
		}
		prs, err := s.discoverRunPRs(r)
		if err != nil {
			log.Printf("[pr-discovery] discover run %s: %v", id, err)
			continue
		}
		for _, p := range prs {
			if err := s.associateTaskRunPR(p); err != nil {
				log.Printf("[pr-discovery] backfill run %s: %v", id, err)
				continue
			}
			log.Printf("[pr-discovery] backfilled PR %s#%d for run %s (issue %s)", p.Repo, p.PRNumber, id, r.issue)
		}
	}
	if len(ids) < prDiscoveryRunLimit {
		return ""
	}
	return after
}
