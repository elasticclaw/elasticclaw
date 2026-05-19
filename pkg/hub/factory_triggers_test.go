package hub

import "testing"

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
	s.completeFactoryTrigger("design", "github-issues", triggerKey, "claw-design")

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
	s.completeFactoryTrigger("code", "linear", triggerKey, "claw-active")

	claimed, err = s.claimFactoryTrigger("code", "linear", triggerKey, "poll", nil)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if claimed {
		t.Fatal("expected active same-factory claim to be idempotent")
	}
}

func TestClaimFactoryTriggerSkipsErroredSameFactoryClaw(t *testing.T) {
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
	s.completeFactoryTrigger("qa", "shortcut", triggerKey, "claw-error")

	claimed, err = s.claimFactoryTrigger("qa", "shortcut", triggerKey, "poll", nil)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if claimed {
		t.Fatal("expected errored same-factory claw to remain idempotent")
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
	s.completeFactoryTrigger("qa", "shortcut", triggerKey, "claw-deleted")

	claimed, err = s.claimFactoryTrigger("qa", "shortcut", triggerKey, "poll", nil)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if !claimed {
		t.Fatal("expected deleted claw to be reclaimable")
	}
}
