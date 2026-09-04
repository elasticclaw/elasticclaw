package hub

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func newClawRetryTestServer(t *testing.T, status string) (*Server, *sql.DB, string) {
	t.Helper()
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`INSERT INTO tenants(id,name,token,claw_token,created_at) VALUES(?,?,?,?,?)`,
		"tenant", "Tenant", "token", "claw-token", now()); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO claws(id,tenant_id,name,template,provider,status,bootstrap_ok,created_at)
		VALUES(?,?,?,?,?,?,?,?)`, "retry-claw", "tenant", "Retry Claw", "template", "noop", status, 1, now()); err != nil {
		t.Fatalf("insert claw: %v", err)
	}
	s := &Server{db: db, hubCfg: &types.HubConfig{}, claws: map[string]*clawConn{}, users: map[string]*userConn{}}
	runID, _, err := s.ensureTaskRunForClaw("retry-claw", TaskRunStart{
		RunKind: taskRunKindPRTask, OwnerType: taskRunOwnerFactory, AnalyticsEnabled: true, RequiresPR: true, Tags: []string{"retry-test"},
	})
	if err != nil {
		t.Fatalf("ensure task run: %v", err)
	}
	return s, db, runID
}

func newFileClawRetryTestServer(t *testing.T, status string) (*Server, *sql.DB, string) {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`INSERT INTO tenants(id,name,token,claw_token,created_at) VALUES(?,?,?,?,?)`,
		"tenant", "Tenant", "token", "claw-token", now()); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO claws(id,tenant_id,name,template,provider,status,bootstrap_ok,created_at)
		VALUES(?,?,?,?,?,?,?,?)`, "retry-claw", "tenant", "Retry Claw", "template", "noop", status, 1, now()); err != nil {
		t.Fatalf("insert claw: %v", err)
	}
	s := &Server{db: db, hubCfg: &types.HubConfig{}, claws: map[string]*clawConn{}, users: map[string]*userConn{}}
	runID, _, err := s.ensureTaskRunForClaw("retry-claw", TaskRunStart{
		RunKind: taskRunKindPRTask, OwnerType: taskRunOwnerFactory, AnalyticsEnabled: true, RequiresPR: true, Tags: []string{"retry-test"},
	})
	if err != nil {
		t.Fatalf("ensure task run: %v", err)
	}
	return s, db, runID
}

func TestPrepareClawRetryCreatesAttemptAndEmitsProviderLost(t *testing.T) {
	s, db, runID := newClawRetryTestServer(t, "connected")

	disposition, plan, err := s.prepareClawRetry("retry-claw", "Provider VM lost: HTTP 404 not found")
	if err != nil {
		t.Fatalf("prepare retry: %v", err)
	}
	if disposition != clawRetryScheduled || plan.attempt != 2 || plan.failureType != taskRunFailureProviderLost {
		t.Fatalf("unexpected retry plan: disposition=%v plan=%+v", disposition, plan)
	}

	var count int
	var currentAttemptID string
	if err := db.QueryRow(`SELECT attempt_count,current_attempt_id FROM task_runs WHERE id=?`, runID).Scan(&count, &currentAttemptID); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if count != 2 {
		t.Fatalf("attempt_count=%d, want 2", count)
	}
	var failedCount, runningCount, retryEvents int
	_ = db.QueryRow(`SELECT COUNT(*) FROM task_run_attempts WHERE run_id=? AND status='failed' AND failure_type='provider_lost'`, runID).Scan(&failedCount)
	_ = db.QueryRow(`SELECT COUNT(*) FROM task_run_attempts WHERE id=? AND status='running'`, currentAttemptID).Scan(&runningCount)
	_ = db.QueryRow(`SELECT COUNT(*) FROM task_run_events WHERE run_id=? AND event_key=?`, runID, "retry:retry-claw:2").Scan(&retryEvents)
	if failedCount != 1 || runningCount != 1 || retryEvents != 1 {
		t.Fatalf("failed=%d running=%d retryEvents=%d, want 1/1/1", failedCount, runningCount, retryEvents)
	}
}

func TestPrepareClawRetryStopsAtMaxAttemptsAndRejectsNonRetryable(t *testing.T) {
	t.Run("attempts exhausted", func(t *testing.T) {
		s, db, runID := newClawRetryTestServer(t, "connected")
		if _, err := db.Exec(`UPDATE task_runs SET attempt_count=? WHERE id=?`, maxClawAttempts, runID); err != nil {
			t.Fatal(err)
		}
		disposition, _, err := s.prepareClawRetry("retry-claw", "sandbox terminated unexpectedly")
		if err != nil || disposition != clawRetryNotApplicable {
			t.Fatalf("disposition=%v err=%v, want terminal", disposition, err)
		}
	})

	t.Run("non-retryable", func(t *testing.T) {
		s, _, _ := newClawRetryTestServer(t, "connected")
		disposition, _, err := s.prepareClawRetry("retry-claw", "provider is not configured")
		if err != nil || disposition != clawRetryNotApplicable {
			t.Fatalf("disposition=%v err=%v, want terminal", disposition, err)
		}
	})
}

func TestPrepareClawRetryFallsThroughForProtectedOrFinishedAttempt(t *testing.T) {
	t.Run("protected status", func(t *testing.T) {
		s, _, _ := newClawRetryTestServer(t, "idle")
		disposition, _, err := s.prepareClawRetry("retry-claw", "sandbox terminated unexpectedly")
		if err != nil || disposition != clawRetryNotApplicable {
			t.Fatalf("disposition=%v err=%v, want terminal", disposition, err)
		}
	})

	t.Run("finished attempt", func(t *testing.T) {
		s, db, runID := newClawRetryTestServer(t, "connected")
		if _, err := db.Exec(`UPDATE task_run_attempts SET status='succeeded' WHERE run_id=?`, runID); err != nil {
			t.Fatal(err)
		}
		disposition, _, err := s.prepareClawRetry("retry-claw", "sandbox terminated unexpectedly")
		if err != nil || disposition != clawRetryNotApplicable {
			t.Fatalf("disposition=%v err=%v, want terminal", disposition, err)
		}
	})

	t.Run("error already handled", func(t *testing.T) {
		s, _, _ := newClawRetryTestServer(t, "error")
		disposition, _, err := s.prepareClawRetry("retry-claw", "sandbox terminated unexpectedly")
		if err != nil || disposition != clawRetryAlreadyHandled {
			t.Fatalf("disposition=%v err=%v, want already handled", disposition, err)
		}
	})
}

func TestStopAgentProtectedStatusUsesTerminalPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, db, runID := newClawRetryTestServer(t, "idle")
	s.stopAgentWithReason("retry-claw", "sandbox terminated unexpectedly", false)

	var status string
	if err := db.QueryRow(`SELECT status FROM claws WHERE id='retry-claw'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "error" {
		t.Fatalf("status=%q, want error", status)
	}
	var stoppedEvents int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_run_events WHERE run_id=? AND event_key='agent_stopped:retry-claw'`, runID).Scan(&stoppedEvents); err != nil {
		t.Fatal(err)
	}
	if stoppedEvents != 1 {
		t.Fatalf("agent_stopped events=%d, want 1", stoppedEvents)
	}
}

func TestConcurrentPrepareClawRetryHasSingleWinner(t *testing.T) {
	s, _, _ := newFileClawRetryTestServer(t, "connected")
	start := make(chan struct{})
	dispositions := make(chan clawRetryDisposition, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			disposition, _, err := s.prepareClawRetry("retry-claw", "Provider VM lost: HTTP 404 not found")
			dispositions <- disposition
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(dispositions)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("prepare retry: %v", err)
		}
	}
	counts := map[clawRetryDisposition]int{}
	for disposition := range dispositions {
		counts[disposition]++
	}
	if counts[clawRetryScheduled] != 1 || counts[clawRetryAlreadyHandled] != 1 {
		t.Fatalf("dispositions=%v, want one scheduled and one already handled", counts)
	}
}

func TestResetClawForRetryRaceGuard(t *testing.T) {
	s, _, _ := newClawRetryTestServer(t, "error")
	reset, err := s.resetClawForRetry("tenant", "retry-claw", "", "retrying", "")
	if err != nil || !reset {
		t.Fatalf("first reset: reset=%v err=%v", reset, err)
	}
	reset, err = s.resetClawForRetry("tenant", "retry-claw", "", "retrying", "")
	if err != nil || reset {
		t.Fatalf("second reset: reset=%v err=%v, want no-op", reset, err)
	}
}

// A replacement sandbox runs a brand-new session, so the predecessor's spent
// auto-resume budget must not follow it: resetClawForRetry zeroes
// idle_resume_count in the same guarded UPDATE that arms rebrief_pending.
// idle_resume_at goes with it: the successor is a different session, and a
// latch the dead session earned can otherwise veto the freshly zeroed budget
// forever (see the assertion below).
func TestResetClawForRetryResetsIdleResumeBudget(t *testing.T) {
	s, db, _ := newClawRetryTestServer(t, "error")
	const latch = int64(1_700_000_000_000)
	if _, err := db.Exec(`UPDATE claws SET idle_resume_at=?, idle_resume_count=? WHERE id=?`, latch, agentIdleResumeMaxAttempts, "retry-claw"); err != nil {
		t.Fatalf("seed idle_resume state: %v", err)
	}
	reset, err := s.resetClawForRetry("tenant", "retry-claw", "", "retrying", "")
	if err != nil || !reset {
		t.Fatalf("reset: reset=%v err=%v", reset, err)
	}
	at, count := clawIdleResumeState(t, db, "retry-claw")
	if count != 0 {
		t.Fatalf("resetClawForRetry left idle_resume_count=%d, want 0", count)
	}
	// The latch goes too, and this is the half that is easy to get wrong. The
	// successor is a different session; on reconnect its lastTurnFinishedAt is
	// seeded from the last claw message, so its first idle stretch can anchor
	// within agentIdleStretchSlack of a latch the DEAD session earned. Leave
	// the latch and checkAgentIdleResume reads "already handled" forever — a
	// budget that was just zeroed and can never be spent.
	if at != 0 {
		t.Fatalf("resetClawForRetry left idle_resume_at=%d, want 0", at)
	}
}

func TestReplaceClawInstanceFailsDanglingAttemptWhenClawChangedState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, db, runID := newClawRetryTestServer(t, "connected")

	disposition, plan, err := s.prepareClawRetry("retry-claw", "Provider VM lost: HTTP 404 not found")
	if err != nil || disposition != clawRetryScheduled {
		t.Fatalf("prepare retry: disposition=%v err=%v", disposition, err)
	}
	// Simulate the claw leaving 'error' during the backoff, e.g. a manual restore.
	if _, err := db.Exec(`UPDATE claws SET status='idle' WHERE id='retry-claw'`); err != nil {
		t.Fatal(err)
	}

	if err := s.replaceClawInstance(context.Background(), "tenant", "retry-claw", "Provider VM lost: HTTP 404 not found", plan.attempt); err != nil {
		t.Fatalf("replace claw instance: %v", err)
	}

	var status, failureType string
	if err := db.QueryRow(`
		SELECT status, failure_type FROM task_run_attempts WHERE run_id=? AND attempt_number=?`,
		runID, plan.attempt).Scan(&status, &failureType); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || failureType != taskRunFailureUnknown {
		t.Fatalf("successor attempt status=%q failure_type=%q, want failed/unknown", status, failureType)
	}
}

func TestRetryCheckpointSkipsCheckpointUsedByPreviousAttempt(t *testing.T) {
	s, db, runID := newClawRetryTestServer(t, "connected")
	if _, err := db.Exec(`
		INSERT INTO claw_checkpoints(id,tenant_id,claw_id,status,manifest_path,root_tree_sha256,created_at)
		VALUES('checkpoint-x','tenant','retry-claw','ready','manifest.json','root-x',?)`, now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE task_run_attempts SET restored_checkpoint_id='checkpoint-x' WHERE run_id=? AND attempt_number=1`, runID); err != nil {
		t.Fatal(err)
	}
	checkpointID, err := s.retryCheckpointID("tenant", "retry-claw", 2)
	if err != nil {
		t.Fatal(err)
	}
	if checkpointID != "" {
		t.Fatalf("checkpoint=%q, want clean provision", checkpointID)
	}
}

func TestRetryCheckpointSkipsBootstrapCheckpointAfterProgress(t *testing.T) {
	insertBootstrapCheckpoint := func(t *testing.T, db *sql.DB) {
		t.Helper()
		if _, err := db.Exec(`
			INSERT INTO claw_checkpoints(id,tenant_id,claw_id,status,reason,manifest_path,root_tree_sha256,created_at)
			VALUES('checkpoint-bootstrap','tenant','retry-claw','ready','bootstrap','manifest.json','root-bootstrap',?)`, now()); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("registered PR forces clean provision", func(t *testing.T) {
		s, db, _ := newClawRetryTestServer(t, "connected")
		insertBootstrapCheckpoint(t, db)
		if _, err := db.Exec(`
			INSERT INTO claw_prs(id,claw_id,repo,pr_number,pr_url,created_at)
			VALUES('pr-1','retry-claw','acme/widgets',42,'https://github.com/acme/widgets/pull/42',?)`, now()); err != nil {
			t.Fatal(err)
		}
		checkpointID, err := s.retryCheckpointID("tenant", "retry-claw", 2)
		if err != nil {
			t.Fatal(err)
		}
		if checkpointID != "" {
			t.Fatalf("checkpoint=%q, want clean provision instead of bootstrap rollback", checkpointID)
		}
	})

	t.Run("pipeline past entry stage forces clean provision", func(t *testing.T) {
		s, db, _ := newClawRetryTestServer(t, "connected")
		insertBootstrapCheckpoint(t, db)
		s.hubCfg.Factories = []*types.FactoryConfig{{
			Name:         "retry-factory",
			PipelineYAML: "stages:\n  - id: plan\n    entry: true\n  - id: implement\n",
		}}
		if _, err := db.Exec(`UPDATE claws SET tags='["factory:retry-factory"]', pipeline_stage='implement' WHERE id='retry-claw'`); err != nil {
			t.Fatal(err)
		}
		checkpointID, err := s.retryCheckpointID("tenant", "retry-claw", 2)
		if err != nil {
			t.Fatal(err)
		}
		if checkpointID != "" {
			t.Fatalf("checkpoint=%q, want clean provision instead of bootstrap rollback", checkpointID)
		}
	})

	t.Run("older non-bootstrap checkpoint is restored instead of bootstrap", func(t *testing.T) {
		s, db, _ := newClawRetryTestServer(t, "connected")
		insertBootstrapCheckpoint(t, db)
		// A periodic checkpoint from an earlier attempt sorts below the
		// successor's bootstrap checkpoint but holds real work — it must win.
		if _, err := db.Exec(`
			INSERT INTO claw_checkpoints(id,tenant_id,claw_id,status,reason,manifest_path,root_tree_sha256,created_at)
			VALUES('checkpoint-periodic','tenant','retry-claw','ready','periodic','manifest.json','root-periodic',?)`, now().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		checkpointID, err := s.retryCheckpointID("tenant", "retry-claw", 2)
		if err != nil {
			t.Fatal(err)
		}
		if checkpointID != "checkpoint-periodic" {
			t.Fatalf("checkpoint=%q, want checkpoint-periodic", checkpointID)
		}
	})

	t.Run("fallback never re-restores the previous attempt's checkpoint", func(t *testing.T) {
		s, db, runID := newClawRetryTestServer(t, "connected")
		insertBootstrapCheckpoint(t, db)
		if _, err := db.Exec(`
			INSERT INTO claw_checkpoints(id,tenant_id,claw_id,status,reason,manifest_path,root_tree_sha256,created_at)
			VALUES('checkpoint-periodic','tenant','retry-claw','ready','periodic','manifest.json','root-periodic',?)`, now().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE task_run_attempts SET restored_checkpoint_id='checkpoint-periodic' WHERE run_id=? AND attempt_number=1`, runID); err != nil {
			t.Fatal(err)
		}
		checkpointID, err := s.retryCheckpointID("tenant", "retry-claw", 2)
		if err != nil {
			t.Fatal(err)
		}
		if checkpointID != "" {
			t.Fatalf("checkpoint=%q, want clean provision", checkpointID)
		}
	})

	t.Run("no progress keeps bootstrap restore", func(t *testing.T) {
		s, db, _ := newClawRetryTestServer(t, "connected")
		insertBootstrapCheckpoint(t, db)
		s.hubCfg.Factories = []*types.FactoryConfig{{
			Name:         "retry-factory",
			PipelineYAML: "stages:\n  - id: plan\n    entry: true\n  - id: implement\n",
		}}
		// Entry stage set right after initialization is not progress.
		if _, err := db.Exec(`UPDATE claws SET tags='["factory:retry-factory"]', pipeline_stage='plan' WHERE id='retry-claw'`); err != nil {
			t.Fatal(err)
		}
		checkpointID, err := s.retryCheckpointID("tenant", "retry-claw", 2)
		if err != nil {
			t.Fatal(err)
		}
		if checkpointID != "checkpoint-bootstrap" {
			t.Fatalf("checkpoint=%q, want checkpoint-bootstrap", checkpointID)
		}
	})
}

func TestRetryCheckpointFallsBackPastBlockedNewest(t *testing.T) {
	s, db, runID := newClawRetryTestServer(t, "connected")
	createdAt := now()
	for _, checkpoint := range []struct {
		id        string
		createdAt time.Time
	}{
		{"checkpoint-newest", createdAt},
		{"checkpoint-mid", createdAt.Add(-time.Hour)},
		{"checkpoint-old", createdAt.Add(-2 * time.Hour)},
	} {
		if _, err := db.Exec(`
			INSERT INTO claw_checkpoints(id,tenant_id,claw_id,status,manifest_path,root_tree_sha256,created_at)
			VALUES(?,?,?,'ready','manifest.json',?,?)`, checkpoint.id, "tenant", "retry-claw", "root-"+checkpoint.id, checkpoint.createdAt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE task_run_attempts SET restored_checkpoint_id='checkpoint-newest' WHERE run_id=? AND attempt_number=1`, runID); err != nil {
		t.Fatal(err)
	}

	checkpointID, err := s.retryCheckpointID("tenant", "retry-claw", 2)
	if err != nil {
		t.Fatal(err)
	}
	if checkpointID != "checkpoint-mid" {
		t.Fatalf("checkpoint=%q, want checkpoint-mid", checkpointID)
	}
}

func TestRetryCheckpointOnlyMetadataOnlyCheckpointsYieldsScratch(t *testing.T) {
	s, db, _ := newClawRetryTestServer(t, "connected")
	for _, checkpoint := range []struct {
		id        string
		createdAt time.Time
	}{
		{"checkpoint-newest", now()},
		{"checkpoint-old", now().Add(-time.Hour)},
	} {
		if _, err := db.Exec(`
			INSERT INTO claw_checkpoints(id,tenant_id,claw_id,status,manifest_path,root_tree_sha256,created_at)
			VALUES(?,?,?,'ready','manifest.json','',?)`, checkpoint.id, "tenant", "retry-claw", checkpoint.createdAt); err != nil {
			t.Fatal(err)
		}
	}

	checkpointID, err := s.retryCheckpointID("tenant", "retry-claw", 2)
	if err != nil {
		t.Fatal(err)
	}
	if checkpointID != "" {
		t.Fatalf("checkpoint=%q, want clean provision", checkpointID)
	}
}

func TestRetryCheckpointSkipsMetadataOnlyCheckpointForOlderRealCheckpoint(t *testing.T) {
	s, db, _ := newClawRetryTestServer(t, "connected")
	if _, err := db.Exec(`
		INSERT INTO claw_checkpoints(id,tenant_id,claw_id,status,manifest_path,root_tree_sha256,created_at)
		VALUES('checkpoint-metadata','tenant','retry-claw','ready','manifest.json','',?)`, now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO claw_checkpoints(id,tenant_id,claw_id,status,manifest_path,root_tree_sha256,created_at)
		VALUES('checkpoint-real','tenant','retry-claw','ready','manifest.json','root-real',?)`, now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	checkpointID, err := s.retryCheckpointID("tenant", "retry-claw", 2)
	if err != nil {
		t.Fatal(err)
	}
	if checkpointID != "checkpoint-real" {
		t.Fatalf("checkpoint=%q, want checkpoint-real", checkpointID)
	}
}

func TestRetryCheckpointChoicePrecedesTerminationCheckpoint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, db, runID := newClawRetryTestServer(t, "error")
	if _, err := db.Exec(`
		INSERT INTO claw_checkpoints(id,tenant_id,claw_id,status,manifest_path,root_tree_sha256,created_at)
		VALUES('checkpoint-x','tenant','retry-claw','ready','manifest.json','root-x',?)`, now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE task_run_attempts SET attempt_number=2, restored_checkpoint_id='checkpoint-x' WHERE run_id=?`, runID); err != nil {
		t.Fatal(err)
	}

	checkpointID, _, err := s.retryCheckpointBeforeTermination("tenant", "retry-claw", 3)
	if err != nil {
		t.Fatal(err)
	}
	if checkpointID != "" {
		t.Fatalf("checkpoint=%q, want clean provision", checkpointID)
	}
	var terminationCheckpoints int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claw_checkpoints WHERE claw_id=? AND reason='termination:automatic-retry' AND status='ready'`, "retry-claw").Scan(&terminationCheckpoints); err != nil {
		t.Fatal(err)
	}
	if terminationCheckpoints != 1 {
		t.Fatalf("termination checkpoints=%d, want 1", terminationCheckpoints)
	}
}

func TestHealthEscalationThresholds(t *testing.T) {
	if heartbeatShouldEscalate(defaultGatewayUnhealthyMax-1, defaultGatewayUnhealthyMax, "connected", true) {
		t.Fatal("heartbeat escalated before threshold")
	}
	if !heartbeatShouldEscalate(defaultGatewayUnhealthyMax, defaultGatewayUnhealthyMax, "connected", true) {
		t.Fatal("heartbeat did not escalate at threshold")
	}
	if heartbeatShouldEscalate(defaultGatewayUnhealthyMax, defaultGatewayUnhealthyMax, "provisioning", true) || heartbeatShouldEscalate(defaultGatewayUnhealthyMax, defaultGatewayUnhealthyMax, "connected", false) {
		t.Fatal("heartbeat escalated an ineligible claw")
	}

	nowAt := time.Now()
	lastActivity := nowAt.Add(-11 * time.Minute)
	if got := watchdogAction(nowAt, "connected", true, true, lastActivity, lastActivity, lastActivity, time.Time{}, defaultSilentDeathMax); got != watchdogHealthWarn {
		t.Fatalf("initial watchdog action=%v, want warn", got)
	}
	if got := watchdogAction(nowAt, "connected", true, true, lastActivity, lastActivity, lastActivity, nowAt.Add(-5*time.Minute), defaultSilentDeathMax); got != watchdogHealthEscalate {
		t.Fatalf("continued watchdog action=%v, want escalate", got)
	}
	if got := watchdogAction(nowAt, "provisioning", true, true, lastActivity, lastActivity, lastActivity, nowAt.Add(-5*time.Minute), defaultSilentDeathMax); got != watchdogHealthNone {
		t.Fatalf("provisioning watchdog action=%v, want none", got)
	}
}

func TestProviderLostClassificationAndFailureType(t *testing.T) {
	failure := classifyAgentFailure("Provider VM lost: replicated VM no longer exists")
	if failure.Kind != agentFailureProviderLost {
		t.Fatalf("kind=%q, want %q", failure.Kind, agentFailureProviderLost)
	}
	if got := taskRunFailureTypeForAgentFailure(failure.Kind); got != taskRunFailureProviderLost {
		t.Fatalf("failure type=%q, want %q", got, taskRunFailureProviderLost)
	}
}

func TestRetryOperationUsesDelaysAndSanitizesFinalError(t *testing.T) {
	var delays []time.Duration
	err := retryOperation(retryOptions{
		Label: "replace", Attempts: 2, Delays: []time.Duration{0, 30 * time.Second},
		Sleep: func(delay time.Duration) { delays = append(delays, delay) },
		Run:   func() error { return errors.New("failed API_TOKEN=super-secret") },
	})
	if len(delays) != 1 || delays[0] != 30*time.Second {
		t.Fatalf("delays=%v, want [30s]", delays)
	}
	if err == nil || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("expected sanitized final error, got %v", err)
	}
}

func TestResolveClawStopDisposition(t *testing.T) {
	t.Run("indeterminate then scheduled", func(t *testing.T) {
		var slept []time.Duration
		results := []clawRetryDisposition{clawRetryIndeterminate, clawRetryIndeterminate, clawRetryScheduled}
		calls := 0
		got := resolveClawStopDisposition(func() clawRetryDisposition {
			calls++
			return results[calls-1]
		}, func(delay time.Duration) { slept = append(slept, delay) })
		if got != clawRetryScheduled {
			t.Fatalf("disposition=%v, want scheduled", got)
		}
		if len(slept) != 2 || slept[0] != clawStopRevaluationDelays[0] || slept[1] != clawStopRevaluationDelays[1] {
			t.Fatalf("slept=%v, want %v", slept, clawStopRevaluationDelays)
		}
	})

	t.Run("always indeterminate falls through to terminal", func(t *testing.T) {
		calls := 0
		got := resolveClawStopDisposition(func() clawRetryDisposition {
			calls++
			return clawRetryIndeterminate
		}, func(time.Duration) {})
		if got != clawRetryNotApplicable {
			t.Fatalf("disposition=%v, want notApplicable", got)
		}
		if calls != 3 {
			t.Fatalf("evaluations=%d, want 3", calls)
		}
	})

	t.Run("already handled returns immediately", func(t *testing.T) {
		var slept []time.Duration
		calls := 0
		got := resolveClawStopDisposition(func() clawRetryDisposition {
			calls++
			return clawRetryAlreadyHandled
		}, func(delay time.Duration) { slept = append(slept, delay) })
		if got != clawRetryAlreadyHandled {
			t.Fatalf("disposition=%v, want alreadyHandled", got)
		}
		if calls != 1 || len(slept) != 0 {
			t.Fatalf("calls=%d slept=%v, want 1 evaluation and no sleeps", calls, slept)
		}
	})
}

// A bridge sending healthy heartbeats is alive, whatever the status channel is
// doing. Before this, a lost status channel froze lastStatusAt and the watchdog
// replaced claws whose bridge was reporting in every 15 seconds.
func TestWatchdogActionTrustsHeartbeatOverStaleStatusChannel(t *testing.T) {
	nowAt := time.Now()
	staleStatus := nowAt.Add(-20 * time.Minute)
	freshHeartbeat := nowAt.Add(-15 * time.Second)
	warnedLongAgo := nowAt.Add(-10 * time.Minute)

	if got := watchdogAction(nowAt, "connected", true, true, staleStatus, freshHeartbeat, staleStatus, warnedLongAgo, defaultSilentDeathMax); got != watchdogHealthNone {
		t.Errorf("got %v, want none: a fresh heartbeat is proof of life even with a dead status channel", got)
	}

	// With both signals stale the claw really is silent and must still escalate.
	if got := watchdogAction(nowAt, "connected", true, true, staleStatus, staleStatus, staleStatus, warnedLongAgo, defaultSilentDeathMax); got != watchdogHealthEscalate {
		t.Errorf("got %v, want escalate: nothing has reported in for 20 minutes", got)
	}

	// A stale heartbeat must not rescue a claw whose status channel is fresher.
	freshStatus := nowAt.Add(-30 * time.Second)
	if got := watchdogAction(nowAt, "connected", true, true, freshStatus, staleStatus, freshStatus, warnedLongAgo, defaultSilentDeathMax); got != watchdogHealthNone {
		t.Errorf("got %v, want none: the status channel is answering", got)
	}
}
