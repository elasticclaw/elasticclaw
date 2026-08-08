package workflowv2

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

type Inspection struct {
	Run                 Run                    `json:"run"`
	Waiting             []WaitingReason        `json:"waiting"`
	ExpectedTransitions []ExpectedTransition   `json:"expected_transitions"`
	Facts               map[string]interface{} `json:"facts"`
	RecentEvents        []EventRecord          `json:"recent_events"`
	Transitions         []Transition           `json:"transitions"`
	Effects             []EffectSummary        `json:"effects"`
	AgentTasks          []AgentTaskSummary     `json:"agent_tasks"`
	Delivery            DeliverySummary        `json:"delivery"`
	Context             ContextSummary         `json:"context"`
}

type WaitingReason struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

type ExpectedTransition struct {
	Name      string                 `json:"name"`
	EventKind string                 `json:"event_kind"`
	ToState   string                 `json:"to_state"`
	When      map[string]interface{} `json:"when,omitempty"`
}

type EventRecord struct {
	ID                   string                     `json:"id"`
	Kind                 string                     `json:"kind"`
	Producer             Producer                   `json:"producer"`
	Disposition          typesv2.ControlDisposition `json:"disposition"`
	ExpectedStateVersion *uint64                    `json:"expected_state_version,omitempty"`
	ObservedStateVersion uint64                     `json:"observed_state_version"`
	Reason               string                     `json:"reason,omitempty"`
	ReceivedAt           time.Time                  `json:"received_at"`
}

type EffectSummary struct {
	ID             string    `json:"id"`
	Kind           string    `json:"kind"`
	Status         string    `json:"status"`
	DefinitionPath string    `json:"definition_path"`
	AttemptCount   int       `json:"attempt_count"`
	LastError      string    `json:"last_error,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type AgentTaskSummary struct {
	ID                string    `json:"id"`
	Status            string    `json:"status"`
	State             string    `json:"state"`
	StateVersion      uint64    `json:"state_version"`
	AttemptID         string    `json:"attempt_id,omitempty"`
	HeartbeatDeadline time.Time `json:"heartbeat_deadline"`
	Deadline          time.Time `json:"deadline"`
	TerminalReason    string    `json:"terminal_reason,omitempty"`
}

type DeliverySummary struct {
	ActivePullRequests int `json:"active_pull_requests"`
	OpenPullRequests   int `json:"open_pull_requests"`
	MergedPullRequests int `json:"merged_pull_requests"`
	ClosedPullRequests int `json:"closed_pull_requests"`
}

type ContextSummary struct {
	BundleID string `json:"bundle_id,omitempty"`
	Revision string `json:"revision,omitempty"`
	Status   string `json:"status,omitempty"`
}

func (s *Store) InspectRun(ctx context.Context, runID string) (Inspection, error) {
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		return Inspection{}, err
	}
	inspection := Inspection{Run: run, Facts: map[string]interface{}{}}

	var workflowYAML string
	if err := s.db.QueryRowContext(ctx, `SELECT workflow_yaml FROM workflow_v2_runs WHERE id=?`, runID).Scan(&workflowYAML); err != nil {
		return Inspection{}, err
	}
	workflow, err := typesv2.ParseAndValidateWorkflow([]byte(workflowYAML))
	if err != nil {
		return Inspection{}, fmt.Errorf("inspect pinned workflow: %w", err)
	}
	for _, name := range sortedKeys(workflow.Workflow.Transitions) {
		definition := workflow.Workflow.Transitions[name]
		from, err := typesv2.FromStates(definition.From)
		if err != nil {
			return Inspection{}, err
		}
		if contains(from, run.State) {
			inspection.ExpectedTransitions = append(inspection.ExpectedTransitions, ExpectedTransition{
				Name: name, EventKind: definition.On, ToState: definition.To, When: definition.When,
			})
		}
	}

	if err := s.inspectFacts(ctx, runID, inspection.Facts); err != nil {
		return Inspection{}, err
	}
	if inspection.RecentEvents, err = s.inspectEvents(ctx, runID); err != nil {
		return Inspection{}, err
	}
	if inspection.Transitions, err = s.inspectTransitions(ctx, runID); err != nil {
		return Inspection{}, err
	}
	if inspection.Effects, err = s.inspectEffects(ctx, runID); err != nil {
		return Inspection{}, err
	}
	if inspection.AgentTasks, err = s.inspectAgentTasks(ctx, runID); err != nil {
		return Inspection{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN active=1 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN active=1 AND state='open' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN active=1 AND state='merged' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN active=1 AND state='closed' THEN 1 ELSE 0 END),0)
		FROM workflow_v2_delivery_prs WHERE run_id=?`, runID).Scan(
		&inspection.Delivery.ActivePullRequests, &inspection.Delivery.OpenPullRequests,
		&inspection.Delivery.MergedPullRequests, &inspection.Delivery.ClosedPullRequests); err != nil {
		return Inspection{}, err
	}
	if run.ContextBundleID != "" {
		if err := s.db.QueryRowContext(ctx, `SELECT id,revision,status FROM workflow_v2_context_bundles WHERE id=? AND run_id=?`,
			run.ContextBundleID, runID).Scan(&inspection.Context.BundleID, &inspection.Context.Revision, &inspection.Context.Status); err != nil && err != sql.ErrNoRows {
			return Inspection{}, err
		}
	}

	inspection.Waiting = deriveWaitingReasons(inspection)
	return inspection, nil
}

func deriveWaitingReasons(inspection Inspection) []WaitingReason {
	if inspection.Run.Status == RunSuspended {
		return []WaitingReason{{Kind: "suspended", Detail: inspection.Run.WaitingReason}}
	}
	if inspection.Run.Status == RunCompleted || inspection.Run.Status == RunCancelled {
		return nil
	}
	for _, task := range inspection.AgentTasks {
		if task.Status == "assigned" || task.Status == "running" {
			return []WaitingReason{{Kind: "agent_task", Detail: fmt.Sprintf("task %s is %s", task.ID, task.Status)}}
		}
	}
	for _, effect := range inspection.Effects {
		if effect.Status == "planned" || effect.Status == "running" || effect.Status == "retryable_failed" || effect.Status == "unknown" {
			return []WaitingReason{{Kind: "effect", Detail: fmt.Sprintf("effect %s (%s) is %s", effect.ID, effect.Kind, effect.Status)}}
		}
	}
	if len(inspection.ExpectedTransitions) == 0 {
		return []WaitingReason{{Kind: "definition", Detail: "no outgoing transition is defined for the current state"}}
	}
	reasons := make([]WaitingReason, 0, len(inspection.ExpectedTransitions))
	for _, transition := range inspection.ExpectedTransitions {
		reasons = append(reasons, WaitingReason{
			Kind: "event", Detail: fmt.Sprintf("waiting for %s to enter %s via %s", transition.EventKind, transition.ToState, transition.Name),
		})
	}
	return reasons
}

func (s *Store) inspectFacts(ctx context.Context, runID string, target map[string]interface{}) error {
	rows, err := s.db.QueryContext(ctx, `SELECT fact_key,value_json FROM workflow_v2_facts WHERE run_id=? ORDER BY fact_key`, runID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			return err
		}
		var value interface{}
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			return err
		}
		setDotted(target, key, value)
	}
	return rows.Err()
}

func (s *Store) inspectEvents(ctx context.Context, runID string) ([]EventRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,kind,producer,disposition,expected_state_version,observed_state_version,reason,received_at
		FROM workflow_v2_events WHERE run_id=? ORDER BY received_at DESC,id DESC LIMIT 50`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []EventRecord
	for rows.Next() {
		var item EventRecord
		var disposition string
		var expected sql.NullInt64
		var received int64
		if err := rows.Scan(&item.ID, &item.Kind, &item.Producer, &disposition, &expected,
			&item.ObservedStateVersion, &item.Reason, &received); err != nil {
			return nil, err
		}
		item.Disposition = typesv2.ControlDisposition(disposition)
		if expected.Valid {
			value := uint64(expected.Int64)
			item.ExpectedStateVersion = &value
		}
		item.ReceivedAt = time.UnixMilli(received).UTC()
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) inspectTransitions(ctx context.Context, runID string) ([]Transition, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,event_id,definition_name,from_state,to_state,from_version,to_version,created_at
		FROM workflow_v2_transitions WHERE run_id=? ORDER BY to_version,id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Transition
	for rows.Next() {
		var item Transition
		var created int64
		if err := rows.Scan(&item.ID, &item.EventID, &item.DefinitionName, &item.FromState, &item.ToState,
			&item.FromVersion, &item.ToVersion, &created); err != nil {
			return nil, err
		}
		item.CreatedAt = time.UnixMilli(created).UTC()
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) inspectEffects(ctx context.Context, runID string) ([]EffectSummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,kind,status,definition_path,attempt_count,last_error,updated_at
		FROM workflow_v2_effects WHERE run_id=? ORDER BY created_at DESC,id DESC LIMIT 50`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []EffectSummary
	for rows.Next() {
		var item EffectSummary
		var updated int64
		if err := rows.Scan(&item.ID, &item.Kind, &item.Status, &item.DefinitionPath, &item.AttemptCount, &item.LastError, &updated); err != nil {
			return nil, err
		}
		item.UpdatedAt = time.UnixMilli(updated).UTC()
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) inspectAgentTasks(ctx context.Context, runID string) ([]AgentTaskSummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,status,state,state_version,attempt_id,heartbeat_deadline,deadline,terminal_reason
		FROM workflow_v2_agent_tasks WHERE run_id=? ORDER BY created_at DESC,id DESC LIMIT 50`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AgentTaskSummary
	for rows.Next() {
		var item AgentTaskSummary
		var heartbeat, deadline int64
		if err := rows.Scan(&item.ID, &item.Status, &item.State, &item.StateVersion, &item.AttemptID,
			&heartbeat, &deadline, &item.TerminalReason); err != nil {
			return nil, err
		}
		item.HeartbeatDeadline = time.UnixMilli(heartbeat).UTC()
		item.Deadline = time.UnixMilli(deadline).UTC()
		result = append(result, item)
	}
	return result, rows.Err()
}
