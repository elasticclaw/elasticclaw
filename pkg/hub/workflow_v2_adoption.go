package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

// errWorkflowV2FinishedAtActivation reports that delivery.adopted moved the run
// to a terminal state during activation. The run, its cron slot and its parent
// task run are already finished, so callers must not provision a claw.
var errWorkflowV2FinishedAtActivation = errors.New("workflow v2 run finished during activation")

// workflowV2FinishedAtActivationError carries the terminal status the run
// reached, so callers can report it without re-reading the run.
type workflowV2FinishedAtActivationError struct{ status workflowv2.RunStatus }

func (e workflowV2FinishedAtActivationError) Error() string {
	return errWorkflowV2FinishedAtActivation.Error() + ": " + string(e.status)
}

func (e workflowV2FinishedAtActivationError) Unwrap() error { return errWorkflowV2FinishedAtActivation }

// completeWorkflowV2Activation adopts delivery before releasing initial effects.
func (s *Server) completeWorkflowV2Activation(ctx context.Context, store *workflowv2.Store, runID string) error {
	var workspaceYAML, workflowYAML string
	if err := s.db.QueryRowContext(ctx, `SELECT workspace_yaml,workflow_yaml FROM workflow_v2_runs WHERE id=?`, runID).Scan(&workspaceYAML, &workflowYAML); err != nil {
		return err
	}
	ws, err := typesv2.ParseAndValidateWorkspace([]byte(workspaceYAML))
	if err != nil {
		return err
	}
	wf, err := typesv2.ParseAndValidateWorkflow([]byte(workflowYAML))
	if err != nil {
		return err
	}
	if wf.Workflow.Delivery != nil && wf.Workflow.Delivery.PullRequests != nil && len(wf.Workflow.Delivery.PullRequests.Branches) > 0 {
		pending, err := store.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		// A delivery.adopted transition would mark the run active and bypass
		// a suspension such as a failed required context source.
		if pending.SuspendedBeyondActivation() {
			return store.CompleteActivation(ctx, runID)
		}
		lookupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		candidates, lookupErr := s.workflowV2AdoptionCandidates(lookupCtx, ws.Workspace, wf.Workflow.Delivery.PullRequests)
		cancel()
		lookupError := ""
		if lookupErr != nil {
			lookupError = lookupErr.Error()
			candidates = nil
			log.Printf("[workflow-v2] run %s: PR adoption lookup failed: %v", runID, lookupErr)
		}
		// Share the sweep's cross-store ownership lock. V2 ownership is checked
		// again inside the store transaction, including runs with no live claw.
		s.branchSweepMu.Lock()
		candidates, err = s.filterLegacyAdoptionOwners(ctx, candidates)
		var adopted []typesv2.VerifiedPullRequest
		if err == nil {
			adopted, err = store.AdoptPullRequests(ctx, runID, candidates, lookupError)
		}
		s.branchSweepMu.Unlock()
		if err != nil {
			return err
		}
		run, err := store.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		var clawID string
		if len(adopted) > 0 && run.CurrentAttemptID != "" {
			if err := s.db.QueryRowContext(ctx, `SELECT claw_id FROM workflow_v2_attempts WHERE id=?`, run.CurrentAttemptID).Scan(&clawID); err != nil {
				return err
			}
			for _, pr := range adopted {
				if err := s.associateWorkflowV2TaskRunPR(clawID, pr); err != nil {
					return err
				}
			}
		}
		// delivery.adopted may itself reach a terminal state.
		if run.Status != workflowv2.RunActive && run.Status != workflowv2.RunSuspended {
			s.maybeFinishWorkflowV2Parent(ctx, runID)
			return workflowV2FinishedAtActivationError{status: run.Status}
		}
	}
	return store.CompleteActivation(ctx, runID)
}

// filterLegacyAdoptionOwners mirrors the branch sweep's ownership rule. A
// legacy claw that finished its task stays 'idle' and is resumed by PR
// feedback, so it still owns its open PR; legacy claws release ownership only
// when they reach 'deleted' (PR merged/closed, teardown) or 'error'.
func (s *Server) filterLegacyAdoptionOwners(ctx context.Context, candidates []typesv2.VerifiedPullRequest) ([]typesv2.VerifiedPullRequest, error) {
	var available []typesv2.VerifiedPullRequest
	for _, pr := range candidates {
		var owned bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM claw_prs p JOIN claws c ON c.id=p.claw_id
   WHERE p.repo=? COLLATE NOCASE AND p.pr_number=? AND (p.mention_only=0 OR p.source IN ('gh_create','branch_sweep'))
   AND c.status NOT IN ('deleted','error'))`, pr.Repository, pr.Number).Scan(&owned); err != nil {
			return nil, err
		}
		if !owned {
			available = append(available, pr)
		}
	}
	return available, nil
}

func (s *Server) workflowV2AdoptionCandidates(ctx context.Context, workspace *typesv2.Workspace, delivery *typesv2.PullRequestDelivery) ([]typesv2.VerifiedPullRequest, error) {
	if !s.hasOwnAppIdentity() {
		return nil, fmt.Errorf("no GitHub App URL configured to identify the hub's bot")
	}
	var candidates []typesv2.VerifiedPullRequest
	seen := map[string]bool{}
	for _, rule := range delivery.Branches {
		if seen[rule.Repository] {
			continue
		}
		seen[rule.Repository] = true
		repo, ok := workspace.Repositories[rule.Repository]
		if !ok || !strings.EqualFold(repo.Provider, "github") {
			return nil, fmt.Errorf("unsupported adoption repository %q", rule.Repository)
		}
		token := s.resolveGitHubTokenForRepo(repo.Repository)
		if token == "" {
			return nil, fmt.Errorf("no GitHub token for repository %q", rule.Repository)
		}
		items, err := githubAPICollectionWithBaseContext(ctx, s.ghBaseURL(), "repos/"+repo.Repository+"/pulls?state=open&per_page=100", token, "")
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			var pr struct {
				Number int    `json:"number"`
				URL    string `json:"html_url"`
				State  string `json:"state"`
				User   struct {
					Login string `json:"login"`
					Type  string `json:"type"`
				} `json:"user"`
				Head struct {
					Ref  string `json:"ref"`
					SHA  string `json:"sha"`
					Repo *struct {
						FullName string `json:"full_name"`
					} `json:"repo"`
				} `json:"head"`
				Base struct {
					Ref string `json:"ref"`
				} `json:"base"`
			}
			if err := json.Unmarshal(item, &pr); err != nil {
				return nil, err
			}
			if pr.State != "open" || !s.isClawAppAuthor(pr.User.Login, pr.User.Type) ||
				!delivery.MatchesBranch(rule.Repository, pr.Head.Ref) {
				continue
			}
			// Branch patterns only identify branches pushed to the target repository.
			if pr.Head.Repo == nil || !strings.EqualFold(pr.Head.Repo.FullName, repo.Repository) {
				continue
			}
			// GitHub returns its canonical owner/repo case, which may differ from the workspace.
			expectedURL := fmt.Sprintf("https://github.com/%s/pull/%d", repo.Repository, pr.Number)
			if pr.Number <= 0 || !strings.EqualFold(pr.URL, expectedURL) || pr.Head.SHA == "" || pr.Base.Ref == "" {
				continue
			}
			observed := time.Now().UTC()
			connection := repo.SourceControl
			if connection == "" {
				connection = "github"
			}
			candidates = append(candidates, typesv2.VerifiedPullRequest{
				URL: pr.URL, RepositoryName: rule.Repository, Repository: repo.Repository, Number: pr.Number,
				SourceBranch: pr.Head.Ref, BaseBranch: pr.Base.Ref, HeadSHA: pr.Head.SHA, State: pr.State, VerifiedAt: observed,
				Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerSourceControl), Connection: connection,
					ExternalID: fmt.Sprintf("%s#%d", repo.Repository, pr.Number), ObservedAt: observed, Reconciled: true},
			})
		}
	}
	return candidates, nil
}
