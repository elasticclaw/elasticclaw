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

	// running tracks active runs to enforce overlap policies (counter for parallel support)
	running   map[string]int // workflow key -> active run count
	runningMu sync.Mutex

	// clawWorkflow tracks which workflow key a claw belongs to, so we can
	// decrement the running counter when the claw actually finishes.
	clawWorkflow   map[string]string // clawID -> workflow key
	clawWorkflowMu sync.Mutex
}

type scheduledWorkflow struct {
	workspace *types.WorkspaceConfig
	workflow  *types.WorkflowConfig
	trigger   *types.CronWorkflowTrigger
	key       string // workspaceName/workflowName
}

type workflowRunStartStatus string

const (
	workflowRunStarted workflowRunStartStatus = "started"
	workflowRunSkipped workflowRunStartStatus = "skipped"
	workflowRunFailed  workflowRunStartStatus = "failed"
)

func newCronScheduler(srv *Server) *cronScheduler {
	return &cronScheduler{
		srv:          srv,
		entries:      make(map[string]cron.EntryID),
		workflows:    make(map[string]*scheduledWorkflow),
		running:      make(map[string]int),
		clawWorkflow: make(map[string]string),
	}
}

// start initializes the cron scheduler and loads all cron workflows.
func (cs *cronScheduler) start() error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	// Create cron with second-level precision (optional) and timezone support
	cs.cron = cron.New(cron.WithSeconds(), cron.WithChain(cron.Recover(cron.DefaultLogger)))

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
			if !isWorkflowEnabled(wf) {
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

func (cs *cronScheduler) reload() error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.cron == nil {
		return nil
	}
	return cs.reloadWorkflows()
}

// parseCronSchedule parses a cron schedule with timezone support.
// It delegates expression parsing to types.ParseCronSchedule so that the
// scheduler and save-time validation always accept the same expressions.
func parseCronSchedule(schedule string, loc *time.Location) (cron.Schedule, error) {
	s, err := types.ParseCronSchedule(schedule)
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
	_, _ = j.scheduler.runWorkflow(j.workflow)
}

// runWorkflow executes a scheduled workflow run.
func (cs *cronScheduler) runWorkflow(sw *scheduledWorkflow) (workflowRunStartStatus, error) {
	key := sw.key

	// Check overlap policy
	cs.runningMu.Lock()
	if cs.running[key] > 0 {
		activeRuns := cs.running[key]
		switch sw.trigger.OverlapPolicy {
		case "skip", "":
			cs.runningMu.Unlock()
			log.Printf("[cron] skipping %s (%d run(s) still active)", key, activeRuns)
			cs.recordRun(uuid.New().String(), sw, "skipped", "", fmt.Sprintf("%d run(s) still active", activeRuns))
			return workflowRunSkipped, nil
		case "queue":
			// Queue is not implemented in v1; treat as skip
			cs.runningMu.Unlock()
			log.Printf("[cron] skipping %s (queue not implemented, %d run(s) active)", key, activeRuns)
			cs.recordRun(uuid.New().String(), sw, "skipped", "", fmt.Sprintf("%d run(s) active, queue not implemented", activeRuns))
			return workflowRunSkipped, nil
		case "parallel":
			// Allow parallel execution
		default:
			cs.runningMu.Unlock()
			log.Printf("[cron] unknown overlap policy %q for %s, skipping", sw.trigger.OverlapPolicy, key)
			cs.recordRun(uuid.New().String(), sw, "skipped", "", "unknown overlap policy")
			return workflowRunSkipped, nil
		}
	}
	cs.running[key]++
	cs.runningMu.Unlock()

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

	// Scheduled workflows cannot prompt an operator for inputs, so resolve
	// declared defaults and fail the run if a required input has no default.
	rawInputs := make(map[string]interface{})
	for _, input := range sw.workflow.Inputs {
		if input.Default != "" {
			rawInputs[input.Name] = input.Default
		}
	}
	inputs, err := validateFactoryInputs(sw.workflow.Inputs, rawInputs)
	if err != nil {
		result := fmt.Sprintf("invalid scheduled inputs: %v", err)
		log.Printf("[cron] failed to resolve inputs for %s: %v", key, err)
		cs.failRun(runID, result)
		cs.releaseWorkflowSlot(key)
		return workflowRunFailed, err
	}

	timeoutAt := time.Time{}
	if sw.trigger.Timeout != "" {
		timeout, err := time.ParseDuration(sw.trigger.Timeout)
		if err != nil {
			result := fmt.Sprintf("invalid timeout: %v", err)
			log.Printf("[cron] failed to resolve timeout for %s: %v", key, err)
			cs.failRun(runID, result)
			cs.releaseWorkflowSlot(key)
			return workflowRunFailed, err
		}
		timeoutAt = now.Add(timeout)
	}

	// Create claw
	clawID, _, err := cs.srv.createClawFromWorkflowWithOptions(
		sw.workspace,
		sw.workflow,
		workflowCreateOptions{
			inputs:      inputs,
			triggerKind: "routine",
			reason:      fmt.Sprintf("cron run %s at %s", runID, now.Format(time.RFC3339)),
			clawName:    fmt.Sprintf("%s-%s", sw.workflow.Name, now.Format("20060102-150405")),
			timeoutAt:   timeoutAt,
		},
	)
	if err != nil {
		log.Printf("[cron] failed to create claw for %s: %v", key, err)
		cs.failRun(runID, fmt.Sprintf("failed to create claw: %v", err))
		// No claw was created, so decrement the running counter immediately
		cs.releaseWorkflowSlot(key)
		return workflowRunFailed, err
	}

	// Update run with claw ID
	cs.updateRun(runID, clawID, "running")

	// Track this claw so we can decrement the running counter when it finishes
	cs.clawWorkflowMu.Lock()
	cs.clawWorkflow[clawID] = key
	cs.clawWorkflowMu.Unlock()

	log.Printf("[cron] started run %s for %s (claw %s)", runID, key, clawID)
	return workflowRunStarted, nil
}

func (cs *cronScheduler) releaseWorkflowSlot(key string) {
	cs.runningMu.Lock()
	cs.running[key]--
	if cs.running[key] < 0 {
		cs.running[key] = 0
	}
	cs.runningMu.Unlock()
}

// recordRun inserts a workflow run record for a scheduled workflow.
func (cs *cronScheduler) recordRun(runID string, sw *scheduledWorkflow, status, clawID, context string) {
	cs.srv.recordWorkflowRun(runID, "", sw.workspace.Name, sw.workflow.Name, "cron", status, clawID, context, time.Time{})
}

// recordWorkflowRun inserts a workflow run record. If runID is empty, a new
// UUID is generated. If tenantID is empty, the first tenant is used. It is used
// by both scheduled cron runs and manual workflow triggers.
func (s *Server) recordWorkflowRun(runID, tenantID, workspaceName, workflowName, triggerType, status, clawID, context string, startedAt time.Time) string {
	if runID == "" {
		runID = uuid.New().String()
	}
	if tenantID == "" {
		_ = s.db.QueryRow(`SELECT id FROM tenants LIMIT 1`).Scan(&tenantID)
	}
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(
		`INSERT INTO workflow_runs (id, tenant_id, workflow_name, workspace_name, trigger_type, status, claw_id, run_context, started_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, tenantID, workflowName, workspaceName, triggerType, status, clawID, context, startedAt, time.Now().UTC(),
	)
	if err != nil {
		log.Printf("[workflow-runs] failed to record run for %s/%s: %v", workspaceName, workflowName, err)
	}
	return runID
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

// failRun marks a workflow run as failed with a result message.
func (cs *cronScheduler) failRun(runID, result string) {
	now := time.Now().UTC()
	_, err := cs.srv.db.Exec(
		`UPDATE workflow_runs SET status = ?, result = ?, finished_at = ? WHERE id = ?`,
		"failed", result, now, runID,
	)
	if err != nil {
		log.Printf("[cron] failed to mark run %s as failed: %v", runID, err)
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

	cs.releaseClawWorkflowSlot(clawID)
}

// releaseClawWorkflowSlot performs the in-memory half of finishing a run.
func (cs *cronScheduler) releaseClawWorkflowSlot(clawID string) {
	if cs == nil {
		return
	}
	// Decrement the running counter for this workflow so overlap policy
	// is enforced against actual claw execution time, not creation time.
	cs.clawWorkflowMu.Lock()
	key, ok := cs.clawWorkflow[clawID]
	if ok {
		delete(cs.clawWorkflow, clawID)
	}
	cs.clawWorkflowMu.Unlock()

	if ok {
		cs.releaseWorkflowSlot(key)
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
				return "", &cronTriggerNotFoundError{msg: fmt.Sprintf("workflow %s/%s is not cron-triggered", workspaceName, workflowName)}
			}
			if !isWorkflowEnabled(wf) {
				return "", &cronTriggerDisabledError{msg: fmt.Sprintf("workflow %s/%s is disabled", workspaceName, workflowName)}
			}

			sw := &scheduledWorkflow{
				workspace: ws,
				workflow:  wf,
				trigger:   wf.Trigger.Cron,
				key:       workspaceName + "/" + workflowName,
			}
			status, err := cs.runWorkflow(sw)
			if status == workflowRunSkipped {
				return "", &cronTriggerSkippedError{msg: fmt.Sprintf("workflow %s/%s: run skipped due to overlap policy", workspaceName, workflowName)}
			}
			if err != nil {
				return "", err
			}
			return sw.key, nil
		}
	}

	return "", &cronTriggerNotFoundError{msg: fmt.Sprintf("workflow %s/%s not found", workspaceName, workflowName)}
}

// cronTriggerNotFoundError is returned when a workflow is not found or not cron-triggered.
type cronTriggerNotFoundError struct {
	msg string
}

func (e *cronTriggerNotFoundError) Error() string {
	return e.msg
}

// cronTriggerDisabledError is returned when a cron workflow is disabled.
type cronTriggerDisabledError struct {
	msg string
}

func (e *cronTriggerDisabledError) Error() string {
	return e.msg
}

// cronTriggerSkippedError is returned when a manual trigger is skipped due to overlap policy.
type cronTriggerSkippedError struct {
	msg string
}

func (e *cronTriggerSkippedError) Error() string {
	return e.msg
}

func isWorkflowEnabled(workflow *types.WorkflowConfig) bool {
	return workflow.Enabled == nil || *workflow.Enabled
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

// getRunByID returns a single workflow run by ID, or nil if not found.
func (cs *cronScheduler) getRunByID(workspaceName, workflowName, runID string) (*types.WorkflowRun, error) {
	var tenantID string
	_ = cs.srv.db.QueryRow(`SELECT id FROM tenants LIMIT 1`).Scan(&tenantID)

	var r types.WorkflowRun
	var finishedAt sql.NullTime
	var runContextJSON string
	err := cs.srv.db.QueryRow(
		`SELECT id, tenant_id, workflow_name, workspace_name, trigger_type, status, result, claw_id, run_context, started_at, finished_at, created_at
		 FROM workflow_runs
		 WHERE tenant_id = ? AND workspace_name = ? AND workflow_name = ? AND id = ?`,
		tenantID, workspaceName, workflowName, runID,
	).Scan(
		&r.ID, &r.TenantID, &r.WorkflowName, &r.WorkspaceName, &r.TriggerType,
		&r.Status, &r.Result, &r.ClawID, &runContextJSON,
		&r.StartedAt, &finishedAt, &r.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if finishedAt.Valid {
		r.FinishedAt = &finishedAt.Time
	}
	if runContextJSON != "" && runContextJSON != "{}" {
		_ = json.Unmarshal([]byte(runContextJSON), &r.RunContext)
	}
	return &r, nil
}

// loadAllWorkspaces loads all workspace configurations.
func (s *Server) loadAllWorkspaces() ([]*types.WorkspaceConfig, error) {
	return loadExternalWorkspaces()
}
