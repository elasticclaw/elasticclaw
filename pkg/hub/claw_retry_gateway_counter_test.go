package hub

import (
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func newRetryCounterTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`INSERT INTO tenants(id,name,token,claw_token,created_at) VALUES(?,?,?,?,?)`,
		"tenant", "tenant", "token", "claw-token", now()); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, template, status, created_at) VALUES(?,?,?,?,?,?)`,
		"claw", "tenant", "claw", "template", "error", now()); err != nil {
		t.Fatalf("seed claw: %v", err)
	}
	return &Server{
		db:                     db,
		hubCfg:                 &types.HubConfig{},
		claws:                  map[string]*clawConn{},
		gatewayUnhealthyCounts: map[string]int{},
		gatewayEscalatedAt:     map[string]time.Time{},
	}
}

// A retry replaces the sandbox but reuses the claw ID, so the gateway health
// counters must not carry over. Otherwise the successor boots with a spent
// budget and the first unhealthy heartbeat re-crosses the threshold.
func TestResetClawForRetryClearsGatewayHealthCounters(t *testing.T) {
	s := newRetryCounterTestServer(t)
	s.gatewayUnhealthyCounts["claw"] = defaultGatewayUnhealthyMax
	s.gatewayEscalatedAt["claw"] = now()

	replaced, err := s.resetClawForRetry("tenant", "claw", "", "Restoring", "")
	if err != nil {
		t.Fatalf("resetClawForRetry: %v", err)
	}
	if !replaced {
		t.Fatal("expected the claw to be reset for retry")
	}

	if got := s.gatewayUnhealthyCount("claw"); got != 0 {
		t.Errorf("gatewayUnhealthyCounts = %d after retry reset, want 0", got)
	}
	s.mu.RLock()
	_, escalated := s.gatewayEscalatedAt["claw"]
	s.mu.RUnlock()
	if escalated {
		t.Error("gatewayEscalatedAt must be cleared so the successor is not held by the predecessor's cooldown")
	}
}

// The counters belong to the claw being replaced. A reset that does not apply
// -- the claw is not in a replaceable status -- must leave them untouched.
func TestResetClawForRetryKeepsCountersWhenItDoesNotApply(t *testing.T) {
	s := newRetryCounterTestServer(t)
	if _, err := s.db.Exec(`UPDATE claws SET status='connected' WHERE id='claw'`); err != nil {
		t.Fatal(err)
	}
	s.gatewayUnhealthyCounts["claw"] = 7

	replaced, err := s.resetClawForRetry("tenant", "claw", "", "Restoring", "")
	if err != nil {
		t.Fatalf("resetClawForRetry: %v", err)
	}
	if replaced {
		t.Fatal("a connected claw must not be reset for retry")
	}
	if got := s.gatewayUnhealthyCount("claw"); got != 7 {
		t.Errorf("gatewayUnhealthyCounts = %d, want 7 left untouched", got)
	}
}
