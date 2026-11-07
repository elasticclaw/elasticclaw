package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

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
			return nil
		}
	}
	return store.CompleteActivation(ctx, runID)
}

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
					Ref string `json:"ref"`
					SHA string `json:"sha"`
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
			expectedURL := fmt.Sprintf("https://github.com/%s/pull/%d", repo.Repository, pr.Number)
			if pr.Number <= 0 || pr.URL != expectedURL || pr.Head.SHA == "" || pr.Base.Ref == "" {
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
