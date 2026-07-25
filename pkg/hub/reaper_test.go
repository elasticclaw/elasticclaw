package hub

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

func TestReaperRedrivesVMAndCommentAfterClosingRows(t *testing.T) {
	s, db := newReaperTestServer(t, &types.HubConfig{})
	tm := time.Now().UTC()
	s.nowFunc = func() time.Time { return tm }
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,provider,provider_id,status,stop_comment_pending,created_at) VALUES('redrive-vm','tenant','vm','docker','vm-1','error',0,?),('redrive-comment','tenant','comment','','','error',1,?)`, tm, tm); err != nil {
		t.Fatal(err)
	}
	terminated := make(chan struct{}, 1)
	s.terminateVMOverride = func(provider, id string) error {
		terminated <- struct{}{}
		return nil
	}

	// The first observation starts both grace windows.
	s.reapOnce()
	tm = tm.Add(redriveGrace + time.Second)
	done := make(chan struct{})
	go func() { s.reapOnce(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reaper blocked while redriving rows on a single DB connection")
	}
	select {
	case <-terminated:
	case <-time.After(time.Second):
		t.Fatal("VM redrive was not dispatched")
	}
	deadline := time.Now().Add(time.Second)
	for {
		var providerID string
		if err := db.QueryRow(`SELECT provider_id FROM claws WHERE id='redrive-vm'`).Scan(&providerID); err != nil {
			t.Fatal(err)
		}
		if providerID == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("VM redrive did not clear provider_id")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var pending int
	if err := db.QueryRow(`SELECT stop_comment_pending FROM claws WHERE id='redrive-comment'`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("unresolved comment pending=%d err=%v, want 0", pending, err)
	}
}

func TestReaperExpiresDetachedPreview(t *testing.T) {
	s, db := newReaperTestServer(t, &types.HubConfig{})
	tm := time.Now().UTC()
	s.nowFunc = func() time.Time { return tm }
	if _, err := db.Exec(
		`INSERT INTO claws(
			id, tenant_id, name, provider, provider_id, status,
			preview_ready, preview_expires_at, created_at
		) VALUES(?,?,?,?,?,?,?,?,?)`,
		"expired-preview", "tenant", "preview", "docker", "preview-vm", "preview",
		1, tm.Add(-time.Second).UnixMilli(), tm,
	); err != nil {
		t.Fatal(err)
	}
	terminated := make(chan struct{}, 1)
	s.terminateVMOverride = func(provider, id string) error {
		if provider != "docker" || id != "preview-vm" {
			t.Errorf("terminate target = %q %q", provider, id)
		}
		terminated <- struct{}{}
		return nil
	}

	s.reapOnce()

	var status string
	if err := db.QueryRow(`SELECT status FROM claws WHERE id='expired-preview'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "deleted" {
		t.Fatalf("status = %q, want deleted", status)
	}
	select {
	case <-terminated:
	case <-time.After(time.Second):
		t.Fatal("expired preview sandbox was not terminated")
	}
}

func TestReaperRedrivesStopComment(t *testing.T) {
	newServer := func(t *testing.T, status int, requests chan<- string) (*Server, *sql.DB) {
		t.Helper()
		tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var request struct {
				Query     string            `json:"query"`
				Variables map[string]string `json:"variables"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode tracker request: %v", err)
			}
			select {
			case requests <- request.Query + "\n" + request.Variables["body"]:
			default:
			}
			if status != http.StatusOK {
				w.WriteHeader(status)
				return
			}
			if strings.Contains(request.Query, "commentCreate") {
				_, _ = w.Write([]byte(`{"data":{"commentCreate":{"success":true}}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"issue":{"id":"issue-1"}}}`))
		}))
		t.Cleanup(tracker.Close)

		s, db := newReaperTestServer(t, &types.HubConfig{Integrations: &types.IntegrationsConfig{
			Linear: []*types.LinearIntegrationConfig{{Workspace: "workspace", Token: "token"}},
		}, Factories: []*types.FactoryConfig{{Name: "factory", Integration: "linear", Workspace: "workspace"}}})
		s.linearBaseURL = tracker.URL
		return s, db
	}

	t.Run("posts and clears pending", func(t *testing.T) {
		requests := make(chan string, 2)
		s, db := newServer(t, http.StatusOK, requests)
		tm := time.Now().UTC()
		s.nowFunc = func() time.Time { return tm }
		if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,status,linear_issue_id,tags,stop_comment_pending,bootstrap_diagnostic,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, "comment", "tenant", "comment", "error", "ENG-1", `["factory:factory"]`, 1, "some reason", tm); err != nil {
			t.Fatal(err)
		}

		s.reapOnce()
		tm = tm.Add(redriveGrace + time.Second)
		s.reapOnce()

		deadline := time.After(time.Second)
		for {
			select {
			case request := <-requests:
				if strings.Contains(request, "commentCreate") {
					if !strings.Contains(request, "Agent stopped") || !strings.Contains(request, "some reason") {
						t.Fatalf("stop comment = %q, want agent-stopped comment with diagnostic", request)
					}
					var pending int
					for stop := time.Now().Add(time.Second); ; time.Sleep(10 * time.Millisecond) {
						if err := db.QueryRow(`SELECT stop_comment_pending FROM claws WHERE id='comment'`).Scan(&pending); err != nil {
							t.Fatal(err)
						}
						if pending == 0 {
							return
						}
						if time.Now().After(stop) {
							t.Fatalf("stop_comment_pending=%d, want 0", pending)
						}
					}
				}
			case <-deadline:
				t.Fatal("tracker did not receive stop comment")
			}
		}
	})

	t.Run("clears unresolved context without tracker request", func(t *testing.T) {
		requests := make(chan string, 1)
		s, db := newServer(t, http.StatusOK, requests)
		tm := time.Now().UTC()
		s.nowFunc = func() time.Time { return tm }
		if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,status,stop_comment_pending,bootstrap_diagnostic,created_at) VALUES(?,?,?,?,?,?,?)`, "unresolved", "tenant", "unresolved", "error", 1, "some reason", tm); err != nil {
			t.Fatal(err)
		}
		s.reapOnce()
		tm = tm.Add(redriveGrace + time.Second)
		s.reapOnce()

		var pending int
		if err := db.QueryRow(`SELECT stop_comment_pending FROM claws WHERE id='unresolved'`).Scan(&pending); err != nil || pending != 0 {
			t.Fatalf("pending=%d err=%v, want 0", pending, err)
		}
		select {
		case <-requests:
			t.Fatal("unresolved claw sent a tracker request")
		default:
		}
	})

	t.Run("retains pending when tracker fails", func(t *testing.T) {
		requests := make(chan string, 1)
		s, db := newServer(t, http.StatusInternalServerError, requests)
		tm := time.Now().UTC()
		s.nowFunc = func() time.Time { return tm }
		if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,status,linear_issue_id,tags,stop_comment_pending,bootstrap_diagnostic,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, "failed", "tenant", "failed", "error", "ENG-2", `["factory:factory"]`, 1, "some reason", tm); err != nil {
			t.Fatal(err)
		}
		s.reapOnce()
		tm = tm.Add(redriveGrace + time.Second)
		s.reapOnce()

		select {
		case <-requests:
		case <-time.After(time.Second):
			t.Fatal("tracker did not receive failed delivery")
		}
		deadline := time.Now().Add(time.Second)
		for {
			var pending int
			if err := db.QueryRow(`SELECT stop_comment_pending FROM claws WHERE id='failed'`).Scan(&pending); err != nil {
				t.Fatal(err)
			}
			if pending == 1 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("stop_comment_pending=%d, want 1 after failed delivery", pending)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

func TestReaperRecoversStrandedTerminalPipelineStage(t *testing.T) {
	const pipelineYAML = `
stages:
  - id: work
    label: "Work"
    triggers:
      - message_contains: "[START]"
  - id: done
    label: "Done"
    triggers:
      - message_contains: "[DONE]"
    terminal: true
`
	newServer := func(t *testing.T) (*Server, *sql.DB, func(time.Duration)) {
		t.Helper()
		s, db := newReaperTestServer(t, &types.HubConfig{Factories: []*types.FactoryConfig{{
			Name: "factory", Integration: "linear", Workspace: "workspace", PipelineYAML: pipelineYAML,
		}}})
		s.cronScheduler = newCronScheduler(s)
		tm := time.Now().UTC()
		s.nowFunc = func() time.Time { return tm }
		if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,status,pipeline_stage,tags,created_at) VALUES('stranded','tenant','stranded','connected','done',?,?)`, `["factory:factory"]`, tm); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO workflow_runs(id,tenant_id,workflow_name,workspace_name,trigger_type,status,claw_id,run_context,started_at,created_at) VALUES('run-stranded','tenant','wf','workspace','cron','running','stranded','{}',?,?)`, tm, tm); err != nil {
			t.Fatal(err)
		}
		return s, db, func(d time.Duration) { tm = tm.Add(d) }
	}

	t.Run("completes after grace", func(t *testing.T) {
		s, db, advance := newServer(t)
		s.reapOnce()
		advance(terminalStageRecoveryGrace + time.Second)
		s.reapOnce()
		var clawStatus, runStatus string
		if err := db.QueryRow(`SELECT status FROM claws WHERE id='stranded'`).Scan(&clawStatus); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT status FROM workflow_runs WHERE id='run-stranded'`).Scan(&runStatus); err != nil {
			t.Fatal(err)
		}
		if clawStatus != "deleted" || runStatus != "completed" {
			t.Fatalf("claw=%q run=%q, want deleted/completed", clawStatus, runStatus)
		}
	})

	t.Run("leaves in-flight termination alone within grace", func(t *testing.T) {
		s, db, advance := newServer(t)
		s.reapOnce()
		advance(terminalStageRecoveryGrace - time.Second)
		s.reapOnce()
		var clawStatus, runStatus string
		if err := db.QueryRow(`SELECT status FROM claws WHERE id='stranded'`).Scan(&clawStatus); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT status FROM workflow_runs WHERE id='run-stranded'`).Scan(&runStatus); err != nil {
			t.Fatal(err)
		}
		if clawStatus != "connected" || runStatus != "running" {
			t.Fatalf("claw=%q run=%q, want connected/running (untouched within grace)", clawStatus, runStatus)
		}
	})
}
