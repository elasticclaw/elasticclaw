package hub

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const (
	runtimeReaperHint             = " The periodic liveness reaper (pkg/hub/reaper.go) should clear these; a nonzero steady-state count means the reaper is broken or disabled."
	runtimeBootReconciliationHint = " Boot reconciliation on hub restart (pkg/hub/reaper.go reconcileOnBoot) repairs these; a persistent count means they have survived restarts or the hub has not restarted since they got stuck."
	runtimeAgeMarginHint          = " This check's threshold is twice the configured grace period to allow for the reaper's observation window."
	runtimeCheckpointMarginHint   = " This check's threshold is twice the configured provisioning grace to avoid flagging healthy in-flight checkpoints."
)

// checkRuntimeState reports records which the liveness reaper should have repaired.
func (s *Server) checkRuntimeState(ctx context.Context) []DoctorCheck {
	cfg, current := s.livenessSettings(), s.reaperNow()
	var checks []DoctorCheck

	appendTimestampCheck := func(title, hint, query string, args ...any) {
		var count int
		var oldest sql.NullInt64
		if err := s.db.QueryRowContext(ctx, query, args...).Scan(&count, &oldest); err != nil {
			checks = append(checks, runtimeQueryErrorCheck(title, err))
			return
		}
		if count == 0 {
			return
		}
		checks = append(checks, runtimeStuckCheck(title, count, current.Sub(time.Unix(oldest.Int64, 0)), hint))
	}

	appendTimestampCheck("Stuck claws", runtimeReaperHint+runtimeAgeMarginHint, `
		SELECT COUNT(*), MIN(CAST(strftime('%s', CASE WHEN status='offline' THEN COALESCE(last_seen, created_at) ELSE created_at END) AS INTEGER))
		FROM claws WHERE (status='offline' AND COALESCE(last_seen, created_at) < ?)
		OR (status IN ('provisioning', 'starting') AND created_at < ?)`,
		current.Add(-2*cfg.offlineGrace), current.Add(-2*cfg.provisioningMaxAge))
	appendTimestampCheck("Orphaned workflow runs", runtimeBootReconciliationHint, `
		SELECT COUNT(*), MIN(CAST(strftime('%s', COALESCE(started_at, created_at)) AS INTEGER)) FROM workflow_runs
		WHERE status='running' AND (claw_id='' OR NOT EXISTS (SELECT 1 FROM claws c WHERE c.id=workflow_runs.claw_id) OR EXISTS (SELECT 1 FROM claws c WHERE c.id=workflow_runs.claw_id AND c.status IN ('error','deleted')))`)
	appendTimestampCheck("Unassigned claimed factory triggers", runtimeReaperHint+runtimeAgeMarginHint, `
		SELECT COUNT(*), MIN(CAST(strftime('%s', created_at) AS INTEGER)) FROM factory_triggers
		WHERE status='claimed' AND claw_id='' AND created_at < ?`, current.Add(-2*cfg.claimTTL))
	appendTimestampCheck("Stuck creating checkpoints", runtimeBootReconciliationHint+runtimeCheckpointMarginHint, `
		SELECT COUNT(*), MIN(CAST(strftime('%s', created_at) AS INTEGER)) FROM claw_checkpoints
		WHERE status='creating' AND created_at < ?`, current.Add(-2*cfg.provisioningMaxAge))

	var timeoutCount int
	var oldestTimeout sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), MIN(tr.timeout_at) FROM task_runs tr
		JOIN claws c ON c.id=tr.claw_id
		WHERE tr.timeout_at > 0 AND tr.timeout_at < ?
		AND c.status IN ('provisioning','starting','connected','idle','offline')`, current.UnixMilli()).Scan(&timeoutCount, &oldestTimeout); err != nil {
		checks = append(checks, runtimeQueryErrorCheck("Timed out task runs", err))
	} else if timeoutCount > 0 {
		checks = append(checks, runtimeStuckCheck("Timed out task runs", timeoutCount, current.Sub(time.UnixMilli(oldestTimeout.Int64)), runtimeReaperHint))
	}

	if len(checks) == 0 {
		return []DoctorCheck{{
			Category:    "runtime",
			Severity:    "info",
			Title:       "No stuck runtime state",
			Description: "No runtime records are awaiting liveness reaper recovery.",
			OK:          true,
		}}
	}
	return checks
}

func runtimeStuckCheck(title string, count int, oldestAge time.Duration, hint string) DoctorCheck {
	return DoctorCheck{
		Category:    "runtime",
		Severity:    "warning",
		Title:       title,
		Description: fmt.Sprintf("%d record(s); oldest offender is %s old.%s", count, oldestAge.Round(time.Second), hint),
		OK:          false,
	}
}

func runtimeQueryErrorCheck(title string, err error) DoctorCheck {
	return DoctorCheck{
		Category:    "runtime",
		Severity:    "warning",
		Title:       "Could not verify runtime state: " + title,
		Description: "Could not verify runtime state; the liveness reaper may be broken or disabled.",
		Error:       err.Error(),
		OK:          false,
	}
}

// infraDeliveryFailureWindow bounds how far back Doctor looks for discarded
// infrastructure alerts. A week outlives any on-call rotation that could
// have missed the outage, and older drops are no longer actionable.
const infraDeliveryFailureWindow = 7 * 24 * time.Hour

// checkInfraDeliveries surfaces infrastructure alerts a route gave up on. The
// transient-failure cap discards an alert after infraMaxTransientFailures
// consecutive send failures so one poisoned send cannot wedge the route, and
// nothing else in the product shows that it happened: the channel simply
// never hears about the outage. Doctor is where that gap becomes visible.
func (s *Server) checkInfraDeliveries(ctx context.Context) []DoctorCheck {
	if s.db == nil {
		return nil
	}
	current := now()
	if s.nowFunc != nil {
		current = s.nowFunc()
	}
	rows, err := s.db.QueryContext(ctx, `SELECT notifier, COUNT(*), MAX(delivered_at) FROM infra_notification_deliveries
		WHERE status=? AND delivered_at>=? GROUP BY notifier ORDER BY notifier`,
		notificationDeliveryStatusFailed, epochMillis(current.Add(-infraDeliveryFailureWindow)))
	if err != nil {
		return []DoctorCheck{runtimeQueryErrorCheck("Dropped infrastructure alerts", err)}
	}
	defer rows.Close()
	var checks []DoctorCheck
	for rows.Next() {
		var via string
		var count int
		var latest int64
		if err := rows.Scan(&via, &count, &latest); err != nil {
			return append(checks, runtimeQueryErrorCheck("Dropped infrastructure alerts", err))
		}
		checks = append(checks, DoctorCheck{
			Category: "notifications", Severity: "warning", OK: false,
			Title: fmt.Sprintf("Infrastructure route %q dropped %d alert(s) in the last 7 days", via, count),
			Description: fmt.Sprintf("The hub gave up on each after %d consecutive failed send attempts, so the channel never received them; the latest was discarded at %s. Check the notifier and the hub log for [notify] errors.",
				infraMaxTransientFailures, time.UnixMilli(latest).UTC().Format(time.RFC3339)),
		})
	}
	return checks
}
