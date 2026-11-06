package workflowv2

import (
	"context"
	"database/sql"
	"fmt"

	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

// RegisterPullRequest associates a verified PR without publishing delivery facts
// or evaluating workflow transitions. The bool reports a newly inserted PR.
func (s *Store) RegisterPullRequest(ctx context.Context, runID, attemptID string, claim typesv2.PullRequestClaim,
	verifier PullRequestVerifier) (typesv2.VerifiedPullRequest, bool, error) {
	verified, inserted, _, err := s.registerPullRequests(ctx, runID, attemptID,
		typesv2.DeliveryManifest{PullRequests: []typesv2.PullRequestClaim{claim}}, verifier, nil)
	if err != nil {
		return typesv2.VerifiedPullRequest{}, false, err
	}
	return verified[0], inserted, nil
}

func (s *Store) registerPullRequests(ctx context.Context, runID, attemptID string, manifest typesv2.DeliveryManifest,
	verifier PullRequestVerifier, envelope *typesv2.ControlEnvelope) ([]typesv2.VerifiedPullRequest, bool, typesv2.ControlReceipt, error) {
	verified, err := s.verifyDeliveryClaims(ctx, runID, attemptID, manifest, verifier)
	if err != nil {
		return nil, false, typesv2.ControlReceipt{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, false, typesv2.ControlReceipt{}, err
	}
	defer tx.Rollback()
	run, _, err := getRunForUpdate(ctx, tx, runID)
	if err != nil {
		return nil, false, typesv2.ControlReceipt{}, err
	}
	if run.Status != RunActive && run.Status != RunSuspended {
		return nil, false, typesv2.ControlReceipt{}, fmt.Errorf("registration run is no longer active")
	}
	if err := authorizeBoundAttempt(ctx, tx, run, EventInput{AttemptID: attemptID}); err != nil {
		return nil, false, typesv2.ControlReceipt{}, err
	}
	if envelope != nil {
		receipt, handled, err := s.preflightDeliveryControlTx(ctx, tx, *envelope)
		if err != nil {
			return nil, false, typesv2.ControlReceipt{}, err
		}
		if handled {
			return nil, false, receipt, tx.Commit()
		}
	}
	now := s.now().UTC()
	inserted := false
	for i := range verified {
		added, err := upsertDeliveryPR(ctx, tx, runID, &verified[i], true, now)
		if err != nil {
			return nil, false, typesv2.ControlReceipt{}, err
		}
		inserted = inserted || added
	}
	var receipt typesv2.ControlReceipt
	if envelope != nil {
		// Record receipt identity directly: claims are audit records, never
		// inputs to the workflow transition engine.
		if err := insertEvent(ctx, tx, envelope.MessageID, runID, envelope.MessageID, string(envelope.Kind),
			attemptID, envelope.ExpectedStateVersion, run.StateVersion, typesv2.DispositionAccepted, "",
			ProducerSourceControl, typesv2.EvidenceProvenance{Producer: string(ProducerSourceControl), ObservedAt: now},
			nil, nil, now); err != nil {
			return nil, false, typesv2.ControlReceipt{}, err
		}
		if err := insertEventReceipt(ctx, tx, runID, envelope.MessageID, envelope.MessageID,
			typesv2.DispositionAccepted, run.StateVersion, "", now); err != nil {
			return nil, false, typesv2.ControlReceipt{}, err
		}
		receipt = typesv2.ControlReceipt{MessageID: envelope.MessageID, Disposition: typesv2.DispositionAccepted,
			StateVersion: run.StateVersion}
	}
	return verified, inserted, receipt, tx.Commit()
}
