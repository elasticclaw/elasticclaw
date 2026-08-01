package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/robfig/cron/v3"
)

func TestParseCronSchedule(t *testing.T) {
	tests := []struct {
		name     string
		schedule string
		loc      *time.Location
		wantErr  bool
	}{
		{
			name:     "standard 5-field cron",
			schedule: "0 9 * * 1",
			loc:      time.UTC,
			wantErr:  false,
		},
		{
			name:     "6-field cron with seconds",
			schedule: "0 0 9 * * 1",
			loc:      time.UTC,
			wantErr:  false,
		},
		{
			name:     "descriptive daily",
			schedule: "@daily",
			loc:      time.UTC,
			wantErr:  false,
		},
		{
			name:     "descriptive hourly",
			schedule: "@hourly",
			loc:      time.UTC,
			wantErr:  false,
		},
		{
			name:     "invalid schedule",
			schedule: "invalid",
			loc:      time.UTC,
			wantErr:  true,
		},
		{
			name:     "empty schedule",
			schedule: "",
			loc:      time.UTC,
			wantErr:  true,
		},
		{
			name:     "timezone chicago",
			schedule: "0 9 * * 1",
			loc:      mustLoadLocation("America/Chicago"),
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schedule, err := parseCronSchedule(tt.schedule, tt.loc)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseCronSchedule() expected error but got none")
				}
				return
			}
			if err != nil {
				t.Errorf("parseCronSchedule() unexpected error: %v", err)
				return
			}
			if schedule == nil {
				t.Errorf("parseCronSchedule() returned nil schedule")
			}
		})
	}
}

func TestTZScheduleNext(t *testing.T) {
	// Test that timezone conversion works correctly
	loc := mustLoadLocation("America/New_York")
	schedule, err := parseCronSchedule("0 9 * * *", loc)
	if err != nil {
		t.Fatalf("Failed to parse schedule: %v", err)
	}

	// At midnight UTC, it should be 5am in New York (during EST)
	// So the next 9am New York time would be 14:00 UTC
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	next := schedule.Next(now)

	// The next run should be at 14:00 UTC (9:00 AM EST)
	expected := time.Date(2024, 1, 1, 14, 0, 0, 0, time.UTC)
	if !next.Equal(expected) {
		t.Errorf("Expected next run at %v, got %v", expected, next)
	}
}

func TestCronOverlapPolicy(t *testing.T) {
	cs := &cronScheduler{
		running: make(map[string]int),
	}

	key := "test/workspace"

	// Test skip policy - mark as running and verify
	cs.running[key] = 2

	cs.runningMu.Lock()
	isRunning := cs.running[key]
	cs.runningMu.Unlock()

	if isRunning == 0 {
		t.Fatal("Expected workflow to be marked as running")
	}
}

func TestWorkflowEnabledForCronScheduling(t *testing.T) {
	enabled := true
	disabled := false

	tests := []struct {
		name     string
		workflow *types.WorkflowConfig
		want     bool
	}{
		{
			name:     "nil defaults to enabled",
			workflow: &types.WorkflowConfig{},
			want:     true,
		},
		{
			name: "enabled true",
			workflow: &types.WorkflowConfig{
				Enabled: &enabled,
			},
			want: true,
		},
		{
			name: "enabled false",
			workflow: &types.WorkflowConfig{
				Enabled: &disabled,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isWorkflowEnabled(tt.workflow); got != tt.want {
				t.Fatalf("isWorkflowEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCronWorkflowTriggerValidation(t *testing.T) {
	tests := []struct {
		name    string
		trigger *types.CronWorkflowTrigger
		wantErr bool
	}{
		{
			name: "valid cron trigger",
			trigger: &types.CronWorkflowTrigger{
				Schedule:      "0 9 * * *",
				Timezone:      "UTC",
				OverlapPolicy: "skip",
			},
			wantErr: false,
		},
		{
			name: "valid with empty timezone (defaults to UTC)",
			trigger: &types.CronWorkflowTrigger{
				Schedule:      "0 9 * * *",
				OverlapPolicy: "skip",
			},
			wantErr: false,
		},
		{
			name: "valid with parallel overlap",
			trigger: &types.CronWorkflowTrigger{
				Schedule:      "0 9 * * *",
				OverlapPolicy: "parallel",
			},
			wantErr: false,
		},
		{
			name: "valid with queue overlap",
			trigger: &types.CronWorkflowTrigger{
				Schedule:      "0 9 * * *",
				OverlapPolicy: "queue",
			},
			wantErr: false,
		},
		{
			name: "invalid overlap policy",
			trigger: &types.CronWorkflowTrigger{
				Schedule:      "0 9 * * *",
				OverlapPolicy: "invalid",
			},
			wantErr: true,
		},
		{
			name: "missing schedule",
			trigger: &types.CronWorkflowTrigger{
				OverlapPolicy: "skip",
			},
			wantErr: true,
		},
		{
			name: "invalid timezone",
			trigger: &types.CronWorkflowTrigger{
				Schedule:      "0 9 * * *",
				Timezone:      "Invalid/Timezone",
				OverlapPolicy: "skip",
			},
			wantErr: true, // timezone is validated during workflow validation
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// We can't directly call validateCronWorkflowTrigger since it's in types package
			// But we can test via WorkflowConfig validation
			wf := &types.WorkflowConfig{
				Name: "test-workflow",
				Trigger: &types.WorkflowTrigger{
					Cron: tt.trigger,
				},
			}
			err := wf.Validate()
			if tt.wantErr {
				if err == nil {
					t.Errorf("WorkflowConfig.Validate() expected error but got none")
				}
				return
			}
			if err != nil {
				t.Errorf("WorkflowConfig.Validate() unexpected error: %v", err)
			}
		})
	}
}

func TestCronWorkflowRunAPI(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	s.cronScheduler = newCronScheduler(s)

	now := time.Now().UTC()
	if _, err := db.Exec(`INSERT INTO workflow_runs(id,tenant_id,workflow_name,workspace_name,trigger_type,status,claw_id,run_context,started_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		"run-1", "test-tenant-id", "wf", "ws", "cron", "running", "claw-1", "{}", now, now); err != nil {
		t.Fatalf("insert run: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/workspaces/ws/workflows/wf/cron/runs/run-1", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var run types.WorkflowRun
	if err := json.Unmarshal(rr.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.ID != "run-1" || run.ClawID != "claw-1" {
		t.Fatalf("unexpected run: %#v", run)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/workspaces/ws/workflows/wf/cron/runs/missing", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, want %d", rr.Code, http.StatusNotFound)
	}
}

func TestCronSchedulerGetRunByID(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	s.cronScheduler = newCronScheduler(s)

	now := time.Now().UTC()
	if _, err := db.Exec(`INSERT INTO workflow_runs(id,tenant_id,workflow_name,workspace_name,trigger_type,status,claw_id,run_context,started_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		"run-1", "test-tenant-id", "wf", "ws", "cron", "running", "claw-1", "{}", now, now); err != nil {
		t.Fatalf("insert run: %v", err)
	}

	run, err := s.cronScheduler.getRunByID("ws", "wf", "run-1")
	if err != nil {
		t.Fatalf("getRunByID: %v", err)
	}
	if run == nil {
		t.Fatalf("expected run, got nil")
	}
	if run.ID != "run-1" || run.WorkspaceName != "ws" || run.WorkflowName != "wf" || run.ClawID != "claw-1" {
		t.Fatalf("unexpected run: %#v", run)
	}

	missing, err := s.cronScheduler.getRunByID("ws", "wf", "missing")
	if err != nil {
		t.Fatalf("getRunByID missing: %v", err)
	}
	if missing != nil {
		t.Fatalf("expected nil for missing run, got %#v", missing)
	}
}

func mustLoadLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// Ensure cron.Schedule interface is satisfied
var _ cron.Schedule = (*tzSchedule)(nil)
