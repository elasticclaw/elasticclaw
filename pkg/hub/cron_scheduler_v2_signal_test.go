package hub

import (
	"context"
	"encoding/json"
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
	for _, field := range []string{"workspace_name", "workflow_name", "tenant_id"} {
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
	if _, err := s.cronSchedulerV2.manualTrigger("engineering", "delivery", "test-tenant-id"); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_runs`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("runs after closing = %d, %v", count, err)
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
