package hub

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenDBSetsBusyTimeout(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var timeout int
	if err := db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if timeout != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", timeout)
	}
}

func TestClawsTriggerActorJSONDefaultIsValidJSON(t *testing.T) {
	t.Run("fresh schema", func(t *testing.T) {
		db, err := openDB(filepath.Join(t.TempDir(), "hub.db"))
		if err != nil {
			t.Fatalf("open db: %v", err)
		}
		defer db.Close()

		if _, err := db.Exec(`INSERT INTO tenants(id, name, token, claw_token, created_at) VALUES(?,?,?,?,?)`, "tenant", "Tenant", "token", "claw-token", time.Now()); err != nil {
			t.Fatalf("insert tenant: %v", err)
		}
		insertClawUsingTriggerActorDefault(t, db, "claw-default")

		assertClawTriggerActorJSONIsValid(t, db, "claw-default")
	})

	t.Run("migrated schema", func(t *testing.T) {
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "hub.db")+"?_time_format=sqlite&_pragma=foreign_keys(on)")
		if err != nil {
			t.Fatalf("open db: %v", err)
		}
		defer db.Close()

		if _, err := db.Exec(`
			CREATE TABLE claws (
				id TEXT PRIMARY KEY,
				tenant_id TEXT NOT NULL,
				name TEXT NOT NULL,
				template TEXT NOT NULL DEFAULT '',
				provider TEXT NOT NULL DEFAULT '',
				status TEXT NOT NULL DEFAULT 'offline',
				created_at DATETIME NOT NULL
			)`); err != nil {
			t.Fatalf("create old claws table: %v", err)
		}
		if err := migrate(db); err != nil {
			t.Fatalf("migrate old claws table: %v", err)
		}
		insertClawUsingTriggerActorDefault(t, db, "claw-default")

		assertClawTriggerActorJSONIsValid(t, db, "claw-default")
	})
}

func TestBackfillTaskRunAgentStartedAtV1(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	ts := int64(1760000000000)

	// PR opened + provisioning recorded: should backfill to provision_started_at.
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-pr-opened", AttemptID: "attempt-pr-opened", ClawID: "claw-pr-opened", TenantID: "test-tenant-id",
		Status: taskRunStatusRunning, Phase: taskRunPhaseWaitingForMerge, OwnerType: taskRunOwnerFactory,
		StartedAt: ts, PRCount: 1, OpenPRCount: 1,
	})
	if _, err := db.Exec(`UPDATE task_run_summaries SET provision_started_at=?, pr_opened_at=? WHERE run_id='run-pr-opened'`, ts+1000, ts+5000); err != nil {
		t.Fatalf("seed provision/pr_opened: %v", err)
	}

	// Finished with a failure type that can only be reached after the agent
	// ran to completion ([DONE] with no PR URLs), but no provision_started_at
	// recorded: should fall back to started_at.
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-done-without-pr", AttemptID: "attempt-done-without-pr", ClawID: "claw-done-without-pr", TenantID: "test-tenant-id",
		Status: taskRunStatusFailed, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerFactory,
		FailureType: taskRunFailureDoneWithoutPR, StartedAt: ts + 2000, FinishedAt: ts + 9000,
	})

	// Never left provisioning: no PR, not finished — must stay untouched.
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-still-provisioning", AttemptID: "attempt-provisioning", ClawID: "claw-provisioning", TenantID: "test-tenant-id",
		Status: taskRunStatusRunning, Phase: taskRunPhaseProvisioning, OwnerType: taskRunOwnerFactory,
		StartedAt: ts + 3000,
	})

	// Finished, but the agent itself never came up: must stay untouched. This
	// is the case Greptile flagged — retries exhausted while a claw was still
	// provisioning/bootstrapping funnel through stopAgentTerminalWithReason,
	// which always records failure_type='agent_stopped' regardless of whether
	// the agent ever actually connected, so agent_stopped alone must never be
	// treated as proof of a real agent start.
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-agent-stopped-while-provisioning", AttemptID: "attempt-stopped-provisioning", ClawID: "claw-stopped-provisioning", TenantID: "test-tenant-id",
		Status: taskRunStatusFailed, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerFactory,
		FailureType: taskRunFailureAgentStopped, StartedAt: ts + 4000, FinishedAt: ts + 4500,
	})

	// Still provisioning, but a dashboard message was sent to it in the
	// meantime: must stay untouched. Human interactions can reach a claw
	// before it ever connects, so human_interaction_count alone isn't proof
	// the agent started either.
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-human-interaction-while-provisioning", AttemptID: "attempt-human-provisioning", ClawID: "claw-human-provisioning", TenantID: "test-tenant-id",
		Status: taskRunStatusRunning, Phase: taskRunPhaseProvisioning, OwnerType: taskRunOwnerFactory,
		StartedAt: ts + 5000, HumanInteractions: 1,
	})

	// NewTestServerWithConfig already ran the migration once on the empty
	// database (marking the sentinel applied). Clear it so this test can
	// exercise the migration against the fixtures seeded above.
	if _, err := db.Exec(`DELETE FROM hub_migrations WHERE name='task_run_agent_started_at_v1'`); err != nil {
		t.Fatalf("reset agent_started_at backfill sentinel: %v", err)
	}
	if err := backfillTaskRunAgentStartedAtV1(s.db); err != nil {
		t.Fatalf("backfill agent_started_at: %v", err)
	}

	assertAgentStartedAt := func(runID string, want int64) {
		t.Helper()
		var got int64
		if err := db.QueryRow(`SELECT agent_started_at FROM task_run_summaries WHERE run_id=?`, runID).Scan(&got); err != nil {
			t.Fatalf("read agent_started_at for %s: %v", runID, err)
		}
		if got != want {
			t.Fatalf("agent_started_at for %s = %d, want %d", runID, got, want)
		}
	}
	assertAgentStartedAt("run-pr-opened", ts+1000)
	assertAgentStartedAt("run-done-without-pr", ts+2000)
	assertAgentStartedAt("run-still-provisioning", 0)
	assertAgentStartedAt("run-agent-stopped-while-provisioning", 0)
	assertAgentStartedAt("run-human-interaction-while-provisioning", 0)
	assertTaskRunEventExists(t, db, "run-pr-opened", taskRunEventAgentStarted, taskRunInteractionNeutral)

	// Re-running the migration must be a no-op (sentinel row already applied)
	// and must not duplicate the agent_started event.
	if err := backfillTaskRunAgentStartedAtV1(s.db); err != nil {
		t.Fatalf("re-run backfill agent_started_at: %v", err)
	}
	var eventCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_run_events WHERE run_id='run-pr-opened' AND event_type=?`, taskRunEventAgentStarted).Scan(&eventCount); err != nil {
		t.Fatalf("count agent_started events: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("expected exactly one agent_started event after re-running the migration, got %d", eventCount)
	}
}

func TestBackfillTaskRunAgentStartedAtV1RetriesSkippedRunsOnNextStartup(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)

	// A candidate with no usable timestamp anywhere (started_at=0 and no
	// provision_started_at) makes backfillTaskRunAgentStartedAt fail — stands
	// in for any transient per-run failure (e.g. a busy DB) without needing
	// to break referential integrity to force it.
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-unbackfillable", AttemptID: "attempt-unbackfillable", ClawID: "claw-unbackfillable", TenantID: "test-tenant-id",
		Status: taskRunStatusFailed, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerFactory,
		FailureType: taskRunFailureNoPR, StartedAt: 0, FinishedAt: 1000,
	})
	if _, err := db.Exec(`DELETE FROM hub_migrations WHERE name='task_run_agent_started_at_v1'`); err != nil {
		t.Fatalf("reset agent_started_at backfill sentinel: %v", err)
	}

	if err := backfillTaskRunAgentStartedAtV1(s.db); err != nil {
		t.Fatalf("first backfill pass: %v", err)
	}
	var agentStartedAt, applied int64
	if err := db.QueryRow(`SELECT agent_started_at FROM task_run_summaries WHERE run_id='run-unbackfillable'`).Scan(&agentStartedAt); err != nil {
		t.Fatalf("read agent_started_at: %v", err)
	}
	if agentStartedAt != 0 {
		t.Fatalf("expected the unbackfillable run to stay unbackfilled, got agent_started_at=%d", agentStartedAt)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM hub_migrations WHERE name='task_run_agent_started_at_v1'`).Scan(&applied); err != nil {
		t.Fatalf("check sentinel: %v", err)
	}
	if applied != 0 {
		t.Fatal("expected the migration sentinel to stay unwritten while a run was skipped, so the next startup retries it")
	}

	// Fix the underlying condition (a real deploy would fix whatever caused
	// the transient failure) and simulate the next hub startup.
	if _, err := db.Exec(`UPDATE task_run_summaries SET started_at=1760000000000 WHERE run_id='run-unbackfillable'`); err != nil {
		t.Fatalf("fix started_at: %v", err)
	}
	if err := backfillTaskRunAgentStartedAtV1(s.db); err != nil {
		t.Fatalf("retry backfill pass: %v", err)
	}
	if err := db.QueryRow(`SELECT agent_started_at FROM task_run_summaries WHERE run_id='run-unbackfillable'`).Scan(&agentStartedAt); err != nil {
		t.Fatalf("read agent_started_at after retry: %v", err)
	}
	if agentStartedAt == 0 {
		t.Fatal("expected the retry to backfill the previously-skipped run")
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM hub_migrations WHERE name='task_run_agent_started_at_v1'`).Scan(&applied); err != nil {
		t.Fatalf("check sentinel after retry: %v", err)
	}
	if applied != 1 {
		t.Fatal("expected the migration sentinel to be written once every candidate succeeds")
	}
}

func insertClawUsingTriggerActorDefault(t *testing.T, db *sql.DB, clawID string) {
	t.Helper()

	if _, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, template, provider, status, created_at) VALUES(?,?,?,?,?,?,?)`, clawID, "tenant", "Claw", "", "docker", "provisioning", time.Now()); err != nil {
		t.Fatalf("insert claw: %v", err)
	}
}

func assertClawTriggerActorJSONIsValid(t *testing.T, db *sql.DB, clawID string) {
	t.Helper()

	var raw string
	if err := db.QueryRow(`SELECT trigger_actor_json FROM claws WHERE id=?`, clawID).Scan(&raw); err != nil {
		t.Fatalf("select trigger_actor_json: %v", err)
	}

	var actor map[string]any
	if err := json.Unmarshal([]byte(raw), &actor); err != nil {
		t.Fatalf("expected trigger_actor_json default to be valid JSON, got %q: %v", raw, err)
	}
}
