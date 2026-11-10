package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func signalWorkflowYAML() string {
	return strings.Replace(cronWorkflowV2SkipYAML, "overlap_policy: skip", "overlap_policy: signal", 1)
}

func signalTestWorkflow(t *testing.T, raw string) *scheduledWorkflowV2 {
	t.Helper()
	wf := &types.WorkflowConfig{Name: "delivery", RawConfig: raw}
	trigger, ok := cronTriggerFromV2Workflow(wf)
	if !ok {
		t.Fatal("missing cron trigger")
	}
	return &scheduledWorkflowV2{
		workspace: &types.WorkspaceConfig{Name: "engineering", Files: map[string]string{"elasticclaw-config.yaml": cronWorkspaceV2YAML}},
		workflow:  wf, trigger: trigger, key: "engineering/delivery",
	}
}

func createSignalTestRun(t *testing.T, store *workflowv2.Store, id, raw string) workflowv2.Run {
	t.Helper()
	run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID: id, TenantID: "test-tenant-id", InitialClawID: "claw-" + id,
		WorkspaceYAML: []byte(cronWorkspaceV2YAML), WorkflowYAML: []byte(raw), TriggerType: "cron",
		Timeout: workflowV2RunTimeout(raw),
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestCronSchedulerV2SignalTicks(t *testing.T) {
	for _, transition := range []bool{false, true} {
		t.Run(fmt.Sprintf("transition=%t", transition), func(t *testing.T) {
			s, db := NewTestServerWithConfig(t, nil, "", "", "")
			store := workflowv2.NewStore(db)
			raw := signalWorkflowYAML()
			if transition {
				raw += "\ntransitions:\n  tick:\n    from: [s]\n    on: cron.tick\n    to: done\n"
			}
			run := createSignalTestRun(t, store, "run-signal", raw)
			cs := newCronSchedulerV2(s)
			sw := signalTestWorkflow(t, raw)
			ticks := 2
			if transition {
				ticks = 1
			}
			// Exercise simultaneous ticks against the same open run.
			var wg sync.WaitGroup
			for i := 0; i < ticks; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					status, err := cs.runWorkflow(sw, "test-tenant-id")
					if err != nil || status != workflowRunSignaled {
						t.Errorf("tick = %s, %v", status, err)
					}
				}()
			}
			wg.Wait()
			var count, receipts int
			if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_runs`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("runs = %d, err = %v", count, err)
			}
			if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_event_receipts r JOIN workflow_v2_events e ON e.id=r.event_id WHERE e.run_id=? AND e.kind='cron.tick' AND r.disposition='accepted'`, run.ID).Scan(&receipts); err != nil || receipts != ticks {
				t.Fatalf("accepted receipts = %d, err = %v", receipts, err)
			}
			updated, err := store.GetRun(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if transition {
				if updated.State != "done" || updated.StateVersion != run.StateVersion+1 {
					t.Fatalf("transition did not fire: %#v", updated)
				}
			} else if updated.State != run.State || updated.StateVersion != run.StateVersion {
				t.Fatalf("unmatched event changed state: %#v", updated)
			}
			history, err := cs.getRunHistory("engineering", "delivery", "test-tenant-id", 50)
			if err != nil || len(history) != ticks {
				t.Fatalf("history = %#v, %v", history, err)
			}
			slots := map[string]bool{}
			for _, row := range history {
				if row.Status != "signaled" || row.Result != "signaled" || row.FinishedAt == nil {
					t.Fatalf("history outcome = %#v", row)
				}
				targets := row.RunContext["target_run_ids"].([]interface{})
				if len(targets) != 1 || targets[0] != run.ID {
					t.Fatalf("targets = %v", targets)
				}
				slot := row.RunContext["slot_id"].(string)
				if slot == "" || slots[slot] {
					t.Fatalf("missing or reused slot: %q", slot)
				}
				slots[slot] = true
				if _, err := time.Parse(time.RFC3339, row.RunContext["scheduled_at"].(string)); err != nil {
					t.Fatal(err)
				}
				var payload string
				if err := db.QueryRow(`SELECT payload_json FROM workflow_v2_events WHERE id=?`, "cron.tick:"+slot+":"+run.ID).Scan(&payload); err != nil {
					t.Fatal(err)
				}
				var event map[string]interface{}
				if err := json.Unmarshal([]byte(payload), &event); err != nil || event["slot_id"] != slot || event["scheduled_at"] != row.RunContext["scheduled_at"] {
					t.Fatalf("event payload = %s, %v", payload, err)
				}
			}
		})
	}
}

// Terminal cleanup for a run a tick completes runs off the signal lock, but
// stop must still wait for it so shutdown never closes the database under it.
func TestCronSchedulerV2StopWaitsForTickTerminalCleanup(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	store := workflowv2.NewStore(db)
	raw := signalWorkflowYAML() + "\ntransitions:\n  tick:\n    from: [s]\n    on: cron.tick\n    to: done\n"
	run := createSignalTestRun(t, store, "run-signal", raw)
	cs := newCronSchedulerV2(s)
	s.cronSchedulerV2 = cs // terminal cleanup releases the slot through it
	if status, err := cs.runWorkflow(signalTestWorkflow(t, raw), "test-tenant-id"); err != nil || status != workflowRunSignaled {
		t.Fatalf("tick = %s, %v", status, err)
	}
	cs.stop()
	var released int
	if err := db.QueryRow(`SELECT cron_slot_released FROM workflow_v2_runs WHERE id=?`, run.ID).Scan(&released); err != nil || released != 1 {
		t.Fatalf("cron_slot_released = %d, %v; stop returned before terminal cleanup", released, err)
	}
}

// A manual trigger that outlives the HTTP drain may still be signaling when
// stop runs: stop must wait for admitted signal work and refuse new work so
// nothing writes after the hub closes the database.
func TestCronSchedulerV2StopWaitsForAdmittedSignalAndRefusesNew(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	store := workflowv2.NewStore(db)
	raw := signalWorkflowYAML() + "\ntransitions:\n  tick:\n    from: [s]\n    on: cron.tick\n    to: done\n"
	run := createSignalTestRun(t, store, "run-signal", raw)
	cs := newCronSchedulerV2(s)
	s.cronSchedulerV2 = cs
	if !cs.admit() {
		t.Fatal("admit refused before stop")
	}
	stopped := make(chan struct{})
	go func() {
		cs.stop()
		close(stopped)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		cs.inflightMu.Lock()
		stopping := cs.stopping
		cs.inflightMu.Unlock()
		if stopping {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stop never began")
		}
		time.Sleep(time.Millisecond)
	}
	if status, err := cs.runWorkflow(signalTestWorkflow(t, raw), "test-tenant-id"); !errors.Is(err, errCronSchedulerStopping) || status != workflowRunFailed {
		t.Fatalf("tick after stop = %s, %v; want refusal", status, err)
	}
	select {
	case <-stopped:
		t.Fatal("stop returned while signal work was still admitted")
	case <-time.After(50 * time.Millisecond):
	}
	cs.inflight.Done()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not return after admitted work finished")
	}
	var events, ticks int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE run_id=? AND kind='cron.tick'`, run.ID).Scan(&events); err != nil || events != 0 {
		t.Fatalf("cron.tick events = %d, %v; refused tick wrote an event", events, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_cron_runs`).Scan(&ticks); err != nil || ticks != 0 {
		t.Fatalf("cron history rows = %d, %v; refused tick was recorded", ticks, err)
	}
}

func TestCronSchedulerV2SignalTargetsOpenRunsOnly(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	store := workflowv2.NewStore(db)
	raw := signalWorkflowYAML()
	for _, status := range []string{"active", "suspended", "completed", "cancelled"} {
		run := createSignalTestRun(t, store, "run-"+status, raw)
		if _, err := db.Exec(`UPDATE workflow_v2_runs SET status=? WHERE id=?`, status, run.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, field := range []string{"workspace_name", "workflow_name"} {
		run := createSignalTestRun(t, store, "other-"+field, raw)
		if _, err := db.Exec(`UPDATE workflow_v2_runs SET `+field+`='other' WHERE id=?`, run.ID); err != nil {
			t.Fatal(err)
		}
	}
	cs := newCronSchedulerV2(s)
	status, err := cs.runWorkflow(signalTestWorkflow(t, raw), "test-tenant-id")
	if err != nil || status != workflowRunSignaled {
		t.Fatalf("tick = %s, %v", status, err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE kind='cron.tick'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("events = %d, %v", count, err)
	}
	history, err := cs.getRunHistory("engineering", "delivery", "test-tenant-id", 50)
	if err != nil || len(history) != 1 {
		t.Fatalf("history = %#v, %v", history, err)
	}
	targets := history[0].RunContext["target_run_ids"].([]interface{})
	if len(targets) != 2 || targets[0] != "run-active" || targets[1] != "run-suspended" {
		t.Fatalf("targets = %v", targets)
	}
}

// Tenant scoping matches countActiveV2Runs: an open run recorded under another
// tenant is still signaled instead of letting the tick start a second run.
func TestCronSchedulerV2SignalIgnoresRunTenant(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	store := workflowv2.NewStore(db)
	raw := signalWorkflowYAML()
	run := createSignalTestRun(t, store, "run-other-tenant", raw)
	if _, err := db.Exec(`UPDATE workflow_v2_runs SET tenant_id='other' WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	status, err := newCronSchedulerV2(s).runWorkflow(signalTestWorkflow(t, raw), "test-tenant-id")
	if err != nil || status != workflowRunSignaled {
		t.Fatalf("tick = %s, %v", status, err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_runs`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("runs = %d, %v", count, err)
	}
}

// A run left pending activation (e.g. by a restart) is neither signaled nor
// duplicated by a new run.
func TestCronSchedulerV2SignalSkipsActivationPendingRun(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	raw := signalWorkflowYAML()
	_, err := workflowv2.NewStore(db).CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID: "run-pending", TenantID: "test-tenant-id", InitialClawID: "claw-run-pending",
		WorkspaceYAML: []byte(cronWorkspaceV2YAML), WorkflowYAML: []byte(raw), TriggerType: "cron",
		ActivationPending: true, Timeout: workflowV2RunTimeout(raw),
	})
	if err != nil {
		t.Fatal(err)
	}
	status, err := newCronSchedulerV2(s).runWorkflow(signalTestWorkflow(t, raw), "test-tenant-id")
	if err != nil || status != workflowRunSkipped {
		t.Fatalf("tick = %s, %v", status, err)
	}
	var runs, events int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_runs`).Scan(&runs); err != nil || runs != 1 {
		t.Fatalf("runs = %d, %v", runs, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE kind='cron.tick'`).Scan(&events); err != nil || events != 0 {
		t.Fatalf("events = %d, %v", events, err)
	}
}

// A matching cron.tick transition resumes a suspended run.
func TestCronSchedulerV2SignalResumesSuspendedRun(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	store := workflowv2.NewStore(db)
	raw := signalWorkflowYAML() + "\ntransitions:\n  tick:\n    from: [s]\n    on: cron.tick\n    to: done\n"
	run := createSignalTestRun(t, store, "run-suspended", raw)
	if _, err := db.Exec(`UPDATE workflow_v2_runs SET status='suspended', waiting_reason='awaiting review' WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	status, err := newCronSchedulerV2(s).runWorkflow(signalTestWorkflow(t, raw), "test-tenant-id")
	if err != nil || status != workflowRunSignaled {
		t.Fatalf("tick = %s, %v", status, err)
	}
	updated, err := store.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != "done" || updated.Status == workflowv2.RunSuspended {
		t.Fatalf("suspended run not resumed: state=%s status=%s", updated.State, updated.Status)
	}
}

func TestCronSchedulerV2SignalManualTriggerStartsThenSignals(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		Token: "test-token", ClawToken: "claw-token",
		Providers: map[string]types.ProviderConfig{"noop": {Type: "noop"}},
	}, "", "", "")
	sw := signalTestWorkflow(t, signalWorkflowYAML())
	SaveWorkspaceForTest(t, sw.workspace, []*types.WorkflowConfig{sw.workflow})
	s.cronSchedulerV2 = newCronSchedulerV2(s)
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/workspaces/engineering/workflows/delivery/cron/trigger", nil)
		req.Header.Set("Authorization", "Bearer test-token")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("trigger %d = %d: %s", i, rr.Code, rr.Body.String())
		}
		var resp map[string]string
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		want := "signaled"
		if i == 0 {
			want = "triggered"
		}
		if resp["status"] != want {
			t.Fatalf("trigger %d status = %q, want %q", i, resp["status"], want)
		}
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_runs`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("runs = %d, %v", count, err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces/engineering/workflows/delivery/cron/runs", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	var history struct {
		Runs []types.WorkflowRun `json:"runs"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &history); err != nil || rr.Code != http.StatusOK {
		t.Fatalf("history = %s, %v", rr.Body.String(), err)
	}
	var signaled, running int
	for _, row := range history.Runs {
		switch row.Status {
		case "signaled":
			signaled++
			if len(row.RunContext["target_run_ids"].([]interface{})) != 1 {
				t.Fatalf("missing targets: %#v", row)
			}
		case "running":
			running++
		}
	}
	if signaled != 2 || running != 1 {
		t.Fatalf("history: running=%d signaled=%d", running, signaled)
	}
	// A closed run no longer receives ticks, even if its in-memory slot has
	// not yet been released by terminal cleanup.
	var runID string
	if err := db.QueryRow(`SELECT id FROM workflow_v2_runs`).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if err := workflowv2.NewStore(db).CancelActivation(context.Background(), runID, "test completed"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.cronSchedulerV2.manualTrigger("engineering", "delivery", "test-tenant-id"); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_runs`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("runs after closing = %d, %v", count, err)
	}
}

// A tick the run rejects (here: the destination invariant fails) suspends the
// run. History still records it as signaled, but lists the run under
// rejected_run_ids so the suspension is visible from the cron history.
func TestCronSchedulerV2SignalRecordsRejectedRuns(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	store := workflowv2.NewStore(db)
	raw := signalWorkflowYAML() + "\ntransitions:\n  tick:\n    from: [s]\n    on: cron.tick\n    to: bad\n"
	raw = strings.Replace(raw, "states:\n", "states:\n  bad:\n    phase: build\n    invariant:\n      impossible:\n        equals: true\n", 1)
	run := createSignalTestRun(t, store, "run-reject", raw)
	cs := newCronSchedulerV2(s)
	sw := signalTestWorkflow(t, raw)

	status, err := cs.runWorkflow(sw, "test-tenant-id")
	if err != nil || status != workflowRunSignaled {
		t.Fatalf("tick = %s, %v", status, err)
	}
	if got, err := store.GetRun(context.Background(), run.ID); err != nil || got.Status != workflowv2.RunSuspended {
		t.Fatalf("run after rejected tick = %#v, %v", got, err)
	}
	history, err := cs.getRunHistory("engineering", "delivery", "test-tenant-id", 50)
	if err != nil || len(history) != 1 {
		t.Fatalf("history = %#v, %v", history, err)
	}
	if history[0].Status != string(workflowRunSignaled) {
		t.Fatalf("history status = %q, want signaled", history[0].Status)
	}
	if rejected, _ := history[0].RunContext["rejected_run_ids"].([]interface{}); len(rejected) != 1 || rejected[0] != run.ID {
		t.Fatalf("rejected_run_ids = %v", history[0].RunContext["rejected_run_ids"])
	}
}

func TestCronSchedulerV2ParallelStillStartsRuns(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		Token: "test-token", ClawToken: "claw-token",
		Providers: map[string]types.ProviderConfig{"noop": {Type: "noop"}},
	}, "", "", "")
	raw := strings.Replace(signalWorkflowYAML(), "overlap_policy: signal", "overlap_policy: parallel", 1)
	sw := signalTestWorkflow(t, raw)
	cs := newCronSchedulerV2(s)
	for i := 0; i < 2; i++ {
		status, err := cs.runWorkflow(sw, "test-tenant-id")
		if err != nil || status != workflowRunStarted {
			t.Fatalf("tick = %s, %v", status, err)
		}
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_runs`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("runs = %d, %v", count, err)
	}
}

// A run selected by openV2RunIDs can close before its tick is applied (reaper,
// manual stop). The tick must not revive it, and a tick whose every target
// closed reports nothing delivered so the caller starts a new run instead.
func TestCronSchedulerV2SignalSkipsRunClosedAfterSelection(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	store := workflowv2.NewStore(db)
	raw := signalWorkflowYAML() + "\ntransitions:\n  tick:\n    from: [s]\n    on: cron.tick\n    to: s2\n"
	raw = strings.Replace(raw, "states:\n", "states:\n  s2:\n    phase: build\n", 1)
	open := createSignalTestRun(t, store, "run-open", raw)
	closed := createSignalTestRun(t, store, "run-closed", raw)
	cs := newCronSchedulerV2(s)
	sw := signalTestWorkflow(t, raw)
	ids, _, err := cs.openV2RunIDs("engineering", "delivery")
	if err != nil || len(ids) != 2 {
		t.Fatalf("open runs = %v, %v", ids, err)
	}
	if err := store.CancelActivation(context.Background(), closed.ID, "stopped"); err != nil {
		t.Fatal(err)
	}

	status, targets, err := cs.signalRuns(store, sw, "test-tenant-id", ids, time.Now().UTC())
	if err != nil || len(targets) != 1 || targets[0] != open.ID || status != workflowRunSignaled {
		t.Fatalf("signal = %s, %v, %v", status, targets, err)
	}
	if run, err := store.GetRun(context.Background(), closed.ID); err != nil || run.Status != workflowv2.RunCancelled {
		t.Fatalf("closed run revived: %#v, %v", run, err)
	}
	if run, err := store.GetRun(context.Background(), open.ID); err != nil || run.State != "s2" {
		t.Fatalf("open run not signaled: %#v, %v", run, err)
	}
	history, err := cs.getRunHistory("engineering", "delivery", "test-tenant-id", 50)
	if err != nil || len(history) != 1 {
		t.Fatalf("history = %#v, %v", history, err)
	}
	if targets := history[0].RunContext["target_run_ids"].([]interface{}); len(targets) != 1 || targets[0] != open.ID {
		t.Fatalf("targets = %v", targets)
	}

	if err := store.CancelActivation(context.Background(), open.ID, "stopped"); err != nil {
		t.Fatal(err)
	}
	status, targets, err = cs.signalRuns(store, sw, "test-tenant-id", ids, time.Now().UTC())
	if err != nil || len(targets) != 0 {
		t.Fatalf("signal to closed runs = %s, %v, %v", status, targets, err)
	}
	var events int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE kind='cron.tick'`).Scan(&events); err != nil || events != 1 {
		t.Fatalf("cron.tick events = %d, %v", events, err)
	}
	if history, err := cs.getRunHistory("engineering", "delivery", "test-tenant-id", 50); err != nil || len(history) != 1 {
		t.Fatalf("history after closed tick = %#v, %v", history, err)
	}
}

// Concurrent first ticks with no open run start exactly one run; the other
// tick signals it.
func TestCronSchedulerV2SignalConcurrentFirstTicksStartOneRun(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		Token: "test-token", ClawToken: "claw-token",
		Providers: map[string]types.ProviderConfig{"noop": {Type: "noop"}},
	}, "", "", "")
	sw := signalTestWorkflow(t, signalWorkflowYAML())
	cs := newCronSchedulerV2(s)
	statuses := make(chan workflowRunStartStatus, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, err := cs.runWorkflow(sw, "test-tenant-id")
			if err != nil {
				t.Errorf("tick: %v", err)
			}
			statuses <- status
		}()
	}
	wg.Wait()
	close(statuses)
	got := map[workflowRunStartStatus]int{}
	for status := range statuses {
		got[status]++
	}
	if got[workflowRunStarted] != 1 || got[workflowRunSignaled] != 1 {
		t.Fatalf("statuses = %v", got)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_runs`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("runs = %d, %v", count, err)
	}
}

// The UI's manual trigger (/trigger, not /cron/trigger) must follow the
// signal policy too: once a run is open, a manual trigger ticks it instead of
// starting a duplicate.
func TestWorkflowV2ManualTriggerFollowsSignalPolicy(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		Token: "test-token", ClawToken: "claw-token",
		Providers: map[string]types.ProviderConfig{"noop": {Type: "noop"}},
	}, "", "", "")
	raw := strings.Replace(signalWorkflowYAML(), "enabled: true\n", "enabled: true\nmanual_trigger: true\n", 1)
	sw := signalTestWorkflow(t, raw)
	SaveWorkspaceForTest(t, sw.workspace, []*types.WorkflowConfig{sw.workflow})
	s.cronSchedulerV2 = newCronSchedulerV2(s)
	var runID string
	for i, want := range []string{"created", "signaled", "signaled"} {
		req := httptest.NewRequest(http.MethodPost, "/api/workspaces/engineering/workflows/delivery/trigger", strings.NewReader(`{"inputs":{}}`))
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("trigger %d = %d: %s", i, rr.Code, rr.Body.String())
		}
		var resp map[string]interface{}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp["status"] != want {
			t.Fatalf("trigger %d status = %q, want %q", i, resp["status"], want)
		}
		if i == 0 {
			runID, _ = resp["run_id"].(string)
			continue
		}
		if targets, _ := resp["target_run_ids"].([]interface{}); len(targets) != 1 || targets[0] != runID {
			t.Fatalf("trigger %d target_run_ids = %v, want [%s]", i, resp["target_run_ids"], runID)
		}
	}
	var runs, ticks int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_runs`).Scan(&runs); err != nil || runs != 1 {
		t.Fatalf("runs = %d, %v", runs, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE kind='cron.tick'`).Scan(&ticks); err != nil || ticks != 2 {
		t.Fatalf("cron.tick events = %d, %v", ticks, err)
	}
}
