package workflowv2

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
	"github.com/google/uuid"
)

type Attempt struct {
	ID        string    `json:"id"`
	RunID     string    `json:"run_id"`
	ClawID    string    `json:"claw_id"`
	Number    int       `json:"number"`
	Status    string    `json:"status"`
	StartedAt time.Time `json:"started_at"`
}

func (s *Store) StartAttempt(ctx context.Context, runID, clawID string) (Attempt, error) {
	if s == nil || s.db == nil {
		return Attempt{}, fmt.Errorf("workflow v2 store is not configured")
	}
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(clawID) == "" {
		return Attempt{}, fmt.Errorf("run id and claw id are required")
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return Attempt{}, err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM workflow_v2_runs WHERE id=?`, runID).Scan(&status); err != nil {
		return Attempt{}, err
	}
	if status != string(RunActive) && status != string(RunSuspended) {
		return Attempt{}, fmt.Errorf("cannot start attempt for %s run", status)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_v2_attempts SET status='lost',finished_at=?,reason='superseded by a new attempt'
		WHERE run_id=? AND status IN ('provisioning','active')`, now.UnixMilli(), runID); err != nil {
		return Attempt{}, err
	}
	var number int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(number),0)+1 FROM workflow_v2_attempts WHERE run_id=?`, runID).Scan(&number); err != nil {
		return Attempt{}, err
	}
	attempt := Attempt{ID: uuid.NewString(), RunID: runID, ClawID: clawID, Number: number, Status: "active", StartedAt: now}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_v2_attempts(id,run_id,claw_id,number,status,started_at,heartbeat_at)
		VALUES(?,?,?,?,?,?,?)`, attempt.ID, runID, clawID, number, attempt.Status, now.UnixMilli(), now.UnixMilli()); err != nil {
		return Attempt{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_v2_runs SET current_attempt_id=?,updated_at=? WHERE id=?`,
		attempt.ID, now.UnixMilli(), runID); err != nil {
		return Attempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return Attempt{}, err
	}
	return attempt, nil
}

func (s *Store) AuthorizeControlAttempt(ctx context.Context, runID, attemptID, clawID, tenantID string) error {
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM workflow_v2_attempts a JOIN workflow_v2_runs r ON r.id=a.run_id
		WHERE r.id=? AND r.tenant_id=? AND r.current_attempt_id=? AND a.id=? AND a.claw_id=? AND a.status='active'`,
		runID, tenantID, attemptID, attemptID, clawID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("active workflow attempt not found")
	}
	return err
}

func (s *Store) Snapshot(ctx context.Context, runID, attemptID string) (typesv2.WorkflowSnapshot, error) {
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		return typesv2.WorkflowSnapshot{}, err
	}
	snapshot := typesv2.WorkflowSnapshot{RunID: run.ID, AttemptID: attemptID, State: run.State,
		DisplayPhase: run.DisplayPhase, StateVersion: run.StateVersion}
	if run.CurrentTaskID != "" {
		var task typesv2.AgentTask
		var status string
		var allowedJSON, artifactsJSON string
		var heartbeat, deadline int64
		err := s.db.QueryRowContext(ctx, `SELECT id,run_id,attempt_id,state,state_version,status,instructions,
			allowed_actions,required_artifacts,heartbeat_deadline,deadline FROM workflow_v2_agent_tasks
			WHERE id=? AND run_id=?`, run.CurrentTaskID, runID).Scan(&task.ID, &task.RunID, &task.AttemptID,
			&task.State, &task.StateVersion, &status, &task.Instructions, &allowedJSON, &artifactsJSON, &heartbeat, &deadline)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return typesv2.WorkflowSnapshot{}, err
		}
		if err == nil {
			task.Status = typesv2.AgentTaskStatus(status)
			_ = json.Unmarshal([]byte(allowedJSON), &task.AllowedActions)
			_ = json.Unmarshal([]byte(artifactsJSON), &task.RequiredArtifacts)
			task.HeartbeatDeadline = time.UnixMilli(heartbeat).UTC()
			task.Deadline = time.UnixMilli(deadline).UTC()
			snapshot.CurrentTask = &task
			snapshot.AllowedActions = append([]string(nil), task.AllowedActions...)
			snapshot.RequiredArtifacts = append([]string(nil), task.RequiredArtifacts...)
		}
	}
	if run.ContextBundleID != "" {
		var ref typesv2.ContextBundleRef
		if err := s.db.QueryRowContext(ctx, `SELECT id,revision FROM workflow_v2_context_bundles WHERE id=? AND run_id=?`,
			run.ContextBundleID, runID).Scan(&ref.ID, &ref.Revision); err == nil {
			snapshot.ContextBundle = &ref
		} else if !errors.Is(err, sql.ErrNoRows) {
			return typesv2.WorkflowSnapshot{}, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,url,repository,repository_name,pr_number,source_branch,base_branch,
		current_head_sha,state,supersedes_id,verified_at,provenance_json FROM workflow_v2_delivery_prs
		WHERE run_id=? AND active=1 ORDER BY repository,pr_number`, runID)
	if err != nil {
		return typesv2.WorkflowSnapshot{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var pr typesv2.VerifiedPullRequest
		var verified int64
		var provenanceJSON string
		if err := rows.Scan(&pr.ID, &pr.URL, &pr.Repository, &pr.RepositoryName, &pr.Number, &pr.SourceBranch,
			&pr.BaseBranch, &pr.HeadSHA, &pr.State, &pr.Supersedes, &verified, &provenanceJSON); err != nil {
			return typesv2.WorkflowSnapshot{}, err
		}
		pr.VerifiedAt = time.UnixMilli(verified).UTC()
		_ = json.Unmarshal([]byte(provenanceJSON), &pr.Provenance)
		snapshot.Delivery = append(snapshot.Delivery, pr)
	}
	return snapshot, rows.Err()
}

func (s *Store) EnqueueControl(ctx context.Context, envelope typesv2.ControlEnvelope) error {
	if err := typesv2.ValidateControlEnvelope(envelope, typesv2.DirectionHubToClaw); err != nil {
		return err
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	now := s.now().UTC().UnixMilli()
	_, err = s.db.ExecContext(ctx, `INSERT INTO workflow_v2_control_outbox(
		message_id,run_id,attempt_id,task_id,kind,envelope_json,status,next_attempt_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,'pending',0,?,?) ON CONFLICT(message_id) DO NOTHING`, envelope.MessageID,
		envelope.RunID, envelope.AttemptID, envelope.TaskID, string(envelope.Kind), string(raw), now, now)
	return err
}

func (s *Store) ReadyControl(ctx context.Context, runID, attemptID string, limit int) ([]typesv2.ControlEnvelope, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	now := s.now().UTC().UnixMilli()
	rows, err := s.db.QueryContext(ctx, `SELECT envelope_json FROM workflow_v2_control_outbox
		WHERE run_id=? AND attempt_id=? AND status IN ('pending','sent') AND next_attempt_at<=?
		ORDER BY created_at,message_id LIMIT ?`, runID, attemptID, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []typesv2.ControlEnvelope
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var envelope typesv2.ControlEnvelope
		if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
			return nil, err
		}
		result = append(result, envelope)
	}
	return result, rows.Err()
}

func (s *Store) MarkControlSent(ctx context.Context, messageID string) error {
	now := s.now().UTC()
	_, err := s.db.ExecContext(ctx, `UPDATE workflow_v2_control_outbox SET status='sent',attempt_count=attempt_count+1,
		next_attempt_at=?,updated_at=? WHERE message_id=? AND status IN ('pending','sent')`,
		now.Add(5*time.Second).UnixMilli(), now.UnixMilli(), messageID)
	return err
}

func (s *Store) AcknowledgeControl(ctx context.Context, runID, attemptID string, receipt typesv2.ControlReceipt) error {
	if strings.TrimSpace(receipt.MessageID) == "" {
		return fmt.Errorf("message id is required")
	}
	now := s.now().UTC().UnixMilli()
	result, err := s.db.ExecContext(ctx, `UPDATE workflow_v2_control_outbox SET status='acknowledged',acknowledged_at=?,updated_at=?
		WHERE message_id=? AND run_id=? AND attempt_id=? AND status IN ('pending','sent')`,
		now, now, receipt.MessageID, runID, attemptID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("control message is not pending for this attempt")
	}
	return nil
}

func (s *Store) ApplyAgentControl(ctx context.Context, envelope typesv2.ControlEnvelope) (typesv2.ControlReceipt, error) {
	if err := typesv2.ValidateControlEnvelope(envelope, typesv2.DirectionClawToHub); err != nil {
		return typesv2.ControlReceipt{}, err
	}
	var active int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM workflow_v2_attempts a JOIN workflow_v2_runs r ON r.id=a.run_id
		WHERE r.id=? AND r.current_attempt_id=? AND a.id=? AND a.status='active'`, envelope.RunID,
		envelope.AttemptID, envelope.AttemptID).Scan(&active); errors.Is(err, sql.ErrNoRows) {
		return typesv2.ControlReceipt{}, fmt.Errorf("control envelope attempt is no longer active")
	} else if err != nil {
		return typesv2.ControlReceipt{}, err
	}
	var payload map[string]interface{}
	if len(envelope.Payload) > 0 {
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
			return typesv2.ControlReceipt{}, fmt.Errorf("decode control payload: %w", err)
		}
	}
	result, err := s.ApplyEvent(ctx, envelope.RunID, EventInput{
		ID: envelope.MessageID, MessageID: envelope.MessageID, Kind: string(envelope.Kind),
		ExpectedStateVersion: envelope.ExpectedStateVersion, Producer: ProducerAgent, Payload: payload,
		Provenance: typesv2.EvidenceProvenance{Producer: string(ProducerAgent), ObservedAt: s.now().UTC()},
	})
	if err != nil {
		return typesv2.ControlReceipt{}, err
	}
	if (result.Disposition == typesv2.DispositionAccepted || result.Disposition == typesv2.DispositionDuplicate) && envelope.TaskID != "" {
		if err := s.updateTaskFromControl(ctx, envelope); err != nil {
			return typesv2.ControlReceipt{}, err
		}
	}
	return typesv2.ControlReceipt{MessageID: envelope.MessageID, Disposition: result.Disposition,
		StateVersion: result.Run.StateVersion, Reason: result.Reason}, nil
}

func (s *Store) updateTaskFromControl(ctx context.Context, envelope typesv2.ControlEnvelope) error {
	now := s.now().UTC()
	status := ""
	finished := int64(0)
	switch envelope.Kind {
	case typesv2.MessageAgentTaskStarted:
		status = string(typesv2.AgentTaskRunning)
	case typesv2.MessageAgentTaskHeartbeat:
		_, err := s.db.ExecContext(ctx, `UPDATE workflow_v2_agent_tasks SET last_heartbeat_at=?,heartbeat_deadline=?,updated_at=?
			WHERE id=? AND run_id=? AND attempt_id=? AND status IN ('assigned','running')`, now.UnixMilli(),
			now.Add(2*time.Minute).UnixMilli(), now.UnixMilli(), envelope.TaskID, envelope.RunID, envelope.AttemptID)
		if err == nil {
			_, err = s.db.ExecContext(ctx, `UPDATE workflow_v2_attempts SET heartbeat_at=? WHERE id=? AND run_id=? AND status='active'`,
				now.UnixMilli(), envelope.AttemptID, envelope.RunID)
		}
		return err
	case typesv2.MessageAgentTaskCompleted:
		status, finished = string(typesv2.AgentTaskCompleted), now.UnixMilli()
	case typesv2.MessageAgentTaskFailed:
		status, finished = string(typesv2.AgentTaskFailed), now.UnixMilli()
	default:
		return nil
	}
	result, err := s.db.ExecContext(ctx, `UPDATE workflow_v2_agent_tasks SET status=?,last_heartbeat_at=?,updated_at=?,finished_at=?
		WHERE id=? AND run_id=? AND attempt_id=? AND status IN ('assigned','running')`, status, now.UnixMilli(),
		now.UnixMilli(), finished, envelope.TaskID, envelope.RunID, envelope.AttemptID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 && envelope.Kind != typesv2.MessageAgentTaskCompleted && envelope.Kind != typesv2.MessageAgentTaskFailed {
		return fmt.Errorf("active task not found")
	}
	return nil
}
