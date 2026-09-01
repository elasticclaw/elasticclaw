package hub

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"runtime/debug"
	"time"
)

const dependencyWatcherInterval = time.Minute

type dependencyStatusState struct {
	ID             string
	Status         string
	Message        string
	Since          time.Time
	NotifiedStatus string
	// LastCheckedAt is the CheckedAt of the snapshot observation that last
	// counted toward the debounce. The watcher ticks every minute but the
	// status cache lives five: without this fence the same vendor-page fetch
	// is re-served to consecutive ticks and one flapping observation would
	// satisfy the two-consecutive-checks debounce all by itself.
	LastCheckedAt time.Time
	// LastAlertAt is when this dependency last produced a degraded/down
	// event, so the opt-in repeat_after can re-alert during a long outage.
	LastAlertAt time.Time
}

// startDependencyWatcher keeps status-page changes visible even when nobody
// has the dashboard open. It runs ticks inline because an overlapping check
// could make two observations look like the debounce had been satisfied.
func (s *Server) startDependencyWatcher() {
	stop := make(chan struct{})
	done := make(chan struct{})
	s.dependencyWatcherStop, s.dependencyWatcherDone = stop, done
	tickCtx, cancelTicks := context.WithCancel(context.Background())
	s.safeGo("dependency watcher", func() {
		defer close(done)
		defer cancelTicks()
		go func() { <-stop; cancelTicks() }()
		for {
			timer := time.NewTimer(dependencyWatcherInterval)
			select {
			case <-stop:
				timer.Stop()
				return
			case <-timer.C:
			}
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[dependencies] watcher tick panic: %v\n%s", r, debug.Stack())
					}
				}()
				s.dependencyWatcherTick(tickCtx, s.nowFunc())
			}()
		}
	})
}

func (s *Server) stopDependencyWatcher(timeout time.Duration) {
	if s.dependencyWatcherStop == nil {
		return
	}
	close(s.dependencyWatcherStop)
	s.dependencyWatcherStop = nil
	select {
	case <-s.dependencyWatcherDone:
	case <-time.After(timeout):
		log.Printf("[dependencies] watcher tick still running after %v; shutting down anyway", timeout)
	}
}

func (s *Server) dependencyWatcherTick(ctx context.Context, nowAt time.Time) {
	if err := ctx.Err(); err != nil {
		return
	}
	repeatAfter := s.infraRepeatAfter()
	for _, dependency := range s.dependencyStatus.snapshotForWatcher(ctx).Dependencies {
		if err := ctx.Err(); err != nil {
			return
		}
		// Limited is an account-local fact with richer retry information in the
		// LLM latch. Unknown means this hub cannot observe the vendor at all;
		// treating it as a transition would manufacture outages during egress loss.
		if dependency.Status == dependencyStatusUnknown || dependency.Status == dependencyStatusLimited {
			continue
		}
		if err := s.observeDependencyStatus(dependency, nowAt, repeatAfter); err != nil {
			log.Printf("[dependencies] record status for %s: %v", dependency.ID, err)
		}
	}
}

// infraRepeatAfter reads notifications.infra.repeat_after on every tick so a
// config change needs no restart. Zero means repeats are off — the default,
// and deliberately so (see InfraNotificationsConfig).
func (s *Server) infraRepeatAfter() time.Duration {
	cfg := s.notificationsConfig()
	if cfg == nil || cfg.Infra == nil || cfg.Infra.RepeatAfter == "" {
		return 0
	}
	if d, err := time.ParseDuration(cfg.Infra.RepeatAfter); err == nil && d > 0 {
		return d
	}
	return 0
}

func (s *Server) observeDependencyStatus(dependency DependencyStatus, nowAt time.Time, repeatAfter time.Duration) error {
	state, found, err := s.loadDependencyStatusState(dependency.ID)
	if err != nil {
		return err
	}
	if !found {
		state = dependencyStatusState{ID: dependency.ID}
	}

	// Only a strictly newer fetch counts as a new check. The per-dependency
	// CheckedAt is stamped at refresh time and survives the cache, so a
	// re-served snapshot carries the same instant and is the same single
	// observation, not a second consecutive one. A zero CheckedAt (only
	// synthetic snapshots produce one) always counts rather than never.
	// Truncated to the millisecond the state row stores, or a reloaded state
	// would compare as older than the very fetch that produced it.
	checkedAt := dependency.CheckedAt.Truncate(time.Millisecond)
	if !checkedAt.IsZero() && !checkedAt.After(state.LastCheckedAt) {
		return nil
	}
	if checkedAt.After(state.LastCheckedAt) {
		state.LastCheckedAt = checkedAt
	}

	if dependency.Status == dependencyStatusOperational {
		if state.NotifiedStatus == dependencyStatusDegraded || state.NotifiedStatus == dependencyStatusDowntime {
			duration := time.Duration(0)
			if !state.Since.IsZero() {
				duration = nowAt.Sub(state.Since)
			}
			if err := s.recordInfraEvent(infraEvent{
				EventKey:  fmt.Sprintf("dependency_recovered:%s:%d", dependency.ID, epochMillis(state.Since)),
				EventType: "dependency_recovered", Subject: dependency.ID,
				Detail:     map[string]any{"id": dependency.ID, "name": dependency.Name, "message": dependency.Message, "duration_ms": duration.Milliseconds()},
				OccurredAt: nowAt,
			}); err != nil {
				return err
			}
		}
		state.Status, state.Message, state.Since, state.NotifiedStatus, state.LastAlertAt = dependency.Status, dependency.Message, time.Time{}, dependencyStatusOperational, time.Time{}
		return s.storeDependencyStatusState(state, nowAt)
	}

	// A different bad status starts a fresh debounce but keeps the outage's
	// original start: recovery duration describes the whole customer-visible
	// incident, not merely its last status-page label.
	if state.Status != dependency.Status {
		if state.Since.IsZero() || state.Status == dependencyStatusOperational || state.Status == "" {
			state.Since = nowAt
		}
		state.Status, state.Message = dependency.Status, dependency.Message
		return s.storeDependencyStatusState(state, nowAt)
	}

	state.Message = dependency.Message
	if state.NotifiedStatus != dependency.Status {
		eventType := ""
		switch dependency.Status {
		case dependencyStatusDegraded:
			if state.NotifiedStatus == "" || state.NotifiedStatus == dependencyStatusOperational {
				eventType = "dependency_degraded"
			}
		case dependencyStatusDowntime:
			if state.NotifiedStatus == "" || state.NotifiedStatus == dependencyStatusOperational || state.NotifiedStatus == dependencyStatusDegraded {
				eventType = "dependency_down"
			}
		}
		if eventType != "" {
			if err := s.recordInfraEvent(infraEvent{
				EventKey:  fmt.Sprintf("%s:%s:%d", eventType, dependency.ID, epochMillis(state.Since)),
				EventType: eventType, Subject: dependency.ID,
				Detail:     map[string]any{"id": dependency.ID, "name": dependency.Name, "message": dependency.Message, "status": dependency.Status},
				OccurredAt: nowAt,
			}); err != nil {
				return err
			}
			state.NotifiedStatus = dependency.Status
			state.LastAlertAt = nowAt
		}
	} else if repeatAfter > 0 && !state.LastAlertAt.IsZero() && nowAt.Sub(state.LastAlertAt) >= repeatAfter {
		// repeat_after is opt-in: re-say a still-standing outage so a channel
		// that scrolled past the original alert hears about it again. A fresh
		// event key per repeat keeps the event-store dedupe from swallowing it.
		eventType := "dependency_degraded"
		if dependency.Status == dependencyStatusDowntime {
			eventType = "dependency_down"
		}
		if err := s.recordInfraEvent(infraEvent{
			EventKey:  fmt.Sprintf("%s:%s:%d:repeat:%d", eventType, dependency.ID, epochMillis(state.Since), epochMillis(nowAt)),
			EventType: eventType, Subject: dependency.ID,
			Detail:     map[string]any{"id": dependency.ID, "name": dependency.Name, "message": dependency.Message, "status": dependency.Status, "repeat": true},
			OccurredAt: nowAt,
		}); err != nil {
			return err
		}
		state.LastAlertAt = nowAt
	}
	return s.storeDependencyStatusState(state, nowAt)
}

func (s *Server) loadDependencyStatusState(id string) (dependencyStatusState, bool, error) {
	var state dependencyStatusState
	var since, lastChecked, lastAlert int64
	err := s.db.QueryRow(`SELECT id, status, message, since, notified_status, last_checked_at, last_alert_at FROM dependency_status_state WHERE id=?`, id).
		Scan(&state.ID, &state.Status, &state.Message, &since, &state.NotifiedStatus, &lastChecked, &lastAlert)
	if err == sql.ErrNoRows {
		return dependencyStatusState{}, false, nil
	}
	if err != nil {
		return dependencyStatusState{}, false, fmt.Errorf("load state: %w", err)
	}
	state.Since = timeFromEpochMillis(since)
	state.LastCheckedAt = timeFromEpochMillis(lastChecked)
	state.LastAlertAt = timeFromEpochMillis(lastAlert)
	return state, true, nil
}

func (s *Server) storeDependencyStatusState(state dependencyStatusState, nowAt time.Time) error {
	_, err := s.db.Exec(`INSERT INTO dependency_status_state(id, status, message, since, notified_status, last_checked_at, last_alert_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET status=excluded.status, message=excluded.message,
		since=excluded.since, notified_status=excluded.notified_status, last_checked_at=excluded.last_checked_at,
		last_alert_at=excluded.last_alert_at, updated_at=excluded.updated_at`,
		state.ID, state.Status, state.Message, epochMillisOrZero(state.Since), state.NotifiedStatus,
		epochMillisOrZero(state.LastCheckedAt), epochMillisOrZero(state.LastAlertAt), epochMillis(nowAt))
	if err != nil {
		return fmt.Errorf("store state: %w", err)
	}
	return nil
}
