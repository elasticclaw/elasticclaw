package hub

import (
	"encoding/json"
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

func TestClaimFactoryTriggerReclaimsErroredClaw(t *testing.T) {
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
	if !claimed {
		t.Fatal("expected errored claw to be reclaimable")
	}
}

func TestClaimFactoryTriggerReclaimsDeletedClaw(t *testing.T) {
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
	if !claimed {
		t.Fatal("expected deleted claw to be reclaimable")
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

func TestCompleteFactoryTriggerResetsRetryCount(t *testing.T) {
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
	if retryCount != 0 {
		t.Fatalf("retry_count = %d, want 0", retryCount)
	}
}
