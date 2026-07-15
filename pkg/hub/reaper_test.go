package hub

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func newReaperTestServer(t *testing.T, cfg *types.HubConfig) (*Server, *sql.DB) {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`INSERT INTO tenants(id,name,token,claw_token,created_at) VALUES('tenant','Tenant','token','claw-token',?)`, now()); err != nil {
		t.Fatal(err)
	}
	return &Server{db: db, hubCfg: cfg, claws: make(map[string]*clawConn), users: make(map[string]*userConn), reaperFirstSeen: make(map[string]time.Time)}, db
}

func TestReconcileOnBootRepairsStrandedRecords(t *testing.T) {
	s, db := newReaperTestServer(t, &types.HubConfig{})
	tm := time.Now().UTC().Add(-time.Hour)
	s.nowFunc = func() time.Time { return tm }
	for _, c := range []struct{ id, provider, providerID, status string }{
		{"daytona01", "daytona", "", "provisioning"},
		{"replicated01", "replicated", "vm-1", "provisioning"},
		{"deleted01", "daytona", "", "deleted"},
	} {
		if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,provider,provider_id,status,created_at) VALUES(?,?,?,?,?,?,?)`, c.id, "tenant", c.id, c.provider, c.providerID, c.status, tm); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []struct{ id, claw string }{{"daytona-run", "daytona01"}, {"orphan-run", "deleted01"}} {
		if _, err := db.Exec(`INSERT INTO workflow_runs(id,tenant_id,workflow_name,workspace_name,status,claw_id,created_at) VALUES(?,?,?,?,?,?,?)`, r.id, "tenant", "workflow", "workspace", "running", r.claw, tm); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO factory_triggers(id,factory_name,integration,trigger_key,status,first_seen_at,last_seen_at,created_at,updated_at) VALUES('trigger','factory','external','key','claimed',?,?,?,?)`, tm, tm, tm, tm); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO claw_checkpoints(id,tenant_id,claw_id,status,created_at) VALUES('checkpoint','tenant','daytona01','creating',?)`, tm); err != nil {
		t.Fatal(err)
	}

	s.reconcileOnBoot()
	for _, tc := range []struct{ query, want string }{
		{`SELECT status FROM claws WHERE id='daytona01'`, "error"},
		{`SELECT status FROM claws WHERE id='replicated01'`, "provisioning"},
		{`SELECT status FROM workflow_runs WHERE id='daytona-run'`, "failed"},
		{`SELECT status FROM workflow_runs WHERE id='orphan-run'`, "failed"},
		{`SELECT status FROM factory_triggers WHERE id='trigger'`, "failed"},
		{`SELECT status FROM claw_checkpoints WHERE id='checkpoint'`, "failed"},
	} {
		var got string
		if err := db.QueryRow(tc.query).Scan(&got); err != nil || got != tc.want {
			t.Errorf("%s = %q, %v; want %q", tc.query, got, err, tc.want)
		}
	}
}

func TestReaperOfflineGraceAndReconnectReset(t *testing.T) {
	enabled := true
	s, db := newReaperTestServer(t, &types.HubConfig{Liveness: &types.LivenessConfig{Enabled: &enabled, OfflineGrace: "10m", ProvisioningMaxAge: "30m", ClaimTTL: "15m", ReaperInterval: "1m"}})
	tm := time.Now().UTC()
	s.nowFunc = func() time.Time { return tm }
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,status,created_at) VALUES('offline01','tenant','offline','offline',?)`, tm); err != nil {
		t.Fatal(err)
	}

	s.reapOnce()
	var status string
	if err := db.QueryRow(`SELECT status FROM claws WHERE id='offline01'`).Scan(&status); err != nil || status != "offline" {
		t.Fatalf("within grace: status=%q err=%v", status, err)
	}
	tm = tm.Add(11 * time.Minute)
	s.reapOnce()
	if err := db.QueryRow(`SELECT status FROM claws WHERE id='offline01'`).Scan(&status); err != nil || status != "error" {
		t.Fatalf("past grace: status=%q err=%v", status, err)
	}

	if _, err := db.Exec(`UPDATE claws SET status='offline' WHERE id='offline01'`); err != nil {
		t.Fatal(err)
	}
	s.reapOnce()
	if _, ok := s.reaperFirstSeen["claw:offline01:offline"]; !ok {
		t.Fatal("offline claw was not tracked")
	}
	if _, err := db.Exec(`UPDATE claws SET status='connected' WHERE id='offline01'`); err != nil {
		t.Fatal(err)
	}
	s.reapOnce()
	if _, ok := s.reaperFirstSeen["claw:offline01:offline"]; ok {
		t.Fatal("reconnected claw retained offline firstSeen entry")
	}
}

func TestReaperProvisioningMaxAgeStartsWhenProvisioningIsObserved(t *testing.T) {
	enabled := true
	s, db := newReaperTestServer(t, &types.HubConfig{Liveness: &types.LivenessConfig{Enabled: &enabled, ProvisioningMaxAge: "30m"}})
	tm := time.Now().UTC()
	s.nowFunc = func() time.Time { return tm }
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,status,created_at) VALUES('provisioning01','tenant','provisioning','provisioning',?)`, tm.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	// This claw may have spent an hour pending before promotion. Its row creation
	// time must not make the newly observed provisioning attempt time out at once.
	s.reapOnce()
	var status string
	if err := db.QueryRow(`SELECT status FROM claws WHERE id='provisioning01'`).Scan(&status); err != nil || status != "provisioning" {
		t.Fatalf("newly observed provisioning claw: status=%q err=%v", status, err)
	}

	tm = tm.Add(31 * time.Minute)
	s.reapOnce()
	if err := db.QueryRow(`SELECT status FROM claws WHERE id='provisioning01'`).Scan(&status); err != nil || status != "error" {
		t.Fatalf("timed out provisioning claw: status=%q err=%v", status, err)
	}
}

func TestStopAgentWithReasonPromotesPendingClaw(t *testing.T) {
	s, db := newReaperTestServer(t, &types.HubConfig{MaxConcurrentClaws: 1})
	tm := time.Now().UTC()
	for _, c := range []struct{ id, status string }{{"active001", "connected"}, {"pending01", "pending"}} {
		if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,status,concurrency_group,created_at) VALUES(?,?,?,?,?,?)`, c.id, "tenant", c.id, c.status, "global", tm); err != nil {
			t.Fatal(err)
		}
	}
	s.stopAgentWithReason("active001", "test failure", true)
	var status string
	if err := db.QueryRow(`SELECT status FROM claws WHERE id='pending01'`).Scan(&status); err != nil || status != "provisioning" {
		t.Fatalf("pending status=%q err=%v, want provisioning", status, err)
	}
}
