package hub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	v2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
)

// cronSchedulerV2 manages scheduled workflow v2 runs using cron expressions.
// It mirrors the behavior of v1 cronScheduler while using the v2 runtime.
type cronSchedulerV2 struct {
	srv       *Server
	mu        sync.RWMutex
	cron      *cron.Cron
	entries   map[string]cron.EntryID         // workflow key -> cron entry ID
	workflows map[string]*scheduledWorkflowV2 // workflow key -> workflow
}

type scheduledWorkflowV2 struct {
	workspace *types.WorkspaceConfig // v1 shell with Files and RawConfig
	workflow  *types.WorkflowConfig  // v1 shell with RawConfig
	trigger   *v2.CronTrigger
	key       string // workspaceName/workflowName
}

func newCronSchedulerV2(srv *Server) *cronSchedulerV2 {
	return &cronSchedulerV2{
		srv:       srv,
		entries:   make(map[string]cron.EntryID),
		workflows: make(map[string]*scheduledWorkflowV2),
	}
}

func (cs *cronSchedulerV2) start() error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	cs.cron = cron.New(cron.WithSeconds(), cron.WithChain(cron.Recover(cron.DefaultLogger)))
	if err := cs.reloadWorkflows(); err != nil {
		return fmt.Errorf("reload v2 cron workflows: %w", err)
	}
	cs.cron.Start()
	log.Println("[cron-v2] scheduler started")
	return nil
}

func (cs *cronSchedulerV2) stop() {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.cron != nil {
		ctx := cs.cron.Stop()
		<-ctx.Done()
		log.Println("[cron-v2] scheduler stopped")
	}
}

func (cs *cronSchedulerV2) reload() error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.cron == nil {
		return nil
	}
	return cs.reloadWorkflows()
}

// removeWorkflow stops the schedule for a single workflow without reloading.
func (cs *cronSchedulerV2) removeWorkflow(workspaceName, workflowName string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.cron == nil {
		return
	}
	key := workspaceName + "/" + workflowName
	for entryKey, entryID := range cs.entries {
		if strings.EqualFold(entryKey, key) {
			cs.cron.Remove(entryID)
			delete(cs.entries, entryKey)
			delete(cs.workflows, entryKey)
			log.Printf("[cron-v2] removed schedule for %s", entryKey)
			return
		}
	}
}

func (cs *cronSchedulerV2) reloadWorkflows() error {
	workspaces, err := cs.srv.loadAllWorkspaces()
	if err != nil {
		return err
	}

	newWorkflows := make(map[string]*scheduledWorkflowV2)
	for _, ws := range workspaces {
		for _, wf := range ws.Workflows {
			if wf == nil || !isWorkflowV2(wf) {
				continue
			}
			trigger, ok := cronTriggerFromV2Workflow(wf)
			if !ok {
				continue
			}
			if !isWorkflowEnabled(wf) {
				continue
			}
			key := ws.Name + "/" + wf.Name
			newWorkflows[key] = &scheduledWorkflowV2{
				workspace: ws,
				workflow:  wf,
				trigger:   trigger,
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
			log.Printf("[cron-v2] removed schedule for %s", key)
		}
	}

	// Add or update entries
	for key, sw := range newWorkflows {
		if _, ok := cs.entries[key]; ok {
			old := cs.workflows[key]
			if old != nil && old.trigger.Schedule == sw.trigger.Schedule && old.trigger.Timezone == sw.trigger.Timezone {
				continue
			}
			cs.cron.Remove(cs.entries[key])
			delete(cs.entries, key)
		}

		loc := time.UTC
		if sw.trigger.Timezone != "" {
			var err error
			loc, err = time.LoadLocation(sw.trigger.Timezone)
			if err != nil {
				log.Printf("[cron-v2] invalid timezone %q for %s, using UTC: %v", sw.trigger.Timezone, key, err)
				loc = time.UTC
			}
		}
		schedule, err := parseCronSchedule(sw.trigger.Schedule, loc)
		if err != nil {
			log.Printf("[cron-v2] invalid schedule %q for %s: %v", sw.trigger.Schedule, key, err)
			continue
		}
		entryID := cs.cron.Schedule(schedule, &cronJobV2{scheduler: cs, workflow: sw})
		cs.entries[key] = entryID
		cs.workflows[key] = sw
		log.Printf("[cron-v2] scheduled %s with %q (timezone: %s)", key, sw.trigger.Schedule, sw.trigger.Timezone)
	}
	return nil
}

func cronTriggerFromV2Workflow(wf *types.WorkflowConfig) (*v2.CronTrigger, bool) {
	if wf == nil || strings.TrimSpace(wf.RawConfig) == "" {
		return nil, false
	}
	resolved, err := v2.ParseWorkflow([]byte(wf.RawConfig))
	if err != nil {
		log.Printf("[cron-v2] could not parse v2 workflow %q: %v", wf.Name, err)
		return nil, false
	}
	if resolved.Trigger == nil || resolved.Trigger.Cron == nil {
		return nil, false
	}
	return resolved.Trigger.Cron, true
}

type cronJobV2 struct {
	scheduler *cronSchedulerV2
	workflow  *scheduledWorkflowV2
}

func (j *cronJobV2) Run() {
	_, _ = j.scheduler.runWorkflow(j.workflow)
}

func (cs *cronSchedulerV2) runWorkflow(sw *scheduledWorkflowV2) (workflowRunStartStatus, error) {
	key := sw.key
	tenantID, err := cs.firstTenantID()
	if err != nil {
		log.Printf("[cron-v2] no tenant for %s: %v", key, err)
		return workflowRunFailed, err
	}

	store := workflowv2.NewStore(cs.srv.db)
	now := time.Now().UTC()

	active, err := cs.countActiveV2Runs(store, sw.workspace.Name, sw.workflow.Name)
	if err != nil {
		log.Printf("[cron-v2] failed to count active runs for %s: %v", key, err)
		return workflowRunFailed, err
	}

	overlapPolicy := strings.ToLower(strings.TrimSpace(sw.trigger.OverlapPolicy))
	if overlapPolicy == "" {
		overlapPolicy = "skip"
	}
	if active > 0 {
		switch overlapPolicy {
		case "skip":
			reason := fmt.Sprintf("%d run(s) still active", active)
			_, _ = store.RecordCronRunSkipped(context.Background(), tenantID, sw.workspace.Name, sw.workflow.Name, "cron", map[string]interface{}{
				"reason": reason,
			})
			log.Printf("[cron-v2] skipping %s (%s)", key, reason)
			return workflowRunSkipped, nil
		case "parallel":
			// allow parallel execution
		default:
			reason := fmt.Sprintf("unknown overlap policy %q", sw.trigger.OverlapPolicy)
			_, _ = store.RecordCronRunSkipped(context.Background(), tenantID, sw.workspace.Name, sw.workflow.Name, "cron", map[string]interface{}{
				"reason": reason,
			})
			log.Printf("[cron-v2] skipping %s (%s)", key, reason)
			return workflowRunSkipped, nil
		}
	}

	// Record the cron history row before provisioning so a failure is still visible.
	cronRunID, err := store.RecordCronRunStarted(context.Background(), tenantID, sw.workspace.Name, sw.workflow.Name, "cron", map[string]interface{}{
		"scheduled_at":   now.Format(time.RFC3339),
		"workflow_name":  sw.workflow.Name,
		"workspace_name": sw.workspace.Name,
		"trigger_type":   "cron",
	})
	if err != nil {
		log.Printf("[cron-v2] failed to record start for %s: %v", key, err)
		return workflowRunFailed, err
	}

	workspaceYAML := []byte(sw.workspace.Files["elasticclaw-config.yaml"])
	workflowYAML := []byte(sw.workflow.RawConfig)

	clawID, _, err := cs.srv.createClawFromWorkflowWithOptions(
		sw.workspace,
		sw.workflow,
		workflowCreateOptions{
			reason:   fmt.Sprintf("cron run %s at %s", cronRunID, now.Format(time.RFC3339)),
			clawName: fmt.Sprintf("%s-%s", sw.workflow.Name, now.Format("20060102-150405")),
			beforeProvision: func(ctx context.Context, clawID, provisionTenantID string) error {
				var taskRunID string
				if err := cs.srv.db.QueryRow(`SELECT id FROM task_runs WHERE claw_id=? ORDER BY created_at DESC LIMIT 1`, clawID).Scan(&taskRunID); err != nil && !errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("lookup parent task run for claw %s: %w", clawID, err)
				}
				run, err := store.CreateRun(ctx, workflowv2.CreateRunRequest{
					ID: uuid.NewString(), TenantID: provisionTenantID, InitialClawID: clawID, TaskRunID: taskRunID,
					WorkspaceYAML: workspaceYAML, WorkflowYAML: workflowYAML, ActivationPending: true,
					TriggerType: "cron",
				})
				if err != nil {
					return err
				}
				if err := store.UpdateCronRunForV2Run(ctx, cronRunID, run.ID, clawID); err != nil {
					return err
				}
				_, err = store.AssembleOrganizationContext(ctx, run.ID, workspaceFileKnowledgeResolver(sw.workspace.Files))
				if err == nil {
					err = store.CompleteActivation(ctx, run.ID)
				}
				if err != nil {
					cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if cleanupErr := store.CancelActivation(cleanupCtx, run.ID, err.Error()); cleanupErr != nil {
						return errors.Join(err, fmt.Errorf("cancel failed workflow v2 activation: %w", cleanupErr))
					}
					return err
				}
				return nil
			},
		},
	)
	if err != nil {
		log.Printf("[cron-v2] failed to create run for %s: %v", key, err)
		if failErr := store.FailCronRun(context.Background(), cronRunID, err.Error()); failErr != nil {
			log.Printf("[cron-v2] failed to mark run %s as failed: %v", cronRunID, failErr)
		}
		return workflowRunFailed, err
	}

	log.Printf("[cron-v2] started run %s for %s (claw %s)", cronRunID, key, clawID)
	return workflowRunStarted, nil
}

// manualTrigger triggers a v2 cron workflow run manually. It matches v1
// cronScheduler.manualTrigger: any enabled cron workflow can be triggered.
func (cs *cronSchedulerV2) manualTrigger(workspaceName, workflowName string) (string, error) {
	workspaces, err := cs.srv.loadAllWorkspaces()
	if err != nil {
		return "", err
	}
	for _, ws := range workspaces {
		if ws.Name != workspaceName {
			continue
		}
		for _, wf := range ws.Workflows {
			if wf == nil || wf.Name != workflowName || !isWorkflowV2(wf) {
				continue
			}
			trigger, ok := cronTriggerFromV2Workflow(wf)
			if !ok {
				return "", &cronTriggerNotFoundError{msg: fmt.Sprintf("workflow %s/%s is not cron-triggered", workspaceName, workflowName)}
			}
			if !isWorkflowEnabled(wf) {
				return "", &cronTriggerDisabledError{msg: fmt.Sprintf("workflow %s/%s is disabled", workspaceName, workflowName)}
			}
			sw := &scheduledWorkflowV2{
				workspace: ws,
				workflow:  wf,
				trigger:   trigger,
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

func (cs *cronSchedulerV2) firstTenantID() (string, error) {
	var tenantID string
	if err := cs.srv.db.QueryRow(`SELECT id FROM tenants LIMIT 1`).Scan(&tenantID); err != nil {
		return "", err
	}
	return tenantID, nil
}

func (cs *cronSchedulerV2) countActiveV2Runs(store *workflowv2.Store, workspaceName, workflowName string) (int, error) {
	var count int
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cs.srv.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_v2_runs
		WHERE workspace_name=? AND workflow_name=? AND status IN ('active','suspended')`,
		workspaceName, workflowName).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// getNextRuns returns the next scheduled run times for all v2 cron workflows.
func (cs *cronSchedulerV2) getNextRuns() map[string]time.Time {
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

// getRunHistory returns the cron run history for a v2 workflow.
func (cs *cronSchedulerV2) getRunHistory(workspaceName, workflowName string, limit int) ([]types.WorkflowRun, error) {
	if limit <= 0 {
		limit = 50
	}
	tenantID, err := cs.firstTenantID()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	history, err := workflowv2.NewStore(cs.srv.db).ListCronRunHistory(ctx, tenantID, workspaceName, workflowName, limit)
	if err != nil {
		return nil, err
	}
	runs := make([]types.WorkflowRun, 0, len(history))
	for _, h := range history {
		runs = append(runs, cronHistoryToWorkflowRun(h))
	}
	return runs, nil
}

// getRunByID returns a single v2 cron run by ID.
func (cs *cronSchedulerV2) getRunByID(workspaceName, workflowName, runID string) (*types.WorkflowRun, error) {
	tenantID, err := cs.firstTenantID()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h, err := workflowv2.NewStore(cs.srv.db).GetCronRun(ctx, tenantID, workspaceName, workflowName, runID)
	if err != nil || h == nil {
		return nil, err
	}
	run := cronHistoryToWorkflowRun(*h)
	return &run, nil
}

// finishRunByV2RunID marks the cron history row for a finished v2 run.
func (cs *cronSchedulerV2) finishRunByV2RunID(runID string, status workflowv2.RunStatus) {
	if cs == nil || runID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := workflowv2.NewStore(cs.srv.db).FinishCronRunByV2RunID(ctx, runID, status); err != nil {
		log.Printf("[cron-v2] failed to finish cron run for v2 run %s: %v", runID, err)
	}
}

// finishRunByClawID marks the cron history row for a finished claw.
func (cs *cronSchedulerV2) finishRunByClawID(clawID string, status workflowv2.RunStatus) {
	if cs == nil || clawID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := workflowv2.NewStore(cs.srv.db).FinishCronRunByClawID(ctx, clawID, status); err != nil {
		log.Printf("[cron-v2] failed to finish cron run for claw %s: %v", clawID, err)
	}
}

func cronHistoryToWorkflowRun(h workflowv2.CronRunHistory) types.WorkflowRun {
	run := types.WorkflowRun{
		ID:            h.ID,
		TenantID:      h.TenantID,
		WorkflowName:  h.WorkflowName,
		WorkspaceName: h.WorkspaceName,
		TriggerType:   h.TriggerType,
		Status:        h.Status,
		Result:        h.Result,
		ClawID:        h.ClawID,
		RunContext:    h.RunContext,
		CreatedAt:     h.CreatedAt,
		StartedAt:     &h.CreatedAt,
	}
	if h.FinishedAt != nil {
		finished := *h.FinishedAt
		run.FinishedAt = &finished
	}
	return run
}
