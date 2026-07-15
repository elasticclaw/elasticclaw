package hub

import (
	"encoding/json"
	"strings"
	"testing"
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
