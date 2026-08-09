package workflowv2

import (
	"context"
	"database/sql"
	"encoding/json"
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
	verified, eventResult, err := s.submitDelivery(ctx, runID, attemptID, uuid.NewString(), "", nil, manifest, verifier)
	if err != nil {
		return nil, err
	}
	if eventResult.Disposition != typesv2.DispositionAccepted {
		return nil, fmt.Errorf("delivery verification event was %s: %s", eventResult.Disposition, eventResult.Reason)
	}
	return verified, nil
}

// ApplyDeliveryControl verifies an untrusted delivery manifest through the
// configured source-control connection and uses the incoming message identity
// for durable deduplication and state-version guarding.
func (s *Store) ApplyDeliveryControl(ctx context.Context, envelope typesv2.ControlEnvelope,
	verifier PullRequestVerifier) (typesv2.ControlReceipt, error) {
	if err := typesv2.ValidateControlEnvelope(envelope, typesv2.DirectionClawToHub); err != nil {
		return typesv2.ControlReceipt{}, err
	}
	if envelope.Kind != typesv2.MessageDeliverySubmitted && envelope.Kind != typesv2.MessagePullRequestClaimed {
		return typesv2.ControlReceipt{}, fmt.Errorf("message kind %q is not a delivery submission", envelope.Kind)
	}
	var manifest typesv2.DeliveryManifest
	if err := json.Unmarshal(envelope.Payload, &manifest); err != nil {
		return typesv2.ControlReceipt{}, fmt.Errorf("decode delivery manifest: %w", err)
	}
	if receipt, handled, err := s.preflightDeliveryControl(ctx, envelope); err != nil {
		return typesv2.ControlReceipt{}, err
	} else if handled {
		return receipt, nil
	}
	_, eventResult, err := s.submitDelivery(ctx, envelope.RunID, envelope.AttemptID, envelope.MessageID,
		envelope.MessageID, envelope.ExpectedStateVersion, manifest, verifier)
	if err != nil {
		return typesv2.ControlReceipt{}, err
	}
	return typesv2.ControlReceipt{MessageID: envelope.MessageID, Disposition: eventResult.Disposition,
		StateVersion: eventResult.Run.StateVersion, Reason: eventResult.Reason}, nil
}

func (s *Store) preflightDeliveryControl(ctx context.Context,
	envelope typesv2.ControlEnvelope) (typesv2.ControlReceipt, bool, error) {
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return typesv2.ControlReceipt{}, false, err
	}
	defer tx.Rollback()
	run, _, err := getRunForUpdate(ctx, tx, envelope.RunID)
	if err != nil {
		return typesv2.ControlReceipt{}, false, err
	}
	input := EventInput{ID: envelope.MessageID, MessageID: envelope.MessageID, Kind: "delivery.verified",
		AttemptID: envelope.AttemptID, ExpectedStateVersion: envelope.ExpectedStateVersion,
		Producer: ProducerSourceControl, Provenance: typesv2.EvidenceProvenance{
			Producer: string(ProducerSourceControl), ObservedAt: now,
		}}
	if err := authorizeBoundAttempt(ctx, tx, run, input); err != nil {
		return typesv2.ControlReceipt{}, false, err
	}
	if existing, found, err := findDuplicateEvent(ctx, tx, run.ID, input.ID, input.MessageID); err != nil {
		return typesv2.ControlReceipt{}, false, err
	} else if found {
		reason := "event already received"
		if err := insertEventReceipt(ctx, tx, run.ID, existing, input.MessageID, typesv2.DispositionDuplicate,
			run.StateVersion, reason, now); err != nil {
			return typesv2.ControlReceipt{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return typesv2.ControlReceipt{}, false, err
		}
		return typesv2.ControlReceipt{MessageID: envelope.MessageID, Disposition: typesv2.DispositionDuplicate,
			StateVersion: run.StateVersion, Reason: reason}, true, nil
	}
	if envelope.ExpectedStateVersion == nil || *envelope.ExpectedStateVersion == run.StateVersion {
		return typesv2.ControlReceipt{}, false, nil
	}
	reason := fmt.Sprintf("expected state version %d, current version is %d", *envelope.ExpectedStateVersion, run.StateVersion)
	if err := insertEvent(ctx, tx, input.ID, run.ID, input.MessageID, input.Kind, input.ExpectedStateVersion,
		run.StateVersion, typesv2.DispositionStaleState, reason, ProducerSourceControl, input.Provenance, nil, nil, now); err != nil {
		return typesv2.ControlReceipt{}, false, err
	}
	if err := insertEventReceipt(ctx, tx, run.ID, input.ID, input.MessageID, typesv2.DispositionStaleState,
		run.StateVersion, reason, now); err != nil {
		return typesv2.ControlReceipt{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return typesv2.ControlReceipt{}, false, err
	}
	return typesv2.ControlReceipt{MessageID: envelope.MessageID, Disposition: typesv2.DispositionStaleState,
		StateVersion: run.StateVersion, Reason: reason}, true, nil
}

func (s *Store) submitDelivery(ctx context.Context, runID, attemptID, eventID, messageID string,
	expectedStateVersion *uint64, manifest typesv2.DeliveryManifest,
	verifier PullRequestVerifier) ([]typesv2.VerifiedPullRequest, EventResult, error) {
	if s == nil || s.db == nil {
		return nil, EventResult{}, fmt.Errorf("workflow v2 store is not configured")
	}
	if verifier == nil && len(manifest.PullRequests) > 0 {
		return nil, EventResult{}, fmt.Errorf("pull request verifier is required")
	}
	if len(manifest.PullRequests) > 100 {
		return nil, EventResult{}, fmt.Errorf("delivery manifest exceeds 100 pull requests")
	}
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		return nil, EventResult{}, err
	}
	var activeAttempt int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM workflow_v2_attempts WHERE id=? AND run_id=? AND status='active'`,
		attemptID, runID).Scan(&activeAttempt); err != nil || run.CurrentAttemptID != attemptID {
		return nil, EventResult{}, fmt.Errorf("delivery attempt is not active")
	}
	var workspaceYAML string
	if err := s.db.QueryRowContext(ctx, `SELECT workspace_yaml FROM workflow_v2_runs WHERE id=?`, runID).Scan(&workspaceYAML); err != nil {
		return nil, EventResult{}, err
	}
	resolvedWorkspace, err := typesv2.ParseAndValidateWorkspace([]byte(workspaceYAML))
	if err != nil {
		return nil, EventResult{}, fmt.Errorf("load pinned workspace: %w", err)
	}

	verified := make([]typesv2.VerifiedPullRequest, 0, len(manifest.PullRequests))
	claimsByURL := map[string]bool{}
	identities := map[string]bool{}
	for i, claim := range manifest.PullRequests {
		claim.URL = strings.TrimSpace(claim.URL)
		if strings.TrimSpace(claim.Supersedes) != "" {
			return nil, EventResult{}, fmt.Errorf("pull_requests[%d].supersedes requires hub-owned source-control reconciliation", i)
		}
		parsed, err := url.Parse(claim.URL)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
			return nil, EventResult{}, fmt.Errorf("pull_requests[%d].url is not an absolute HTTP(S) URL", i)
		}
		if claimsByURL[claim.URL] {
			return nil, EventResult{}, fmt.Errorf("pull_requests[%d].url is duplicated", i)
		}
		claimsByURL[claim.URL] = true
		pr, err := verifier.VerifyPullRequest(ctx, run, resolvedWorkspace.Workspace, claim)
		if err != nil {
			return nil, EventResult{}, fmt.Errorf("verify pull_requests[%d] %s: %w", i, claim.URL, err)
		}
		if err := validateVerifiedPullRequest(resolvedWorkspace.Workspace, claim, pr); err != nil {
			return nil, EventResult{}, fmt.Errorf("verify pull_requests[%d] %s: %w", i, claim.URL, err)
		}
		identity := pr.Repository + "#" + fmt.Sprint(pr.Number)
		if identities[identity] {
			return nil, EventResult{}, fmt.Errorf("pull_requests[%d] duplicates verified PR %s", i, identity)
		}
		identities[identity] = true
		verified = append(verified, pr)
	}

	now := s.now().UTC()
	eventResult, err := s.applyEventWithMutation(ctx, runID, EventInput{
		ID: eventID, MessageID: messageID, Kind: "delivery.verified", AttemptID: attemptID,
		ExpectedStateVersion: expectedStateVersion, Producer: ProducerSourceControl,
		Provenance: typesv2.EvidenceProvenance{Producer: string(ProducerSourceControl), ObservedAt: now},
	}, func(ctx context.Context, tx *sql.Tx, input *EventInput) error {
		for i := range verified {
			pr := &verified[i]
			var existingID, existingHead string
			err := tx.QueryRowContext(ctx, `SELECT id,current_head_sha FROM workflow_v2_delivery_prs
				WHERE run_id=? AND repository=? AND pr_number=?`, runID, pr.Repository, pr.Number).Scan(&existingID, &existingHead)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			if existingID == "" {
				pr.ID = uuid.NewString()
			} else {
				pr.ID = existingID
			}
			provenanceJSON, err := marshalObject(pr.Provenance)
			if err != nil {
				return err
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
				return err
			}
			if existingHead != pr.HeadSHA {
				var generation int
				if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation),0)+1 FROM workflow_v2_delivery_heads WHERE pr_id=?`,
					pr.ID).Scan(&generation); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_v2_delivery_heads(id,pr_id,head_sha,generation,observed_at)
					VALUES(?,?,?,?,?)`, uuid.NewString(), pr.ID, pr.HeadSHA, generation, now.UnixMilli()); err != nil {
					return err
				}
				if existingHead != "" {
					if _, err := tx.ExecContext(ctx, `UPDATE workflow_v2_evidence SET superseded_at=?
						WHERE run_id=? AND pr_id=? AND superseded_at=0`, now.UnixMilli(), runID, pr.ID); err != nil {
						return err
					}
				}
			}
		}
		summary, err := deliveryFactsFrom(ctx, tx, runID)
		if err != nil {
			return err
		}
		input.Payload = map[string]interface{}{"delivery": summary}
		input.Facts = map[string]interface{}{
			"delivery.count": summary["count"], "delivery.open": summary["open"],
			"delivery.merged": summary["merged"], "delivery.all_merged": summary["all_merged"],
		}
		return nil
	})
	if err != nil {
		return nil, EventResult{}, err
	}
	if eventResult.Disposition == typesv2.DispositionAccepted {
		if _, err := s.evaluateAndPublishDeliveryPolicy(ctx, runID, attemptID, "", now); err != nil {
			return nil, EventResult{}, err
		}
	}
	sort.Slice(verified, func(i, j int) bool {
		if verified[i].Repository == verified[j].Repository {
			return verified[i].Number < verified[j].Number
		}
		return verified[i].Repository < verified[j].Repository
	})
	return verified, eventResult, nil
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

type rowQueryer interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}

func deliveryFactsFrom(ctx context.Context, queryer rowQueryer, runID string) (map[string]interface{}, error) {
	var count, open, merged int
	if err := queryer.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN state='open' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN state='merged' THEN 1 ELSE 0 END),0)
		FROM workflow_v2_delivery_prs WHERE run_id=? AND active=1`, runID).Scan(&count, &open, &merged); err != nil {
		return nil, err
	}
	return map[string]interface{}{"count": count, "open": open, "merged": merged,
		"all_merged": merged == count}, nil
}
