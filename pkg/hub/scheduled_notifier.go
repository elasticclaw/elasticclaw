package hub

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/notify"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

type scheduledReportBuilder func(ctx context.Context, s *Server) (*notify.Message, bool, error)

var scheduledReportRegistry = struct {
	sync.RWMutex
	builders map[string]scheduledReportBuilder
}{builders: make(map[string]scheduledReportBuilder)}

func registerScheduledReport(name string, builder scheduledReportBuilder) {
	scheduledReportRegistry.Lock()
	defer scheduledReportRegistry.Unlock()
	scheduledReportRegistry.builders[name] = builder
}

// ScheduledReportSupported reports whether name is a registered scheduled report.
func ScheduledReportSupported(name string) bool {
	scheduledReportRegistry.RLock()
	defer scheduledReportRegistry.RUnlock()
	_, ok := scheduledReportRegistry.builders[name]
	return ok
}

// scheduledReportNames lists every registered report, sorted. It backs the
// error text a rejected report name gets and the doctor check for a schedule
// naming a report this build does not carry.
func scheduledReportNames() []string {
	scheduledReportRegistry.RLock()
	defer scheduledReportRegistry.RUnlock()
	names := make([]string, 0, len(scheduledReportRegistry.builders))
	for name := range scheduledReportRegistry.builders {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func scheduledReport(name string) (scheduledReportBuilder, bool) {
	scheduledReportRegistry.RLock()
	defer scheduledReportRegistry.RUnlock()
	builder, ok := scheduledReportRegistry.builders[name]
	return builder, ok
}

// startScheduledNotifier launches the background loop that fires scheduled
// reports. It stops when stopScheduledNotifier is called (graceful shutdown,
// before the DB closes).
func (s *Server) startScheduledNotifier() {
	stop := make(chan struct{})
	done := make(chan struct{})
	s.scheduledNotifierStop, s.scheduledNotifierDone = stop, done
	s.safeGo("scheduled notifier", func() {
		defer close(done)
		for {
			timer := time.NewTimer(time.Minute)
			select {
			case <-stop:
				timer.Stop()
				return
			case <-timer.C:
			}
			// Run the tick inline (not via safeGo) so ticks never overlap and
			// shutdown can wait for the one in flight. A panic is contained to
			// this iteration.
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[notify] scheduled notifier tick panic: %v\n%s", r, debug.Stack())
					}
				}()
				s.scheduledNotifierTick(s.nowFunc())
			}()
		}
	})
}

// stopScheduledNotifier stops the tick loop and waits (bounded) for an
// in-flight tick to finish, for the same reason stopLifecycleNotifier does:
// a tick in flight at shutdown can complete an external Slack send and then
// fail the dedupe-state upsert against a closed DB, re-sending the same slot
// after restart. The timeout keeps a pathologically slow tick from wedging
// shutdown, accepting the duplicate-send window in that case.
func (s *Server) stopScheduledNotifier(timeout time.Duration) {
	if s.scheduledNotifierStop == nil {
		return
	}
	close(s.scheduledNotifierStop)
	s.scheduledNotifierStop = nil
	select {
	case <-s.scheduledNotifierDone:
	case <-time.After(timeout):
		log.Printf("[notify] scheduled notifier tick still running after %v; shutting down anyway", timeout)
	}
}

func (s *Server) scheduledNotifierTick(nowAt time.Time) {
	cfg := s.notificationsConfig()
	if cfg == nil {
		return
	}
	for _, schedule := range cfg.Scheduled {
		slot, ok := scheduledNotificationSlot(nowAt, schedule)
		if !ok {
			continue
		}
		paused := schedule.Enabled != nil && !*schedule.Enabled
		pending := make([]string, 0, len(schedule.Via))
		for _, via := range schedule.Via {
			fired, found, err := s.scheduledLastFired(schedule.ID, via)
			if err != nil {
				log.Printf("[notify] read scheduled state for %s via %s: %v", schedule.ID, via, err)
				continue
			}
			// A destination with no state row is one this hub has never
			// delivered to — a schedule (or via) just created or first seen.
			// The slot search reaches up to 8 days back, so firing here would
			// replay a slot from before the schedule existed; seed the row to
			// the current slot instead and deliver from the next one on. A
			// paused schedule advances the same way: slots that pass while it
			// is paused are skipped, not queued up for the re-enable.
			if !found || paused {
				if !found || fired.Before(slot) {
					s.setScheduledLastFired(schedule.ID, via, slot)
				}
				continue
			}
			if fired.Before(slot) {
				pending = append(pending, via)
			}
		}
		if len(pending) == 0 {
			continue
		}
		builder, found := scheduledReport(schedule.Report)
		if !found {
			log.Printf("[notify] scheduled report %q is not registered", schedule.Report)
			continue
		}
		message, hasReport, err := builder(context.Background(), s)
		if err != nil {
			log.Printf("[notify] build scheduled report %q: %v", schedule.Report, err)
			continue
		}
		if !hasReport {
			for _, via := range pending {
				s.setScheduledLastFired(schedule.ID, via, slot)
			}
			continue
		}
		for _, via := range pending {
			notifier, err := s.notifierFor(via, cfg.Notifiers[via], s.hubSecretResolver())
			constructionFailed := err != nil
			if err == nil {
				_, err = notifier.Send(context.Background(), *message)
			}
			if err == nil {
				s.setScheduledLastFired(schedule.ID, via, slot)
				continue
			}
			if !constructionFailed && notify.Classify(err) == notify.ErrorTransient {
				log.Printf("[notify] send scheduled report %q via %s: %v", schedule.Report, via, err)
				continue
			}
			log.Printf("[notify] permanently failed scheduled report %q via %s: %v", schedule.Report, via, err)
			s.setScheduledLastFired(schedule.ID, via, slot)
		}
	}
}

func scheduledNotificationSlot(nowAt time.Time, schedule types.ScheduledNotificationConfig) (time.Time, bool) {
	location := time.UTC
	if schedule.Timezone != "" {
		var err error
		location, err = time.LoadLocation(schedule.Timezone)
		if err != nil {
			return time.Time{}, false
		}
	}
	at, err := time.Parse("15:04", schedule.At)
	if err != nil {
		return time.Time{}, false
	}
	localNow := nowAt.In(location)
	allowed := make(map[string]bool, len(schedule.Weekdays))
	for _, weekday := range schedule.Weekdays {
		allowed[weekday] = true
	}
	for daysAgo := 0; daysAgo < 8; daysAgo++ {
		day := localNow.AddDate(0, 0, -daysAgo)
		weekday := strings.ToLower(day.Weekday().String()[:3])
		if len(allowed) != 0 && !allowed[weekday] {
			continue
		}
		candidate := time.Date(day.Year(), day.Month(), day.Day(), at.Hour(), at.Minute(), 0, 0, location)
		if candidate.Hour() != at.Hour() || candidate.Minute() != at.Minute() {
			continue
		}
		if !candidate.After(localNow) {
			return candidate, true
		}
	}
	return time.Time{}, false
}

func scheduledStateKey(id, via string) string {
	return "scheduled:last_fired:" + id + ":" + via
}

func (s *Server) scheduledLastFired(id, via string) (time.Time, bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT value FROM slack_notifier_state WHERE key=?`, scheduledStateKey(id, via)).Scan(&raw)
	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse scheduled state %q: %w", raw, err)
	}
	return value, true, nil
}

func (s *Server) setScheduledLastFired(id, via string, slot time.Time) {
	if _, err := s.db.Exec(`
		INSERT INTO slack_notifier_state(key, value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		scheduledStateKey(id, via), slot.Format(time.RFC3339)); err != nil {
		log.Printf("[notify] persist scheduled state for %s via %s: %v", id, via, err)
	}
}
