package hub

import (
	"database/sql"
	"encoding/json"
	"strings"
)

// taskRunAnalyticsWallClockView reports a coarse wall-clock breakdown for a run.
// All figures are derived from hub-side receipt timestamps, which cannot
// establish provider/model latency; the residual is therefore always labeled
// "unattributed time", never "model time".
type taskRunAnalyticsWallClockView struct {
	ToolTimeMs         int64 `json:"toolTimeMs"`
	QueueTimeMs        int64 `json:"queueTimeMs"`
	HumanWaitMs        int64 `json:"humanWaitMs"`
	UnattributedTimeMs int64 `json:"unattributedTimeMs"`
	TotalMs            int64 `json:"totalMs"`
}

// taskRunActivityRecord is the portion of an activity message used for
// wall-clock accounting.
type taskRunActivityRecord struct {
	Kind       string `json:"kind"`
	Phase      string `json:"phase"`
	CallID     string `json:"call_id"`
	DurationMs int64  `json:"duration_ms"`
}

func parseTaskRunActivityRecord(content string) (taskRunActivityRecord, bool) {
	var record taskRunActivityRecord
	if err := json.Unmarshal([]byte(content), &record); err != nil {
		return taskRunActivityRecord{}, false
	}
	return record, true
}

// dedupeActivityMessages removes heartbeat noise and retains at most one
// terminal duration claim for every non-empty tool call ID.
func dedupeActivityMessages(records []taskRunActivityRecord) []taskRunActivityRecord {
	deduped := make([]taskRunActivityRecord, 0, len(records))
	seenToolCalls := make(map[string]bool)
	for _, record := range records {
		kind := strings.ToLower(strings.TrimSpace(record.Kind))
		if kind == "still_working" {
			continue
		}
		if kind != "tool" {
			deduped = append(deduped, record)
			continue
		}
		if record.DurationMs <= 0 || !isTerminalToolActivityPhase(record.Phase) {
			continue
		}
		// Empty IDs are independent singleton groups: a missing ID cannot prove
		// that two activity rows belong to the same tool call.
		if record.CallID != "" {
			if seenToolCalls[record.CallID] {
				continue
			}
			seenToolCalls[record.CallID] = true
		}
		deduped = append(deduped, record)
	}
	return deduped
}

func isTerminalToolActivityPhase(phase string) bool {
	switch strings.ToLower(strings.TrimSpace(phase)) {
	case "completed", "complete", "done", "failed", "error", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func taskRunAnalyticsWallClock(run taskRunAnalyticsRunView, humanWait taskRunAnalyticsHumanWaitView, activity []taskRunActivityRecord) taskRunAnalyticsWallClockView {
	view := taskRunAnalyticsWallClockView{}
	if run.FinishedAt > run.StartedAt && run.StartedAt > 0 {
		view.TotalMs = run.FinishedAt - run.StartedAt
	}
	if run.ProvisionStartedAt >= run.QueuedAt && run.QueuedAt > 0 {
		view.QueueTimeMs = run.ProvisionStartedAt - run.QueuedAt
	}
	for _, record := range activity {
		if strings.EqualFold(strings.TrimSpace(record.Kind), "tool") && record.DurationMs > 0 {
			view.ToolTimeMs += record.DurationMs
		}
	}
	for _, interval := range humanWait.Intervals {
		if interval.DurationMs > 0 {
			view.HumanWaitMs += interval.DurationMs
		}
	}
	if humanWait.PROpenToMergeMs > 0 {
		view.HumanWaitMs += humanWait.PROpenToMergeMs
	}
	view.UnattributedTimeMs = view.TotalMs - view.ToolTimeMs - view.QueueTimeMs - view.HumanWaitMs
	if view.UnattributedTimeMs < 0 {
		view.UnattributedTimeMs = 0
	}
	return view
}

// readTaskRunWallClockActivity loads activity for the run's primary claw and
// every retry claw in one query, then applies the in-memory accounting rules.
func (s *Server) readTaskRunWallClockActivity(tenantID, runID, runClawID string) ([]taskRunActivityRecord, error) {
	clawIDs, err := taskRunAnalyticsClawIDs(s.db, tenantID, runID, runClawID)
	if err != nil {
		return nil, err
	}
	return s.readTaskRunActivityRecords(tenantID, clawIDs)
}

// readTaskRunActivityRecords loads and parses activity rows for a known claw
// set, leaving deduplication as a pure in-memory operation.
func (s *Server) readTaskRunActivityRecords(tenantID string, clawIDs []string) ([]taskRunActivityRecord, error) {
	if len(clawIDs) == 0 {
		return []taskRunActivityRecord{}, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(clawIDs)), ",")
	args := make([]any, 0, len(clawIDs)+1)
	for _, clawID := range clawIDs {
		args = append(args, clawID)
	}
	args = append(args, tenantID)
	rows, err := s.db.Query(`
		SELECT content
		  FROM messages
		 WHERE claw_id IN (`+placeholders+`) AND tenant_id=? AND role='activity'
		 ORDER BY created_at ASC, rowid ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []taskRunActivityRecord{}
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			return nil, err
		}
		if record, ok := parseTaskRunActivityRecord(content); ok {
			records = append(records, record)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return dedupeActivityMessages(records), nil
}

func taskRunAnalyticsClawIDs(db *sql.DB, tenantID, runID, runClawID string) ([]string, error) {
	rows, err := db.Query(`
		SELECT claw_id
		  FROM task_run_attempts
		 WHERE tenant_id=? AND run_id=?
		 ORDER BY attempt_number ASC`, tenantID, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	clawIDs := []string{}
	seen := map[string]bool{}
	add := func(clawID string) {
		if clawID != "" && !seen[clawID] {
			clawIDs = append(clawIDs, clawID)
			seen[clawID] = true
		}
	}
	add(runClawID)
	for rows.Next() {
		var clawID string
		if err := rows.Scan(&clawID); err != nil {
			return nil, err
		}
		add(clawID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return clawIDs, nil
}
