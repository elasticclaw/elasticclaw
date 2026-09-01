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

// initInfraNotifierBaseline stamps every configured route's watermark at the
// head of infra_events before any event producer starts, for the same reason
// initLifecycleNotifierBaseline does: the producers (dependency watcher, LLM
// limit latch) run unconditionally from boot, so a route whose cursor were
// left at zero would replay the entire recorded history — weeks of resolved
// outages — into its channel the first time it delivered. Only a missing
// watermark is written; persisted cursors stay authoritative.
func (s *Server) initInfraNotifierBaseline() {
	cfg := s.notificationsConfig()
	if cfg == nil || !cfg.Infra.IsEnabled() {
		return
	}
	for _, route := range cfg.Infra.Routes {
		via := strings.TrimSpace(route.Via)
		if via == "" {
			continue
		}
		if _, found, err := s.notifierStateInt64(infraWatermarkKey(via)); err == nil && !found {
			if maxRow, err := s.infraMaxEventRowID(); err == nil {
				s.setNotifierStateInt64(infraWatermarkKey(via), maxRow)
			} else {
				log.Printf("[notify] init infra watermark baseline for %q: %v", via, err)
			}
		}
	}
}

func (s *Server) infraMaxEventRowID() (int64, error) {
	var maxRow int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(rowid),0) FROM infra_events`).Scan(&maxRow)
	return maxRow, err
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
	s.flushPendingInfraDeliveries()
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
	watermark, found, err := s.notifierStateInt64(infraWatermarkKey(via))
	if err != nil {
		return err
	}
	if !found {
		// A route seen for the first time (added at runtime, or enabled after
		// the hub had already been recording events) starts at the head of the
		// stream: everything before it existed is history, not news. The boot
		// path stamps configured routes synchronously (initInfraNotifierBaseline);
		// this is the backstop for runtime config changes.
		maxRow, err := s.infraMaxEventRowID()
		if err != nil {
			return err
		}
		s.setNotifierStateInt64(infraWatermarkKey(via), maxRow)
		return nil
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
		// A delivery whose bookkeeping row is still stashed in memory counts
		// as delivered: the send already happened, only its record is owed.
		if s.infraDeliveryPending(e.RowID, via) {
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
			s.recordInfraDelivery(e.RowID, via, notificationDeliveryStatusFailed, nowAt)
		} else {
			s.clearNotifierState(infraFailureKey(e.RowID, via))
			s.recordInfraDelivery(e.RowID, via, notificationDeliveryStatusSent, nowAt)
		}
		s.setNotifierStateInt64(infraWatermarkKey(via), e.RowID)
	}
	return nil
}

// infraDeliveryKey identifies one stashed infra delivery row.
type infraDeliveryKey struct {
	rowid int64
	via   string
}

// infraPendingDelivery is a delivery whose post-send bookkeeping write failed;
// flushPendingInfraDeliveries retries it. While stashed, the (event, route)
// pair still counts as delivered (see deliverInfraRoute): the send already
// reached the channel, so retrying the SEND on a sick database would page the
// same alert every tick — the lesson recordNotificationDelivery learned.
type infraPendingDelivery struct {
	status      string
	deliveredAt time.Time
}

func (s *Server) infraDeliveryPending(rowid int64, via string) bool {
	_, ok := s.infraPendingDeliveries[infraDeliveryKey{rowid: rowid, via: via}]
	return ok
}

func (s *Server) recordInfraDelivery(rowid int64, via, status string, at time.Time) {
	if err := s.execInfraDelivery(rowid, via, status, at); err != nil {
		log.Printf("[notify] record infra delivery for event %d via %s (will retry): %v", rowid, via, err)
		if s.infraPendingDeliveries == nil {
			s.infraPendingDeliveries = make(map[infraDeliveryKey]infraPendingDelivery)
		}
		s.infraPendingDeliveries[infraDeliveryKey{rowid: rowid, via: via}] = infraPendingDelivery{status: status, deliveredAt: at}
	}
}

func (s *Server) execInfraDelivery(rowid int64, via, status string, at time.Time) error {
	_, err := s.db.Exec(`INSERT INTO infra_notification_deliveries(event_rowid,notifier,delivered_at,status) VALUES(?,?,?,?) ON CONFLICT(event_rowid,notifier) DO NOTHING`, rowid, via, epochMillis(at), status)
	return err
}

// flushPendingInfraDeliveries retries delivery rows whose insert failed after
// a successful send. Runs at the top of every tick so the stash drains as
// soon as the database accepts writes again. Like the lifecycle stash, it
// only closes the dedupe gap within one process lifetime — see
// flushPendingNotificationDeliveries for why that window is accepted.
func (s *Server) flushPendingInfraDeliveries() {
	for key, p := range s.infraPendingDeliveries {
		if err := s.execInfraDelivery(key.rowid, key.via, p.status, p.deliveredAt); err != nil {
			log.Printf("[notify] retry infra delivery for event %d via %s: %v", key.rowid, key.via, err)
			continue
		}
		delete(s.infraPendingDeliveries, key)
	}
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
	fields := []notify.Field{{Label: "Event", Value: strings.ReplaceAll(e.EventType, "_", " ")}}
	if strings.HasPrefix(e.EventType, "dependency_") {
		if statusPage := stringValue(e.Detail, "status_page"); statusPage != "" {
			fields = append(fields, notify.Field{Label: "Status page", Value: statusPage})
		}
	} else if strings.HasPrefix(e.EventType, "provider_limit_") {
		for _, field := range []struct{ label, key string }{{"Provider", "provider"}, {"Key", "key_id"}, {"Claws parked", "parked_claws"}, {"Deadline", "deadline"}} {
			if value := stringValue(e.Detail, field.key); value != "" {
				fields = append(fields, notify.Field{Label: field.label, Value: value})
			}
		}
	}
	fields = append(fields, notify.Field{Label: "Occurred", Value: e.OccurredAt.UTC().Format(time.RFC3339)})
	return notify.Message{Emoji: style.emoji, Title: style.title, Severity: style.severity, Subject: subject, Body: body, Fields: fields, Summary: []string{style.title, subject}}
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
	// Details straight from a producer (the test-send path) carry Go ints;
	// details read back from the store arrive as float64 via json.Unmarshal.
	// Both spellings of the same count must render.
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	default:
		return ""
	}
}
