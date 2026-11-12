package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
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
	// The promoted claw provisions asynchronously; the noop provider lets it
	// finish deterministically instead of racing a provisioning failure.
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	s, db := newReaperTestServer(t, &types.HubConfig{MaxConcurrentClaws: 1})
	tm := time.Now().UTC()
	insertQueueTestClaw(t, db, "active001", "connected", "global", tm)
	insertQueueTestClaw(t, db, "pending01", "pending", "global", tm)
	s.stopAgentWithReason("active001", "test failure", true)
	waitForQueueTestPromotion(t, db, "pending01")
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

func TestReaperCancelsTimedOutWorkflowV2Run(t *testing.T) {
	s, db := newReaperTestServer(t, &types.HubConfig{})
	tm := time.Now().UTC()
	s.nowFunc = func() time.Time { return tm }

	insertWorkflowV2Run(t, db, "run-timeout", "att-timeout", tm.Add(-time.Hour), tm.Add(-time.Hour))

	s.reapOnce()

	var status, attemptStatus string
	if err := db.QueryRow(`SELECT status FROM workflow_v2_runs WHERE id='run-timeout'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" {
		t.Fatalf("run status = %q, want cancelled", status)
	}
	if err := db.QueryRow(`SELECT status FROM workflow_v2_attempts WHERE id='att-timeout'`).Scan(&attemptStatus); err != nil {
		t.Fatal(err)
	}
	if attemptStatus != "cancelled" {
		t.Fatalf("attempt status = %q, want cancelled", attemptStatus)
	}
}

func TestReaperLeavesLegacyWorkflowV2RunWithoutTimeout(t *testing.T) {
	s, db := newReaperTestServer(t, &types.HubConfig{})
	tm := time.Now().UTC()
	s.nowFunc = func() time.Time { return tm }

	insertWorkflowV2Run(t, db, "run-legacy", "att-legacy", time.Time{}, tm.Add(-workflowV2DefaultRunTimeout-time.Hour))

	s.reapOnce()

	var status string
	if err := db.QueryRow(`SELECT status FROM workflow_v2_runs WHERE id='run-legacy'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("run status = %q, want active (legacy rows without timeout_at must not be auto-cancelled)", status)
	}
}

func TestReaperLeavesWorkflowV2RunWithinTimeout(t *testing.T) {
	s, db := newReaperTestServer(t, &types.HubConfig{})
	tm := time.Now().UTC()
	s.nowFunc = func() time.Time { return tm }

	insertWorkflowV2Run(t, db, "run-fresh", "att-fresh", tm.Add(time.Hour), tm)

	s.reapOnce()

	var status string
	if err := db.QueryRow(`SELECT status FROM workflow_v2_runs WHERE id='run-fresh'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("run status = %q, want active", status)
	}
}

func insertWorkflowV2Run(t *testing.T, db *sql.DB, runID, attemptID string, timeoutAt, createdAt time.Time) {
	t.Helper()
	timeoutMillis := int64(0)
	if !timeoutAt.IsZero() {
		timeoutMillis = timeoutAt.UnixMilli()
	}
	if _, err := db.Exec(`INSERT INTO workflow_v2_runs(
		id,tenant_id,workspace_name,workflow_name,workspace_revision,workflow_revision,
		workspace_yaml,workflow_yaml,state,display_phase,state_version,status,waiting_reason,
		current_attempt_id,task_run_id,trigger_type,timeout_at,created_at,updated_at,finished_at
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		runID, "tenant", "ws", "wf", "rev", "rev", "", "", "building", "build", 1, "active", "",
		attemptID, "", "manual", timeoutMillis, createdAt.UnixMilli(), createdAt.UnixMilli(), 0); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO workflow_v2_attempts(id,run_id,claw_id,number,status,started_at,heartbeat_at)
		VALUES(?,?,?,?,?,?,?)`,
		attemptID, runID, "claw-"+runID, 1, "active", createdAt.UnixMilli(), createdAt.UnixMilli()); err != nil {
		t.Fatalf("insert attempt: %v", err)
	}
}

func TestCancelWorkflowV2RunForClawCancelsActiveRun(t *testing.T) {
	s, db := newReaperTestServer(t, &types.HubConfig{})
	now := time.Now().UTC()
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,template,provider,status,created_at) VALUES(?,?,?,?,?,?,?)`,
		"claw-dead", "tenant", "dead", "ws", "replicated", "deleted", now); err != nil {
		t.Fatal(err)
	}
	insertWorkflowV2Run(t, db, "run-dead", "att-dead", now.Add(time.Hour), now)
	if _, err := db.Exec(`UPDATE workflow_v2_attempts SET claw_id=? WHERE id=?`, "claw-dead", "att-dead"); err != nil {
		t.Fatal(err)
	}

	s.cancelWorkflowV2RunForClaw(context.Background(), "claw-dead", "claw deleted")

	var status, attemptStatus string
	if err := db.QueryRow(`SELECT status FROM workflow_v2_runs WHERE id='run-dead'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" {
		t.Fatalf("run status = %q, want cancelled", status)
	}
	if err := db.QueryRow(`SELECT status FROM workflow_v2_attempts WHERE id='att-dead'`).Scan(&attemptStatus); err != nil {
		t.Fatal(err)
	}
	if attemptStatus != "cancelled" {
		t.Fatalf("attempt status = %q, want cancelled", attemptStatus)
	}
}

// TestFinishClawTerminalTxCancelsBoundWorkflowV2Run guards the #691 follow-up:
// terminal claw transitions (bootstrap failure, watchdog stop, reaper delete)
// must cancel an active workflow v2 run bound to the claw. Before the fix only
// the workflow-creator provisioning paths cancelled, so a bootstrap failure in
// the VM sync loop left the run active with a dead claw, holding task delivery
// until the 24h run reaper.
func TestFinishClawTerminalTxCancelsBoundWorkflowV2Run(t *testing.T) {
	s, db := newReaperTestServer(t, &types.HubConfig{})
	now := time.Now().UTC()
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,template,provider,status,created_at) VALUES(?,?,?,?,?,?,?)`,
		"claw-bootstrap-dead", "tenant", "dead", "ws", "replicated", "connected", now); err != nil {
		t.Fatal(err)
	}
	insertWorkflowV2Run(t, db, "run-terminal-cancel", "att-terminal-cancel", now.Add(time.Hour), now)
	if _, err := db.Exec(`UPDATE workflow_v2_attempts SET claw_id=? WHERE id=?`,
		"claw-bootstrap-dead", "att-terminal-cancel"); err != nil {
		t.Fatal(err)
	}

	applied, err := s.finishClawTerminalTx("claw-bootstrap-dead", "error",
		"Bootstrap failed: could not configure GitHub credentials", "failed",
		"Bootstrap failed: could not configure GitHub credentials", terminalTxOpts{})
	if err != nil || !applied {
		t.Fatalf("finishClawTerminalTx = applied:%v err:%v", applied, err)
	}

	var runStatus, attemptStatus, clawStatus string
	if err := db.QueryRow(`SELECT status FROM workflow_v2_runs WHERE id='run-terminal-cancel'`).Scan(&runStatus); err != nil {
		t.Fatal(err)
	}
	if runStatus != "cancelled" {
		t.Fatalf("run status = %q, want cancelled", runStatus)
	}
	if err := db.QueryRow(`SELECT status FROM workflow_v2_attempts WHERE id='att-terminal-cancel'`).Scan(&attemptStatus); err != nil {
		t.Fatal(err)
	}
	if attemptStatus != "cancelled" {
		t.Fatalf("attempt status = %q, want cancelled", attemptStatus)
	}
	if err := db.QueryRow(`SELECT status FROM claws WHERE id='claw-bootstrap-dead'`).Scan(&clawStatus); err != nil {
		t.Fatal(err)
	}
	if clawStatus != "error" {
		t.Fatalf("claw status = %q, want error", clawStatus)
	}
}

func TestReleaseWorkflowV2CronSlotLoadsBeforeMarker(t *testing.T) {
	s, db := newReaperTestServer(t, &types.HubConfig{})
	s.cronSchedulerV2 = newCronSchedulerV2(s)
	s.cronSchedulerV2.running["ws/wf"] = 1
	now := time.Now().UTC()

	if _, err := db.Exec(`INSERT INTO workflow_v2_runs(
		id,tenant_id,workspace_name,workflow_name,workspace_revision,workflow_revision,
		workspace_yaml,workflow_yaml,state,display_phase,state_version,status,waiting_reason,
		current_attempt_id,task_run_id,trigger_type,timeout_at,created_at,updated_at,finished_at,
		cron_slot_released
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"run-cron", "tenant", "ws", "wf", "rev", "rev", "", "", "done", "done", 1, "cancelled", "",
		"att-cron", "", "cron", 0, now.UnixMilli(), now.UnixMilli(), now.UnixMilli(), 0); err != nil {
		t.Fatal(err)
	}

	// A cancelled context should fail the run load before the marker is flipped,
	// so the slot can still be released on retry.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	s.releaseWorkflowV2CronSlot(cancelledCtx, "run-cron")

	var marker int
	if err := db.QueryRow(`SELECT cron_slot_released FROM workflow_v2_runs WHERE id='run-cron'`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker != 0 {
		t.Fatalf("cron_slot_released = %d, want 0 (marker consumed before load succeeded)", marker)
	}
	if s.cronSchedulerV2.running["ws/wf"] != 1 {
		t.Fatalf("running counter was decremented despite load failure")
	}

	// On a valid context the slot is released exactly once.
	s.releaseWorkflowV2CronSlot(context.Background(), "run-cron")
	if err := db.QueryRow(`SELECT cron_slot_released FROM workflow_v2_runs WHERE id='run-cron'`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker != 1 {
		t.Fatalf("cron_slot_released = %d, want 1", marker)
	}
	if s.cronSchedulerV2.running["ws/wf"] != 0 {
		t.Fatalf("running counter = %d, want 0", s.cronSchedulerV2.running["ws/wf"])
	}

	// Idempotent: a second call does not decrement below zero.
	s.releaseWorkflowV2CronSlot(context.Background(), "run-cron")
	if s.cronSchedulerV2.running["ws/wf"] != 0 {
		t.Fatalf("running counter decremented twice, got %d", s.cronSchedulerV2.running["ws/wf"])
	}
}

func TestReaperOfflineReplicatedVM(t *testing.T) {
	for _, tc := range []struct {
		name, provider, providerID string
		terminated                 bool
		lookupErr                  error
		wantLookup, wantRetryable  bool
	}{
		{"expired or missing", "replicated", "current-vm", true, nil, true, true},
		{"replacement running", "replicated", "replacement-vm", false, nil, true, false},
		{"lookup error", "replicated", "current-vm", false, errors.New("network error"), true, false},
		{"other provider", "daytona", "current-vm", true, nil, false, false},
		{"cleared provider ID", "replicated", "", true, nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, db := newReaperTestServer(t, &types.HubConfig{})
			tm := time.Now().UTC()
			s.nowFunc = func() time.Time { return tm }
			// Recovery keeps created_at even when it replaces the VM.
			if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,provider,provider_id,status,created_at) VALUES('offline-vm','tenant','offline',?,?,'offline',?)`, tc.provider, tc.providerID, tm.Add(-72*time.Hour)); err != nil {
				t.Fatal(err)
			}
			s.terminateVMOverride = func(string, string) error { return nil }
			calls := 0
			s.replicatedVMTerminatedOverride = func(ctx context.Context, id string, at time.Time) (bool, error) {
				calls++
				if id != tc.providerID || !at.Equal(tm) {
					t.Errorf("lookup = %q, %v; want %q, %v", id, at, tc.providerID, tm)
				}
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 10*time.Second {
					t.Error("lookup must have a deadline of at most 10 seconds")
				}
				return tc.terminated, tc.lookupErr
			}
			s.reapOnce()
			if calls != 0 {
				t.Fatal("looked up VM within offline grace")
			}
			tm = tm.Add(11 * time.Minute)
			s.reapOnce()
			if (calls == 1) != tc.wantLookup || calls > 1 {
				t.Fatalf("lookup calls = %d; want lookup=%v", calls, tc.wantLookup)
			}
			// Stopping clears provider_id, asynchronously when it terminates the
			// VM; wait so that write cannot outlive the test's database.
			var status, reason, providerID string
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
				if err := db.QueryRow(`SELECT status, bootstrap_diagnostic, COALESCE(provider_id,'') FROM claws WHERE id='offline-vm'`).Scan(&status, &reason, &providerID); err != nil {
					t.Fatal(err)
				}
				if providerID == "" || time.Now().After(deadline) {
					break
				}
			}
			if providerID != "" {
				t.Fatalf("provider_id = %q; want cleared after stop", providerID)
			}
			if status != "error" {
				t.Fatalf("status = %q; want error", status)
			}
			failure := classifyAgentFailure(reason)
			if tc.wantRetryable {
				if failure.Kind != agentFailureSandboxTerminated || !retryableAgentFailure(failure.Kind) {
					t.Fatalf("reason %q classified as %v; want retryable sandbox termination", reason, failure.Kind)
				}
			} else if reason != "agent offline for 10m0s, sandbox presumed dead" {
				t.Fatalf("unexpected fallback reason: %q", reason)
			}
		})
	}
}

func TestReaperOfflineVMLookupActionLimit(t *testing.T) {
	s, db := newReaperTestServer(t, &types.HubConfig{})
	tm := time.Now().UTC()
	s.nowFunc = func() time.Time { return tm }
	for i := 0; i < reaperActionLimit+1; i++ {
		id := fmt.Sprintf("offline-%02d", i)
		if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,provider,provider_id,status,created_at) VALUES(?,'tenant',?,'replicated',?,'offline',?)`, id, id, id, tm); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	s.replicatedVMTerminatedOverride = func(context.Context, string, time.Time) (bool, error) {
		calls++
		return false, nil
	}
	s.terminateVMOverride = func(string, string) error { return nil }
	s.reapOnce()
	tm = tm.Add(11 * time.Minute)
	s.reapOnce()
	var stopped int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claws WHERE status='error'`).Scan(&stopped); err != nil {
		t.Fatal(err)
	}
	if calls != reaperActionLimit || stopped != reaperActionLimit {
		t.Fatalf("lookups=%d, stopped=%d; want %d each", calls, stopped, reaperActionLimit)
	}
}

func TestReaperOfflineVMLookupSkipsClawChangedDuringLookup(t *testing.T) {
	for _, tc := range []struct{ name, update string }{
		{"reconnected", `UPDATE claws SET status='connected' WHERE id='offline-vm'`},
		{"replaced VM", `UPDATE claws SET provider_id='replacement-vm' WHERE id='offline-vm'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, db := newReaperTestServer(t, &types.HubConfig{})
			tm := time.Now().UTC()
			s.nowFunc = func() time.Time { return tm }
			if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,provider,provider_id,status,created_at) VALUES('offline-vm','tenant','offline','replicated','current-vm','offline',?)`, tm); err != nil {
				t.Fatal(err)
			}
			s.terminateVMOverride = func(string, string) error {
				t.Error("terminated VM of a claw that changed during lookup")
				return nil
			}
			calls := 0
			s.replicatedVMTerminatedOverride = func(context.Context, string, time.Time) (bool, error) {
				calls++
				if _, err := db.Exec(tc.update); err != nil {
					t.Error(err)
				}
				return true, nil
			}
			s.reapOnce()
			tm = tm.Add(11 * time.Minute)
			s.reapOnce()
			if calls != 1 {
				t.Fatalf("lookup calls = %d; want 1", calls)
			}
			var status, reason string
			if err := db.QueryRow(`SELECT status, COALESCE(bootstrap_diagnostic,'') FROM claws WHERE id='offline-vm'`).Scan(&status, &reason); err != nil {
				t.Fatal(err)
			}
			if status == "error" || reason != "" {
				t.Fatalf("claw stopped after changing during lookup: status=%q reason=%q", status, reason)
			}
		})
	}
}

func TestReaperOfflineVMLookupBudget(t *testing.T) {
	prev := reaperVMLookupBudget
	reaperVMLookupBudget = time.Millisecond
	t.Cleanup(func() { reaperVMLookupBudget = prev })
	s, db := newReaperTestServer(t, &types.HubConfig{})
	tm := time.Now().UTC()
	s.nowFunc = func() time.Time { return tm }
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("offline-%02d", i)
		if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,provider,provider_id,status,created_at) VALUES(?,'tenant',?,'replicated',?,'offline',?)`, id, id, id, tm); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	s.replicatedVMTerminatedOverride = func(ctx context.Context, _ string, _ time.Time) (bool, error) {
		calls++
		<-ctx.Done() // the first lookup spends the whole tick budget
		return false, ctx.Err()
	}
	s.terminateVMOverride = func(string, string) error { return nil }
	s.reapOnce()
	tm = tm.Add(11 * time.Minute)
	s.reapOnce()
	if calls != 1 {
		t.Fatalf("lookup calls = %d; want 1 before the budget is spent", calls)
	}
	// Fallback stops terminate the VM asynchronously and then clear provider_id;
	// wait so that write cannot outlive the test's database.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var pending int
		if err := db.QueryRow(`SELECT COUNT(*) FROM claws WHERE COALESCE(provider_id,'') != ''`).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending == 0 || time.Now().After(deadline) {
			break
		}
	}
	rows, err := db.Query(`SELECT id, status, bootstrap_diagnostic FROM claws`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, status, reason string
		if err := rows.Scan(&id, &status, &reason); err != nil {
			t.Fatal(err)
		}
		if status != "error" || reason != "agent offline for 10m0s, sandbox presumed dead" {
			t.Fatalf("claw %s: status=%q reason=%q; want fallback stop", id, status, reason)
		}
	}
}

func TestReaperOfflineVMLookupBudgetSpentRechecksLaterClaws(t *testing.T) {
	prev := reaperVMLookupBudget
	reaperVMLookupBudget = time.Millisecond
	t.Cleanup(func() { reaperVMLookupBudget = prev })
	s, db := newReaperTestServer(t, &types.HubConfig{})
	tm := time.Now().UTC()
	s.nowFunc = func() time.Time { return tm }
	for _, id := range []string{"offline-00", "offline-01"} {
		if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,provider,provider_id,status,created_at) VALUES(?,'tenant',?,'replicated',?,'offline',?)`, id, id, id, tm); err != nil {
			t.Fatal(err)
		}
	}
	s.replicatedVMTerminatedOverride = func(ctx context.Context, _ string, _ time.Time) (bool, error) {
		// Both claws reconnect while the first lookup spends the tick budget.
		if _, err := db.Exec(`UPDATE claws SET status='connected'`); err != nil {
			t.Error(err)
		}
		<-ctx.Done()
		return false, ctx.Err()
	}
	s.terminateVMOverride = func(string, string) error {
		t.Error("terminated VM of a claw that reconnected during the tick")
		return nil
	}
	s.reapOnce()
	tm = tm.Add(11 * time.Minute)
	s.reapOnce()
	var stopped int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claws WHERE status='error'`).Scan(&stopped); err != nil {
		t.Fatal(err)
	}
	if stopped != 0 {
		t.Fatalf("stopped %d reconnected claws; want 0", stopped)
	}
}

// TestDeleteClawCancelsBoundWorkflowV2Run guards the manual kill path (UI and
// `elasticclaw kill`): it soft-deletes the claw without finishClawTerminalTx,
// so it must cancel the bound v2 run itself or the run stays active forever.
func TestDeleteClawCancelsBoundWorkflowV2Run(t *testing.T) {
	s, db := newReaperTestServer(t, &types.HubConfig{})
	s.cronSchedulerV2 = newCronSchedulerV2(s)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, id := range []string{"claw-killed", "claw-other", "claw-plain"} {
		if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,template,provider,status,created_at) VALUES(?,?,?,?,?,?,?)`,
			id, "tenant", id, "ws", "replicated", "connected", now); err != nil {
			t.Fatal(err)
		}
	}
	insertWorkflowV2Run(t, db, "run-killed", "att-killed", time.Time{}, now)
	insertWorkflowV2Run(t, db, "run-other", "att-other", time.Time{}, now)
	if _, err := db.Exec(`UPDATE workflow_v2_attempts SET claw_id=? WHERE id=?`, "claw-killed", "att-killed"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE workflow_v2_attempts SET claw_id=? WHERE id=?`, "claw-other", "att-other"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE workflow_v2_runs SET trigger_type='cron' WHERE id='run-killed'`); err != nil {
		t.Fatal(err)
	}
	store := workflowv2.NewStore(db)
	cronRunID, err := store.RecordCronRunStarted(ctx, "tenant", "ws", "wf", "cron", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateCronRunForV2Run(ctx, cronRunID, "run-killed", "claw-killed"); err != nil {
		t.Fatal(err)
	}
	// Record the run status at the moment the claw becomes deleted: a cron
	// tick in between would be absorbed by a still-active run.
	if _, err := db.Exec(`CREATE TABLE delete_probe(run_status TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER probe_claw_delete AFTER UPDATE OF status ON claws
		WHEN NEW.status='deleted' AND NEW.id='claw-killed'
		BEGIN INSERT INTO delete_probe SELECT status FROM workflow_v2_runs WHERE id='run-killed'; END`); err != nil {
		t.Fatal(err)
	}

	if code := deleteClawForTest(t, s, db, "claw-killed"); code != http.StatusNoContent {
		t.Fatalf("delete status = %d", code)
	}
	if code := deleteClawForTest(t, s, db, "claw-plain"); code != http.StatusNoContent {
		t.Fatalf("delete of claw without a v2 run: status = %d", code)
	}

	for _, want := range []struct{ query, status string }{
		{`SELECT status FROM workflow_v2_runs WHERE id='run-killed'`, "cancelled"},
		{`SELECT status FROM workflow_v2_attempts WHERE id='att-killed'`, "cancelled"},
		{`SELECT run_status FROM delete_probe`, "cancelled"},
		{`SELECT status FROM workflow_v2_cron_runs WHERE id='` + cronRunID + `'`, "canceled"},
		{`SELECT status FROM workflow_v2_runs WHERE id='run-other'`, "active"},
		{`SELECT status FROM workflow_v2_attempts WHERE id='att-other'`, "active"},
		{`SELECT status FROM claws WHERE id='claw-plain'`, "deleted"},
	} {
		var got string
		if err := db.QueryRow(want.query).Scan(&got); err != nil {
			t.Fatalf("%s: %v", want.query, err)
		}
		if got != want.status {
			t.Fatalf("%s = %q, want %q", want.query, got, want.status)
		}
	}
}

// A kill whose v2 cancel fails must not soft-delete the claw: nothing would
// retry the cancel afterwards, so the run would stay active on a dead claw.
func TestDeleteClawKeepsClawWhenWorkflowV2CancelFails(t *testing.T) {
	s, db := newReaperTestServer(t, &types.HubConfig{})
	now := time.Now().UTC()
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,template,provider,status,created_at) VALUES(?,?,?,?,?,?,?)`,
		"claw-killed", "tenant", "killed", "ws", "replicated", "connected", now); err != nil {
		t.Fatal(err)
	}
	insertWorkflowV2Run(t, db, "run-killed", "att-killed", time.Time{}, now)
	if _, err := db.Exec(`UPDATE workflow_v2_attempts SET claw_id=? WHERE id=?`, "claw-killed", "att-killed"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_run_cancel BEFORE UPDATE OF status ON workflow_v2_runs
		WHEN NEW.status='cancelled' BEGIN SELECT RAISE(ABORT, 'db busy'); END`); err != nil {
		t.Fatal(err)
	}

	if code := deleteClawForTest(t, s, db, "claw-killed"); code != http.StatusInternalServerError {
		t.Fatalf("delete status = %d, want 500", code)
	}
	var clawStatus, runStatus string
	if err := db.QueryRow(`SELECT c.status, r.status FROM claws c, workflow_v2_runs r WHERE c.id='claw-killed' AND r.id='run-killed'`).Scan(&clawStatus, &runStatus); err != nil {
		t.Fatal(err)
	}
	if clawStatus != "connected" || runStatus != "active" {
		t.Fatalf("after failed cancel claw=%q run=%q; want connected/active", clawStatus, runStatus)
	}

	if _, err := db.Exec(`DROP TRIGGER fail_run_cancel`); err != nil {
		t.Fatal(err)
	}
	if code := deleteClawForTest(t, s, db, "claw-killed"); code != http.StatusNoContent {
		t.Fatalf("retried delete status = %d", code)
	}
	if err := db.QueryRow(`SELECT c.status, r.status FROM claws c, workflow_v2_runs r WHERE c.id='claw-killed' AND r.id='run-killed'`).Scan(&clawStatus, &runStatus); err != nil {
		t.Fatal(err)
	}
	if clawStatus != "deleted" || runStatus != "cancelled" {
		t.Fatalf("after retried delete claw=%q run=%q; want deleted/cancelled", clawStatus, runStatus)
	}
}

// deleteClawForTest kills a claw through the handler. After a successful kill
// it waits for the async cleanup, whose last step deletes the claw's PR rows,
// so the goroutine never outlives the test's database and temp dir.
func deleteClawForTest(t *testing.T, s *Server, db *sql.DB, clawID string) int {
	t.Helper()
	if _, err := db.Exec(`INSERT OR REPLACE INTO claw_prs(id,claw_id,repo,pr_number,pr_url,created_at) VALUES(?,?,?,?,?,?)`,
		"kill-sentinel-"+clawID, clawID, "acme/app", 1, "https://github.com/acme/app/pull/1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/claws/"+clawID, nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxTenantKey{}, "tenant"))
	req.SetPathValue("id", clawID)
	rec := httptest.NewRecorder()
	s.handleClawDetail(rec, req)
	if rec.Code == http.StatusNoContent {
		deadline := time.Now().Add(5 * time.Second)
		for {
			var n int
			if err := db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id=?`, clawID).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("kill cleanup for %s did not finish", clawID)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	return rec.Code
}

// A kill retried after a failed v2 cancel must comment the linked story once:
// the comment is only posted after the delete commits.
func TestDeleteClawCommentsTrackerOnceAcrossRetriedKill(t *testing.T) {
	var comments atomic.Int32
	shortcut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") {
			comments.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer shortcut.Close()
	factory := taskRunLifecycleFactoryConfig("kill-factory", "shortcut")
	s, db := newReaperTestServer(t, &types.HubConfig{
		Factories:    []*types.FactoryConfig{factory},
		Integrations: &types.IntegrationsConfig{Shortcut: []*types.ShortcutIntegrationConfig{{Workspace: factory.Workspace, Token: "token"}}},
	})
	s.shortcutBaseURL = shortcut.URL
	now := time.Now().UTC()
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,template,provider,status,created_at,shortcut_story_id,tags) VALUES(?,?,?,?,?,?,?,?,?)`,
		"claw-killed", "tenant", "killed", "ws", "replicated", "connected", now, "42", `["factory:kill-factory"]`); err != nil {
		t.Fatal(err)
	}
	insertWorkflowV2Run(t, db, "run-killed", "att-killed", time.Time{}, now)
	if _, err := db.Exec(`UPDATE workflow_v2_attempts SET claw_id=? WHERE id=?`, "claw-killed", "att-killed"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_run_cancel BEFORE UPDATE OF status ON workflow_v2_runs
		WHEN NEW.status='cancelled' BEGIN SELECT RAISE(ABORT, 'db busy'); END`); err != nil {
		t.Fatal(err)
	}
	if code := deleteClawForTest(t, s, db, "claw-killed"); code != http.StatusInternalServerError {
		t.Fatalf("delete status = %d, want 500", code)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_run_cancel`); err != nil {
		t.Fatal(err)
	}
	if code := deleteClawForTest(t, s, db, "claw-killed"); code != http.StatusNoContent {
		t.Fatalf("retried delete status = %d", code)
	}
	if got := comments.Load(); got != 1 {
		t.Fatalf("tracker comments = %d, want 1", got)
	}
}
