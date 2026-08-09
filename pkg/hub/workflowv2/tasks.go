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

// MaterializeAgentTask atomically completes an agent.task effect, creates the
// durable task, and queues its typed assignment for the run's active attempt.
func (s *Store) MaterializeAgentTask(ctx context.Context, effectID, effectAttemptID, worker string,
	heartbeatTimeout, taskTimeout time.Duration) (typesv2.AgentTask, error) {
	if s == nil || s.db == nil {
		return typesv2.AgentTask{}, fmt.Errorf("workflow v2 store is not configured")
	}
	if strings.TrimSpace(effectID) == "" || strings.TrimSpace(effectAttemptID) == "" || strings.TrimSpace(worker) == "" {
		return typesv2.AgentTask{}, fmt.Errorf("effect id, effect attempt id, and worker are required")
	}
	if heartbeatTimeout <= 0 {
		heartbeatTimeout = 2 * time.Minute
	}
	if taskTimeout <= 0 {
		taskTimeout = 2 * time.Hour
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return typesv2.AgentTask{}, err
	}
	defer tx.Rollback()

	var runID, runAttemptID, state, kind, payloadJSON string
	var stateVersion uint64
	err = tx.QueryRowContext(ctx, `SELECT e.run_id,r.current_attempt_id,r.state,r.state_version,e.kind,e.payload_json
		FROM workflow_v2_effects e JOIN workflow_v2_runs r ON r.id=e.run_id
		JOIN workflow_v2_effect_attempts ea ON ea.effect_id=e.id
		WHERE e.id=? AND ea.id=? AND ea.status='running' AND e.status='running' AND e.lease_owner=? AND e.lease_expires_at>=?
		AND r.status='active'`, effectID, effectAttemptID, worker, now.UnixMilli()).Scan(
		&runID, &runAttemptID, &state, &stateVersion, &kind, &payloadJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return typesv2.AgentTask{}, fmt.Errorf("agent task effect is not actively leased")
	}
	if err != nil {
		return typesv2.AgentTask{}, err
	}
	if kind != "agent.task" {
		return typesv2.AgentTask{}, fmt.Errorf("effect %s has kind %q, want agent.task", effectID, kind)
	}
	if runAttemptID == "" {
		return typesv2.AgentTask{}, fmt.Errorf("run %s has no active attempt", runID)
	}
	var activeAttempt int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM workflow_v2_attempts WHERE id=? AND run_id=? AND status='active'`,
		runAttemptID, runID).Scan(&activeAttempt); err != nil {
		return typesv2.AgentTask{}, fmt.Errorf("load active run attempt: %w", err)
	}

	var payload struct {
		Prompt            string   `json:"prompt"`
		Instructions      string   `json:"instructions"`
		AllowedActions    []string `json:"allowed_actions"`
		RequiredArtifacts []string `json:"required_artifacts"`
		IncludeFacts      []string `json:"include_facts"`
	}
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		return typesv2.AgentTask{}, fmt.Errorf("decode agent task effect: %w", err)
	}
	instructions := strings.TrimSpace(payload.Instructions)
	if instructions == "" {
		instructions = strings.TrimSpace(payload.Prompt)
	}
	if instructions == "" {
		return typesv2.AgentTask{}, fmt.Errorf("agent task effect instructions are required")
	}
	if len(payload.IncludeFacts) > 20 {
		return typesv2.AgentTask{}, fmt.Errorf("agent task effect includes more than 20 facts")
	}
	if len(payload.IncludeFacts) > 0 {
		factValues := map[string]interface{}{}
		seenFacts := map[string]bool{}
		for _, key := range payload.IncludeFacts {
			key = strings.TrimSpace(key)
			if key == "" || seenFacts[key] {
				return typesv2.AgentTask{}, fmt.Errorf("agent task effect include_facts must contain unique non-empty keys")
			}
			seenFacts[key] = true
			var raw []byte
			if err := tx.QueryRowContext(ctx, `SELECT value_json FROM workflow_v2_facts WHERE run_id=? AND fact_key=?`,
				runID, key).Scan(&raw); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return typesv2.AgentTask{}, fmt.Errorf("agent task required fact %q is unavailable", key)
				}
				return typesv2.AgentTask{}, err
			}
			var value interface{}
			if err := json.Unmarshal(raw, &value); err != nil {
				return typesv2.AgentTask{}, fmt.Errorf("decode agent task fact %q: %w", key, err)
			}
			factValues[key] = value
		}
		factsJSON, err := json.Marshal(factValues)
		if err != nil {
			return typesv2.AgentTask{}, err
		}
		instructions += "\n\nTyped workflow facts:\n" + string(factsJSON)
	}
	if payload.AllowedActions == nil {
		payload.AllowedActions = []string{}
	}
	if payload.RequiredArtifacts == nil {
		payload.RequiredArtifacts = []string{}
	}
	allowedJSON, _ := json.Marshal(payload.AllowedActions)
	artifactsJSON, _ := json.Marshal(payload.RequiredArtifacts)
	task := typesv2.AgentTask{
		ID: uuid.NewString(), RunID: runID, AttemptID: runAttemptID, State: state, StateVersion: stateVersion,
		Status: typesv2.AgentTaskAssigned, Instructions: instructions, AllowedActions: payload.AllowedActions,
		RequiredArtifacts: payload.RequiredArtifacts, HeartbeatDeadline: now.Add(heartbeatTimeout), Deadline: now.Add(taskTimeout),
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_v2_agent_tasks(
		id,run_id,effect_id,attempt_id,state,state_version,status,instructions,allowed_actions,required_artifacts,
		heartbeat_deadline,deadline,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		task.ID, runID, effectID, runAttemptID, state, stateVersion, string(task.Status), instructions,
		string(allowedJSON), string(artifactsJSON), task.HeartbeatDeadline.UnixMilli(), task.Deadline.UnixMilli(),
		now.UnixMilli(), now.UnixMilli()); err != nil {
		return typesv2.AgentTask{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE workflow_v2_runs SET current_task_id=?,updated_at=?
		WHERE id=? AND current_attempt_id=? AND state=? AND state_version=? AND status='active'`,
		task.ID, now.UnixMilli(), runID, runAttemptID, state, stateVersion)
	if err != nil {
		return typesv2.AgentTask{}, err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return typesv2.AgentTask{}, fmt.Errorf("run changed while materializing agent task")
	}
	taskJSON, err := json.Marshal(task)
	if err != nil {
		return typesv2.AgentTask{}, err
	}
	envelope := typesv2.ControlEnvelope{
		ProtocolVersion: typesv2.ControlProtocolVersion, MessageID: uuid.NewString(), Kind: typesv2.MessageAgentTaskAssign,
		RunID: runID, AttemptID: runAttemptID, TaskID: task.ID, ExpectedStateVersion: &stateVersion,
		CausationID: effectID, SentAt: now, Payload: taskJSON,
	}
	if err := typesv2.ValidateControlEnvelope(envelope, typesv2.DirectionHubToClaw); err != nil {
		return typesv2.AgentTask{}, err
	}
	envelopeJSON, _ := json.Marshal(envelope)
	if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_v2_control_outbox(
		message_id,run_id,attempt_id,task_id,kind,envelope_json,status,next_attempt_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,'pending',0,?,?)`, envelope.MessageID, runID, runAttemptID, task.ID,
		string(envelope.Kind), string(envelopeJSON), now.UnixMilli(), now.UnixMilli()); err != nil {
		return typesv2.AgentTask{}, err
	}
	receiptJSON, _ := json.Marshal(map[string]interface{}{"task_id": task.ID, "message_id": envelope.MessageID})
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_v2_effect_attempts SET status='succeeded',receipt_json=?,finished_at=?
		WHERE id=? AND effect_id=? AND status='running'`, string(receiptJSON), now.UnixMilli(), effectAttemptID, effectID); err != nil {
		return typesv2.AgentTask{}, err
	}
	result, err = tx.ExecContext(ctx, `UPDATE workflow_v2_effects SET status='succeeded',lease_owner='',lease_expires_at=0,
		receipt_json=?,last_error='',updated_at=? WHERE id=? AND status='running' AND lease_owner=?`,
		string(receiptJSON), now.UnixMilli(), effectID, worker)
	if err != nil {
		return typesv2.AgentTask{}, err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return typesv2.AgentTask{}, fmt.Errorf("agent task effect lease changed while materializing")
	}
	if err := tx.Commit(); err != nil {
		return typesv2.AgentTask{}, err
	}
	return task, nil
}

// ExpireAgentTasks moves timed-out tasks and their runs to an inspectable,
// conservative suspended state. Retrying requires an explicit recovery action.
func (s *Store) ExpireAgentTasks(ctx context.Context) ([]string, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("workflow v2 store is not configured")
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,run_id FROM workflow_v2_agent_tasks
		WHERE status IN ('assigned','running') AND (deadline<? OR heartbeat_deadline<?) ORDER BY created_at,id`,
		now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return nil, err
	}
	type expiredTask struct{ id, runID string }
	var expired []expiredTask
	for rows.Next() {
		var task expiredTask
		if err := rows.Scan(&task.id, &task.runID); err != nil {
			rows.Close()
			return nil, err
		}
		expired = append(expired, task)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, task := range expired {
		reason := "agent task " + task.id + " missed its heartbeat or execution deadline"
		if _, err := tx.ExecContext(ctx, `UPDATE workflow_v2_agent_tasks SET status='timed_out',terminal_reason=?,
			updated_at=?,finished_at=? WHERE id=? AND status IN ('assigned','running')`, reason, now.UnixMilli(), now.UnixMilli(), task.id); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE workflow_v2_runs SET status='suspended',waiting_reason=?,updated_at=?
			WHERE id=? AND current_task_id=? AND status='active'`, reason, now.UnixMilli(), task.runID, task.id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	ids := make([]string, len(expired))
	for i := range expired {
		ids[i] = expired[i].id
	}
	return ids, nil
}
