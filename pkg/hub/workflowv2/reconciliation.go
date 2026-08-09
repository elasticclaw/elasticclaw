package workflowv2

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
	"github.com/google/uuid"
)

// DeliveryTarget is a currently owned, hub-verified delivery subject that an
// external adapter may reconcile. Repository identity always originates from
// the pinned workspace and the verified delivery row, never from chat text.
type DeliveryTarget struct {
	RunID          string
	AttemptID      string
	PRID           string
	RepositoryName string
	Repository     string
	Number         int
	URL            string
	HeadSHA        string
	HeadObservedAt time.Time
	State          string
	WorkspaceYAML  string
	WorkflowYAML   string
}

// ActiveDeliveryTargets finds every active V2 run that owns a verified PR. A PR
// may intentionally belong to more than one run, so callers must reconcile
// every returned target.
func (s *Store) ActiveDeliveryTargets(ctx context.Context, repository string, number int) ([]DeliveryTarget, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("workflow v2 store is not configured")
	}
	repository = strings.TrimSpace(repository)
	if repository == "" || number <= 0 {
		return nil, fmt.Errorf("repository and positive pull request number are required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.id,r.current_attempt_id,p.id,p.repository_name,p.repository,
		p.pr_number,p.url,p.current_head_sha,p.state,r.workspace_yaml,r.workflow_yaml,
		COALESCE((SELECT h.observed_at FROM workflow_v2_delivery_heads h WHERE h.pr_id=p.id
			ORDER BY h.generation DESC LIMIT 1),p.verified_at)
		FROM workflow_v2_delivery_prs p JOIN workflow_v2_runs r ON r.id=p.run_id
		JOIN workflow_v2_attempts a ON a.id=r.current_attempt_id
		WHERE p.repository=? AND p.pr_number=? AND p.active=1 AND r.status='active'
		AND a.status='active' ORDER BY r.id`, repository, number)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDeliveryTargets(rows)
}

// ActiveDeliveryTargetsByHead resolves check webhooks that omit pull request
// numbers (notably fork PRs). The SHA was independently verified when the
// delivery manifest was accepted, so it remains a trusted subject lookup key.
func (s *Store) ActiveDeliveryTargetsByHead(ctx context.Context, headSHA string) ([]DeliveryTarget, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("workflow v2 store is not configured")
	}
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		return nil, fmt.Errorf("pull request head SHA is required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.id,r.current_attempt_id,p.id,p.repository_name,p.repository,
		p.pr_number,p.url,p.current_head_sha,p.state,r.workspace_yaml,r.workflow_yaml,
		COALESCE((SELECT h.observed_at FROM workflow_v2_delivery_heads h WHERE h.pr_id=p.id
			ORDER BY h.generation DESC LIMIT 1),p.verified_at)
		FROM workflow_v2_delivery_prs p JOIN workflow_v2_runs r ON r.id=p.run_id
		JOIN workflow_v2_attempts a ON a.id=r.current_attempt_id
		WHERE p.current_head_sha=? AND p.active=1 AND r.status='active' AND a.status='active'
		ORDER BY r.id,p.repository,p.pr_number`, headSHA)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDeliveryTargets(rows)
}

// ActiveDeliveryTargetsAll returns every currently owned V2 delivery subject
// for periodic provider reconciliation. V1 rows and suspended runs are never
// selected.
func (s *Store) ActiveDeliveryTargetsAll(ctx context.Context) ([]DeliveryTarget, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("workflow v2 store is not configured")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.id,r.current_attempt_id,p.id,p.repository_name,p.repository,
		p.pr_number,p.url,p.current_head_sha,p.state,r.workspace_yaml,r.workflow_yaml,
		COALESCE((SELECT h.observed_at FROM workflow_v2_delivery_heads h WHERE h.pr_id=p.id
			ORDER BY h.generation DESC LIMIT 1),p.verified_at)
		FROM workflow_v2_delivery_prs p JOIN workflow_v2_runs r ON r.id=p.run_id
		JOIN workflow_v2_attempts a ON a.id=r.current_attempt_id
		WHERE p.active=1 AND r.status='active' AND a.status='active'
		ORDER BY r.updated_at,p.repository,p.pr_number,r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDeliveryTargets(rows)
}

func scanDeliveryTargets(rows *sql.Rows) ([]DeliveryTarget, error) {
	var targets []DeliveryTarget
	for rows.Next() {
		var target DeliveryTarget
		var headObservedAt int64
		if err := rows.Scan(&target.RunID, &target.AttemptID, &target.PRID, &target.RepositoryName,
			&target.Repository, &target.Number, &target.URL, &target.HeadSHA, &target.State,
			&target.WorkspaceYAML, &target.WorkflowYAML, &headObservedAt); err != nil {
			return nil, err
		}
		target.HeadObservedAt = time.UnixMilli(headObservedAt).UTC()
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

// ReconcilePullRequest applies a trusted source-control observation to one
// existing delivery subject, invalidates old-head evidence, emits a typed
// workflow event, and then recomputes aggregate delivery policy.
func (s *Store) ReconcilePullRequest(ctx context.Context, target DeliveryTarget, eventID, eventKind string,
	verified typesv2.VerifiedPullRequest) (EventResult, error) {
	if s == nil || s.db == nil {
		return EventResult{}, fmt.Errorf("workflow v2 store is not configured")
	}
	if target.RunID == "" || target.AttemptID == "" || target.PRID == "" {
		return EventResult{}, fmt.Errorf("delivery target run, attempt, and pull request are required")
	}
	if !strings.HasPrefix(eventKind, "pull_request.") {
		return EventResult{}, fmt.Errorf("source-control reconciliation event %q must use pull_request namespace", eventKind)
	}
	workspace, err := typesv2.ParseAndValidateWorkspace([]byte(target.WorkspaceYAML))
	if err != nil {
		return EventResult{}, fmt.Errorf("load pinned workspace: %w", err)
	}
	if err := validateVerifiedPullRequest(workspace.Workspace, typesv2.PullRequestClaim{URL: target.URL}, verified); err != nil {
		return EventResult{}, err
	}
	if verified.Repository != target.Repository || verified.Number != target.Number || verified.RepositoryName != target.RepositoryName {
		return EventResult{}, fmt.Errorf("reconciled pull request does not match owned delivery target")
	}
	if strings.TrimSpace(eventID) == "" {
		eventID = uuid.NewString()
	}
	observed := verified.Provenance.ObservedAt.UTC()
	if observed.IsZero() {
		observed = s.now().UTC()
	}
	event, err := s.applyEventWithMutation(ctx, target.RunID, EventInput{
		ID: eventID, MessageID: eventID, Kind: eventKind, AttemptID: target.AttemptID,
		Producer: ProducerSourceControl, Provenance: verified.Provenance,
	}, func(ctx context.Context, tx *sql.Tx, input *EventInput) error {
		var currentHead string
		var currentState string
		if err := tx.QueryRowContext(ctx, `SELECT current_head_sha,state FROM workflow_v2_delivery_prs
			WHERE id=? AND run_id=? AND repository=? AND pr_number=? AND active=1`, target.PRID,
			target.RunID, target.Repository, target.Number).Scan(&currentHead, &currentState); err != nil {
			return err
		}
		if currentHead != target.HeadSHA || currentState != target.State {
			return fmt.Errorf("verified delivery changed while reconciling")
		}
		provenanceJSON, err := marshalObject(verified.Provenance)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE workflow_v2_delivery_prs SET url=?,source_branch=?,base_branch=?,
			current_head_sha=?,state=?,provenance_json=?,verified_at=?,updated_at=?
			WHERE id=? AND run_id=? AND active=1`, verified.URL, verified.SourceBranch, verified.BaseBranch,
			verified.HeadSHA, verified.State, provenanceJSON, verified.VerifiedAt.UnixMilli(), observed.UnixMilli(),
			target.PRID, target.RunID)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil || changed != 1 {
			return fmt.Errorf("verified delivery changed while reconciling")
		}
		if currentHead != verified.HeadSHA {
			var generation int
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation),0)+1 FROM workflow_v2_delivery_heads
				WHERE pr_id=?`, target.PRID).Scan(&generation); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_v2_delivery_heads(id,pr_id,head_sha,generation,observed_at)
				VALUES(?,?,?,?,?)`, uuid.NewString(), target.PRID, verified.HeadSHA, generation, observed.UnixMilli()); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE workflow_v2_evidence SET superseded_at=?
				WHERE run_id=? AND pr_id=? AND superseded_at=0`, observed.UnixMilli(), target.RunID, target.PRID); err != nil {
				return err
			}
		}
		summary, err := deliveryFactsFrom(ctx, tx, target.RunID)
		if err != nil {
			return err
		}
		pullRequest := map[string]interface{}{
			"id": target.PRID, "repository": verified.Repository, "number": verified.Number,
			"url": verified.URL, "head_sha": verified.HeadSHA, "state": verified.State,
			"previous_head_sha": currentHead, "previous_state": currentState,
			"head_changed": currentHead != verified.HeadSHA,
		}
		input.Payload = map[string]interface{}{"pull_request": pullRequest, "delivery": summary}
		input.Facts = map[string]interface{}{
			"pull_request.id": target.PRID, "pull_request.repository": verified.Repository,
			"pull_request.number": verified.Number, "pull_request.url": verified.URL,
			"pull_request.head_sha": verified.HeadSHA, "pull_request.state": verified.State,
			"pull_request.head_changed": currentHead != verified.HeadSHA,
			"delivery.count":            summary["count"], "delivery.open": summary["open"],
			"delivery.merged": summary["merged"], "delivery.all_merged": summary["all_merged"],
		}
		return nil
	})
	if err != nil {
		return EventResult{}, err
	}
	if event.Disposition == typesv2.DispositionAccepted {
		if _, err := s.evaluateAndPublishDeliveryPolicy(ctx, target.RunID, target.AttemptID, "", observed); err != nil {
			return EventResult{}, err
		}
	}
	return event, nil
}

// ApplyReviewFeedback records a typed, trusted review event and current fact.
// Workflows may use this event to transition back to build and include the fact
// in the resulting agent task; no conversation row participates.
func (s *Store) ApplyReviewFeedback(ctx context.Context, target DeliveryTarget, eventID string,
	feedback map[string]interface{}, provenance typesv2.EvidenceProvenance) (EventResult, error) {
	if target.RunID == "" || target.AttemptID == "" || target.PRID == "" || target.HeadSHA == "" {
		return EventResult{}, fmt.Errorf("review feedback requires an owned delivery target")
	}
	if strings.TrimSpace(eventID) == "" {
		eventID = uuid.NewString()
	}
	return s.applyEventWithMutation(ctx, target.RunID, EventInput{
		ID: eventID, MessageID: eventID, Kind: "review.feedback.received", AttemptID: target.AttemptID,
		Producer: ProducerReview, Provenance: provenance,
	}, func(ctx context.Context, tx *sql.Tx, input *EventInput) error {
		var currentProvenance string
		err := tx.QueryRowContext(ctx, `SELECT provenance_json FROM workflow_v2_facts
			WHERE run_id=? AND fact_key='review.feedback'`, target.RunID).Scan(&currentProvenance)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil {
			var current typesv2.EvidenceProvenance
			if err := json.Unmarshal([]byte(currentProvenance), &current); err != nil {
				return fmt.Errorf("decode current review feedback provenance: %w", err)
			}
			if current.ObservedAt.After(provenance.ObservedAt) {
				return fmt.Errorf("review feedback observation is older than the current fact")
			}
		}
		var headSHA string
		if err := tx.QueryRowContext(ctx, `SELECT current_head_sha FROM workflow_v2_delivery_prs
			WHERE id=? AND run_id=? AND repository=? AND pr_number=? AND active=1`, target.PRID,
			target.RunID, target.Repository, target.Number).Scan(&headSHA); err != nil {
			return fmt.Errorf("load review delivery target: %w", err)
		}
		if headSHA != target.HeadSHA {
			return fmt.Errorf("review feedback head %s is stale; current head is %s", target.HeadSHA, headSHA)
		}
		input.Payload = map[string]interface{}{"review": map[string]interface{}{
			"feedback": feedback, "pull_request_id": target.PRID, "head_sha": headSHA,
		}}
		input.Facts = map[string]interface{}{"review.feedback": feedback}
		return nil
	})
}
