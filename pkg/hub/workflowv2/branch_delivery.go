package workflowv2

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
	"github.com/google/uuid"
)

func (s *Store) branchDeliveryConfig(ctx context.Context, runID string) (*typesv2.Workflow, *typesv2.Workspace, error) {
	var workflowYAML, workspaceYAML string
	if err := s.db.QueryRowContext(ctx, `SELECT workflow_yaml,workspace_yaml FROM workflow_v2_runs WHERE id=?`, runID).Scan(&workflowYAML, &workspaceYAML); err != nil {
		return nil, nil, err
	}
	wf, err := typesv2.ParseAndValidateWorkflow([]byte(workflowYAML))
	if err != nil {
		return nil, nil, err
	}
	ws, err := typesv2.ParseAndValidateWorkspace([]byte(workspaceYAML))
	if err != nil {
		return nil, nil, err
	}
	return wf.Workflow, ws.Workspace, nil
}

func matchesDeliveryBranch(wf *typesv2.Workflow, pr typesv2.VerifiedPullRequest) bool {
	return wf.Delivery != nil && wf.Delivery.PullRequests.MatchesBranch(pr.RepositoryName, pr.SourceBranch)
}

// claimBranchDelivery transfers only released ownership. The caller holds the
// same transaction that inserts the replacement row and its delivery event.
func claimBranchDelivery(ctx context.Context, tx *sql.Tx, run Run, pr *typesv2.VerifiedPullRequest) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT p.id,r.status,r.tenant_id FROM workflow_v2_delivery_prs p
 JOIN workflow_v2_runs r ON r.id=p.run_id
 WHERE p.repository=? COLLATE NOCASE AND p.pr_number=? AND p.active=1 AND p.run_id<>?
 ORDER BY p.updated_at DESC,p.id`, pr.Repository, pr.Number, run.ID)
	if err != nil {
		return false, err
	}
	var previous []string
	blocked := false
	for rows.Next() {
		var id, status, tenantID string
		if err := rows.Scan(&id, &status, &tenantID); err != nil {
			rows.Close()
			return false, err
		}
		if status == string(RunActive) || status == string(RunSuspended) || tenantID != run.TenantID {
			blocked = true
		}
		previous = append(previous, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	if blocked {
		return false, nil
	}
	pr.Supersedes = ""
	if len(previous) > 0 {
		pr.Supersedes = previous[0]
	}
	for _, id := range previous {
		if _, err := tx.ExecContext(ctx, `UPDATE workflow_v2_delivery_prs SET active=0 WHERE id=?`, id); err != nil {
			return false, err
		}
	}
	return true, nil
}

func deliveryEventFacts(ctx context.Context, tx *sql.Tx, runID string, input *EventInput) error {
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
}

// AdoptPullRequests accepts only hub-resolved candidates. Untrusted manifests
// continue to reject supersedes; the prior owner is selected under transaction.
// A stable receipt makes activation retries unable to steal ownership again.
func (s *Store) AdoptPullRequests(ctx context.Context, runID string, candidates []typesv2.VerifiedPullRequest, lookupError string) ([]typesv2.VerifiedPullRequest, error) {
	wf, ws, err := s.branchDeliveryConfig(ctx, runID)
	if err != nil {
		return nil, err
	}
	if wf.Delivery == nil || wf.Delivery.PullRequests == nil || len(wf.Delivery.PullRequests.Branches) == 0 {
		return nil, nil
	}
	now := s.now().UTC()
	var adopted []typesv2.VerifiedPullRequest
	result, err := s.applyEventWithMutation(ctx, runID, EventInput{
		ID: runID + "/delivery-adoption", Kind: "delivery.adoption_checked", Producer: ProducerSourceControl,
		Provenance: typesv2.EvidenceProvenance{Producer: string(ProducerSourceControl), ObservedAt: now},
	}, func(ctx context.Context, tx *sql.Tx, input *EventInput) error {
		run, _, err := getRunForUpdate(ctx, tx, runID)
		if err != nil {
			return err
		}
		if run.Status != RunActive && run.Status != RunSuspended {
			return fmt.Errorf("adoption run is no longer active")
		}
		seen := map[string]bool{}
		for _, pr := range candidates {
			if lookupError != "" {
				break
			}
			if pr.State != "open" || !matchesDeliveryBranch(wf, pr) {
				continue
			}
			if err := validateVerifiedPullRequest(ws, typesv2.PullRequestClaim{URL: pr.URL}, pr); err != nil {
				return err
			}
			key := fmt.Sprintf("%s#%d", pr.Repository, pr.Number)
			if seen[key] {
				continue
			}
			seen[key] = true
			allowed, err := claimBranchDelivery(ctx, tx, run, &pr)
			if err != nil {
				return err
			}
			if !allowed {
				continue
			}
			if _, err := upsertDeliveryPR(ctx, tx, runID, &pr, false, now); err != nil {
				return err
			}
			adopted = append(adopted, pr)
		}
		if err := deliveryEventFacts(ctx, tx, runID, input); err != nil {
			return err
		}
		input.Facts["delivery.adopted"] = len(adopted) > 0
		input.Facts["delivery.adopted_count"] = len(adopted)
		if lookupError != "" {
			input.Payload["delivery"].(map[string]interface{})["adoption_error"] = lookupError
		}
		if len(adopted) > 0 {
			input.Kind = "delivery.adopted"
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if result.Disposition == typesv2.DispositionAccepted && len(adopted) > 0 {
		_, err = s.evaluateAndPublishDeliveryPolicy(ctx, runID, "", "", now)
	}
	return adopted, err
}

var errNoBranchPromotion = errors.New("registration did not promote a delivery branch")

func (s *Store) registerBranchPullRequests(ctx context.Context, runID, attemptID string, wf *typesv2.Workflow,
	verified []typesv2.VerifiedPullRequest, envelope *typesv2.ControlEnvelope) ([]typesv2.VerifiedPullRequest, bool, typesv2.ControlReceipt, error) {
	now := s.now().UTC()
	input := EventInput{ID: uuid.NewString(), Kind: "pull_request.claimed", AttemptID: attemptID,
		Producer: ProducerSourceControl, Provenance: typesv2.EvidenceProvenance{Producer: string(ProducerSourceControl), ObservedAt: now}}
	if envelope != nil {
		input.ID, input.MessageID, input.ExpectedStateVersion = envelope.MessageID, envelope.MessageID, envelope.ExpectedStateVersion
	}
	inserted, promoted := false, false
	result, err := s.applyEventWithMutation(ctx, runID, input, func(ctx context.Context, tx *sql.Tx, input *EventInput) error {
		run, _, err := getRunForUpdate(ctx, tx, runID)
		if err != nil {
			return err
		}
		if run.Status != RunActive && run.Status != RunSuspended {
			return fmt.Errorf("registration run is no longer active")
		}
		for i := range verified {
			pr := &verified[i]
			// A PR owned by another run stays registration-only here.
			promote := matchesDeliveryBranch(wf, *pr)
			if promote {
				if promote, err = claimBranchDelivery(ctx, tx, run, pr); err != nil {
					return err
				}
			}
			added, err := upsertDeliveryPR(ctx, tx, runID, pr, true, now)
			if err != nil {
				return err
			}
			inserted = inserted || added
			if promote {
				result, err := tx.ExecContext(ctx, `UPDATE workflow_v2_delivery_prs SET registered_only=0 WHERE id=? AND active=1 AND registered_only=1`, pr.ID)
				if err != nil {
					return err
				}
				count, err := result.RowsAffected()
				if err != nil {
					return err
				}
				promoted = promoted || count > 0
			}
		}
		if !promoted {
			return errNoBranchPromotion
		}
		input.Kind = "delivery.verified"
		return deliveryEventFacts(ctx, tx, runID, input)
	})
	if err != nil {
		return nil, false, typesv2.ControlReceipt{}, err
	}
	if promoted && result.Disposition == typesv2.DispositionAccepted {
		if _, err := s.evaluateAndPublishDeliveryPolicy(ctx, runID, attemptID, "", now); err != nil {
			return nil, false, typesv2.ControlReceipt{}, err
		}
	}
	return verified, inserted, typesv2.ControlReceipt{MessageID: input.MessageID, Disposition: result.Disposition, StateVersion: result.Run.StateVersion, Reason: result.Reason}, nil
}
