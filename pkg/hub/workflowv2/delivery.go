package workflowv2

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"sort"
	"strings"

	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
	"github.com/google/uuid"
)

type PullRequestVerifier interface {
	VerifyPullRequest(context.Context, Run, *typesv2.Workspace, typesv2.PullRequestClaim) (typesv2.VerifiedPullRequest, error)
}

type PullRequestVerifierFunc func(context.Context, Run, *typesv2.Workspace,
	typesv2.PullRequestClaim) (typesv2.VerifiedPullRequest, error)

func (f PullRequestVerifierFunc) VerifyPullRequest(ctx context.Context, run Run, workspace *typesv2.Workspace,
	claim typesv2.PullRequestClaim) (typesv2.VerifiedPullRequest, error) {
	return f(ctx, run, workspace, claim)
}

// SubmitDelivery verifies every untrusted PR claim before atomically amending
// the run-owned delivery collection. Workflows never declare repository lists;
// authority comes solely from the pinned workspace repository map.
func (s *Store) SubmitDelivery(ctx context.Context, runID, attemptID string, manifest typesv2.DeliveryManifest,
	verifier PullRequestVerifier) ([]typesv2.VerifiedPullRequest, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("workflow v2 store is not configured")
	}
	if verifier == nil && len(manifest.PullRequests) > 0 {
		return nil, fmt.Errorf("pull request verifier is required")
	}
	if len(manifest.PullRequests) > 100 {
		return nil, fmt.Errorf("delivery manifest exceeds 100 pull requests")
	}
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	var activeAttempt int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM workflow_v2_attempts WHERE id=? AND run_id=? AND status='active'`,
		attemptID, runID).Scan(&activeAttempt); err != nil || run.CurrentAttemptID != attemptID {
		return nil, fmt.Errorf("delivery attempt is not active")
	}
	var workspaceYAML string
	if err := s.db.QueryRowContext(ctx, `SELECT workspace_yaml FROM workflow_v2_runs WHERE id=?`, runID).Scan(&workspaceYAML); err != nil {
		return nil, err
	}
	resolvedWorkspace, err := typesv2.ParseAndValidateWorkspace([]byte(workspaceYAML))
	if err != nil {
		return nil, fmt.Errorf("load pinned workspace: %w", err)
	}

	verified := make([]typesv2.VerifiedPullRequest, 0, len(manifest.PullRequests))
	trustedSupersedes := make(map[string]string)
	claimsByURL := map[string]bool{}
	identities := map[string]bool{}
	for i, claim := range manifest.PullRequests {
		claim.URL = strings.TrimSpace(claim.URL)
		parsed, err := url.Parse(claim.URL)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
			return nil, fmt.Errorf("pull_requests[%d].url is not an absolute HTTP(S) URL", i)
		}
		if claimsByURL[claim.URL] {
			return nil, fmt.Errorf("pull_requests[%d].url is duplicated", i)
		}
		claimsByURL[claim.URL] = true
		pr, err := verifier.VerifyPullRequest(ctx, run, resolvedWorkspace.Workspace, claim)
		if err != nil {
			return nil, fmt.Errorf("verify pull_requests[%d] %s: %w", i, claim.URL, err)
		}
		if err := validateVerifiedPullRequest(resolvedWorkspace.Workspace, claim, pr); err != nil {
			return nil, fmt.Errorf("verify pull_requests[%d] %s: %w", i, claim.URL, err)
		}
		identity := pr.Repository + "#" + fmt.Sprint(pr.Number)
		if identities[identity] {
			return nil, fmt.Errorf("pull_requests[%d] duplicates verified PR %s", i, identity)
		}
		identities[identity] = true
		if supersedes := strings.TrimSpace(claim.Supersedes); supersedes != "" {
			var targetID, targetURL, targetHead string
			err := s.db.QueryRowContext(ctx, `SELECT id,url,current_head_sha FROM workflow_v2_delivery_prs
				WHERE run_id=? AND active=1 AND (id=? OR url=?)`, runID, supersedes, supersedes).Scan(&targetID, &targetURL, &targetHead)
			if err != nil {
				return nil, fmt.Errorf("verify pull_requests[%d] supersedes %q: active target not found", i, supersedes)
			}
			resolvedTarget, err := verifier.VerifyPullRequest(ctx, run, resolvedWorkspace.Workspace,
				typesv2.PullRequestClaim{URL: targetURL})
			if err != nil {
				return nil, fmt.Errorf("verify pull_requests[%d] superseded target %s: %w", i, targetURL, err)
			}
			if err := validateVerifiedPullRequest(resolvedWorkspace.Workspace, typesv2.PullRequestClaim{URL: targetURL}, resolvedTarget); err != nil {
				return nil, fmt.Errorf("verify pull_requests[%d] superseded target %s: %w", i, targetURL, err)
			}
			if resolvedTarget.State != "closed" && resolvedTarget.State != "merged" {
				return nil, fmt.Errorf("verify pull_requests[%d] supersedes %q: target is still %s", i, supersedes, resolvedTarget.State)
			}
			if resolvedTarget.HeadSHA != targetHead {
				return nil, fmt.Errorf("verify pull_requests[%d] supersedes %q: target head changed; reconcile it first", i, supersedes)
			}
			pr.Supersedes = targetID
			trustedSupersedes[pr.Repository+"#"+fmt.Sprint(pr.Number)] = targetID
		}
		verified = append(verified, pr)
	}

	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM workflow_v2_attempts a JOIN workflow_v2_runs r ON r.id=a.run_id
		WHERE r.id=? AND r.current_attempt_id=? AND a.id=? AND a.status='active'`, runID, attemptID, attemptID).Scan(&activeAttempt); err != nil {
		return nil, fmt.Errorf("delivery attempt was superseded during verification")
	}
	for i := range verified {
		pr := &verified[i]
		var existingID, existingHead string
		err := tx.QueryRowContext(ctx, `SELECT id,current_head_sha FROM workflow_v2_delivery_prs
			WHERE run_id=? AND repository=? AND pr_number=?`, runID, pr.Repository, pr.Number).Scan(&existingID, &existingHead)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		if existingID == "" {
			pr.ID = uuid.NewString()
		} else {
			pr.ID = existingID
		}
		provenanceJSON, err := marshalObject(pr.Provenance)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO workflow_v2_delivery_prs(
			id,run_id,url,repository_name,repository,pr_number,source_branch,base_branch,current_head_sha,state,
			active,supersedes_id,provenance_json,verified_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,1,?,?,?,?)
			ON CONFLICT(run_id,repository,pr_number) DO UPDATE SET url=excluded.url,repository_name=excluded.repository_name,
			source_branch=excluded.source_branch,base_branch=excluded.base_branch,current_head_sha=excluded.current_head_sha,
			state=excluded.state,active=1,supersedes_id=excluded.supersedes_id,provenance_json=excluded.provenance_json,
			verified_at=excluded.verified_at,updated_at=excluded.updated_at`,
			pr.ID, runID, pr.URL, pr.RepositoryName, pr.Repository, pr.Number, pr.SourceBranch, pr.BaseBranch,
			pr.HeadSHA, pr.State, pr.Supersedes, provenanceJSON, pr.VerifiedAt.UnixMilli(), now.UnixMilli())
		if err != nil {
			return nil, err
		}
		if existingHead != pr.HeadSHA {
			var generation int
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation),0)+1 FROM workflow_v2_delivery_heads WHERE pr_id=?`,
				pr.ID).Scan(&generation); err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_v2_delivery_heads(id,pr_id,head_sha,generation,observed_at)
				VALUES(?,?,?,?,?)`, uuid.NewString(), pr.ID, pr.HeadSHA, generation, now.UnixMilli()); err != nil {
				return nil, err
			}
			if existingHead != "" {
				if _, err := tx.ExecContext(ctx, `UPDATE workflow_v2_evidence SET superseded_at=?
					WHERE run_id=? AND pr_id=? AND head_sha=? AND superseded_at=0`, now.UnixMilli(), runID, pr.ID, existingHead); err != nil {
					return nil, err
				}
			}
		}
		if pr.Supersedes != "" {
			identity := pr.Repository + "#" + fmt.Sprint(pr.Number)
			if trustedSupersedes[identity] != pr.Supersedes {
				return nil, fmt.Errorf("superseded pull request was not trusted by the verifier")
			}
			result, err := tx.ExecContext(ctx, `UPDATE workflow_v2_delivery_prs SET active=0,updated_at=?
				WHERE run_id=? AND active=1 AND id!=? AND id=? AND state IN ('closed','merged')`,
				now.UnixMilli(), runID, pr.ID, pr.Supersedes)
			if err != nil {
				return nil, err
			}
			if changed, err := result.RowsAffected(); err != nil || changed != 1 {
				return nil, fmt.Errorf("superseded pull request %q is not an active delivery", pr.Supersedes)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	summary, err := s.deliveryFacts(ctx, runID)
	if err != nil {
		return nil, err
	}
	eventResult, err := s.ApplyEvent(ctx, runID, EventInput{
		ID: uuid.NewString(), Kind: "delivery.verified", Producer: ProducerSourceControl,
		Payload: map[string]interface{}{"delivery": summary},
		Facts: map[string]interface{}{
			"delivery.count": summary["count"], "delivery.open": summary["open"],
			"delivery.merged": summary["merged"], "delivery.all_merged": summary["all_merged"],
		},
		Provenance: typesv2.EvidenceProvenance{Producer: string(ProducerSourceControl), ObservedAt: now},
	})
	if err != nil {
		return nil, err
	}
	if eventResult.Disposition != typesv2.DispositionAccepted {
		return nil, fmt.Errorf("delivery verification event was %s: %s", eventResult.Disposition, eventResult.Reason)
	}
	policyResult, err := s.EvaluateDeliveryPolicy(ctx, runID)
	if err != nil {
		return nil, err
	}
	if err := s.publishEvidencePolicyEvents(ctx, runID, "", policyResult, now); err != nil {
		return nil, err
	}
	sort.Slice(verified, func(i, j int) bool {
		if verified[i].Repository == verified[j].Repository {
			return verified[i].Number < verified[j].Number
		}
		return verified[i].Repository < verified[j].Repository
	})
	return verified, nil
}

func validateVerifiedPullRequest(workspace *typesv2.Workspace, claim typesv2.PullRequestClaim,
	pr typesv2.VerifiedPullRequest) error {
	repo, ok := workspace.Repositories[pr.RepositoryName]
	if !ok {
		return fmt.Errorf("repository name %q is not in pinned workspace", pr.RepositoryName)
	}
	if repo.Repository != pr.Repository {
		return fmt.Errorf("repository %q does not match workspace repository %q", pr.Repository, repo.Repository)
	}
	if pr.Number <= 0 || strings.TrimSpace(pr.URL) == "" || strings.TrimSpace(pr.HeadSHA) == "" ||
		strings.TrimSpace(pr.SourceBranch) == "" || strings.TrimSpace(pr.BaseBranch) == "" {
		return fmt.Errorf("verified pull request identity, branches, and head SHA are required")
	}
	if pr.State != "open" && pr.State != "closed" && pr.State != "merged" {
		return fmt.Errorf("verified pull request state %q is invalid", pr.State)
	}
	if strings.TrimSpace(pr.Provenance.Producer) != string(ProducerSourceControl) || pr.Provenance.ObservedAt.IsZero() || pr.VerifiedAt.IsZero() {
		return fmt.Errorf("verified pull request requires trusted source-control provenance")
	}
	if claim.URL != pr.URL {
		return fmt.Errorf("verified URL %q does not match claimed URL", pr.URL)
	}
	return nil
}

func (s *Store) deliveryFacts(ctx context.Context, runID string) (map[string]interface{}, error) {
	var count, open, merged int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN state='open' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN state='merged' THEN 1 ELSE 0 END),0)
		FROM workflow_v2_delivery_prs WHERE run_id=? AND active=1`, runID).Scan(&count, &open, &merged); err != nil {
		return nil, err
	}
	return map[string]interface{}{"count": count, "open": open, "merged": merged,
		"all_merged": count > 0 && merged == count}, nil
}
