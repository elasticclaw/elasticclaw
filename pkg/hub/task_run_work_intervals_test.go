package hub

import (
	"testing"
	"time"
)

func TestComputeTicketWorkTimes(t *testing.T) {
	tests := []struct {
		name                string
		intervals           []taskRunWorkInterval
		opened, merged, end int64
		factory, waiting    int64
		tracked             bool
	}{
		{"overlapping turns across runs", []taskRunWorkInterval{{10, 40}, {20, 50}, {50, 60}}, 0, 0, 100, 50, 0, true},
		{"clip both ends", []taskRunWorkInterval{{0, 20}, {80, 120}, {150, 200}}, 30, 90, 100, 30, 50, true},
		{"open PR uses now", []taskRunWorkInterval{{10, 30}}, 40, 0, 100, 20, 60, true},
		{"no PR", []taskRunWorkInterval{{20, 50}}, 0, 0, 100, 30, 0, true},
		{"review rework is factory", []taskRunWorkInterval{{10, 30}, {60, 80}, {70, 90}}, 40, 100, 100, 50, 30, true},
		{"historical reports nothing", nil, 40, 100, 100, 0, 0, false},
		{"invalid turns", []taskRunWorkInterval{{20, 20}, {40, 30}}, 0, 0, 100, 0, 0, false},
		{"merge clips work", []taskRunWorkInterval{{70, 120}}, 40, 80, 80, 10, 30, true},
		{"waiting clipped to lead", []taskRunWorkInterval{{150, 200}}, 1, 200, 100, 0, 90, true},
		{"outside window still tracked", []taskRunWorkInterval{{150, 200}}, 0, 0, 100, 0, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			factory, waiting, tracked := computeTicketWorkTimes(tt.intervals, 10, tt.end, tt.opened, tt.merged, 100)
			if factory != tt.factory || waiting != tt.waiting || tracked != tt.tracked {
				t.Fatalf("got (%d,%d,%v), want (%d,%d,%v)", factory, waiting, tracked, tt.factory, tt.waiting, tt.tracked)
			}
		})
	}
}

func insertLegacyWorkClaw(t *testing.T) (*Server, *clawConn, chan error) {
	t.Helper()
	s, db := newTaskRunAnalyticsAPITestServer(t)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{OwnerType: taskRunOwnerWorkflow, RunID: "work-run", AttemptID: "work-attempt", ClawID: "work-claw", TenantID: "test-tenant-id", StartedAt: 1000})
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,template,status,pipeline_stage,task_run_id,created_at) VALUES('work-claw','test-tenant-id','work','elasticclaw','running','implement','work-run',datetime('now'))`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 4)
	cc := &clawConn{id: "work-claw", onWorkTurn: func(turn taskRunWorkTurn) { done <- s.recordTaskRunWorkTurnForClaw("work-claw", turn) }}
	return s, cc, done
}

func waitWorkTurn(t *testing.T, done chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interval not persisted")
	}
}

func readWorkRun(t *testing.T, s *Server, requestNow int64) []taskRunWorkInterval {
	t.Helper()
	got, err := s.readTaskRunWorkIntervals("test-tenant-id", []string{"work-run"}, requestNow)
	if err != nil {
		t.Fatal(err)
	}
	return got["work-run"]
}

func TestLegacyTurnPersistsWorkIntervalAfterUnlock(t *testing.T) {
	for _, boundary := range []string{"complete", "abort", "disconnect"} {
		t.Run(boundary, func(t *testing.T) {
			s, cc, done := insertLegacyWorkClaw(t)
			start := now().Add(-time.Second)
			cc.streamingStartedAt = start
			cc.mu.Lock()
			switch boundary {
			case "complete":
				cc.finishTurnLocked()
			case "abort":
				cc.abortTurnLocked()
			case "disconnect":
				cc.resetTurnStateLocked()
			}
			select {
			case <-done:
				cc.mu.Unlock()
				t.Fatal("persisted while turn lock held")
			default:
			}
			cc.mu.Unlock()
			intervals := readWorkRun(t, s, now().UnixMilli())
			if boundary == "abort" {
				// A reservation that never reached the bridge is not work.
				select {
				case <-done:
					t.Fatal("aborted reservation persisted")
				case <-time.After(50 * time.Millisecond):
				}
				if len(intervals) != 0 {
					t.Fatalf("intervals = %#v", intervals)
				}
				return
			}
			waitWorkTurn(t, done)
			intervals = readWorkRun(t, s, now().UnixMilli())
			if len(intervals) != 1 || intervals[0].Start != start.UnixMilli() || intervals[0].End <= intervals[0].Start {
				t.Fatalf("intervals = %#v", intervals)
			}
			if err := s.recordTaskRunWorkTurnForClaw("missing-claw", taskRunWorkTurn{ID: "missing", Start: 1, End: 2}); err != nil {
				t.Fatal(err)
			}
			if err := migrate(s.db); err != nil {
				t.Fatalf("idempotent migration: %v", err)
			}
			var count int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM task_run_work_intervals`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("count=%d error=%v", count, err)
			}
			if _, err := s.db.Exec(`DELETE FROM task_runs WHERE id='work-run'`); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM task_run_work_intervals`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("cascade count=%d error=%v", count, err)
			}
		})
	}
}

func TestLegacyInFlightTurnCheckpoints(t *testing.T) {
	for _, boundary := range []string{"finish", "abort", "restart"} {
		t.Run(boundary, func(t *testing.T) {
			s, cc, done := insertLegacyWorkClaw(t)
			start := now().Add(-time.Minute)
			cc.streamingStartedAt = start
			s.mu.Lock()
			s.claws["work-claw"] = cc
			s.mu.Unlock()
			s.checkpointTaskRunWorkTurns()
			// In-flight work is visible up to the request time.
			requestNow := now().Add(time.Hour).UnixMilli()
			if got := readWorkRun(t, s, requestNow); len(got) != 1 || got[0].Start != start.UnixMilli() || got[0].End != requestNow {
				t.Fatalf("open intervals = %#v", got)
			}
			s.checkpointTaskRunWorkTurns() // idempotent
			switch boundary {
			case "finish":
				cc.mu.Lock()
				cc.finishTurnLocked()
				cc.mu.Unlock()
				waitWorkTurn(t, done)
				got := readWorkRun(t, s, requestNow)
				if len(got) != 1 || got[0].End >= requestNow || got[0].End <= got[0].Start {
					t.Fatalf("closed intervals = %#v", got)
				}
			case "abort":
				cc.mu.Lock()
				cc.abortTurnLocked()
				cc.mu.Unlock()
				waitWorkTurn(t, done)
				s.checkpointTaskRunWorkTurns()
				if got := readWorkRun(t, s, requestNow); len(got) != 0 {
					t.Fatalf("aborted intervals = %#v", got)
				}
			case "restart":
				s.recoverTaskRunWorkTurns()
				got := readWorkRun(t, s, requestNow)
				if len(got) != 1 || got[0].End >= requestNow || got[0].End <= got[0].Start {
					t.Fatalf("recovered intervals = %#v", got)
				}
			}
		})
	}
}
