package workflowv2

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
	"github.com/google/uuid"
)

type EvidenceInput struct {
	ID         string
	RunID      string
	PRID       string
	HeadSHA    string
	Domain     string
	Connection string
	ExternalID string
	Kind       string
	Status     string
	Payload    map[string]interface{}
	Provenance typesv2.EvidenceProvenance
	ObservedAt time.Time
}

type DeliveryPolicyResult struct {
	Revision         string `json:"revision"`
	PullRequestCount int    `json:"pull_request_count"`
	MinimumMet       bool   `json:"minimum_met"`
	CISatisfied      bool   `json:"ci_satisfied"`
	CIStatus         string `json:"ci_status"`
	ReviewSatisfied  bool   `json:"review_satisfied"`
	ReviewStatus     string `json:"review_status"`
	AllMerged        bool   `json:"all_merged"`
	Satisfied        bool   `json:"satisfied"`
}

// RecordEvidence accepts only adapter-owned evidence for the current verified
// PR head, then recomputes the workflow's aggregate delivery policy.
func (s *Store) RecordEvidence(ctx context.Context, input EvidenceInput, producer Producer) (DeliveryPolicyResult, error) {
	if s == nil || s.db == nil {
		return DeliveryPolicyResult{}, fmt.Errorf("workflow v2 store is not configured")
	}
	input.Domain = strings.ToLower(strings.TrimSpace(input.Domain))
	input.Connection = strings.TrimSpace(input.Connection)
	input.ExternalID = strings.TrimSpace(input.ExternalID)
	input.Kind = strings.TrimSpace(input.Kind)
	input.Status = strings.ToLower(strings.TrimSpace(input.Status))
	if err := validateEvidenceProducer(input.Domain, producer); err != nil {
		return DeliveryPolicyResult{}, err
	}
	if input.RunID == "" || input.PRID == "" || input.HeadSHA == "" || input.ExternalID == "" || input.Kind == "" || input.Status == "" {
		return DeliveryPolicyResult{}, fmt.Errorf("run, pull request, head, external id, kind, and status are required")
	}
	if strings.TrimSpace(input.Provenance.Producer) != string(producer) {
		return DeliveryPolicyResult{}, fmt.Errorf("evidence provenance producer %q does not match trusted adapter %q",
			input.Provenance.Producer, producer)
	}
	observed := input.ObservedAt.UTC()
	if observed.IsZero() {
		observed = s.now().UTC()
	}
	if input.Provenance.ObservedAt.IsZero() {
		input.Provenance.ObservedAt = observed
	}
	payloadJSON, err := json.Marshal(input.Payload)
	if err != nil {
		return DeliveryPolicyResult{}, err
	}
	if input.Payload == nil {
		payloadJSON = []byte("{}")
	}
	provenanceJSON, err := marshalObject(input.Provenance)
	if err != nil {
		return DeliveryPolicyResult{}, err
	}
	id := strings.TrimSpace(input.ID)
	if id == "" {
		id = uuid.NewString()
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return DeliveryPolicyResult{}, err
	}
	defer tx.Rollback()
	var currentHead string
	var headGeneration int
	if err := tx.QueryRowContext(ctx, `SELECT p.current_head_sha,COALESCE(MAX(h.generation),0)
		FROM workflow_v2_delivery_prs p LEFT JOIN workflow_v2_delivery_heads h ON h.pr_id=p.id
		WHERE p.id=? AND p.run_id=? AND p.active=1 GROUP BY p.id`, input.PRID, input.RunID).Scan(
		&currentHead, &headGeneration); err != nil {
		return DeliveryPolicyResult{}, fmt.Errorf("load active delivery: %w", err)
	}
	if currentHead != input.HeadSHA {
		return DeliveryPolicyResult{}, fmt.Errorf("evidence head %s is stale; current head is %s", input.HeadSHA, currentHead)
	}
	if headGeneration < 1 {
		return DeliveryPolicyResult{}, fmt.Errorf("active delivery has no verified head generation")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO workflow_v2_evidence(
		id,run_id,pr_id,head_sha,head_generation,domain,connection,external_id,kind,status,payload_json,provenance_json,observed_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(run_id,pr_id,domain,connection,external_id,kind,head_generation) DO UPDATE SET
		status=excluded.status,payload_json=excluded.payload_json,provenance_json=excluded.provenance_json,
		observed_at=excluded.observed_at,superseded_at=0
		WHERE excluded.observed_at>=workflow_v2_evidence.observed_at`, id, input.RunID, input.PRID, input.HeadSHA,
		headGeneration, input.Domain, input.Connection, input.ExternalID, input.Kind, input.Status, string(payloadJSON),
		provenanceJSON, observed.UnixMilli())
	if err != nil {
		return DeliveryPolicyResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeliveryPolicyResult{}, err
	}
	return s.evaluateAndPublishDeliveryPolicy(ctx, input.RunID, "", input.Domain, observed)
}

var errDeliveryPolicyChanged = errors.New("delivery policy inputs changed before publication")

func (s *Store) evaluateAndPublishDeliveryPolicy(ctx context.Context, runID, attemptID, domain string,
	observed time.Time) (DeliveryPolicyResult, error) {
	for attempt := 0; attempt < 5; attempt++ {
		result, err := s.EvaluateDeliveryPolicy(ctx, runID)
		if err != nil {
			return DeliveryPolicyResult{}, err
		}
		if err := s.publishEvidencePolicyEvents(ctx, runID, attemptID, domain, result, observed); err != nil {
			if errors.Is(err, errDeliveryPolicyChanged) {
				continue
			}
			return DeliveryPolicyResult{}, err
		}
		return result, nil
	}
	return DeliveryPolicyResult{}, fmt.Errorf("delivery policy kept changing during publication")
}

func (s *Store) publishEvidencePolicyEvents(ctx context.Context, runID, attemptID, domain string,
	result DeliveryPolicyResult, observed time.Time) error {
	var workflowYAML string
	if err := s.db.QueryRowContext(ctx, `SELECT workflow_yaml FROM workflow_v2_runs WHERE id=?`, runID).Scan(&workflowYAML); err != nil {
		return err
	}
	workflow, err := typesv2.ParseAndValidateWorkflow([]byte(workflowYAML))
	if err != nil {
		return err
	}
	if workflow.Workflow.Delivery != nil && workflow.Workflow.Delivery.PullRequests != nil {
		requirements := workflow.Workflow.Delivery.PullRequests
		if domain == "ci" && requirements.CIPolicy != "" {
			status := result.CIStatus
			event, err := s.applyDeliveryPolicyEvent(ctx, runID, result, EventInput{
				ID: uuid.NewString(), Kind: "ci.policy.evaluated", AttemptID: attemptID, Producer: ProducerCI,
				Payload: map[string]interface{}{"ci": map[string]interface{}{
					"policy": requirements.CIPolicy, "status": status, "revision": result.Revision,
				}},
				Facts: map[string]interface{}{
					"ci.policy": requirements.CIPolicy, "ci.status": status, "ci.policy_revision": result.Revision,
				},
				Provenance: typesv2.EvidenceProvenance{Producer: string(ProducerCI), ObservedAt: observed},
			})
			if err != nil {
				return err
			}
			if event.Disposition != typesv2.DispositionAccepted {
				return fmt.Errorf("CI policy event was %s: %s", event.Disposition, event.Reason)
			}
		}
		if domain == "review" && requirements.ReviewPolicy != "" {
			status := result.ReviewStatus
			event, err := s.applyDeliveryPolicyEvent(ctx, runID, result, EventInput{
				ID: uuid.NewString(), Kind: "review.policy.evaluated", AttemptID: attemptID, Producer: ProducerReview,
				Payload: map[string]interface{}{"review": map[string]interface{}{
					"policy": requirements.ReviewPolicy, "status": status, "revision": result.Revision,
				}},
				Facts: map[string]interface{}{
					"review.policy": requirements.ReviewPolicy, "review.status": status,
					"review.policy_revision": result.Revision,
				},
				Provenance: typesv2.EvidenceProvenance{Producer: string(ProducerReview), ObservedAt: observed},
			})
			if err != nil {
				return err
			}
			if event.Disposition != typesv2.DispositionAccepted {
				return fmt.Errorf("review policy event was %s: %s", event.Disposition, event.Reason)
			}
		}
	}
	deliveryEvent, err := s.applyDeliveryPolicyEvent(ctx, runID, result, EventInput{
		ID: uuid.NewString(), Kind: "workflow.delivery.evaluated", AttemptID: attemptID, Producer: ProducerEngine,
		Payload: map[string]interface{}{"workflow": map[string]interface{}{"delivery": result}},
		Facts: map[string]interface{}{
			"delivery.minimum_met": result.MinimumMet, "delivery.ci_satisfied": result.CISatisfied,
			"delivery.review_satisfied": result.ReviewSatisfied, "delivery.all_merged": result.AllMerged,
			"delivery.satisfied": result.Satisfied, "delivery.policy_revision": result.Revision,
		},
		Provenance: typesv2.EvidenceProvenance{Producer: string(ProducerEngine), ObservedAt: observed},
	})
	if err != nil {
		return err
	}
	if deliveryEvent.Disposition != typesv2.DispositionAccepted {
		return fmt.Errorf("delivery policy event was %s: %s", deliveryEvent.Disposition, deliveryEvent.Reason)
	}
	return nil
}

func (s *Store) applyDeliveryPolicyEvent(ctx context.Context, runID string, expected DeliveryPolicyResult,
	input EventInput) (EventResult, error) {
	return s.applyEventWithMutation(ctx, runID, input, func(ctx context.Context, tx *sql.Tx, _ *EventInput) error {
		current, err := evaluateDeliveryPolicy(ctx, tx, runID)
		if err != nil {
			return err
		}
		if current.Revision != expected.Revision {
			return errDeliveryPolicyChanged
		}
		return nil
	})
}

func validateEvidenceProducer(domain string, producer Producer) error {
	allowed := false
	switch domain {
	case "ci":
		allowed = producer == ProducerCI
	case "review":
		allowed = producer == ProducerReview
	case "pull_request":
		allowed = producer == ProducerSourceControl
	case "operator":
		allowed = producer == ProducerOperator
	case "effect":
		allowed = producer == ProducerEffect
	case "context":
		allowed = producer == ProducerContext
	default:
		return fmt.Errorf("unsupported evidence domain %q", domain)
	}
	if !allowed {
		return fmt.Errorf("producer %q cannot record %s evidence", producer, domain)
	}
	return nil
}

type storedEvidence struct {
	Domain     string
	Connection string
	ExternalID string
	Kind       string
	Status     string
	Payload    map[string]interface{}
	ObservedAt time.Time
}

func (s *Store) EvaluateDeliveryPolicy(ctx context.Context, runID string) (DeliveryPolicyResult, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable, ReadOnly: true})
	if err != nil {
		return DeliveryPolicyResult{}, err
	}
	defer tx.Rollback()
	result, err := evaluateDeliveryPolicy(ctx, tx, runID)
	if err != nil {
		return DeliveryPolicyResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeliveryPolicyResult{}, err
	}
	return result, nil
}

type policyQueryer interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
}

type deliveryPolicySubject struct {
	ID         string           `json:"id"`
	Repository string           `json:"repository"`
	Number     int              `json:"number"`
	State      string           `json:"state"`
	HeadSHA    string           `json:"head_sha"`
	Generation int              `json:"generation"`
	Evidence   []storedEvidence `json:"evidence"`
}

func evaluateDeliveryPolicy(ctx context.Context, queryer policyQueryer, runID string) (DeliveryPolicyResult, error) {
	var workflowYAML string
	if err := queryer.QueryRowContext(ctx, `SELECT workflow_yaml FROM workflow_v2_runs WHERE id=?`, runID).Scan(&workflowYAML); err != nil {
		return DeliveryPolicyResult{}, err
	}
	workflow, err := typesv2.ParseAndValidateWorkflow([]byte(workflowYAML))
	if err != nil {
		return DeliveryPolicyResult{}, err
	}
	subjects, err := loadDeliveryPolicySubjects(ctx, queryer, runID)
	if err != nil {
		return DeliveryPolicyResult{}, err
	}
	revisionJSON, err := json.Marshal(subjects)
	if err != nil {
		return DeliveryPolicyResult{}, err
	}
	revisionHash := sha256.Sum256(revisionJSON)
	merged := 0
	for _, subject := range subjects {
		if subject.State == "merged" {
			merged++
		}
	}
	result := DeliveryPolicyResult{Revision: "sha256:" + hex.EncodeToString(revisionHash[:]), PullRequestCount: len(subjects),
		CISatisfied: true, CIStatus: policySatisfied, ReviewSatisfied: true, ReviewStatus: policySatisfied,
		AllMerged: len(subjects) == merged}
	policy := workflow.Workflow.Delivery
	if policy == nil || policy.PullRequests == nil {
		result.MinimumMet = true
		result.Satisfied = true
		return result, nil
	}
	requirements := policy.PullRequests
	result.MinimumMet = len(subjects) >= requirements.Minimum && (!requirements.Required || len(subjects) > 0)
	for _, subject := range subjects {
		if requirements.CIPolicy != "" {
			definition := workflow.Workflow.CI.Policies[requirements.CIPolicy]
			status, err := evaluateEvidencePolicyStatus(definition, "ci", subject.Evidence)
			if err != nil {
				return DeliveryPolicyResult{}, fmt.Errorf("evaluate CI policy %s: %w", requirements.CIPolicy, err)
			}
			result.CIStatus = combineAllPolicyStatus(result.CIStatus, status)
		}
		if requirements.ReviewPolicy != "" {
			definition := workflow.Workflow.Review.Policies[requirements.ReviewPolicy]
			status, err := evaluateEvidencePolicyStatus(definition, "review", subject.Evidence)
			if err != nil {
				return DeliveryPolicyResult{}, fmt.Errorf("evaluate review policy %s: %w", requirements.ReviewPolicy, err)
			}
			result.ReviewStatus = combineAllPolicyStatus(result.ReviewStatus, status)
		}
	}
	result.CISatisfied = result.CIStatus == policySatisfied
	result.ReviewSatisfied = result.ReviewStatus == policySatisfied
	completionMet := true
	if requirements.Completion == typesv2.DeliveryCompletionAllMerged {
		completionMet = result.AllMerged
	}
	result.Satisfied = result.MinimumMet && result.CISatisfied && result.ReviewSatisfied && completionMet
	return result, nil
}

func loadDeliveryPolicySubjects(ctx context.Context, queryer policyQueryer, runID string) ([]deliveryPolicySubject, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT p.id,p.repository,p.pr_number,p.state,p.current_head_sha,
		COALESCE(MAX(h.generation),0) FROM workflow_v2_delivery_prs p
		LEFT JOIN workflow_v2_delivery_heads h ON h.pr_id=p.id
		WHERE p.run_id=? AND p.active=1 GROUP BY p.id ORDER BY p.repository,p.pr_number`, runID)
	if err != nil {
		return nil, err
	}
	var subjects []deliveryPolicySubject
	for rows.Next() {
		var subject deliveryPolicySubject
		if err := rows.Scan(&subject.ID, &subject.Repository, &subject.Number, &subject.State, &subject.HeadSHA,
			&subject.Generation); err != nil {
			return nil, err
		}
		if subject.Generation < 1 {
			return nil, fmt.Errorf("active delivery %s has no verified head generation", subject.ID)
		}
		subjects = append(subjects, subject)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range subjects {
		evidence, err := loadCurrentEvidence(ctx, queryer, runID, subjects[i].ID, subjects[i].Generation)
		if err != nil {
			return nil, err
		}
		subjects[i].Evidence = evidence
	}
	return subjects, nil
}

func loadCurrentEvidence(ctx context.Context, queryer policyQueryer, runID, prID string, generation int) ([]storedEvidence, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT domain,connection,external_id,kind,status,payload_json,observed_at
		FROM workflow_v2_evidence WHERE run_id=? AND pr_id=? AND head_generation=? AND superseded_at=0
		ORDER BY domain,connection,external_id,kind,observed_at`, runID, prID, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []storedEvidence
	for rows.Next() {
		var item storedEvidence
		var payload string
		var observedAt int64
		if err := rows.Scan(&item.Domain, &item.Connection, &item.ExternalID, &item.Kind, &item.Status, &payload, &observedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(payload), &item.Payload); err != nil {
			return nil, err
		}
		item.ObservedAt = time.UnixMilli(observedAt).UTC()
		result = append(result, item)
	}
	return result, rows.Err()
}

func evaluateEvidencePolicy(policy typesv2.Policy, domain string, evidence []storedEvidence) (bool, error) {
	status, err := evaluateEvidencePolicyStatus(policy, domain, evidence)
	return status == policySatisfied, err
}

const (
	policyPending     = "pending"
	policySatisfied   = "satisfied"
	policyUnsatisfied = "unsatisfied"
)

func evaluateEvidencePolicyStatus(policy typesv2.Policy, domain string, evidence []storedEvidence) (string, error) {
	return evaluateEvidenceNode(map[string]interface{}(policy), domain, evidence)
}

func combineAllPolicyStatus(left, right string) string {
	if left == policyUnsatisfied || right == policyUnsatisfied {
		return policyUnsatisfied
	}
	if left == policyPending || right == policyPending {
		return policyPending
	}
	return policySatisfied
}

func evaluateEvidenceNode(node interface{}, domain string, evidence []storedEvidence) (string, error) {
	switch value := node.(type) {
	case map[string]interface{}:
		if raw, ok := value["all"]; ok {
			items, ok := raw.([]interface{})
			if !ok || len(items) == 0 {
				return "", fmt.Errorf("all requires a non-empty list")
			}
			status := policySatisfied
			for _, item := range items {
				itemStatus, err := evaluateEvidenceNode(item, domain, evidence)
				if err != nil {
					return "", err
				}
				status = combineAllPolicyStatus(status, itemStatus)
			}
			return status, nil
		}
		if raw, ok := value["any"]; ok {
			items, ok := raw.([]interface{})
			if !ok || len(items) == 0 {
				return "", fmt.Errorf("any requires a non-empty list")
			}
			pending := false
			for _, item := range items {
				itemStatus, err := evaluateEvidenceNode(item, domain, evidence)
				if err != nil {
					return "", err
				}
				if itemStatus == policySatisfied {
					return policySatisfied, nil
				}
				pending = pending || itemStatus == policyPending
			}
			if pending {
				return policyPending, nil
			}
			return policyUnsatisfied, nil
		}
		if raw, ok := value["not"]; ok {
			status, err := evaluateEvidenceNode(raw, domain, evidence)
			if err != nil || status == policyPending {
				return status, err
			}
			if status == policySatisfied {
				return policyUnsatisfied, nil
			}
			return policySatisfied, nil
		}
		return evaluateEvidenceLeaf(value, domain, evidence)
	case typesv2.Policy:
		return evaluateEvidenceNode(map[string]interface{}(value), domain, evidence)
	default:
		return "", fmt.Errorf("policy node has unsupported type %T", node)
	}
}

func evaluateEvidenceLeaf(leaf map[string]interface{}, domain string, evidence []storedEvidence) (string, error) {
	if domain == "ci" {
		pipeline, _ := leaf["pipeline"].(string)
		if pipeline == "" {
			return "", fmt.Errorf("CI policy leaf requires pipeline")
		}
		checks, err := stringList(leaf["checks"])
		if err != nil {
			return "", fmt.Errorf("CI policy leaf checks: %w", err)
		}
		if len(checks) == 0 {
			return "", fmt.Errorf("CI policy leaf checks must not be empty")
		}
		for _, check := range checks {
			var latest *storedEvidence
			for _, item := range evidence {
				if item.Domain == "ci" && item.Kind == check && item.Payload["pipeline"] == pipeline {
					candidate := item
					if latest == nil || candidate.ObservedAt.After(latest.ObservedAt) ||
						(candidate.ObservedAt.Equal(latest.ObservedAt) && candidate.ExternalID > latest.ExternalID) {
						latest = &candidate
					}
				}
			}
			if latest == nil {
				return policyPending, nil
			}
			if isFailureEvidenceStatus(latest.Status) {
				return policyUnsatisfied, nil
			}
			if latest.Status != "success" {
				return policyPending, nil
			}
		}
		return policySatisfied, nil
	}
	connection, _ := leaf["connection"].(string)
	if connection == "" {
		return "", fmt.Errorf("review policy leaf requires connection")
	}
	minimum := 1
	if approvals, ok := storedPolicyMap(leaf["approvals"]); ok {
		if raw, exists := approvals["minimum"]; exists {
			parsed, err := integerValue(raw)
			if err != nil || parsed < 0 {
				return "", fmt.Errorf("review approvals.minimum is invalid")
			}
			minimum = parsed
		}
	}
	latestByPrincipal := map[string]storedEvidence{}
	for _, item := range evidence {
		if item.Domain != "review" || item.Connection != connection || item.Kind != "approval" {
			continue
		}
		current, ok := latestByPrincipal[item.ExternalID]
		if !ok || item.ObservedAt.After(current.ObservedAt) ||
			(item.ObservedAt.Equal(current.ObservedAt) && item.Status > current.Status) {
			latestByPrincipal[item.ExternalID] = item
		}
	}
	approvals := 0
	changesRequested := false
	for _, item := range latestByPrincipal {
		if item.Status == "approved" {
			approvals++
		}
		changesRequested = changesRequested || isFailureEvidenceStatus(item.Status)
	}
	if approvals >= minimum && !changesRequested {
		return policySatisfied, nil
	}
	if changesRequested {
		return policyUnsatisfied, nil
	}
	return policyPending, nil
}

func storedPolicyMap(value interface{}) (map[string]interface{}, bool) {
	switch typed := value.(type) {
	case map[string]interface{}:
		return typed, true
	case typesv2.Policy:
		return map[string]interface{}(typed), true
	default:
		return nil, false
	}
}

func isFailureEvidenceStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failure", "failed", "error", "cancelled", "timed_out", "changes_requested", "rejected":
		return true
	default:
		return false
	}
}

func stringList(raw interface{}) ([]string, error) {
	items, ok := raw.([]interface{})
	if !ok {
		if typed, ok := raw.([]string); ok {
			return typed, nil
		}
		return nil, fmt.Errorf("must be a list")
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		value, ok := item.(string)
		if !ok || strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("must contain non-empty strings")
		}
		result = append(result, value)
	}
	return result, nil
}

func integerValue(raw interface{}) (int, error) {
	switch value := raw.(type) {
	case int:
		return value, nil
	case int64:
		return int(value), nil
	case float64:
		if value != float64(int(value)) {
			return 0, fmt.Errorf("must be an integer")
		}
		return int(value), nil
	case json.Number:
		parsed, err := strconv.Atoi(string(value))
		return parsed, err
	default:
		return 0, fmt.Errorf("must be an integer")
	}
}
