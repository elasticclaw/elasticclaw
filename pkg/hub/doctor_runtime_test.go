package hub

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestCheckRuntimeStateClean(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")
	checks := s.checkRuntimeState(context.Background())
	if len(checks) != 1 || !checks[0].OK || checks[0].Title != "No stuck runtime state" {
		t.Fatalf("runtime checks = %#v, want one clean check", checks)
	}
}

func TestCheckRuntimeStateFindsStuckRecords(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")
	current := time.Now().UTC()
	s.nowFunc = func() time.Time { return current }
	old := current.Add(-2 * time.Hour)
	insertRuntimeClaw(t, db, "offline-claw", "offline", old, old)
	insertRuntimeClaw(t, db, "error-claw", "error", old, old)
	insertRuntimeClaw(t, db, "timeout-claw", "connected", old, old)

	if _, err := db.Exec(`INSERT INTO workflow_runs(id, workflow_name, workspace_name, status, claw_id, created_at) VALUES(?,?,?,?,?,?)`, "orphaned-run", "workflow", "workspace", "running", "error-claw", old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO factory_triggers(id, factory_name, integration, trigger_key, claw_id, status, first_seen_at, last_seen_at, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, "claimed-trigger", "factory", "test", "trigger", "", "claimed", old, old, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO claw_checkpoints(id, tenant_id, claw_id, status, created_at) VALUES(?,?,?,?,?)`, "creating-checkpoint", "test-tenant-id", "offline-claw", "creating", old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO task_runs(id, tenant_id, initial_attempt_id, run_kind, owner_type, claw_id, timeout_at, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, "timed-out-run", "test-tenant-id", "attempt", taskRunKindCodeTask, taskRunOwnerManual, "timeout-claw", old.UnixMilli(), old.UnixMilli(), old.UnixMilli()); err != nil {
		t.Fatal(err)
	}

	checks := s.checkRuntimeState(context.Background())
	for _, title := range []string{"Stuck claws", "Orphaned workflow runs", "Unassigned claimed factory triggers", "Stuck creating checkpoints", "Timed out task runs"} {
		if !hasFailedRuntimeCheck(checks, title) {
			t.Errorf("missing failed %q check in %#v", title, checks)
		}
	}
	for title, want := range map[string]string{
		"Stuck claws":                         "threshold is twice the configured grace period",
		"Orphaned workflow runs":              "Boot reconciliation on hub restart",
		"Unassigned claimed factory triggers": "threshold is twice the configured grace period",
		"Stuck creating checkpoints":          "Boot reconciliation on hub restart",
	} {
		for _, check := range checks {
			if check.Title == title && !strings.Contains(check.Description, want) {
				t.Errorf("%q description = %q, want %q", title, check.Description, want)
			}
		}
	}
}

func TestCheckRuntimeStateAllowsReaperObservationWindow(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")
	current := time.Now().UTC()
	s.nowFunc = func() time.Time { return current }
	insertRuntimeClaw(t, db, "recent-offline-claw", "offline", current.Add(-15*time.Minute), current.Add(-15*time.Minute))

	checks := s.checkRuntimeState(context.Background())
	if hasFailedRuntimeCheck(checks, "Stuck claws") {
		t.Fatalf("offline claw within twice the configured grace period reported stuck: %#v", checks)
	}
}

func insertRuntimeClaw(t *testing.T, db *sql.DB, id, status string, createdAt, lastSeen time.Time) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, status, created_at, last_seen) VALUES(?,?,?,?,?,?)`, id, "test-tenant-id", id, status, createdAt, lastSeen); err != nil {
		t.Fatal(err)
	}
}

func hasFailedRuntimeCheck(checks []DoctorCheck, title string) bool {
	for _, check := range checks {
		if check.Title == title && !check.OK && check.Category == "runtime" && strings.Contains(check.Description, "oldest offender") {
			return true
		}
	}
	return false
}

// Alerts a route discarded at the transient-failure cap have no other
// operator-visible trace, so Doctor lists them per route for a week.
func TestCheckInfraDeliveriesSurfacesDroppedAlerts(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")
	current := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	s.nowFunc = func() time.Time { return current }
	if checks := s.checkInfraDeliveries(context.Background()); len(checks) != 0 {
		t.Fatalf("clean table produced checks: %#v", checks)
	}
	for _, row := range []struct {
		rowid  int64
		via    string
		status string
		at     time.Time
	}{
		{1, "ops", notificationDeliveryStatusFailed, current.Add(-time.Hour)},
		{2, "ops", notificationDeliveryStatusFailed, current.Add(-2 * time.Hour)},
		{3, "ops", notificationDeliveryStatusSent, current.Add(-time.Hour)},
		{4, "oncall", notificationDeliveryStatusFailed, current.Add(-8 * 24 * time.Hour)},
	} {
		if _, err := db.Exec(`INSERT INTO infra_notification_deliveries(event_rowid,notifier,delivered_at,status) VALUES(?,?,?,?)`, row.rowid, row.via, epochMillis(row.at), row.status); err != nil {
			t.Fatal(err)
		}
	}
	checks := s.checkInfraDeliveries(context.Background())
	if len(checks) != 1 {
		t.Fatalf("checks = %#v, want one for ops only (oncall's drop is older than the window)", checks)
	}
	c := checks[0]
	if c.OK || c.Severity != "warning" || c.Title != `Infrastructure route "ops" dropped 2 alert(s) in the last 7 days` || !strings.Contains(c.Description, current.Add(-time.Hour).Format(time.RFC3339)) {
		t.Fatalf("unexpected check: %#v", c)
	}
}
