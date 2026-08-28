package hub

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"runtime/debug"
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

func scheduledReport(name string) (scheduledReportBuilder, bool) {
	scheduledReportRegistry.RLock()
	defer scheduledReportRegistry.RUnlock()
	builder, ok := scheduledReportRegistry.builders[name]
	return builder, ok
}

func (s *Server) startScheduledNotifier() {
	s.safeGo("scheduled notifier", func() {
		for {
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[notify] scheduled notifier loop panic, restarting: %v\n%s", r, debug.Stack())
					}
				}()
				ticker := time.NewTicker(time.Minute)
				defer ticker.Stop()
				for range ticker.C {
					func() {
						defer func() {
							if r := recover(); r != nil {
								log.Printf("[notify] scheduled notifier tick panic: %v\n%s", r, debug.Stack())
							}
						}()
						s.scheduledNotifierTick(s.nowFunc())
					}()
				}
			}()
		}
	})
}

func (s *Server) scheduledNotifierTick(nowAt time.Time) {
	cfg := s.notificationsConfig()
	if cfg == nil {
		return
	}
	for _, schedule := range cfg.Scheduled {
		if schedule.Enabled != nil && !*schedule.Enabled {
			continue
		}
		slot, ok := scheduledNotificationSlot(nowAt, schedule)
		if !ok {
			continue
		}
		pending := make([]string, 0, len(schedule.Via))
		for _, via := range schedule.Via {
			fired, found, err := s.scheduledLastFired(schedule.ID, via)
			if err != nil {
				log.Printf("[notify] read scheduled state for %s via %s: %v", schedule.ID, via, err)
				continue
			}
			if !found || fired.Before(slot) {
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
