package workflowv2

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
	"github.com/google/uuid"
)

type RunStatus string

const (
	RunActive    RunStatus = "active"
	RunSuspended RunStatus = "suspended"
	RunCompleted RunStatus = "completed"
	RunCancelled RunStatus = "cancelled"
)

type Producer string

const (
	ProducerHub           Producer = "hub"
	ProducerEngine        Producer = "engine"
	ProducerAgent         Producer = "agent"
	ProducerCI            Producer = "ci"
	ProducerSourceControl Producer = "source_control"
	ProducerReview        Producer = "review"
	ProducerOperator      Producer = "operator"
	ProducerEffect        Producer = "effect"
	ProducerContext       Producer = "context"
	ProducerCustom        Producer = "custom"
)

type Run struct {
	ID                string               `json:"id"`
	TenantID          string               `json:"tenant_id"`
	WorkspaceName     string               `json:"workspace_name"`
	WorkflowName      string               `json:"workflow_name"`
	WorkspaceRevision string               `json:"workspace_revision"`
	WorkflowRevision  string               `json:"workflow_revision"`
	State             string               `json:"state"`
	DisplayPhase      typesv2.DisplayPhase `json:"display_phase"`
	StateVersion      uint64               `json:"state_version"`
	Status            RunStatus            `json:"status"`
	WaitingReason     string               `json:"waiting_reason,omitempty"`
	CurrentAttemptID  string               `json:"current_attempt_id,omitempty"`
	CurrentTaskID     string               `json:"current_task_id,omitempty"`
	ContextBundleID   string               `json:"context_bundle_id,omitempty"`
	CreatedAt         time.Time            `json:"created_at"`
	UpdatedAt         time.Time            `json:"updated_at"`
	FinishedAt        *time.Time           `json:"finished_at,omitempty"`
}

type CreateRunRequest struct {
	ID            string
	TenantID      string
	WorkspaceYAML []byte
	WorkflowYAML  []byte
	// InitialClawID atomically binds the first execution attempt while the run
	// is created. Production activation uses this so a newly provisioned bridge
	// cannot connect before its control-plane binding exists.
	InitialClawID string
	// ActivationPending keeps effects unclaimable until organization context
	// has been assembled and CompleteActivation has released the run.
	ActivationPending bool
}

type EventInput struct {
	ID                   string
	MessageID            string
	Kind                 string
	AttemptID            string
	TaskID               string
	ExpectedStateVersion *uint64
	Producer             Producer
	Provenance           typesv2.EvidenceProvenance
	Payload              map[string]interface{}
	Facts                map[string]interface{}
	mutation             eventMutation
}

type eventMutation func(context.Context, *sql.Tx, *EventInput) error

type EventResult struct {
	EventID     string                     `json:"event_id"`
	Disposition typesv2.ControlDisposition `json:"disposition"`
	Reason      string                     `json:"reason,omitempty"`
	Run         Run                        `json:"run"`
	Transition  *Transition                `json:"transition,omitempty"`
}

type Transition struct {
	ID             string    `json:"id"`
	EventID        string    `json:"event_id"`
	DefinitionName string    `json:"definition_name"`
	FromState      string    `json:"from_state"`
	ToState        string    `json:"to_state"`
	FromVersion    uint64    `json:"from_version"`
	ToVersion      uint64    `json:"to_version"`
	CreatedAt      time.Time `json:"created_at"`
}

type Store struct {
	db  *sql.DB
	now func() time.Time
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Store) SetClock(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

func (s *Store) CreateRun(ctx context.Context, req CreateRunRequest) (Run, error) {
	if s == nil || s.db == nil {
		return Run{}, fmt.Errorf("workflow v2 store is not configured")
	}
	if strings.TrimSpace(req.TenantID) == "" {
		return Run{}, fmt.Errorf("tenant_id is required")
	}
	rwf, rws, err := typesv2.ParseAndValidateWorkflowPair(req.WorkflowYAML, req.WorkspaceYAML)
	if err != nil {
		return Run{}, err
	}
	if !rwf.Workflow.Enabled {
		return Run{}, fmt.Errorf("workflow %q is disabled", rwf.Workflow.Name)
	}
	initial := rwf.Workflow.States[rwf.Workflow.InitialState]
	if initial.Phase == "" {
		return Run{}, fmt.Errorf("workflow %q initial state %q has no display phase", rwf.Workflow.Name, rwf.Workflow.InitialState)
	}

	runID := strings.TrimSpace(req.ID)
	if runID == "" {
		runID = uuid.NewString()
	}
	now := s.now().UTC()
	status := RunActive
	waitingReason := ""
	finishedAt := int64(0)
	if initial.Terminal {
		status = terminalRunStatus(rwf.Workflow.InitialState)
		finishedAt = now.UnixMilli()
	} else if req.ActivationPending {
		status = RunSuspended
		waitingReason = activationPendingReason
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	currentAttemptID := ""
	if strings.TrimSpace(req.InitialClawID) != "" {
		if initial.Terminal {
			return Run{}, fmt.Errorf("terminal workflow cannot start an execution attempt")
		}
		currentAttemptID = uuid.NewString()
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO workflow_v2_runs(
		id,tenant_id,workspace_name,workflow_name,workspace_revision,workflow_revision,
		workspace_yaml,workflow_yaml,state,display_phase,state_version,status,waiting_reason,current_attempt_id,created_at,updated_at,finished_at
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		runID, req.TenantID, rws.Workspace.Name, rwf.Workflow.Name, string(rws.Revision), string(rwf.Revision),
		string(req.WorkspaceYAML), string(req.WorkflowYAML), rwf.Workflow.InitialState, string(initial.Phase), 1, string(status), waitingReason, currentAttemptID,
		now.UnixMilli(), now.UnixMilli(), finishedAt)
	if err != nil {
		return Run{}, fmt.Errorf("create workflow v2 run: %w", err)
	}
	if currentAttemptID != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_v2_attempts(
			id,run_id,claw_id,number,status,started_at,heartbeat_at) VALUES(?,?,?,?,?,?,?)`,
			currentAttemptID, runID, strings.TrimSpace(req.InitialClawID), 1, "active", now.UnixMilli(), now.UnixMilli()); err != nil {
			return Run{}, fmt.Errorf("create initial workflow v2 attempt: %w", err)
		}
	}

	eventID := uuid.NewString()
	provenance := typesv2.EvidenceProvenance{Producer: string(ProducerEngine), ObservedAt: now}
	if err := insertEvent(ctx, tx, eventID, runID, "", "workflow.run.created", nil, 0,
		typesv2.DispositionAccepted, "", ProducerEngine, provenance, nil, nil, now); err != nil {
		return Run{}, err
	}
	transitionID := uuid.NewString()
	_, err = tx.ExecContext(ctx, `INSERT INTO workflow_v2_transitions(
		id,run_id,event_id,definition_name,from_state,to_state,from_version,to_version,
		workspace_revision,workflow_revision,fact_delta_json,created_at
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, transitionID, runID, eventID, "initial", "", rwf.Workflow.InitialState,
		0, 1, string(rws.Revision), string(rwf.Revision), `{}`, now.UnixMilli())
	if err != nil {
		return Run{}, fmt.Errorf("record initial transition: %w", err)
	}
	if initial.OnEnter != nil {
		writes := mergeWrites(initial.OnEnter.Assert, initial.OnEnter.Set)
		if err := writeFacts(ctx, tx, runID, eventID, ProducerEngine, provenance, writes, now); err != nil {
			return Run{}, err
		}
		if err := scheduleEffects(ctx, tx, runID, "initial", transitionID, "states."+rwf.Workflow.InitialState+".on_enter.effects", initial.OnEnter.Effects, now); err != nil {
			return Run{}, err
		}
	}
	if err := insertEventReceipt(ctx, tx, runID, eventID, "", typesv2.DispositionAccepted, 1, "", now); err != nil {
		return Run{}, err
	}
	if err := tx.Commit(); err != nil {
		return Run{}, err
	}
	return s.GetRun(ctx, runID)
}

const activationPendingReason = "workflow activation pending organization context"

// CompleteActivation releases a context-pinned run for effect execution. It is
// idempotent when context assembly already advanced the run or deliberately
// suspended it on a domain-level context failure.
func (s *Store) CompleteActivation(ctx context.Context, runID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("workflow v2 store is not configured")
	}
	now := s.now().UTC().UnixMilli()
	result, err := s.db.ExecContext(ctx, `UPDATE workflow_v2_runs SET status='active',waiting_reason='',updated_at=?
		WHERE id=? AND status='suspended' AND waiting_reason=? AND context_bundle_id!=''`,
		now, runID, activationPendingReason)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return err
	} else if changed == 1 {
		return nil
	}
	var status, waitingReason, contextBundleID string
	if err := s.db.QueryRowContext(ctx, `SELECT status,waiting_reason,context_bundle_id FROM workflow_v2_runs WHERE id=?`,
		runID).Scan(&status, &waitingReason, &contextBundleID); err != nil {
		return err
	}
	if contextBundleID == "" {
		return fmt.Errorf("workflow v2 run has no organization context bundle")
	}
	if status == string(RunActive) || (status == string(RunSuspended) && waitingReason != activationPendingReason) {
		return nil
	}
	return fmt.Errorf("workflow v2 run cannot complete activation from status %q", status)
}

// CancelActivation terminally closes a run whose pre-provision activation
// failed, including every attempt and queued effect that could otherwise be
// left bound to the claw being deleted by the caller.
func (s *Store) CancelActivation(ctx context.Context, runID, reason string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("workflow v2 store is not configured")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "workflow activation failed"
	}
	now := s.now().UTC().UnixMilli()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_v2_effect_attempts SET status='cancelled',error=?,finished_at=?
		WHERE status='running' AND effect_id IN (SELECT id FROM workflow_v2_effects WHERE run_id=?)`, reason, now, runID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_v2_effects SET status='cancelled',lease_owner='',lease_expires_at=0,
		last_error=?,updated_at=? WHERE run_id=? AND status IN ('planned','running','retryable_failed','unknown')`, reason, now, runID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_v2_control_outbox SET status='cancelled',last_error=?,updated_at=?
		WHERE run_id=? AND status IN ('pending','sent')`, reason, now, runID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_v2_agent_tasks SET status='cancelled',terminal_reason=?,updated_at=?,finished_at=?
		WHERE run_id=? AND status IN ('assigned','running')`, reason, now, now, runID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_v2_attempts SET status='cancelled',reason=?,finished_at=?
		WHERE run_id=? AND status IN ('provisioning','active')`, reason, now, runID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE workflow_v2_runs SET status='cancelled',waiting_reason=?,current_task_id='',
		updated_at=?,finished_at=? WHERE id=? AND status IN ('active','suspended')`, reason, now, now, runID)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return err
	} else if changed != 1 {
		return fmt.Errorf("workflow v2 run is not cancellable during activation")
	}
	return tx.Commit()
}

func (s *Store) GetRun(ctx context.Context, runID string) (Run, error) {
	if s == nil || s.db == nil {
		return Run{}, fmt.Errorf("workflow v2 store is not configured")
	}
	return scanRun(s.db.QueryRowContext(ctx, `SELECT id,tenant_id,workspace_name,workflow_name,
		workspace_revision,workflow_revision,state,display_phase,state_version,status,waiting_reason,
		current_attempt_id,current_task_id,context_bundle_id,created_at,updated_at,finished_at
		FROM workflow_v2_runs WHERE id=?`, runID))
}

func (s *Store) ApplyEvent(ctx context.Context, runID string, input EventInput) (EventResult, error) {
	if s == nil || s.db == nil {
		return EventResult{}, fmt.Errorf("workflow v2 store is not configured")
	}
	if strings.TrimSpace(input.ID) == "" {
		return EventResult{}, fmt.Errorf("event id is required")
	}
	if strings.TrimSpace(input.Kind) == "" {
		return EventResult{}, fmt.Errorf("event kind is required")
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return EventResult{}, err
	}
	defer tx.Rollback()

	stored, workflowYAML, err := getRunForUpdate(ctx, tx, runID)
	if err != nil {
		return EventResult{}, err
	}
	if err := authorizeBoundAttempt(ctx, tx, stored, input); err != nil {
		return EventResult{}, err
	}
	if existing, found, err := findDuplicateEvent(ctx, tx, runID, input.ID, input.MessageID); err != nil {
		return EventResult{}, err
	} else if found {
		reason := "event already received"
		if err := insertEventReceipt(ctx, tx, runID, existing, input.MessageID, typesv2.DispositionDuplicate, stored.StateVersion, reason, now); err != nil {
			return EventResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return EventResult{}, err
		}
		return EventResult{EventID: existing, Disposition: typesv2.DispositionDuplicate, Reason: reason, Run: stored}, nil
	}
	if err := authorizeBoundTask(ctx, tx, stored, input); err != nil {
		return EventResult{}, err
	}
	// Preserve authorization precedence over compare-and-swap handling. The
	// mutation callback may add trusted payload/facts and is authorized again
	// below after it runs.
	if disposition, reason := authorizeEvent(input); disposition != "" {
		if err := insertEvent(ctx, tx, input.ID, runID, input.MessageID, input.Kind, input.ExpectedStateVersion,
			stored.StateVersion, disposition, reason, input.Producer, input.Provenance, input.Payload, input.Facts, now); err != nil {
			return EventResult{}, err
		}
		if err := insertEventReceipt(ctx, tx, runID, input.ID, input.MessageID, disposition, stored.StateVersion, reason, now); err != nil {
			return EventResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return EventResult{}, err
		}
		return EventResult{EventID: input.ID, Disposition: disposition, Reason: reason, Run: stored}, nil
	}
	// A mutation callback may write authoritative domain state. Reject stale
	// compare-and-swap inputs before invoking it so a stale control frame cannot
	// alter delivery, evidence, or other domain rows while receiving a
	// stale_state disposition.
	if input.ExpectedStateVersion != nil && *input.ExpectedStateVersion != stored.StateVersion {
		reason := fmt.Sprintf("expected state version %d, current version is %d", *input.ExpectedStateVersion, stored.StateVersion)
		if err := insertEvent(ctx, tx, input.ID, runID, input.MessageID, input.Kind, input.ExpectedStateVersion,
			stored.StateVersion, typesv2.DispositionStaleState, reason, input.Producer, input.Provenance,
			input.Payload, input.Facts, now); err != nil {
			return EventResult{}, err
		}
		if err := insertEventReceipt(ctx, tx, runID, input.ID, input.MessageID, typesv2.DispositionStaleState,
			stored.StateVersion, reason, now); err != nil {
			return EventResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return EventResult{}, err
		}
		return EventResult{EventID: input.ID, Disposition: typesv2.DispositionStaleState, Reason: reason, Run: stored}, nil
	}
	if input.mutation != nil {
		if err := input.mutation(ctx, tx, &input); err != nil {
			return EventResult{}, err
		}
	}

	workflow, err := typesv2.ParseAndValidateWorkflow([]byte(workflowYAML))
	if err != nil {
		return EventResult{}, fmt.Errorf("load pinned workflow: %w", err)
	}
	if string(workflow.Revision) != stored.WorkflowRevision {
		return EventResult{}, fmt.Errorf("pinned workflow revision mismatch: stored %s decoded %s", stored.WorkflowRevision, workflow.Revision)
	}

	disposition, reason := authorizeEvent(input)
	if disposition != "" {
		if err := insertEvent(ctx, tx, input.ID, runID, input.MessageID, input.Kind, input.ExpectedStateVersion,
			stored.StateVersion, disposition, reason, input.Producer, input.Provenance, input.Payload, input.Facts, now); err != nil {
			return EventResult{}, err
		}
		if err := insertEventReceipt(ctx, tx, runID, input.ID, input.MessageID, disposition, stored.StateVersion, reason, now); err != nil {
			return EventResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return EventResult{}, err
		}
		return EventResult{EventID: input.ID, Disposition: disposition, Reason: reason, Run: stored}, nil
	}

	if err := insertEvent(ctx, tx, input.ID, runID, input.MessageID, input.Kind, input.ExpectedStateVersion,
		stored.StateVersion, typesv2.DispositionAccepted, "", input.Producer, input.Provenance, input.Payload, input.Facts, now); err != nil {
		return EventResult{}, err
	}
	if err := writeFacts(ctx, tx, runID, input.ID, input.Producer, input.Provenance, input.Facts, now); err != nil {
		return EventResult{}, err
	}

	facts, err := loadFacts(ctx, tx, runID)
	if err != nil {
		return EventResult{}, err
	}
	matchFacts := deepMerge(cloneMap(facts), input.Payload)
	state := workflow.Workflow.States[stored.State]

	clauseName, clause, clauseCount, err := matchingEventClause(workflow.Workflow, input.Kind, stored.State, matchFacts)
	if err != nil {
		return EventResult{}, err
	}
	if clauseCount > 1 {
		reason = fmt.Sprintf("runtime ambiguity: %d event clauses matched %s in state %s", clauseCount, input.Kind, stored.State)
		return s.rejectAndSuspend(ctx, tx, stored, input, reason, now)
	}
	clauseWrites := map[string]interface{}{}
	if clause != nil && !clause.Ignore {
		clauseWrites = mergeWrites(clause.Assert, clause.Set)
		facts = applyWritesToMap(facts, clauseWrites)
		matchFacts = deepMerge(cloneMap(facts), input.Payload)
	}

	name, transitionDef, matchCount, err := matchingTransition(workflow.Workflow, input.Kind, stored.State, matchFacts)
	if err != nil {
		return EventResult{}, err
	}
	if matchCount > 1 {
		reason = fmt.Sprintf("runtime ambiguity: %d transitions matched %s in state %s", matchCount, input.Kind, stored.State)
		return s.rejectAndSuspend(ctx, tx, stored, input, reason, now)
	}
	if transitionDef == nil {
		valid, err := typesv2.MatchPredicate(state.Invariant, facts)
		if err != nil {
			return EventResult{}, err
		}
		if !valid {
			reason = fmt.Sprintf("state invariant no longer holds for %s and no transition matched %s", stored.State, input.Kind)
			return s.rejectAndSuspend(ctx, tx, stored, input, reason, now)
		}
		if err := writeFacts(ctx, tx, runID, input.ID, ProducerEngine, input.Provenance, clauseWrites, now); err != nil {
			return EventResult{}, err
		}
		if clause != nil && !clause.Ignore {
			if err := scheduleEffects(ctx, tx, runID, "event_clause", input.ID,
				"events."+input.Kind+".clauses["+clauseName+"].effects", clause.Effects, now); err != nil {
				return EventResult{}, err
			}
		}
		if err := updateBoundTask(ctx, tx, runID, input, now); err != nil {
			return EventResult{}, err
		}
		if err := insertEventReceipt(ctx, tx, runID, input.ID, input.MessageID, typesv2.DispositionAccepted, stored.StateVersion, "", now); err != nil {
			return EventResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return EventResult{}, err
		}
		updated, err := s.GetRun(ctx, runID)
		return EventResult{EventID: input.ID, Disposition: typesv2.DispositionAccepted, Run: updated}, err
	}

	destination := workflow.Workflow.States[transitionDef.To]
	writes := mergeWrites(clauseWrites, transitionDef.Assert, transitionDef.Set)
	if destination.OnEnter != nil {
		writes = mergeWrites(writes, mergeWrites(destination.OnEnter.Assert, destination.OnEnter.Set))
	}
	prospectiveFacts := applyWritesToMap(cloneMap(facts), writes)
	valid, err := typesv2.MatchPredicate(destination.Invariant, prospectiveFacts)
	if err != nil {
		return EventResult{}, err
	}
	if !valid {
		reason = fmt.Sprintf("destination invariant does not hold for state %s", transitionDef.To)
		return s.rejectAndSuspend(ctx, tx, stored, input, reason, now)
	}

	transitionID := uuid.NewString()
	toVersion := stored.StateVersion + 1
	status := RunActive
	finishedAt := int64(0)
	if destination.Terminal {
		status = terminalRunStatus(transitionDef.To)
		finishedAt = now.UnixMilli()
	}
	result, err := tx.ExecContext(ctx, `UPDATE workflow_v2_runs SET state=?,display_phase=?,state_version=?,status=?,waiting_reason='',updated_at=?,finished_at=?
		WHERE id=? AND state=? AND state_version=? AND workspace_revision=? AND workflow_revision=?`,
		transitionDef.To, string(destination.Phase), toVersion, string(status), now.UnixMilli(), finishedAt,
		runID, stored.State, stored.StateVersion, stored.WorkspaceRevision, stored.WorkflowRevision)
	if err != nil {
		return EventResult{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return EventResult{}, err
	}
	if changed != 1 {
		return EventResult{}, fmt.Errorf("state compare-and-swap failed for run %s version %d", runID, stored.StateVersion)
	}

	deltaJSON, err := marshalObject(writes)
	if err != nil {
		return EventResult{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO workflow_v2_transitions(
		id,run_id,event_id,definition_name,from_state,to_state,from_version,to_version,
		workspace_revision,workflow_revision,fact_delta_json,created_at
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, transitionID, runID, input.ID, name, stored.State, transitionDef.To,
		stored.StateVersion, toVersion, stored.WorkspaceRevision, stored.WorkflowRevision, deltaJSON, now.UnixMilli())
	if err != nil {
		return EventResult{}, fmt.Errorf("record transition: %w", err)
	}
	if err := writeFacts(ctx, tx, runID, input.ID, ProducerEngine, input.Provenance, writes, now); err != nil {
		return EventResult{}, err
	}
	if clause != nil && !clause.Ignore {
		if err := scheduleEffects(ctx, tx, runID, "event_clause", input.ID,
			"events."+input.Kind+".clauses["+clauseName+"].effects", clause.Effects, now); err != nil {
			return EventResult{}, err
		}
	}
	if err := scheduleEffects(ctx, tx, runID, "transition", transitionID, "transitions."+name+".effects", transitionDef.Effects, now); err != nil {
		return EventResult{}, err
	}
	if destination.OnEnter != nil {
		if err := scheduleEffects(ctx, tx, runID, "transition", transitionID,
			"states."+transitionDef.To+".on_enter.effects", destination.OnEnter.Effects, now); err != nil {
			return EventResult{}, err
		}
	}
	if err := updateBoundTask(ctx, tx, runID, input, now); err != nil {
		return EventResult{}, err
	}
	if err := insertEventReceipt(ctx, tx, runID, input.ID, input.MessageID, typesv2.DispositionAccepted, toVersion, "", now); err != nil {
		return EventResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return EventResult{}, err
	}
	updated, err := s.GetRun(ctx, runID)
	if err != nil {
		return EventResult{}, err
	}
	return EventResult{
		EventID: input.ID, Disposition: typesv2.DispositionAccepted, Run: updated,
		Transition: &Transition{ID: transitionID, EventID: input.ID, DefinitionName: name,
			FromState: stored.State, ToState: transitionDef.To, FromVersion: stored.StateVersion,
			ToVersion: toVersion, CreatedAt: now},
	}, nil
}

func (s *Store) applyEventWithMutation(ctx context.Context, runID string, input EventInput,
	mutation eventMutation) (EventResult, error) {
	input.mutation = mutation
	return s.ApplyEvent(ctx, runID, input)
}

func (s *Store) rejectAndSuspend(ctx context.Context, tx *sql.Tx, run Run, input EventInput, reason string, now time.Time) (EventResult, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_v2_events SET disposition=?,reason=? WHERE id=?`,
		string(typesv2.DispositionRejected), reason, input.ID); err != nil {
		return EventResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_v2_runs SET status='suspended',waiting_reason=?,updated_at=? WHERE id=? AND state_version=?`,
		reason, now.UnixMilli(), run.ID, run.StateVersion); err != nil {
		return EventResult{}, err
	}
	if err := insertEventReceipt(ctx, tx, run.ID, input.ID, input.MessageID, typesv2.DispositionRejected, run.StateVersion, reason, now); err != nil {
		return EventResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return EventResult{}, err
	}
	updated, err := s.GetRun(ctx, run.ID)
	return EventResult{EventID: input.ID, Disposition: typesv2.DispositionRejected, Reason: reason, Run: updated}, err
}

func authorizeEvent(input EventInput) (typesv2.ControlDisposition, string) {
	if !producerMayEmit(input.Producer, input.Kind) {
		return typesv2.DispositionUnauthorized, fmt.Sprintf("producer %q cannot emit event %q", input.Producer, input.Kind)
	}
	for key := range input.Payload {
		if !producerMaySupplyPayload(input.Producer, key) {
			return typesv2.DispositionUnauthorized, fmt.Sprintf("producer %q cannot supply payload namespace %q", input.Producer, key)
		}
	}
	for key := range input.Facts {
		if !producerOwnsFact(input.Producer, key) {
			return typesv2.DispositionUnauthorized, fmt.Sprintf("producer %q cannot write fact %q", input.Producer, key)
		}
	}
	return "", ""
}

func producerMaySupplyPayload(producer Producer, namespace string) bool {
	namespace = strings.SplitN(strings.TrimSpace(namespace), ".", 2)[0]
	switch producer {
	case ProducerAgent:
		return namespace == "agent" || namespace == "task" || namespace == "plan" || namespace == "delivery" ||
			namespace == "help" || namespace == "artifact" || namespace == "checkpoint"
	case ProducerCI:
		return namespace == "ci"
	case ProducerSourceControl:
		return namespace == "pull_request" || namespace == "delivery"
	case ProducerReview:
		return namespace == "review"
	case ProducerOperator:
		return namespace == "operator" || namespace == "command"
	case ProducerEffect:
		return namespace == "effect" || namespace == "effects"
	case ProducerContext:
		return namespace == "context"
	case ProducerCustom:
		return namespace == "custom"
	case ProducerHub, ProducerEngine:
		return namespace == "workflow" || namespace == "run" || namespace == "setup" || namespace == "task" || namespace == "exec"
	default:
		return false
	}
}

func producerMayEmit(producer Producer, kind string) bool {
	prefix := strings.SplitN(strings.TrimSpace(kind), ".", 2)[0]
	switch producer {
	case ProducerAgent:
		return prefix == "agent" || prefix == "plan" || prefix == "delivery" || prefix == "help" || prefix == "artifact" || prefix == "checkpoint"
	case ProducerCI:
		return prefix == "ci"
	case ProducerSourceControl:
		return prefix == "pull_request" || prefix == "delivery"
	case ProducerReview:
		return prefix == "review"
	case ProducerOperator:
		return prefix == "operator" || prefix == "command"
	case ProducerEffect:
		return prefix == "effect" || prefix == "effects"
	case ProducerContext:
		return prefix == "context"
	case ProducerCustom:
		return prefix == "custom"
	case ProducerHub, ProducerEngine:
		return prefix == "workflow" || prefix == "run" || prefix == "setup" || prefix == "task" || prefix == "exec"
	default:
		return false
	}
}

func producerOwnsFact(producer Producer, key string) bool {
	namespace := strings.SplitN(strings.TrimSpace(key), ".", 2)[0]
	switch namespace {
	case "ci":
		return producer == ProducerCI
	case "pull_request":
		return producer == ProducerSourceControl
	case "delivery":
		return producer == ProducerSourceControl || producer == ProducerEngine
	case "review":
		return producer == ProducerReview
	case "operator":
		return producer == ProducerOperator
	case "effects":
		return producer == ProducerEffect
	case "workflow":
		return producer == ProducerEngine
	case "context":
		return producer == ProducerContext
	case "exec":
		return producer == ProducerEngine || producer == ProducerHub
	case "work", "custom":
		return producer == ProducerEngine || producer == ProducerCustom
	default:
		return false
	}
}

func matchingTransition(workflow *typesv2.Workflow, kind, state string, facts map[string]interface{}) (string, *typesv2.Transition, int, error) {
	names := sortedKeys(workflow.Transitions)
	var matchedName string
	var matched *typesv2.Transition
	count := 0
	for _, name := range names {
		definition := workflow.Transitions[name]
		from, err := typesv2.FromStates(definition.From)
		if err != nil {
			return "", nil, 0, err
		}
		if !contains(from, state) || definition.On != kind {
			continue
		}
		ok, err := typesv2.MatchPredicate(definition.When, facts)
		if err != nil {
			return "", nil, 0, fmt.Errorf("evaluate transition %s: %w", name, err)
		}
		if ok {
			copy := definition
			matchedName, matched = name, &copy
			count++
		}
	}
	return matchedName, matched, count, nil
}

func matchingEventClause(workflow *typesv2.Workflow, kind, state string, facts map[string]interface{}) (string, *typesv2.EventClause, int, error) {
	definition, ok := workflow.Events[kind]
	if !ok {
		return "", nil, 0, nil
	}
	var matched *typesv2.EventClause
	matchedIndex := ""
	count := 0
	for i, clause := range definition.Clauses {
		from, err := typesv2.FromStates(clause.From)
		if err != nil {
			return "", nil, 0, err
		}
		if !contains(from, state) {
			continue
		}
		ok, err := typesv2.MatchPredicate(clause.When, facts)
		if err != nil {
			return "", nil, 0, fmt.Errorf("evaluate event clause %d: %w", i, err)
		}
		if ok {
			copy := clause
			matched, matchedIndex = &copy, fmt.Sprintf("%d", i)
			count++
		}
	}
	return matchedIndex, matched, count, nil
}

func scheduleEffects(ctx context.Context, tx *sql.Tx, runID, originType, originID, basePath string, effects []map[string]interface{}, now time.Time) error {
	for index, effect := range effects {
		for kind, payload := range effect {
			definitionPath := fmt.Sprintf("%s[%d]", basePath, index)
			key := effectKey(runID, originID, definitionPath)
			payloadJSON, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO workflow_v2_effects(
				id,run_id,origin_type,origin_id,definition_path,effect_key,kind,payload_json,status,created_at,updated_at
			) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(effect_key) DO NOTHING`,
				uuid.NewString(), runID, originType, originID, definitionPath, key, kind, string(payloadJSON), "planned", now.UnixMilli(), now.UnixMilli())
			if err != nil {
				return fmt.Errorf("schedule effect %s: %w", definitionPath, err)
			}
		}
	}
	return nil
}

func effectKey(runID, originID, path string) string {
	sum := sha256.Sum256([]byte(runID + "\x00" + originID + "\x00" + path))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func writeFacts(ctx context.Context, tx *sql.Tx, runID, eventID string, producer Producer, provenance typesv2.EvidenceProvenance, writes map[string]interface{}, now time.Time) error {
	provenanceJSON, err := marshalObject(provenance)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(writes))
	for key := range writes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := writes[key]
		if value == nil {
			if _, err := tx.ExecContext(ctx, `DELETE FROM workflow_v2_facts WHERE run_id=? AND fact_key=?`, runID, key); err != nil {
				return err
			}
			continue
		}
		valueJSON, err := json.Marshal(value)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO workflow_v2_facts(run_id,fact_key,value_json,producer,provenance_json,event_id,updated_at)
			VALUES(?,?,?,?,?,?,?) ON CONFLICT(run_id,fact_key) DO UPDATE SET
			value_json=excluded.value_json,producer=excluded.producer,provenance_json=excluded.provenance_json,event_id=excluded.event_id,updated_at=excluded.updated_at`,
			runID, key, string(valueJSON), string(producer), provenanceJSON, eventID, now.UnixMilli())
		if err != nil {
			return fmt.Errorf("write fact %s: %w", key, err)
		}
	}
	return nil
}

func loadFacts(ctx context.Context, tx *sql.Tx, runID string) (map[string]interface{}, error) {
	rows, err := tx.QueryContext(ctx, `SELECT fact_key,value_json FROM workflow_v2_facts WHERE run_id=? ORDER BY fact_key`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	facts := map[string]interface{}{}
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, err
		}
		var value interface{}
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		setDotted(facts, key, value)
	}
	return facts, rows.Err()
}

func setDotted(target map[string]interface{}, key string, value interface{}) {
	parts := strings.Split(key, ".")
	current := target
	for _, part := range parts[:len(parts)-1] {
		next, ok := current[part].(map[string]interface{})
		if !ok {
			next = map[string]interface{}{}
			current[part] = next
		}
		current = next
	}
	current[parts[len(parts)-1]] = value
}

func applyWritesToMap(facts map[string]interface{}, writes map[string]interface{}) map[string]interface{} {
	for key, value := range writes {
		if value == nil {
			deleteDotted(facts, key)
		} else {
			setDotted(facts, key, value)
		}
	}
	return facts
}

func deleteDotted(target map[string]interface{}, key string) {
	parts := strings.Split(key, ".")
	current := target
	for _, part := range parts[:len(parts)-1] {
		next, ok := current[part].(map[string]interface{})
		if !ok {
			return
		}
		current = next
	}
	delete(current, parts[len(parts)-1])
}

func mergeWrites(maps ...map[string]interface{}) map[string]interface{} {
	result := map[string]interface{}{}
	for _, values := range maps {
		for key, value := range values {
			result[key] = value
		}
	}
	return result
}

func cloneMap(source map[string]interface{}) map[string]interface{} {
	result := map[string]interface{}{}
	for key, value := range source {
		if nested, ok := value.(map[string]interface{}); ok {
			result[key] = cloneMap(nested)
		} else {
			result[key] = value
		}
	}
	return result
}

func deepMerge(target, overlay map[string]interface{}) map[string]interface{} {
	for key, value := range overlay {
		if nested, ok := value.(map[string]interface{}); ok {
			base, _ := target[key].(map[string]interface{})
			if base == nil {
				base = map[string]interface{}{}
			}
			target[key] = deepMerge(base, nested)
		} else {
			target[key] = value
		}
	}
	return target
}

func insertEvent(ctx context.Context, tx *sql.Tx, id, runID, messageID, kind string, expected *uint64, observed uint64,
	disposition typesv2.ControlDisposition, reason string, producer Producer, provenance typesv2.EvidenceProvenance,
	payload, facts map[string]interface{}, now time.Time) error {
	payloadJSON, err := marshalObject(payload)
	if err != nil {
		return err
	}
	factsJSON, err := marshalObject(facts)
	if err != nil {
		return err
	}
	provenanceJSON, err := marshalObject(provenance)
	if err != nil {
		return err
	}
	var expectedValue interface{}
	if expected != nil {
		expectedValue = *expected
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO workflow_v2_events(
		id,run_id,message_id,kind,expected_state_version,observed_state_version,disposition,reason,
		producer,provenance_json,payload_json,facts_json,received_at
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, runID, messageID, kind, expectedValue, observed, string(disposition), reason,
		string(producer), provenanceJSON, payloadJSON, factsJSON, now.UnixMilli())
	if err != nil {
		return fmt.Errorf("record event: %w", err)
	}
	return nil
}

func insertEventReceipt(ctx context.Context, tx *sql.Tx, runID, eventID, messageID string,
	disposition typesv2.ControlDisposition, version uint64, reason string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO workflow_v2_event_receipts(
		id,run_id,event_id,message_id,disposition,observed_state_version,reason,received_at
	) VALUES(?,?,?,?,?,?,?,?)`, uuid.NewString(), runID, eventID, messageID, string(disposition), version, reason, now.UnixMilli())
	return err
}

func findDuplicateEvent(ctx context.Context, tx *sql.Tx, runID, eventID, messageID string) (string, bool, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM workflow_v2_events WHERE run_id=? AND (id=? OR (? != '' AND message_id=?)) LIMIT 1`,
		runID, eventID, messageID, messageID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

func getRunForUpdate(ctx context.Context, tx *sql.Tx, runID string) (Run, string, error) {
	var workflowYAML string
	row := tx.QueryRowContext(ctx, `SELECT id,tenant_id,workspace_name,workflow_name,
		workspace_revision,workflow_revision,state,display_phase,state_version,status,waiting_reason,
		current_attempt_id,current_task_id,context_bundle_id,created_at,updated_at,finished_at,workflow_yaml
		FROM workflow_v2_runs WHERE id=?`, runID)
	run, err := scanRunWithWorkflow(row, &workflowYAML)
	return run, workflowYAML, err
}

type scanner interface{ Scan(...interface{}) error }

func scanRun(row scanner) (Run, error) {
	var run Run
	var phase, status string
	var created, updated, finished int64
	err := row.Scan(&run.ID, &run.TenantID, &run.WorkspaceName, &run.WorkflowName,
		&run.WorkspaceRevision, &run.WorkflowRevision, &run.State, &phase, &run.StateVersion, &status, &run.WaitingReason,
		&run.CurrentAttemptID, &run.CurrentTaskID, &run.ContextBundleID, &created, &updated, &finished)
	if err != nil {
		return Run{}, err
	}
	finishRunScan(&run, phase, status, created, updated, finished)
	return run, nil
}

func scanRunWithWorkflow(row scanner, workflowYAML *string) (Run, error) {
	var run Run
	var phase, status string
	var created, updated, finished int64
	err := row.Scan(&run.ID, &run.TenantID, &run.WorkspaceName, &run.WorkflowName,
		&run.WorkspaceRevision, &run.WorkflowRevision, &run.State, &phase, &run.StateVersion, &status, &run.WaitingReason,
		&run.CurrentAttemptID, &run.CurrentTaskID, &run.ContextBundleID, &created, &updated, &finished, workflowYAML)
	if err != nil {
		return Run{}, err
	}
	finishRunScan(&run, phase, status, created, updated, finished)
	return run, nil
}

func finishRunScan(run *Run, phase, status string, created, updated, finished int64) {
	run.DisplayPhase = typesv2.DisplayPhase(phase)
	run.Status = RunStatus(status)
	run.CreatedAt = time.UnixMilli(created).UTC()
	run.UpdatedAt = time.UnixMilli(updated).UTC()
	if finished > 0 {
		value := time.UnixMilli(finished).UTC()
		run.FinishedAt = &value
	}
}

func terminalRunStatus(state string) RunStatus {
	lower := strings.ToLower(state)
	if strings.Contains(lower, "cancel") {
		return RunCancelled
	}
	return RunCompleted
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func marshalObject(value interface{}) (string, error) {
	if value == nil {
		return `{}`, nil
	}
	if object, ok := value.(map[string]interface{}); ok && object == nil {
		return `{}`, nil
	}
	encoded, err := json.Marshal(value)
	return string(encoded), err
}
