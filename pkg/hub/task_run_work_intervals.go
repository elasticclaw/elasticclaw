package hub

import (
	"context"
	"database/sql"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// All timestamps and durations use Unix milliseconds, like task_run_events.
type taskRunWorkInterval struct{ Start, End int64 }

// taskRunWorkTurn is one persisted agent turn. Open turns are written eagerly
// (and checkpointed) so in-progress work is visible and survives a hub restart.
type taskRunWorkTurn struct {
	ID         string
	V2TaskID   string
	Start, End int64
	Open       bool
}

const taskRunWorkCheckpointInterval = 30 * time.Second

// Writes are order-independent: a closed turn is final, so a late open
// checkpoint (or a duplicate start) can never reopen or move it.
const upsertTaskRunWorkTurnSQL = `INSERT INTO task_run_work_intervals(id,tenant_id,run_id,claw_id,v2_task_id,open,started_at,ended_at,created_at) VALUES(?,?,?,?,?,?,?,?,?)
	ON CONFLICT(id) DO UPDATE SET
		open=excluded.open,
		ended_at=CASE WHEN excluded.open=1 THEN max(task_run_work_intervals.ended_at, excluded.ended_at) ELSE max(task_run_work_intervals.started_at, excluded.ended_at) END
	WHERE task_run_work_intervals.open=1`

func (s *Server) recordTaskRunWorkTurnForClaw(clawID string, turn taskRunWorkTurn) error {
	if turn.End < turn.Start {
		turn.End = turn.Start
	}
	tenantID, runID, _, ok, err := s.taskRunContextForClaw(clawID)
	if err != nil || !ok {
		return err
	}
	_, err = s.db.Exec(upsertTaskRunWorkTurnSQL, turn.ID, tenantID, runID, clawID, turn.V2TaskID, boolInt(turn.Open), turn.Start, turn.End, now().UnixMilli())
	return err
}

func (s *Server) persistTaskRunWorkTurn(clawID string, turn taskRunWorkTurn) {
	if err := s.recordTaskRunWorkTurnForClaw(clawID, turn); err != nil {
		log.Printf("[task-run-analytics] record work interval for claw %s: %v", shortID(clawID), err)
	}
}

// closeTaskRunWorkTurn finalizes an open turn without needing the claw's run
// context (v2 turns are always opened before they can be closed).
func (s *Server) closeTaskRunWorkTurn(id string, endedAt int64) {
	if _, err := s.db.Exec(`UPDATE task_run_work_intervals SET open=0, ended_at=max(started_at, ?) WHERE id=? AND open=1`, endedAt, id); err != nil {
		log.Printf("[task-run-analytics] close work interval %s: %v", id, err)
	}
}

// runTaskRunWorkCheckpointer persists in-flight turns so they are visible
// before they end and lose at most one interval on a hub restart.
func (s *Server) runTaskRunWorkCheckpointer(ctx context.Context) {
	s.recoverTaskRunWorkTurns()
	ticker := time.NewTicker(taskRunWorkCheckpointInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.checkpointTaskRunWorkTurns()
			return
		case <-ticker.C:
			s.checkpointTaskRunWorkTurns()
		}
	}
}

// Legacy turns live on a clawConn that does not survive a restart, so their
// open rows end at the last checkpoint. V2 turns stay open: the task still
// owns them and the bridge's completion (or a hub-side terminal state) closes them.
func (s *Server) recoverTaskRunWorkTurns() {
	if _, err := s.db.Exec(`UPDATE task_run_work_intervals SET open=0 WHERE open=1 AND v2_task_id=''`); err != nil {
		log.Printf("[task-run-analytics] recover open work intervals: %v", err)
	}
}

func (s *Server) checkpointTaskRunWorkTurns() {
	nowMS := now().UnixMilli()
	type legacyTurn struct {
		clawID string
		turn   taskRunWorkTurn
	}
	var legacy []legacyTurn
	s.mu.RLock()
	conns := make([]*clawConn, 0, len(s.claws))
	for _, cc := range s.claws {
		conns = append(conns, cc)
	}
	s.mu.RUnlock()
	for _, cc := range conns {
		cc.mu.Lock()
		if !cc.streamingStartedAt.IsZero() {
			if cc.workTurnID == "" {
				cc.workTurnID = uuid.NewString()
			}
			legacy = append(legacy, legacyTurn{cc.id, taskRunWorkTurn{ID: cc.workTurnID, Start: cc.streamingStartedAt.UnixMilli(), End: nowMS, Open: true}})
		}
		cc.mu.Unlock()
	}
	for _, item := range legacy {
		s.persistTaskRunWorkTurn(item.clawID, item.turn)
	}
	s.checkpointWorkflowV2WorkTurns(nowMS)
}

// checkpointWorkflowV2WorkTurns closes v2 turns the hub ended on its own
// (timeout, supersede, cancel) so they cannot absorb a suspended period.
func (s *Server) checkpointWorkflowV2WorkTurns(nowMS int64) {
	rows, err := s.db.Query(`SELECT w.id, w.started_at, t.status, COALESCE(t.finished_at,0), COALESCE(t.last_heartbeat_at,0)
		FROM task_run_work_intervals w LEFT JOIN workflow_v2_agent_tasks t ON t.id=w.v2_task_id
		WHERE w.open=1 AND w.v2_task_id!=''`)
	if err != nil {
		log.Printf("[task-run-analytics] checkpoint v2 work intervals: %v", err)
		return
	}
	type closeOp struct {
		id  string
		end int64
	}
	var active []string
	var closes []closeOp
	for rows.Next() {
		var id string
		var start, finishedAt, heartbeatAt int64
		var status sql.NullString
		if err := rows.Scan(&id, &start, &status, &finishedAt, &heartbeatAt); err != nil {
			log.Printf("[task-run-analytics] checkpoint v2 work intervals: %v", err)
			break
		}
		switch {
		case !status.Valid:
			closes = append(closes, closeOp{id, 0}) // task gone: keep the last checkpoint
		case status.String == "assigned" || status.String == "running":
			active = append(active, id)
		case status.String == "timed_out" && heartbeatAt > start:
			closes = append(closes, closeOp{id, heartbeatAt})
		default:
			closes = append(closes, closeOp{id, finishedAt})
		}
	}
	rows.Close()
	for _, id := range active {
		if _, err := s.db.Exec(`UPDATE task_run_work_intervals SET ended_at=max(ended_at, ?) WHERE id=? AND open=1`, nowMS, id); err != nil {
			log.Printf("[task-run-analytics] checkpoint v2 work interval %s: %v", id, err)
		}
	}
	for _, op := range closes {
		if op.end == 0 {
			_, err = s.db.Exec(`UPDATE task_run_work_intervals SET open=0 WHERE id=? AND open=1`, op.id)
			if err != nil {
				log.Printf("[task-run-analytics] close v2 work interval %s: %v", op.id, err)
			}
			continue
		}
		s.closeTaskRunWorkTurn(op.id, op.end)
	}
}

// Open turns are reported up to requestNow so in-progress work counts as factory time.
func (s *Server) readTaskRunWorkIntervals(tenantID string, runIDs []string, requestNow int64) (map[string][]taskRunWorkInterval, error) {
	result := make(map[string][]taskRunWorkInterval)
	if len(runIDs) == 0 {
		return result, nil
	}
	args := []any{requestNow, tenantID}
	for _, id := range runIDs {
		args = append(args, id)
	}
	rows, err := s.db.Query(`SELECT run_id, started_at, CASE WHEN open=1 THEN max(ended_at, ?) ELSE ended_at END FROM task_run_work_intervals WHERE tenant_id=? AND run_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(runIDs)), ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var interval taskRunWorkInterval
		if err := rows.Scan(&id, &interval.Start, &interval.End); err != nil {
			return nil, err
		}
		if interval.End > interval.Start {
			result[id] = append(result[id], interval)
		}
	}
	return result, rows.Err()
}

// computeTicketWorkTimes unions real turns before intersecting both windows.
// A historical ticket without valid recorded turns remains explicitly untracked.
func computeTicketWorkTimes(intervals []taskRunWorkInterval, leadStart, leadEnd, prOpenedAt, mergedAt, requestNow int64) (factory, awaiting int64, tracked bool) {
	union := make([]taskRunWorkInterval, 0, len(intervals))
	for _, interval := range intervals {
		if interval.End <= interval.Start {
			continue
		}
		tracked = true
		interval.Start = max(interval.Start, leadStart)
		interval.End = min(interval.End, leadEnd)
		if interval.End > interval.Start {
			union = append(union, interval)
		}
	}
	sort.Slice(union, func(i, j int) bool { return union[i].Start < union[j].Start })
	merged := union[:0]
	for _, interval := range union {
		if len(merged) > 0 && interval.Start <= merged[len(merged)-1].End {
			merged[len(merged)-1].End = max(merged[len(merged)-1].End, interval.End)
		} else {
			merged = append(merged, interval)
		}
	}
	waitStart, waitEnd := max(leadStart, prOpenedAt), mergedAt
	if waitEnd == 0 {
		waitEnd = requestNow
	}
	waitEnd = min(waitEnd, leadEnd)
	if prOpenedAt > 0 {
		awaiting = max(int64(0), waitEnd-waitStart)
	}
	for _, interval := range merged {
		factory += interval.End - interval.Start
		if prOpenedAt > 0 {
			awaiting -= max(int64(0), min(interval.End, waitEnd)-max(interval.Start, waitStart))
		}
	}
	return
}
