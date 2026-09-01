package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/notify"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

const (
	infraDefaultPollInterval  = time.Minute
	infraMaxTransientFailures = 60
)

// Infra uses its own cursor because these events describe the fleet, not a
// task run. A route that is temporarily broken must not make a healthy route
// skip an outage it never received.
func infraWatermarkKey(via string) string { return "infra_watermark_rowid:" + via }
func infraFailureKey(rowid int64, via string) string {
	return fmt.Sprintf("infra_transient:%s:%d", via, rowid)
}

type infraEventRow struct {
	RowID              int64
	EventType, Subject string
	Detail             map[string]any
	OccurredAt         time.Time
}

func (s *Server) startInfraNotifier() {
	stop, done := make(chan struct{}), make(chan struct{})
	s.infraNotifierStop, s.infraNotifierDone = stop, done
	tickCtx, cancel := context.WithCancel(context.Background())
	s.safeGo("infra notifier", func() {
		defer close(done)
		defer cancel()
		go func() { <-stop; cancel() }()
		for {
			cfg := s.notificationsConfig()
			interval := infraDefaultPollInterval
			if cfg != nil && cfg.Infra != nil && cfg.Infra.PollInterval != "" {
				if d, err := time.ParseDuration(cfg.Infra.PollInterval); err == nil && d > 0 {
					interval = d
				}
			}
			timer := time.NewTimer(interval)
			select {
			case <-stop:
				timer.Stop()
				return
			case <-timer.C:
			}
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[notify] infra notifier tick panic: %v\n%s", r, debug.Stack())
					}
				}()
				s.infraNotifierTick(tickCtx, s.nowFunc())
			}()
		}
	})
}

func (s *Server) stopInfraNotifier(timeout time.Duration) {
	if s.infraNotifierStop == nil {
		return
	}
	close(s.infraNotifierStop)
	s.infraNotifierStop = nil
	select {
	case <-s.infraNotifierDone:
	case <-time.After(timeout):
		log.Printf("[notify] infra notifier tick still running after %v; shutting down anyway", timeout)
	}
}

func (s *Server) infraNotifierTick(ctx context.Context, nowAt time.Time) {
	cfg := s.notificationsConfig()
	if cfg == nil || !cfg.Infra.IsEnabled() {
		return
	}
	if err := types.ValidateInfraNotificationsConfig(cfg); err != nil {
		s.logPollWarningOnce("infra-config", "[notify] invalid notifications config — infrastructure notifications paused: %v", err)
		return
	}
	s.clearPollWarning("infra-config")
	for _, route := range cfg.Infra.Routes {
		via := strings.TrimSpace(route.Via)
		n, err := s.notifierFor(via, cfg.Notifiers[via], s.hubSecretResolver())
		if err != nil {
			s.logPollWarningOnce("infra-notifier:"+via, "[notify] notifier %q unavailable — its infrastructure notifications are held until it can be built: %v", via, err)
			continue
		}
		s.clearPollWarning("infra-notifier:" + via)
		if err := s.deliverInfraRoute(ctx, nowAt, via, route, n); err != nil {
			log.Printf("[notify] infrastructure route %q: %v", via, err)
		}
	}
}

func (s *Server) deliverInfraRoute(ctx context.Context, nowAt time.Time, via string, route types.InfraRoute, n notify.Notifier) error {
	watermark, _, err := s.notifierStateInt64(infraWatermarkKey(via))
	if err != nil {
		return err
	}
	// Collect the whole batch before issuing any further query on this
	// connection: the test harness (and a modest deployment) runs SQLite
	// with a single pooled connection, so a query/exec nested inside these
	// still-open rows would block forever waiting for a connection that
	// only this same in-flight Query holds.
	rows, err := s.db.Query(`SELECT rowid,event_type,subject,detail,occurred_at FROM infra_events WHERE rowid>? ORDER BY rowid LIMIT 200`, watermark)
	if err != nil {
		return err
	}
	var events []infraEventRow
	for rows.Next() {
		var e infraEventRow
		var raw string
		var occurred int64
		if err := rows.Scan(&e.RowID, &e.EventType, &e.Subject, &raw, &occurred); err != nil {
			rows.Close()
			return err
		}
		e.OccurredAt = timeFromEpochMillis(occurred)
		_ = json.Unmarshal([]byte(raw), &e.Detail)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	allowed := map[string]bool{}
	for _, e := range route.Events {
		allowed[e] = true
	}
	for _, e := range events {
		if len(allowed) != 0 && !allowed[e.EventType] {
			s.setNotifierStateInt64(infraWatermarkKey(via), e.RowID)
			continue
		}
		var exists int
		if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM infra_notification_deliveries WHERE event_rowid=? AND notifier=?)`, e.RowID, via).Scan(&exists); err != nil {
			return err
		}
		if exists != 0 {
			s.setNotifierStateInt64(infraWatermarkKey(via), e.RowID)
			continue
		}
		sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, sendErr := n.Send(sendCtx, buildInfraMessage(e, nowAt))
		cancel()
		if sendErr != nil {
			if notify.Classify(sendErr) == notify.ErrorConfig {
				return nil
			}
			if notify.Classify(sendErr) != notify.ErrorPermanent {
				count, _, stateErr := s.notifierStateInt64(infraFailureKey(e.RowID, via))
				if stateErr != nil {
					return stateErr
				}
				count++
				if count < infraMaxTransientFailures {
					s.setNotifierStateInt64(infraFailureKey(e.RowID, via), count)
					return nil
				}
				s.clearNotifierState(infraFailureKey(e.RowID, via))
			}
			if _, err := s.db.Exec(`INSERT INTO infra_notification_deliveries(event_rowid,notifier,delivered_at,status) VALUES(?,?,?,?)`, e.RowID, via, epochMillis(nowAt), notificationDeliveryStatusFailed); err != nil {
				return err
			}
		} else {
			s.clearNotifierState(infraFailureKey(e.RowID, via))
			if _, err := s.db.Exec(`INSERT INTO infra_notification_deliveries(event_rowid,notifier,delivered_at,status) VALUES(?,?,?,?)`, e.RowID, via, epochMillis(nowAt), notificationDeliveryStatusSent); err != nil {
				return err
			}
		}
		s.setNotifierStateInt64(infraWatermarkKey(via), e.RowID)
	}
	return nil
}

type infraEventStyle struct {
	emoji, title string
	severity     notify.Severity
}

var infraEventStyles = map[string]infraEventStyle{
	"dependency_down": {":fire:", "Dependency is down", notify.SeverityError}, "dependency_degraded": {":warning:", "Dependency is degraded", notify.SeverityWarning}, "dependency_recovered": {":white_check_mark:", "Dependency recovered", notify.SeveritySuccess},
	"provider_limit_opened": {":credit_card:", "Provider account is capped", notify.SeverityWarning}, "provider_limit_exhausted": {":no_entry:", "Provider cap needs a human", notify.SeverityError}, "provider_limit_released": {":white_check_mark:", "Provider cap lifted", notify.SeveritySuccess},
}

func buildInfraMessage(e infraEventRow, _ time.Time) notify.Message {
	style, ok := infraEventStyles[e.EventType]
	if !ok {
		style = infraEventStyle{":warning:", "Infrastructure event", notify.SeverityWarning}
	}
	subject := strings.TrimSpace(stringValue(e.Detail, "name"))
	if subject == "" {
		subject = e.Subject
	}
	body := "Wait for recovery and watch the dependency's status page."
	if strings.HasPrefix(e.EventType, "provider_limit_") {
		body = "Raise the provider account cap in its billing console, or wait until the stated reset time."
	}
	if e.EventType == "dependency_recovered" || e.EventType == "provider_limit_released" {
		body = "Service has recovered; resume watching normal work."
	}
	if msg := strings.TrimSpace(stringValue(e.Detail, "message")); msg != "" {
		body += "\n" + msg
	}
	return notify.Message{Emoji: style.emoji, Title: style.title, Severity: style.severity, Subject: subject, Body: body, Fields: []notify.Field{{Label: "Event", Value: strings.ReplaceAll(e.EventType, "_", " ")}, {Label: "Occurred", Value: e.OccurredAt.UTC().Format(time.RFC3339)}}, Summary: []string{style.title, subject}}
}

func stringValue(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
}
