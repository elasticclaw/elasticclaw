package hub

import (
	"log"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// All timestamps and durations use Unix milliseconds, like task_run_events.
type taskRunWorkInterval struct{ Start, End int64 }

func (s *Server) recordTaskRunWorkIntervalForClaw(clawID string, interval taskRunWorkInterval) error {
	if interval.End <= interval.Start {
		return nil
	}
	tenantID, runID, _, ok, err := s.taskRunContextForClaw(clawID)
	if err != nil || !ok {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO task_run_work_intervals(id,tenant_id,run_id,claw_id,started_at,ended_at,created_at) VALUES(?,?,?,?,?,?,?)`, uuid.NewString(), tenantID, runID, clawID, interval.Start, interval.End, now().UnixMilli())
	return err
}

func (s *Server) persistTaskRunWorkInterval(clawID string, interval taskRunWorkInterval) {
	if err := s.recordTaskRunWorkIntervalForClaw(clawID, interval); err != nil {
		log.Printf("[task-run-analytics] record work interval for claw %s: %v", shortID(clawID), err)
	}
}

func (s *Server) readTaskRunWorkIntervals(tenantID string, runIDs []string) (map[string][]taskRunWorkInterval, error) {
	result := make(map[string][]taskRunWorkInterval)
	if len(runIDs) == 0 {
		return result, nil
	}
	args := []any{tenantID}
	for _, id := range runIDs {
		args = append(args, id)
	}
	rows, err := s.db.Query(`SELECT run_id, started_at, ended_at FROM task_run_work_intervals WHERE tenant_id=? AND run_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(runIDs)), ",")+`)`, args...)
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
		result[id] = append(result[id], interval)
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
