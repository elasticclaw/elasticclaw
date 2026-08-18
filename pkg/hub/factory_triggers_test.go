package hub

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newFactoryTriggerTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	_, _ = db.Exec(`INSERT INTO tenants(id,name,token,claw_token,created_at) VALUES(?,?,?,?,?)`,
		"tenant", "tenant", "token", "claw-token", now())
	t.Cleanup(func() { db.Close() })
	return &Server{db: db}
}

func TestFailedFactoryTriggerIsRetriedAfterServerRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "hub.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open initial db: %v", err)
	}
	s := &Server{db: db}
	key := factoryTriggerKey("linear", "ELA-restart")
	claimed, err := s.claimFactoryTrigger("code", "linear", key, "poll", nil)
	if err != nil || !claimed {
		t.Fatalf("initial claim = %v, %v", claimed, err)
	}
	// A creation failure leaves the durable trigger claim eligible for retry.
	s.failFactoryTrigger("code", "linear", key)
	if _, err := db.Exec(`UPDATE factory_triggers SET updated_at=? WHERE trigger_key=?`, time.Now().UTC().Add(-61*time.Second), key); err != nil {
		t.Fatalf("age failed trigger beyond backoff: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close initial db: %v", err)
	}

	restartedDB, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open restarted db: %v", err)
	}
	t.Cleanup(func() { restartedDB.Close() })
	restarted := &Server{db: restartedDB}
	claimed, err = restarted.claimFactoryTrigger("code", "linear", key, "poll", nil)
	if err != nil || !claimed {
		t.Fatalf("claim after restart = %v, %v", claimed, err)
	}
	if err := restarted.completeFactoryTrigger("code", "linear", key, "claw-restarted"); err != nil {
		t.Fatalf("complete retried trigger: %v", err)
	}

	var status string
	if err := restartedDB.QueryRow(`SELECT status FROM factory_triggers WHERE trigger_key=?`, key).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("trigger status after retry = %q, want active", status)
	}
}

func TestClaimFactoryTriggerScopesByFactoryName(t *testing.T) {
	s := newFactoryTriggerTestServer(t)
	triggerKey := factoryTriggerKey("github-issues", "elasticclaw/elasticclaw/245")

	claimed, err := s.claimFactoryTrigger("design", "github-issues", triggerKey, "poll", nil)
	if err != nil {
		t.Fatalf("claim design: %v", err)
	}
	if !claimed {
		t.Fatal("expected design claim")
	}
	if err := s.completeFactoryTrigger("design", "github-issues", triggerKey, "claw-design"); err != nil {
		t.Fatalf("complete design: %v", err)
	}

	claimed, err = s.claimFactoryTrigger("code", "github-issues", triggerKey, "poll", nil)
	if err != nil {
		t.Fatalf("claim code: %v", err)
	}
	if !claimed {
		t.Fatal("expected separate code factory claim for same trigger")
	}
}

func TestClaimFactoryTriggerSkipsActiveSameFactory(t *testing.T) {
	s := newFactoryTriggerTestServer(t)
	triggerKey := factoryTriggerKey("linear", "ELA-123")
	_, _ = s.db.Exec(`INSERT INTO claws(id, tenant_id, name, template, status, created_at) VALUES(?,?,?,?,?,?)`,
		"claw-active", "tenant", "claw", "template", "provisioning", now())

	claimed, err := s.claimFactoryTrigger("code", "linear", triggerKey, "webhook", nil)
	if err != nil {
		t.Fatalf("initial claim: %v", err)
	}
	if !claimed {
		t.Fatal("expected initial claim")
	}
	if err := s.completeFactoryTrigger("code", "linear", triggerKey, "claw-active"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	claimed, err = s.claimFactoryTrigger("code", "linear", triggerKey, "poll", nil)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if claimed {
		t.Fatal("expected active same-factory claim to be idempotent")
	}
}

func TestClaimFactoryTriggerReclaimsErroredClawAfterBackoff(t *testing.T) {
	s := newFactoryTriggerTestServer(t)
	triggerKey := factoryTriggerKey("shortcut", "sc-123")
	_, _ = s.db.Exec(`INSERT INTO claws(id, tenant_id, name, template, status, created_at) VALUES(?,?,?,?,?,?)`,
		"claw-error", "tenant", "claw", "template", "error", now())

	claimed, err := s.claimFactoryTrigger("qa", "shortcut", triggerKey, "webhook", nil)
	if err != nil {
		t.Fatalf("initial claim: %v", err)
	}
	if !claimed {
		t.Fatal("expected initial claim")
	}
	if err := s.completeFactoryTrigger("qa", "shortcut", triggerKey, "claw-error"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	claimed, err = s.claimFactoryTrigger("qa", "shortcut", triggerKey, "poll", nil)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if claimed {
		t.Fatal("expected the dead claw to be charged a retry, not reclaimed on the spot")
	}

	ageTrigger(t, s, triggerKey, 61*time.Second)
	claimed, err = s.claimFactoryTrigger("qa", "shortcut", triggerKey, "poll", nil)
	if err != nil {
		t.Fatalf("claim after backoff: %v", err)
	}
	if !claimed {
		t.Fatal("expected errored claw to be reclaimable once the backoff elapsed")
	}
}

func TestClaimFactoryTriggerReclaimsDeletedClawAfterBackoff(t *testing.T) {
	s := newFactoryTriggerTestServer(t)
	triggerKey := factoryTriggerKey("shortcut", "sc-123")
	_, _ = s.db.Exec(`INSERT INTO claws(id, tenant_id, name, template, status, created_at) VALUES(?,?,?,?,?,?)`,
		"claw-deleted", "tenant", "claw", "template", "deleted", now())

	claimed, err := s.claimFactoryTrigger("qa", "shortcut", triggerKey, "webhook", nil)
	if err != nil {
		t.Fatalf("initial claim: %v", err)
	}
	if !claimed {
		t.Fatal("expected initial claim")
	}
	if err := s.completeFactoryTrigger("qa", "shortcut", triggerKey, "claw-deleted"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	claimed, err = s.claimFactoryTrigger("qa", "shortcut", triggerKey, "poll", nil)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if claimed {
		t.Fatal("expected the dead claw to be charged a retry, not reclaimed on the spot")
	}

	ageTrigger(t, s, triggerKey, 61*time.Second)
	claimed, err = s.claimFactoryTrigger("qa", "shortcut", triggerKey, "poll", nil)
	if err != nil {
		t.Fatalf("claim after backoff: %v", err)
	}
	if !claimed {
		t.Fatal("expected deleted claw to be reclaimable once the backoff elapsed")
	}
}

func TestTriggerPayloadJSONDropsOversizedPayload(t *testing.T) {
	got := triggerPayloadJSON(map[string]string{"large": strings.Repeat("x", 20*1024)})
	if got != "{}" {
		t.Fatalf("expected oversized payload to be dropped, got %d bytes", len(got))
	}
	if !json.Valid([]byte(got)) {
		t.Fatalf("expected valid JSON, got %q", got)
	}
}

func TestCompleteFactoryTriggerReturnsMissingClaimError(t *testing.T) {
	s := newFactoryTriggerTestServer(t)
	err := s.completeFactoryTrigger("missing", "linear", "linear:ELA-123", "claw-1")
	if err == nil {
		t.Fatal("expected missing claim error")
	}
}

func TestFailedFactoryTriggerBackoffAndWebhookBypass(t *testing.T) {
	s := newFactoryTriggerTestServer(t)
	key := factoryTriggerKey("linear", "ELA-456")
	claimed, err := s.claimFactoryTrigger("code", "linear", key, "poll", nil)
	if err != nil || !claimed {
		t.Fatalf("initial claim = %v, %v", claimed, err)
	}
	s.failFactoryTrigger("code", "linear", key)

	var status string
	var retryCount int
	if err := s.db.QueryRow(`SELECT status, retry_count FROM factory_triggers WHERE trigger_key=?`, key).Scan(&status, &retryCount); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || retryCount != 1 {
		t.Fatalf("failed trigger = status %q retry_count %d, want failed/1", status, retryCount)
	}

	claimed, err = s.claimFactoryTrigger("code", "linear", key, "poll", nil)
	if err != nil || claimed {
		t.Fatalf("claim before backoff = %v, %v; want false, nil", claimed, err)
	}
	_, err = s.db.Exec(`UPDATE factory_triggers SET updated_at=? WHERE trigger_key=?`, time.Now().UTC().Add(-61*time.Second), key)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err = s.claimFactoryTrigger("code", "linear", key, "poll", nil)
	if err != nil || !claimed {
		t.Fatalf("claim after backoff = %v, %v", claimed, err)
	}

	otherKey := factoryTriggerKey("linear", "ELA-457")
	claimed, err = s.claimFactoryTrigger("code", "linear", otherKey, "poll", nil)
	if err != nil || !claimed {
		t.Fatalf("other initial claim = %v, %v", claimed, err)
	}
	s.failFactoryTrigger("code", "linear", otherKey)
	claimed, err = s.claimFactoryTrigger("code", "linear", otherKey, "webhook", nil)
	if err != nil || !claimed {
		t.Fatalf("webhook bypass claim = %v, %v", claimed, err)
	}
}

// Completing a claim means a claw exists, not that the ticket succeeded, so the
// retry ladder must survive it — otherwise a claw that keeps dying after
// creation retries at a flat 30s forever.
func TestCompleteFactoryTriggerKeepsRetryCount(t *testing.T) {
	s := newFactoryTriggerTestServer(t)
	key := factoryTriggerKey("linear", "ELA-789")
	claimed, err := s.claimFactoryTrigger("code", "linear", key, "poll", nil)
	if err != nil || !claimed {
		t.Fatalf("initial claim = %v, %v", claimed, err)
	}
	s.failFactoryTrigger("code", "linear", key)
	if err := s.completeFactoryTrigger("code", "linear", key, "claw-789"); err != nil {
		t.Fatal(err)
	}
	var retryCount int
	if err := s.db.QueryRow(`SELECT retry_count FROM factory_triggers WHERE trigger_key=?`, key).Scan(&retryCount); err != nil {
		t.Fatal(err)
	}
	if retryCount != 1 {
		t.Fatalf("retry_count = %d, want 1", retryCount)
	}
}

// seedTriggerWithClaw creates a claw row in the given status and links it to a
// freshly claimed trigger, i.e. the state the hub is in right after a claw was
// provisioned for a ticket.
func seedTriggerWithClaw(t *testing.T, s *Server, factory, integration, key, clawID, clawStatus, source string) {
	t.Helper()
	claimed, err := s.claimFactoryTrigger(factory, integration, key, source, nil)
	if err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	if !claimed {
		t.Fatal("seed claim: expected trigger to be claimable")
	}
	linkClawToTrigger(t, s, factory, integration, key, clawID, clawStatus)
}

// linkClawToTrigger creates a claw row in the given status and completes an
// open claim with it, i.e. what a provisioning path does after claiming.
func linkClawToTrigger(t *testing.T, s *Server, factory, integration, key, clawID, clawStatus string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO claws(id, tenant_id, name, template, status, created_at) VALUES(?,?,?,?,?,?)`,
		clawID, "tenant", clawID, "template", clawStatus, now()); err != nil {
		t.Fatalf("insert claw %s: %v", clawID, err)
	}
	if err := s.completeFactoryTrigger(factory, integration, key, clawID); err != nil {
		t.Fatalf("complete claim with %s: %v", clawID, err)
	}
}

func triggerRetryCount(t *testing.T, s *Server, key string) int {
	t.Helper()
	var retryCount int
	if err := s.db.QueryRow(`SELECT retry_count FROM factory_triggers WHERE trigger_key=?`, key).Scan(&retryCount); err != nil {
		t.Fatalf("read retry_count: %v", err)
	}
	return retryCount
}

func ageTrigger(t *testing.T, s *Server, key string, d time.Duration) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE factory_triggers SET updated_at=? WHERE trigger_key=?`, now().Add(-d), key); err != nil {
		t.Fatalf("age trigger: %v", err)
	}
}

// A claw that dies after creation must be charged a retry instead of being
// reclaimed on the next tick, which is what produced several identical claws
// per ticket for a deterministic failure.
func TestClaimFactoryTriggerChargesDeadClawAsFailedAttempt(t *testing.T) {
	cases := []struct {
		name        string
		clawStatus  string
		source      string
		wantClaimed bool
	}{
		{name: "errored claw polled", clawStatus: "error", source: "poll", wantClaimed: false},
		{name: "deleted claw polled", clawStatus: "deleted", source: "poll", wantClaimed: false},
		{name: "errored claw integration webhook", clawStatus: "error", source: "linear webhook", wantClaimed: false},
		{name: "errored claw exempt webhook", clawStatus: "error", source: "webhook", wantClaimed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newFactoryTriggerTestServer(t)
			key := factoryTriggerKey("linear", "ELA-dead")
			seedTriggerWithClaw(t, s, "code", "linear", key, "claw-dead", tc.clawStatus, "poll")

			claimed, err := s.claimFactoryTrigger("code", "linear", key, tc.source, nil)
			if err != nil {
				t.Fatalf("claim after claw death: %v", err)
			}
			if claimed != tc.wantClaimed {
				t.Fatalf("claim after claw death = %v, want %v", claimed, tc.wantClaimed)
			}
			if got := triggerRetryCount(t, s, key); got != 1 {
				t.Fatalf("retry_count = %d, want 1", got)
			}
		})
	}
}

// The dead-claw path must climb the same ladder a creation failure climbs, and
// completing a claim must not knock it back down to the first rung.
func TestClaimFactoryTriggerDeadClawBackoffLadder(t *testing.T) {
	s := newFactoryTriggerTestServer(t)
	key := factoryTriggerKey("linear", "ELA-ladder")
	claimed, err := s.claimFactoryTrigger("code", "linear", key, "poll", nil)
	if err != nil || !claimed {
		t.Fatalf("initial claim = %v, %v", claimed, err)
	}

	// The wait is computed from the post-increment retry_count, so the first
	// rung after a claw dies is 60s, the same rung a creation failure lands on.
	for attempt, wait := range []time.Duration{60 * time.Second, 120 * time.Second, 240 * time.Second} {
		linkClawToTrigger(t, s, "code", "linear", key, fmt.Sprintf("claw-ladder-%d", attempt), "error")

		claimed, err = s.claimFactoryTrigger("code", "linear", key, "poll", nil)
		if err != nil {
			t.Fatalf("attempt %d: detect death: %v", attempt, err)
		}
		if claimed {
			t.Fatalf("attempt %d: dead claw reclaimed with no wait", attempt)
		}
		if got, want := triggerRetryCount(t, s, key), attempt+1; got != want {
			t.Fatalf("attempt %d: retry_count = %d, want %d", attempt, got, want)
		}

		ageTrigger(t, s, key, wait-time.Second)
		claimed, err = s.claimFactoryTrigger("code", "linear", key, "poll", nil)
		if err != nil {
			t.Fatalf("attempt %d: claim inside backoff: %v", attempt, err)
		}
		if claimed {
			t.Fatalf("attempt %d: reclaimed %s after death, want wait of %s", attempt, wait-time.Second, wait)
		}

		ageTrigger(t, s, key, wait+time.Second)
		claimed, err = s.claimFactoryTrigger("code", "linear", key, "poll", nil)
		if err != nil {
			t.Fatalf("attempt %d: claim after backoff: %v", attempt, err)
		}
		if !claimed {
			t.Fatalf("attempt %d: not reclaimed %s after death, want reclaim after %s", attempt, wait+time.Second, wait)
		}
	}
}

func TestFactoryTriggerBackoffCap(t *testing.T) {
	cases := []struct {
		retryCount int
		want       time.Duration
	}{
		{retryCount: 0, want: 30 * time.Second},
		{retryCount: 1, want: time.Minute},
		{retryCount: 5, want: 16 * time.Minute},
		{retryCount: 6, want: 30 * time.Minute},
		{retryCount: 40, want: 30 * time.Minute},
	}
	for _, tc := range cases {
		if got := factoryTriggerBackoff(tc.retryCount); got != tc.want {
			t.Fatalf("factoryTriggerBackoff(%d) = %s, want %s", tc.retryCount, got, tc.want)
		}
	}
}

func TestClaimFactoryTriggerDeadClawBackoffIsCapped(t *testing.T) {
	s := newFactoryTriggerTestServer(t)
	key := factoryTriggerKey("linear", "ELA-cap")
	seedTriggerWithClaw(t, s, "code", "linear", key, "claw-cap", "error", "poll")
	if _, err := s.db.Exec(`UPDATE factory_triggers SET retry_count=20 WHERE trigger_key=?`, key); err != nil {
		t.Fatalf("seed retry_count: %v", err)
	}

	claimed, err := s.claimFactoryTrigger("code", "linear", key, "poll", nil)
	if err != nil {
		t.Fatalf("detect death: %v", err)
	}
	if claimed {
		t.Fatal("dead claw reclaimed with no wait")
	}

	ageTrigger(t, s, key, 29*time.Minute)
	claimed, err = s.claimFactoryTrigger("code", "linear", key, "poll", nil)
	if err != nil {
		t.Fatalf("claim inside cap: %v", err)
	}
	if claimed {
		t.Fatal("reclaimed after 29m, want 30m cap")
	}

	ageTrigger(t, s, key, 31*time.Minute)
	claimed, err = s.claimFactoryTrigger("code", "linear", key, "poll", nil)
	if err != nil {
		t.Fatalf("claim past cap: %v", err)
	}
	if !claimed {
		t.Fatal("not reclaimed after 31m, want backoff capped at 30m")
	}
}

// A ticket nobody has attempted yet must never inherit a wait.
func TestClaimFactoryTriggerFreshTriggerDoesNotWait(t *testing.T) {
	s := newFactoryTriggerTestServer(t)
	failedKey := factoryTriggerKey("linear", "ELA-failed")
	seedTriggerWithClaw(t, s, "code", "linear", failedKey, "claw-failed", "error", "poll")
	if claimed, err := s.claimFactoryTrigger("code", "linear", failedKey, "poll", nil); err != nil || claimed {
		t.Fatalf("dead claw claim = %v, %v; want false, nil", claimed, err)
	}

	freshKey := factoryTriggerKey("linear", "ELA-fresh")
	claimed, err := s.claimFactoryTrigger("code", "linear", freshKey, "poll", nil)
	if err != nil || !claimed {
		t.Fatalf("fresh trigger claim = %v, %v; want true, nil", claimed, err)
	}
}

// A trigger whose claw row is gone from the database never recorded a failure,
// so it is reclaimed straight away.
func TestClaimFactoryTriggerMissingClawRowReclaimsImmediately(t *testing.T) {
	s := newFactoryTriggerTestServer(t)
	key := factoryTriggerKey("linear", "ELA-gone")
	seedTriggerWithClaw(t, s, "code", "linear", key, "claw-gone", "running", "poll")
	if _, err := s.db.Exec(`DELETE FROM claws WHERE id=?`, "claw-gone"); err != nil {
		t.Fatalf("delete claw row: %v", err)
	}

	claimed, err := s.claimFactoryTrigger("code", "linear", key, "poll", nil)
	if err != nil || !claimed {
		t.Fatalf("claim with missing claw row = %v, %v; want true, nil", claimed, err)
	}
	if got := triggerRetryCount(t, s, key); got != 0 {
		t.Fatalf("retry_count = %d, want 0", got)
	}
}

// A claw that ran for a while and was then cleaned up is not churn. Charging it
// a retry would make every successful ticket pay a backoff the next time its
// trigger fires, and — since nothing ever resets retry_count — the wait would
// ratchet up for the rest of that trigger's life.
func TestClaimFactoryTriggerDoesNotChargeLongLivedClaw(t *testing.T) {
	s := newFactoryTriggerTestServer(t)
	triggerKey := factoryTriggerKey("shortcut", "sc-long-lived")
	// Created two hours ago: this claw did its job before being deleted.
	if _, err := s.db.Exec(`INSERT INTO claws(id, tenant_id, name, template, status, created_at) VALUES(?,?,?,?,?,?)`,
		"claw-old", "tenant", "claw", "template", "deleted", now().Add(-2*time.Hour)); err != nil {
		t.Fatalf("seed claw: %v", err)
	}

	claimed, err := s.claimFactoryTrigger("qa", "shortcut", triggerKey, "poll", nil)
	if err != nil {
		t.Fatalf("initial claim: %v", err)
	}
	if !claimed {
		t.Fatal("expected initial claim")
	}
	if err := s.completeFactoryTrigger("qa", "shortcut", triggerKey, "claw-old"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	claimed, err = s.claimFactoryTrigger("qa", "shortcut", triggerKey, "poll", nil)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if !claimed {
		t.Fatal("a long-lived claw must be reclaimed immediately, not charged a retry")
	}
	var retryCount int
	if err := s.db.QueryRow(`SELECT retry_count FROM factory_triggers WHERE trigger_key=?`, triggerKey).Scan(&retryCount); err != nil {
		t.Fatalf("read retry_count: %v", err)
	}
	if retryCount != 0 {
		t.Fatalf("a successful run must not leave a retry charge behind, got retry_count=%d", retryCount)
	}
}
