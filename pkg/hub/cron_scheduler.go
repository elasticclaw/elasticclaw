package hub

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
)

// cronScheduler manages scheduled workflow runs using cron expressions.
type cronScheduler struct {
	srv *Server

	mu        sync.RWMutex
	cron      *cron.Cron
	entries   map[string]cron.EntryID       // workflow key -> cron entry ID
	workflows map[string]*scheduledWorkflow // workflow key -> workflow

	// running tracks active runs to enforce overlap policies
	running   map[string]bool // workflow key -> has active run
	runningMu sync.Mutex
}

type scheduledWorkflow struct {
	workspace *types.WorkspaceConfig
	workflow  *types.WorkflowConfig
	trigger   *types.CronWorkflowTrigger
	key       string // workspaceName/workflowName
}

func newCronScheduler(srv *Server) *cronScheduler {
	return &cronScheduler{
		srv:       srv,
		entries:   make(map[string]cron.EntryID),
		workflows: make(map[string]*scheduledWorkflow),
		running:   make(map[string]bool),
	}
}

// start initializes the cron scheduler and loads all cron workflows.
func (cs *cronScheduler) start() error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	// Create cron with second-level precision (optional) and timezone support
	cs.cron = cron.New(cron.WithSeconds())

	// Load all workspaces and register cron workflows
	if err := cs.reloadWorkflows(); err != nil {
		return fmt.Errorf("reload cron workflows: %w", err)
	}

	cs.cron.Start()
	log.Println("[cron] scheduler started")
	return nil
}

// stop gracefully shuts down the cron scheduler.
func (cs *cronScheduler) stop() {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if cs.cron != nil {
		ctx := cs.cron.Stop()
		<-ctx.Done()
		log.Println("[cron] scheduler stopped")
	}
}

// reloadWorkflows scans all workspaces and registers cron-triggered workflows.
func (cs *cronScheduler) reloadWorkflows() error {
	workspaces, err := cs.srv.loadAllWorkspaces()
	if err != nil {
		return err
	}

	// Build new set of workflows
	newWorkflows := make(map[string]*scheduledWorkflow)
	for _, ws := range workspaces {
		for _, wf := range ws.Workflows {
			if wf == nil || wf.Trigger == nil || wf.Trigger.Cron == nil {
				continue
			}
			key := ws.Name + "/" + wf.Name
			newWorkflows[key] = &scheduledWorkflow{
				workspace: ws,
				workflow:  wf,
				trigger:   wf.Trigger.Cron,
				key:       key,
			}
		}
	}

	// Remove old entries not in new set
	for key, entryID := range cs.entries {
		if _, ok := newWorkflows[key]; !ok {
			cs.cron.Remove(entryID)
			delete(cs.entries, key)
			delete(cs.workflows, key)
			log.Printf("[cron] removed schedule for %s", key)
		}
	}

	// Add new entries
	for key, sw := range newWorkflows {
		if _, ok := cs.entries[key]; ok {
			// Already scheduled; check if schedule changed
			old := cs.workflows[key]
			if old != nil && old.trigger.Schedule == sw.trigger.Schedule && old.trigger.Timezone == sw.trigger.Timezone {
				continue // no change
			}
			// Remove old entry
			cs.cron.Remove(cs.entries[key])
			delete(cs.entries, key)
		}

		// Parse timezone
		loc := time.UTC
		if sw.trigger.Timezone != "" {
			var err error
			loc, err = time.LoadLocation(sw.trigger.Timezone)
			if err != nil {
				log.Printf("[cron] invalid timezone %q for %s, using UTC: %v", sw.trigger.Timezone, key, err)
				loc = time.UTC
			}
		}

		// Create a new cron instance with the specific timezone for this entry
		// We use a wrapper approach: create schedule parser with timezone
		schedule, err := parseCronSchedule(sw.trigger.Schedule, loc)
		if err != nil {
			log.Printf("[cron] invalid schedule %q for %s: %v", sw.trigger.Schedule, key, err)
			continue
		}

		entryID := cs.cron.Schedule(schedule, &cronJob{
			scheduler: cs,
			workflow:  sw,
		})

		cs.entries[key] = entryID
		cs.workflows[key] = sw
		log.Printf("[cron] scheduled %s with %q (timezone: %s)", key, sw.trigger.Schedule, sw.trigger.Timezone)
	}

	return nil
}

// parseCronSchedule parses a cron schedule with timezone support.
// If the schedule has seconds field (6 fields), we use WithSeconds.
func parseCronSchedule(schedule string, loc *time.Location) (cron.Schedule, error) {
	parser := cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	s, err := parser.Parse(schedule)
	if err != nil {
		return nil, err
	}
	return &tzSchedule{Schedule: s, loc: loc}, nil
}

// tzSchedule wraps a cron.Schedule to apply timezone.
type tzSchedule struct {
	cron.Schedule
	loc *time.Location
}

func (t *tzSchedule) Next(now time.Time) time.Time {
	// Convert to target timezone, get next scheduled time, convert back to UTC
	localNow := now.In(t.loc)
	nextLocal := t.Schedule.Next(localNow)
	return nextLocal.In(time.UTC)
}

// cronJob implements cron.Job for a scheduled workflow.
type cronJob struct {
	scheduler *cronScheduler
	workflow  *scheduledWorkflow
}

func (j *cronJob) Run() {
	j.scheduler.runWorkflow(j.workflow)
}

// runWorkflow executes a scheduled workflow run.
func (cs *cronScheduler) runWorkflow(sw *scheduledWorkflow) {
	key := sw.key

	// Check overlap policy
	cs.runningMu.Lock()
	if cs.running[key] {
		switch sw.trigger.OverlapPolicy {
		case "skip", "":
			cs.runningMu.Unlock()
			log.Printf("[cron] skipping %s (previous run still active)", key)
			cs.recordRun(uuid.New().String(), sw, "skipped", "", "previous run still active")
			return
		case "queue":
			// Queue is not implemented in v1; treat as skip
			cs.runningMu.Unlock()
			log.Printf("[cron] skipping %s (queue not implemented, previous run active)", key)
			cs.recordRun(uuid.New().String(), sw, "skipped", "", "previous run still active, queue not implemented")
			return
		case "parallel":
			// Allow parallel execution
		default:
			cs.runningMu.Unlock()
			log.Printf("[cron] unknown overlap policy %q for %s, skipping", sw.trigger.OverlapPolicy, key)
			cs.recordRun(uuid.New().String(), sw, "skipped", "", "unknown overlap policy")
			return
		}
	}
	cs.running[key] = true
	cs.runningMu.Unlock()

	defer func() {
		cs.runningMu.Lock()
		cs.running[key] = false
		cs.runningMu.Unlock()
	}()

	// Generate run context
	runID := uuid.New().String()
	now := time.Now().UTC()
	contextData := map[string]interface{}{
		"run_id":         runID,
		"scheduled_at":   now.Format(time.RFC3339),
		"workflow_name":  sw.workflow.Name,
		"workspace_name": sw.workspace.Name,
		"trigger_type":   "cron",
	}
	contextJSON, _ := json.Marshal(contextData)

	// Record run start
	cs.recordRun(runID, sw, "running", "", string(contextJSON))

	// Build inputs (empty for cron, but could be extended)
	inputs := make(map[string]string)

	// Create claw
	clawID, _, err := cs.srv.createClawFromWorkflowWithOptions(
		sw.workspace,
		sw.workflow,
		workflowCreateOptions{
			inputs:   inputs,
			reason:   fmt.Sprintf("cron run %s at %s", runID, now.Format(time.RFC3339)),
			clawName: fmt.Sprintf("%s-%s", sw.workflow.Name, now.Format("20060102-150405")),
		},
	)
	if err != nil {
		log.Printf("[cron] failed to create claw for %s: %v", key, err)
		cs.recordRun(runID, sw, "failed", "", fmt.Sprintf("failed to create claw: %v", err))
		return
	}

	// Update run with claw ID
	cs.updateRun(runID, clawID, "running")

	log.Printf("[cron] started run %s for %s (claw %s)", runID, key, clawID)
}

// recordRun inserts a workflow run record.
func (cs *cronScheduler) recordRun(runID string, sw *scheduledWorkflow, status, clawID, context string) {
	now := time.Now().UTC()

	var tenantID string
	_ = cs.srv.db.QueryRow(`SELECT id FROM tenants LIMIT 1`).Scan(&tenantID)

	_, err := cs.srv.db.Exec(
		`INSERT INTO workflow_runs (id, tenant_id, workflow_name, workspace_name, trigger_type, status, claw_id, run_context, started_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, tenantID, sw.workflow.Name, sw.workspace.Name, "cron", status, clawID, context, now, now,
	)
	if err != nil {
		log.Printf("[cron] failed to record run for %s: %v", sw.key, err)
	}
}

// updateRun updates an existing workflow run with claw ID and status.
func (cs *cronScheduler) updateRun(runID, clawID, status string) {
	_, err := cs.srv.db.Exec(
		`UPDATE workflow_runs SET claw_id = ?, status = ? WHERE id = ?`,
		clawID, status, runID,
	)
	if err != nil {
		log.Printf("[cron] failed to update run %s: %v", runID, err)
	}
}

// finishRun marks a workflow run as completed.
func (cs *cronScheduler) finishRun(runID, result string) {
	now := time.Now().UTC()
	_, err := cs.srv.db.Exec(
		`UPDATE workflow_runs SET status = ?, result = ?, finished_at = ? WHERE id = ?`,
		"completed", result, now, runID,
	)
	if err != nil {
		log.Printf("[cron] failed to finish run %s: %v", runID, err)
	}
}

// finishRunByClawID marks a workflow run as completed by claw ID.
// Called when a claw reaches a terminal status (idle, deleted, error).
func (cs *cronScheduler) finishRunByClawID(clawID, status, result string) {
	if cs == nil {
		return
	}
	now := time.Now().UTC()
	_, err := cs.srv.db.Exec(
		`UPDATE workflow_runs SET status = ?, result = ?, finished_at = ? WHERE claw_id = ? AND status = 'running'`,
		status, result, now, clawID,
	)
	if err != nil {
		log.Printf("[cron] failed to finish run for claw %s: %v", clawID, err)
	}
}

// manualTrigger triggers a workflow run manually.
func (cs *cronScheduler) manualTrigger(workspaceName, workflowName string) (string, error) {
	workspaces, err := cs.srv.loadAllWorkspaces()
	if err != nil {
		return "", err
	}

	for _, ws := range workspaces {
		if ws.Name != workspaceName {
			continue
		}
		for _, wf := range ws.Workflows {
			if wf == nil || wf.Name != workflowName {
				continue
			}
			if wf.Trigger == nil || wf.Trigger.Cron == nil {
				return "", fmt.Errorf("workflow %s/%s is not cron-triggered", workspaceName, workflowName)
			}

			sw := &scheduledWorkflow{
				workspace: ws,
				workflow:  wf,
				trigger:   wf.Trigger.Cron,
				key:       workspaceName + "/" + workflowName,
			}
			cs.runWorkflow(sw)
			return sw.key, nil
		}
	}

	return "", fmt.Errorf("workflow %s/%s not found", workspaceName, workflowName)
}

// getNextRuns returns the next scheduled run times for all cron workflows.
func (cs *cronScheduler) getNextRuns() map[string]time.Time {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	result := make(map[string]time.Time)
	if cs.cron == nil {
		return result
	}

	for key, entryID := range cs.entries {
		entry := cs.cron.Entry(entryID)
		if entry.Valid() {
			result[key] = entry.Next
		}
	}
	return result
}

// getRunHistory returns the run history for a workflow.
func (cs *cronScheduler) getRunHistory(workspaceName, workflowName string, limit int) ([]types.WorkflowRun, error) {
	if limit <= 0 {
		limit = 50
	}

	var tenantID string
	_ = cs.srv.db.QueryRow(`SELECT id FROM tenants LIMIT 1`).Scan(&tenantID)

	rows, err := cs.srv.db.Query(
		`SELECT id, tenant_id, workflow_name, workspace_name, trigger_type, status, result, claw_id, run_context, started_at, finished_at, created_at
		 FROM workflow_runs
		 WHERE tenant_id = ? AND workspace_name = ? AND workflow_name = ?
		 ORDER BY created_at DESC
		 LIMIT ?`,
		tenantID, workspaceName, workflowName, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []types.WorkflowRun
	for rows.Next() {
		var r types.WorkflowRun
		var finishedAt sql.NullTime
		var runContextJSON string
		err := rows.Scan(
			&r.ID, &r.TenantID, &r.WorkflowName, &r.WorkspaceName, &r.TriggerType,
			&r.Status, &r.Result, &r.ClawID, &runContextJSON,
			&r.StartedAt, &finishedAt, &r.CreatedAt,
		)
		if err != nil {
			return nil, err
		}
		if finishedAt.Valid {
			r.FinishedAt = &finishedAt.Time
		}
		if runContextJSON != "" && runContextJSON != "{}" {
			_ = json.Unmarshal([]byte(runContextJSON), &r.RunContext)
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// loadAllWorkspaces loads all workspace configurations.
func (s *Server) loadAllWorkspaces() ([]*types.WorkspaceConfig, error) {
	rows, err := s.db.Query(`SELECT name, config FROM workspaces`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var workspaces []*types.WorkspaceConfig
	for rows.Next() {
		var name string
		var configJSON []byte
		if err := rows.Scan(&name, &configJSON); err != nil {
			return nil, err
		}
		var ws types.WorkspaceConfig
		if err := json.Unmarshal(configJSON, &ws); err != nil {
			log.Printf("[cron] failed to unmarshal workspace %s: %v", name, err)
			continue
		}
		workspaces = append(workspaces, &ws)
	}
	return workspaces, rows.Err()
}