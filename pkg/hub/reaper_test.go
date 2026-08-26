package hub

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
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

func TestReaperPipelineStageTimeout(t *testing.T) {
	const pipelineYAML = `
stages:
  - id: work
    triggers:
      - stage_timeout:
          after: 10m
          go_to: timed_out
  - id: timed_out
  - id: ordinary
`
	newServer := func(t *testing.T, enteredAt time.Duration, enabled *bool) (*Server, *sql.DB) {
		t.Helper()
		s, db := newReaperTestServer(t, &types.HubConfig{Liveness: &types.LivenessConfig{StageTimeoutEnabled: enabled}, Factories: []*types.FactoryConfig{{Name: "factory", Integration: "linear", Workspace: "workspace", PipelineYAML: pipelineYAML}}})
		n := time.Now().UTC()
		s.nowFunc = func() time.Time { return n }
		if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,status,pipeline_stage,pipeline_stage_entered_at,tags,created_at) VALUES('timeout01','tenant','timeout','connected','work',?,?,?)`, n.Add(enteredAt).UnixMilli(), `["factory:factory"]`, n); err != nil {
			t.Fatal(err)
		}
		return s, db
	}
	stage := func(t *testing.T, db *sql.DB) string {
		t.Helper()
		var got string
		if err := db.QueryRow(`SELECT pipeline_stage FROM claws WHERE id='timeout01'`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	// The A5 fix dispatches the stage-timeout transition and its on_enter work
	// off the reaper tick (see reapPipelineStageTimeouts' safeGo call), so the
	// stage change and event row are no longer visible synchronously right
	// after reapOnce() returns. Poll briefly for the async effect instead of
	// asserting immediately.
	waitForStage := func(t *testing.T, db *sql.DB, want string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		var got string
		for time.Now().Before(deadline) {
			got = stage(t, db)
			if got == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("stage = %q, want %q (timed out waiting)", got, want)
	}
	t.Run("transitions after timeout and does not refire", func(t *testing.T) {
		s, db := newServer(t, -11*time.Minute, nil)
		n := s.reaperNow().UnixMilli()
		if _, err := db.Exec(`INSERT INTO task_runs(id,tenant_id,initial_attempt_id,current_attempt_id,run_kind,owner_type,claw_id,created_at,updated_at) VALUES('run-timeout','tenant','attempt-timeout','attempt-timeout','code_task','factory','timeout01',?,?)`, n, n); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE claws SET task_run_id='run-timeout' WHERE id='timeout01'`); err != nil {
			t.Fatal(err)
		}
		s.reapOnce()
		waitForStage(t, db, "timed_out")
		var events int
		if err := db.QueryRow(`SELECT COUNT(*) FROM task_run_events WHERE run_id='run-timeout' AND event_type=?`, taskRunEventStageTimeout).Scan(&events); err != nil || events != 1 {
			t.Fatalf("stage timeout events = %d, err=%v; want 1", events, err)
		}
		s.reapOnce()
		// The second tick must not re-fire; give any (incorrect) async
		// re-transition a moment to land before asserting it didn't.
		time.Sleep(50 * time.Millisecond)
		if got := stage(t, db); got != "timed_out" {
			t.Fatalf("stale timeout changed stage to %q", got)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM task_run_events WHERE run_id='run-timeout' AND event_type=?`, taskRunEventStageTimeout).Scan(&events); err != nil || events != 1 {
			t.Fatalf("stage timeout events after stale tick = %d, err=%v; want 1", events, err)
		}
	})
	t.Run("does not transition within window", func(t *testing.T) {
		s, db := newServer(t, -9*time.Minute, nil)
		s.reapOnce()
		if got := stage(t, db); got != "work" {
			t.Fatalf("stage = %q, want work", got)
		}
	})
	t.Run("does not redirect a claw that already advanced", func(t *testing.T) {
		s, db := newServer(t, -11*time.Minute, nil)
		if _, err := db.Exec(`UPDATE claws SET pipeline_stage='ordinary' WHERE id='timeout01'`); err != nil {
			t.Fatal(err)
		}
		s.reapOnce()
		if got := stage(t, db); got != "ordinary" {
			t.Fatalf("stale timeout changed stage to %q, want ordinary", got)
		}
	})
	t.Run("respects liveness kill switch", func(t *testing.T) {
		disabled := false
		s, db := newServer(t, -11*time.Minute, &disabled)
		s.reapOnce()
		if got := stage(t, db); got != "work" {
			t.Fatalf("stage = %q, want work", got)
		}
	})
	t.Run("claim commits disposition atomically before on_enter", func(t *testing.T) {
		s, db := newServer(t, -11*time.Minute, nil)
		n := s.reaperNow().UnixMilli()
		if _, err := db.Exec(`INSERT INTO task_runs(id,tenant_id,initial_attempt_id,current_attempt_id,run_kind,owner_type,claw_id,created_at,updated_at) VALUES('run-timeout','tenant','attempt-timeout','attempt-timeout','code_task','factory','timeout01',?,?)`, n, n); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE claws SET task_run_id='run-timeout' WHERE id='timeout01'`); err != nil {
			t.Fatal(err)
		}
		if !s.claimPipelineStageTimeout("timeout01", "work", "timed_out", 10*time.Minute) {
			t.Fatal("claimPipelineStageTimeout returned false")
		}
		if got := stage(t, db); got != "timed_out" {
			t.Fatalf("stage=%q, want timed_out", got)
		}
		var events, count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM task_run_events WHERE run_id='run-timeout' AND event_type=?`, taskRunEventStageTimeout).Scan(&events); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT stage_timeout_count FROM task_run_summaries WHERE run_id='run-timeout'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if events != 1 || count != 1 {
			t.Fatalf("events=%d count=%d, want 1/1", events, count)
		}
	})
	t.Run("claim rolls back when no task run can receive disposition", func(t *testing.T) {
		s, db := newServer(t, -11*time.Minute, nil)
		if s.claimPipelineStageTimeout("timeout01", "work", "timed_out", 10*time.Minute) {
			t.Fatal("claim succeeded without a task run")
		}
		if got := stage(t, db); got != "work" {
			t.Fatalf("stage=%q, want rollback to work", got)
		}
	})
	t.Run("timeout records absent signal contract", func(t *testing.T) {
		s, db := newServer(t, -11*time.Minute, nil)
		n := s.reaperNow().UnixMilli()
		if _, err := db.Exec(`INSERT INTO task_runs(id,tenant_id,initial_attempt_id,current_attempt_id,run_kind,owner_type,claw_id,created_at,updated_at) VALUES('run-timeout','tenant','attempt-timeout','attempt-timeout','code_task','factory','timeout01',?,?)`, n, n); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE claws SET task_run_id='run-timeout' WHERE id='timeout01'`); err != nil {
			t.Fatal(err)
		}
		s.reapOnce()
		waitForStage(t, db, "timed_out")
		for _, eventType := range []string{taskRunEventSignalAdvanceCause, taskRunEventSignalEmission} {
			var raw string
			if err := db.QueryRow(`SELECT detail FROM task_run_events WHERE run_id='run-timeout' AND event_type=?`, eventType).Scan(&raw); err != nil {
				t.Fatalf("%s: %v", eventType, err)
			}
			var detail map[string]any
			if err := json.Unmarshal([]byte(raw), &detail); err != nil {
				t.Fatal(err)
			}
			if eventType == taskRunEventSignalEmission && detail["emission"] != "absent" {
				t.Fatalf("emission=%v, want absent", detail["emission"])
			}
		}
	})
}

func TestReaperPipelineStageTimeoutClampsAndCachesParsedPipelines(t *testing.T) {
	const pipelineYAML = "stages:\n  - id: work\n    triggers:\n      - stage_timeout:\n          after: 10m\n          go_to: timed_out\n  - id: timed_out\n"
	newServer := func(t *testing.T, enteredAt time.Duration) (*Server, *sql.DB) {
		t.Helper()
		s, db := newReaperTestServer(t, &types.HubConfig{Factories: []*types.FactoryConfig{{Name: "factory", Integration: "linear", Workspace: "workspace", PipelineYAML: pipelineYAML}}})
		n := time.Now().UTC()
		s.nowFunc = func() time.Time { return n }
		for i := 0; i < 5; i++ {
			if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,status,pipeline_stage,pipeline_stage_entered_at,tags,created_at) VALUES(?,?,?,?,?,?,?,?)`, fmt.Sprintf("timeout-%d", i), "tenant", "timeout", "connected", "work", n.Add(enteredAt).UnixMilli(), `["factory:factory"]`, n); err != nil {
				t.Fatal(err)
			}
		}
		return s, db
	}
	t.Run("min and max clamp effective timeout", func(t *testing.T) {
		s, db := newServer(t, -2*time.Minute)
		n := s.reaperNow().UnixMilli()
		if _, err := db.Exec(`INSERT INTO task_runs(id,tenant_id,initial_attempt_id,current_attempt_id,run_kind,owner_type,claw_id,created_at,updated_at) VALUES('run-clamp','tenant','attempt-clamp','attempt-clamp','code_task','factory','timeout-0',?,?)`, n, n); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE claws SET task_run_id='run-clamp' WHERE id='timeout-0'`); err != nil {
			t.Fatal(err)
		}
		s.reapPipelineStageTimeouts(s.reaperNow(), func() bool { return true }, map[string]bool{}, 5*time.Minute, 0)
		time.Sleep(50 * time.Millisecond)
		var stage string
		if err := db.QueryRow(`SELECT pipeline_stage FROM claws WHERE id='timeout-0'`).Scan(&stage); err != nil {
			t.Fatal(err)
		}
		if stage != "work" {
			t.Fatalf("min clamp stage=%q, want work", stage)
		}
		s.reapPipelineStageTimeouts(s.reaperNow(), func() bool { return true }, map[string]bool{}, 0, time.Minute)
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			_ = db.QueryRow(`SELECT pipeline_stage FROM claws WHERE id='timeout-0'`).Scan(&stage)
			if stage == "timed_out" {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("max clamp did not fire; stage=%q", stage)
	})
	t.Run("parses shared YAML once per tick", func(t *testing.T) {
		s, _ := newServer(t, -2*time.Minute)
		parses := 0
		s.reaperPipelineParse = func(ctx pipelineContext) *pipeline.Pipeline { parses++; return parsePipelineForContext(ctx) }
		s.reapPipelineStageTimeouts(s.reaperNow(), func() bool { return true }, map[string]bool{}, 5*time.Minute, 0)
		if parses != 1 {
			t.Fatalf("pipeline parses=%d, want 1 for five shared definitions", parses)
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
