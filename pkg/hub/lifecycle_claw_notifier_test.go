package hub

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/notify"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// insertSlackTestClaw inserts a claw whose created_at is age in the past.
// An empty taskRunID makes it ad-hoc from the claw pass's point of view (once it is
// older than the grace period).
func insertSlackTestClaw(t *testing.T, db *sql.DB, id, status string, bootstrapOK int, taskRunID string, age time.Duration) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO claws(id, tenant_id, name, status, bootstrap_ok, task_run_id, created_at)
		VALUES(?,?,?,?,?,?,?)`,
		id, "test-tenant-id", "name-"+id, status, bootstrapOK, taskRunID, time.Now().UTC().Add(-age))
	if err != nil {
		t.Fatalf("insert claw %s: %v", id, err)
	}
}

func insertSlackTestClawPR(t *testing.T, db *sql.DB, id, clawID, repo string, prNumber int, prURL string) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO claw_prs(id, claw_id, repo, pr_number, pr_url, created_at)
		VALUES(?,?,?,?,?,?)`,
		id, clawID, repo, prNumber, prURL, time.Now().UTC())
	if err != nil {
		t.Fatalf("insert claw_pr %s: %v", id, err)
	}
}

// setLifecycleClawBaseline positions the claw pass as "enabled with empty
// history", mirroring what setSlackWatermark does for the task-run pass.
func setLifecycleClawBaseline(t *testing.T, s *Server) {
	t.Helper()
	s.setNotifierStateInt64(lifecycleStateClawBaselineKey, 1)
	// Multi-route configs also carry a per-route baseline (a route added later
	// must not replay the current claw list into its channel); mark the
	// configured routes as already baselined so the helper keeps meaning
	// "enabled with empty history".
	if cfg := s.notificationsConfig(); cfg != nil && cfg.Lifecycle != nil {
		for _, route := range cfg.Lifecycle.EffectiveRoutes() {
			s.setNotifierStateInt64(lifecycleClawRouteBaselineKey(strings.TrimSpace(route.Via)), 1)
		}
	}
}

// oldEnough is safely past the ad-hoc grace period.
const oldEnough = lifecycleClawAdhocGrace + 8*time.Minute

// insertSlackTestWorkflowRun inserts a workflow run for a claw, as the
// pipeline-terminal and factory-trigger paths leave behind.
func insertSlackTestWorkflowRun(t *testing.T, db *sql.DB, id, clawID, status string, createdAt time.Time) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO workflow_runs(id, tenant_id, workflow_name, workspace_name, status, claw_id, created_at)
		VALUES(?,?,?,?,?,?,?)`,
		id, "test-tenant-id", "wf", "ws", status, clawID, createdAt)
	if err != nil {
		t.Fatalf("insert workflow run %s: %v", id, err)
	}
}

func TestLifecycleClawNotifierAdhocLifecyclePostsTopLevel(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	setSlackWatermark(t, s, 0)
	setLifecycleClawBaseline(t, s)

	insertSlackTestClaw(t, db, "claw-adhoc", "connected", 1, "", oldEnough)

	// agent started
	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("agent_started not sent for ad-hoc claw: %d messages", fake.count())
	}
	root := fake.request(0)
	if root.ThreadTS != "" {
		t.Fatalf("first claw message must be top-level, got thread_ts %q", root.ThreadTS)
	}
	if !strings.Contains(root.Fallback, "Agent started") || !strings.Contains(root.Fallback, "name-claw-adhoc") {
		t.Fatalf("agent_started fallback = %q", root.Fallback)
	}
	if root.Color != notify.SlackColorInfo {
		t.Fatalf("agent_started color = %q, want %q", root.Color, notify.SlackColorInfo)
	}

	// PR opened
	insertSlackTestClawPR(t, db, "pr-1", "claw-adhoc", "acme/app", 7, "https://github.com/acme/app/pull/7")
	s.lifecycleNotifierTick()
	if fake.count() != 2 {
		t.Fatalf("pr_opened not sent for ad-hoc claw: %d messages", fake.count())
	}
	prMsg := fake.request(1)
	if !strings.Contains(prMsg.Fallback, "PR opened") || !strings.Contains(prMsg.Fallback, "acme/app#7") {
		t.Fatalf("pr_opened fallback = %q", prMsg.Fallback)
	}
	if prMsg.Color != notify.SlackColorSuccess {
		t.Fatalf("pr_opened color = %q, want %q", prMsg.Color, notify.SlackColorSuccess)
	}

	// failure
	if _, err := db.Exec(`UPDATE claws SET status='error', bootstrap_diagnostic='agent crashed hard' WHERE id='claw-adhoc'`); err != nil {
		t.Fatalf("set claw error: %v", err)
	}
	s.lifecycleNotifierTick()
	if fake.count() != 3 {
		t.Fatalf("failure not sent for ad-hoc claw: %d messages", fake.count())
	}
	failMsg := fake.request(2)
	if !strings.Contains(failMsg.Fallback, "Agent died") {
		t.Fatalf("failure fallback = %q", failMsg.Fallback)
	}
	if failMsg.Color != notify.SlackColorError {
		t.Fatalf("failure color = %q, want %q", failMsg.Color, notify.SlackColorError)
	}
	var reasonSeen bool
	for _, b := range failMsg.Blocks {
		if text, ok := b["text"].(map[string]any); ok {
			if str, _ := text["text"].(string); strings.Contains(str, "agent crashed hard") {
				reasonSeen = true
			}
		}
	}
	if !reasonSeen {
		t.Fatal("failure message does not include bootstrap_diagnostic as the reason")
	}

	if prMsg.ThreadTS != "" || failMsg.ThreadTS != "" {
		t.Fatalf("claw lifecycle messages must be top-level: pr=%q fail=%q", prMsg.ThreadTS, failMsg.ThreadTS)
	}
	var threadRows int
	if err := db.QueryRow(`SELECT COUNT(1) FROM slack_run_threads`).Scan(&threadRows); err != nil || threadRows != 0 {
		t.Fatalf("claw lifecycle notifier wrote %d thread rows (err %v), want 0", threadRows, err)
	}

	// Deliveries recorded under the namespaced synthetic keys.
	for _, key := range []string{
		lifecycleClawStartedKey("claw-adhoc"),
		lifecycleClawPRKey("claw-adhoc", "https://github.com/acme/app/pull/7"),
		lifecycleClawFailureKey("claw-adhoc"),
	} {
		if status, ok := slackDeliveryStatus(t, db, key); !ok || status != notificationDeliveryStatusSent {
			t.Fatalf("delivery %s = %q, %v", key, status, ok)
		}
	}
}

func TestLifecycleClawNotifierIgnoresTaskRunClaws(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	setSlackWatermark(t, s, 0)
	setLifecycleClawBaseline(t, s)

	// A claw owned by a task run must never produce claw-path messages, even
	// in notifiable states and with a claw_prs row.
	insertSlackTestClaw(t, db, "claw-run", "connected", 1, "run-1", oldEnough)
	insertSlackTestClawPR(t, db, "pr-1", "claw-run", "acme/app", 7, "https://github.com/acme/app/pull/7")
	s.lifecycleNotifierTick()
	if fake.count() != 0 {
		t.Fatalf("claw path sent %d messages for a task-run claw", fake.count())
	}
	if _, err := db.Exec(`UPDATE claws SET status='error', bootstrap_diagnostic='boom' WHERE id='claw-run'`); err != nil {
		t.Fatalf("set claw error: %v", err)
	}
	s.lifecycleNotifierTick()
	if fake.count() != 0 {
		t.Fatalf("claw path sent %d messages for a failed task-run claw", fake.count())
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(1) FROM slack_notification_deliveries WHERE event_id LIKE 'claw:%' AND status != ?`, notificationDeliveryStatusSkipped).Scan(&n); err != nil || n != 0 {
		t.Fatalf("claw path recorded %d deliveries for a task-run claw (err %v)", n, err)
	}
}

// The section-B race: a task-run claw is briefly visible with an empty task_run_id
// because ensureTaskRunForClaw attaches the run shortly after the claw row is
// inserted. The grace period must keep the claw pass away until ownership is
// settled, so the agent produces exactly one notification (from the task-run
// path), not two.
func TestLifecycleClawNotifierGraceAvoidsDoubleSendOnLateTaskRunAttach(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	setSlackWatermark(t, s, 0)
	setLifecycleClawBaseline(t, s)

	// Freshly created claw: task_run_id not populated yet, already connected.
	insertSlackTestClaw(t, db, "claw-fresh", "connected", 1, "", 0)
	s.lifecycleNotifierTick()
	if fake.count() != 0 {
		t.Fatalf("claw pass notified a claw younger than the grace period: %d messages", fake.count())
	}

	// The task run attaches, and the task-run path emits its own event.
	if _, err := db.Exec(`UPDATE claws SET task_run_id='run-1' WHERE id='claw-fresh'`); err != nil {
		t.Fatalf("attach task run: %v", err)
	}
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-1", AttemptID: "attempt-run-1", ClawID: "claw-fresh", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory,
		Factory: "bugfix", Repo: "acme/app", StartedAt: epochMillis(now()) - 1000, IssueTitle: "Fix login bug",
	})
	insertSlackTestEvent(t, db, "ev-started", "run-1", taskRunEventAgentStarted, epochMillis(now()), "", "", "")
	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("want exactly one notification total for the racing claw, got %d", fake.count())
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(1) FROM slack_notification_deliveries WHERE event_id LIKE 'claw:%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("claw path recorded %d deliveries for a claw ceded to the task-run path (err %v)", n, err)
	}
}

// Defense-in-depth for the same race: even when a claw was already selected as
// an ad-hoc candidate, the pre-send re-check must cede it to the task-run path
// once task_run_id is populated.
func TestLifecycleClawNotifierPreSendRecheckCedesToTaskRunPath(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	setLifecycleClawBaseline(t, s)

	insertSlackTestClaw(t, db, "claw-x", "connected", 1, "", oldEnough)
	d := testLifecycleDelivery(t, s)
	route := d.effectiveRoutes()[0]
	claws, err := s.selectLifecycleClawStateCandidates(lifecycleClawKindStarted, route.notifier)
	if err != nil || len(claws) != 1 {
		t.Fatalf("candidates = %d, err %v; want 1 candidate", len(claws), err)
	}

	// The task run attaches between selection and send.
	if _, err := db.Exec(`UPDATE claws SET task_run_id='run-1' WHERE id='claw-x'`); err != nil {
		t.Fatalf("attach task run: %v", err)
	}

	ev, deliveryKey := lifecycleClawStateEvent(lifecycleClawKindStarted, claws[0])
	if !s.deliverLifecycleClawEvent(d, route, claws[0], ev, lifecycleClawRunContext(claws[0]), deliveryKey) {
		t.Fatal("ceding to the task-run path must count as handled")
	}
	if fake.count() != 0 {
		t.Fatalf("re-check failed: %d messages sent for a claw that acquired a task run", fake.count())
	}
	if _, delivered := slackDeliveryStatus(t, db, deliveryKey); delivered {
		t.Fatal("ceded claw must not get a delivery row (the task-run path owns its keys)")
	}
}

func TestLifecycleClawNotifierFirstEnableDoesNotReplay(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)

	// Pre-existing history: a connected ad-hoc claw with a PR, and a failed one.
	insertSlackTestClaw(t, db, "claw-old", "connected", 1, "", oldEnough)
	insertSlackTestClawPR(t, db, "pr-old", "claw-old", "acme/app", 1, "https://github.com/acme/app/pull/1")
	insertSlackTestClaw(t, db, "claw-dead", "error", 0, "", oldEnough)

	// First tick initializes both cursors; nothing is replayed then or later.
	s.lifecycleNotifierTick()
	s.lifecycleNotifierTick()
	if fake.count() != 0 {
		t.Fatalf("first enable replayed pre-existing claw history: %d messages", fake.count())
	}

	// Genuinely new activity after enabling is delivered.
	insertSlackTestClaw(t, db, "claw-new", "connected", 1, "", oldEnough)
	insertSlackTestClawPR(t, db, "pr-new", "claw-new", "acme/app", 9, "https://github.com/acme/app/pull/9")
	s.lifecycleNotifierTick()
	if fake.count() != 2 {
		t.Fatalf("new claw activity after enable was not delivered: %d messages", fake.count())
	}
}

func TestLifecycleClawNotifierDedupesRescans(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	setSlackWatermark(t, s, 0)
	setLifecycleClawBaseline(t, s)

	insertSlackTestClaw(t, db, "claw-adhoc", "connected", 1, "", oldEnough)
	insertSlackTestClawPR(t, db, "pr-1", "claw-adhoc", "acme/app", 7, "https://github.com/acme/app/pull/7")

	s.lifecycleNotifierTick()
	if fake.count() != 2 {
		t.Fatalf("first tick sent %d messages, want agent_started + pr_opened", fake.count())
	}
	// The same state (and the same claw_prs rows — the PR pass has no cursor)
	// is scanned again: the delivery rows must prevent duplicates.
	s.lifecycleNotifierTick()
	if fake.count() != 2 {
		t.Fatalf("re-scan duplicated claw events: %d messages", fake.count())
	}
}

func TestLifecycleClawNotifierRespectsTogglesAndParksThem(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, func(cfg *types.LifecycleNotificationsConfig) {
		cfg.Events = &types.LifecycleEventToggles{
			AgentStarted: boolPtr(false),
			PROpened:     boolPtr(false),
		}
	})
	setSlackWatermark(t, s, 0)
	setLifecycleClawBaseline(t, s)

	insertSlackTestClaw(t, db, "claw-adhoc", "connected", 1, "", oldEnough)
	insertSlackTestClawPR(t, db, "pr-1", "claw-adhoc", "acme/app", 7, "https://github.com/acme/app/pull/7")
	s.lifecycleNotifierTick()
	if fake.count() != 0 {
		t.Fatalf("disabled toggles still sent %d claw messages", fake.count())
	}

	// Re-enabling must not flush the muted window: the connected claw and its
	// PR were parked while the toggles were off.
	s.mu.Lock()
	s.hubCfg.Notifications.Lifecycle.Events = nil
	s.mu.Unlock()
	s.lifecycleNotifierTick()
	if fake.count() != 0 {
		t.Fatalf("re-enabling toggles flushed %d parked claw events", fake.count())
	}

	// Failures stayed enabled the whole time and must still work.
	if _, err := db.Exec(`UPDATE claws SET status='error', bootstrap_diagnostic='boom' WHERE id='claw-adhoc'`); err != nil {
		t.Fatalf("set claw error: %v", err)
	}
	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("failure after re-enable was not delivered: %d messages", fake.count())
	}
}

func TestLifecycleClawNotifierFailureToggleOff(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, func(cfg *types.LifecycleNotificationsConfig) {
		cfg.Events = &types.LifecycleEventToggles{Failures: boolPtr(false)}
	})
	setSlackWatermark(t, s, 0)
	setLifecycleClawBaseline(t, s)

	insertSlackTestClaw(t, db, "claw-dead", "error", 0, "", oldEnough)
	s.lifecycleNotifierTick()
	if fake.count() != 0 {
		t.Fatalf("failures toggle off, but claw path sent %d messages", fake.count())
	}
}

// Task-run and ad-hoc claw events must all remain independently visible at
// the channel top level.
func TestLifecycleClawAndTaskRunEventsAreTopLevel(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	setSlackWatermark(t, s, 0)
	setLifecycleClawBaseline(t, s)

	base := epochMillis(now())
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-1", AttemptID: "attempt-run-1", ClawID: "claw-run", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory,
		Factory: "bugfix", Repo: "acme/app", StartedAt: base - 1000,
	})
	insertSlackTestClaw(t, db, "claw-run", "connected", 1, "run-1", oldEnough)
	insertSlackTestEvent(t, db, "ev-run-started", "run-1", taskRunEventAgentStarted, base+10, "", "", "")
	insertSlackTestEvent(t, db, "ev-run-pr", "run-1", taskRunEventPROpened, base+20,
		"https://github.com/acme/app/pull/7", "", `{"repo":"acme/app","prNumber":7}`)

	insertSlackTestClaw(t, db, "claw-adhoc", "connected", 1, "", oldEnough)
	insertSlackTestClawPR(t, db, "pr-adhoc", "claw-adhoc", "acme/tools", 3, "https://github.com/acme/tools/pull/3")

	s.lifecycleNotifierTick()
	if fake.count() != 4 {
		t.Fatalf("sent %d messages, want 2 task-run + 2 claw", fake.count())
	}

	for i := 0; i < fake.count(); i++ {
		req := fake.request(i)
		if req.ThreadTS != "" {
			t.Fatalf("message %d posted in thread %q, want top-level", i, req.ThreadTS)
		}
	}
	var threadRows int
	if err := db.QueryRow(`SELECT COUNT(1) FROM slack_run_threads`).Scan(&threadRows); err != nil || threadRows != 0 {
		t.Fatalf("lifecycle notifier wrote %d thread rows (err %v), want 0", threadRows, err)
	}
}

// Regression: agent_started must not require bootstrap_ok=1 for providers that
// never set it. Only the VM bootstrap flows (daytona/replicated/exedev) call
// markBootstrapReady; docker/local/noop claws are upserted straight to
// 'connected' by bridge registration with bootstrap_ok still 0 — exactly the
// `make dev-claw` shape this feature was built for.
func TestLifecycleClawNotifierStartedFiresForNonVMProvidersWithoutBootstrapOK(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	setSlackWatermark(t, s, 0)
	setLifecycleClawBaseline(t, s)

	// make dev-claw: provider 'docker', connected via registration, bootstrap_ok=0.
	insertSlackTestClaw(t, db, "claw-docker", "connected", 0, "", oldEnough)
	if _, err := db.Exec(`UPDATE claws SET provider='docker' WHERE id='claw-docker'`); err != nil {
		t.Fatalf("set provider: %v", err)
	}
	// A VM-provider claw stays gated on bootstrap_ok.
	insertSlackTestClaw(t, db, "claw-vm", "connected", 0, "", oldEnough)
	if _, err := db.Exec(`UPDATE claws SET provider='daytona' WHERE id='claw-vm'`); err != nil {
		t.Fatalf("set provider: %v", err)
	}

	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("want exactly the docker claw's agent_started, got %d messages", fake.count())
	}
	if !strings.Contains(fake.request(0).Fallback, "name-claw-docker") {
		t.Fatalf("agent_started sent for the wrong claw: %q", fake.request(0).Fallback)
	}
	if status, ok := slackDeliveryStatus(t, db, lifecycleClawStartedKey("claw-docker")); !ok || status != notificationDeliveryStatusSent {
		t.Fatalf("docker claw delivery = %q, %v", status, ok)
	}
	if _, ok := slackDeliveryStatus(t, db, lifecycleClawStartedKey("claw-vm")); ok {
		t.Fatal("VM claw with bootstrap_ok=0 must stay gated")
	}

	// Once the VM claw finishes bootstrap it notifies too.
	if _, err := db.Exec(`UPDATE claws SET bootstrap_ok=1 WHERE id='claw-vm'`); err != nil {
		t.Fatalf("set bootstrap_ok: %v", err)
	}
	s.lifecycleNotifierTick()
	if fake.count() != 2 {
		t.Fatalf("bootstrapped VM claw did not notify: %d messages", fake.count())
	}
}

// Regression: the grace period must be sized to the insert-to-attach window it
// defends against, not swallow short-lived states. An ad-hoc claw that errors
// shortly after creation (and is soft-deleted by the operator soon after) must
// still produce its failure notification.
func TestLifecycleClawNotifierShortLivedFailureStillNotifies(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	setSlackWatermark(t, s, 0)
	setLifecycleClawBaseline(t, s)

	// Failed 30s after creation — inside the old 2-minute grace that used to
	// drop this event, past the current one.
	insertSlackTestClaw(t, db, "claw-shortlived", "error", 0, "", 30*time.Second)
	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("short-lived failure not notified: %d messages", fake.count())
	}

	// The operator deletes the errored claw from the dashboard (unconditional
	// status overwrite); the already-sent failure must not resend.
	if _, err := db.Exec(`UPDATE claws SET status='deleted' WHERE id='claw-shortlived'`); err != nil {
		t.Fatalf("delete claw: %v", err)
	}
	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("failure resent after delete: %d messages", fake.count())
	}
}

// Regression: the pipeline-terminal and factory-trigger failure paths record
// failures as finishClawTerminalTx(claw, "deleted", "", "failed", ...) — claw
// status 'deleted' with the workflow run 'failed' — and never pass through
// status='error'. The claw pass must notify on that shape while keeping the
// success paths (also 'deleted', run 'completed') silent.
func TestLifecycleClawNotifierFailureOnDeletedClawWithFailedWorkflowRun(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	setSlackWatermark(t, s, 0)
	setLifecycleClawBaseline(t, s)

	base := time.Now().UTC()
	// Pipeline terminal failure: deleted + latest run failed -> notify.
	insertSlackTestClaw(t, db, "claw-pipeline", "deleted", 1, "", oldEnough)
	insertSlackTestWorkflowRun(t, db, "wr-failed", "claw-pipeline", "failed", base)
	// Success path: deleted + run completed -> silent.
	insertSlackTestClaw(t, db, "claw-done", "deleted", 1, "", oldEnough)
	insertSlackTestWorkflowRun(t, db, "wr-done", "claw-done", "completed", base)
	// Plain deletion with no workflow run -> silent.
	insertSlackTestClaw(t, db, "claw-plain", "deleted", 1, "", oldEnough)
	// Retried claw whose latest run succeeded -> silent (the verdict is the
	// most recent run, not any historical failure).
	insertSlackTestClaw(t, db, "claw-retried", "deleted", 1, "", oldEnough)
	insertSlackTestWorkflowRun(t, db, "wr-old-failed", "claw-retried", "failed", base.Add(-time.Hour))
	insertSlackTestWorkflowRun(t, db, "wr-new-done", "claw-retried", "completed", base)

	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("want exactly the pipeline failure, got %d messages", fake.count())
	}
	if !strings.Contains(fake.request(0).Fallback, "name-claw-pipeline") {
		t.Fatalf("failure sent for the wrong claw: %q", fake.request(0).Fallback)
	}
	if status, ok := slackDeliveryStatus(t, db, lifecycleClawFailureKey("claw-pipeline")); !ok || status != notificationDeliveryStatusSent {
		t.Fatalf("pipeline failure delivery = %q, %v", status, ok)
	}
	// Rescan does not duplicate.
	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("deleted+failed shape resent: %d messages", fake.count())
	}
}

// ── Manual trigger endpoint (claw variant) ────────────────────────────────────

func TestSlackTestEndpointClawIDDryRun(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)

	insertSlackTestClaw(t, db, "claw-adhoc", "connected", 1, "", oldEnough)
	insertSlackTestClawPR(t, db, "pr-1", "claw-adhoc", "acme/app", 7, "https://github.com/acme/app/pull/7")

	rr := postSlackTest(t, s, `{"event_type":"pr_opened","claw_id":"claw-adhoc","dry_run":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		DryRun  bool `json:"dry_run"`
		Payload struct {
			Attachments []struct {
				Fallback string `json:"fallback"`
			} `json:"attachments"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.DryRun || len(resp.Payload.Attachments) != 1 ||
		!strings.Contains(resp.Payload.Attachments[0].Fallback, "acme/app#7") {
		t.Fatalf("unexpected dry-run response: %s", rr.Body.String())
	}
	if fake.count() != 0 {
		t.Fatalf("dry_run hit Slack %d times", fake.count())
	}

	// Real claw context in a failure render, including the diagnostic.
	if _, err := db.Exec(`UPDATE claws SET status='error', bootstrap_diagnostic='sandbox died' WHERE id='claw-adhoc'`); err != nil {
		t.Fatalf("set claw error: %v", err)
	}
	rr = postSlackTest(t, s, `{"event_type":"agent_stopped","claw_id":"claw-adhoc","dry_run":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "sandbox died") {
		t.Fatalf("failure dry-run does not use the claw's bootstrap_diagnostic: %s", rr.Body.String())
	}
}

func TestSlackTestEndpointClawIDRealSendLeavesNoState(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)

	insertSlackTestClaw(t, db, "claw-adhoc", "connected", 1, "", oldEnough)
	rr := postSlackTest(t, s, `{"event_type":"agent_started","claw_id":"claw-adhoc"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if fake.count() != 1 {
		t.Fatalf("expected exactly one Slack call, got %d", fake.count())
	}
	if req := fake.request(0); req.ThreadTS != "" {
		t.Fatal("test sends must post top-level, never into a claw thread")
	}
	if !strings.Contains(fake.request(0).Fallback, "name-claw-adhoc") {
		t.Fatalf("test send does not carry the claw context: %q", fake.request(0).Fallback)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(1) FROM slack_notification_deliveries`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("test send wrote %d delivery rows (err %v)", n, err)
	}
	if err := db.QueryRow(`SELECT COUNT(1) FROM slack_run_threads`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("test send wrote %d thread rows (err %v)", n, err)
	}
}

func TestSlackTestEndpointClawIDNotFound(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, _ := newSlackNotifierTestServer(t, fake.server.URL, nil)

	rr := postSlackTest(t, s, `{"event_type":"agent_started","claw_id":"nope"}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

func TestSlackTestEndpointRejectsRunIDAndClawIDTogether(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, _ := newSlackNotifierTestServer(t, fake.server.URL, nil)

	rr := postSlackTest(t, s, `{"event_type":"agent_started","run_id":"run-1","claw_id":"claw-1"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "mutually exclusive") {
		t.Fatalf("unexpected body: %s", rr.Body.String())
	}
}

// The idle candidate query's ownership filter: only a DELIVERED unresolved PR
// (mention_only=0) counts as "PR out, awaiting humans" and suppresses the
// agent_idle notification. A mention-only row is a polling target the agent
// merely linked — suppressing on it would hide a hung claw the watcher will
// also never finalize. Removing `AND p.mention_only = 0` from the NOT EXISTS
// clause in selectLifecycleClawIdleCandidates fails the first assertion here.
func TestLifecycleClawIdleCandidatesIgnoreMentionOnlyPRs(t *testing.T) {
	s, db := newSlackNotifierTestServer(t, "", nil)
	setLifecycleClawBaseline(t, s)

	insertSlackTestClaw(t, db, "claw-idle-mention", "connected", 1, "", oldEnough)
	if _, err := db.Exec(`UPDATE claws SET pipeline_stage='work', idle_since=? WHERE id='claw-idle-mention'`,
		time.Now().Add(-lifecycleClawAdhocGrace-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	insertSlackTestClawPR(t, db, "pr-idle-mention", "claw-idle-mention", "acme/app", 7, "https://github.com/acme/app/pull/7")

	// Unresolved mention-only row: NOT an ownership signal — the claw must
	// still be an idle candidate.
	if _, err := db.Exec(`UPDATE claw_prs SET mention_only=1 WHERE id='pr-idle-mention'`); err != nil {
		t.Fatal(err)
	}
	claws, _, err := s.selectLifecycleClawIdleCandidates("")
	if err != nil {
		t.Fatal(err)
	}
	if len(claws) != 1 || claws[0].ID != "claw-idle-mention" {
		t.Fatalf("idle candidates with an unresolved MENTION-ONLY PR = %v, want exactly claw-idle-mention (a linked PR must not suppress the idle alert)", claws)
	}

	// Same row flipped to delivered: "PR out, awaiting humans" — suppressed.
	if _, err := db.Exec(`UPDATE claw_prs SET mention_only=0 WHERE id='pr-idle-mention'`); err != nil {
		t.Fatal(err)
	}
	claws, _, err = s.selectLifecycleClawIdleCandidates("")
	if err != nil {
		t.Fatal(err)
	}
	if len(claws) != 0 {
		t.Fatalf("idle candidates with an unresolved DELIVERED PR = %v, want none (an owned open PR suppresses the idle alert)", claws)
	}
}
