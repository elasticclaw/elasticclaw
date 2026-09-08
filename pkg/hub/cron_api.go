package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// handleCronWorkflowTrigger handles POST /api/workspaces/{workspace}/workflows/{workflow}/cron/trigger
// Manually triggers a cron workflow run. It dispatches to the v1 or v2 scheduler
// depending on the workflow's schema version, matching v1 behavior where any
// enabled cron workflow is manually triggerable.
func (s *Server) handleCronWorkflowTrigger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	workspace := r.PathValue("workspace")
	workflow := r.PathValue("workflow")

	isV2, err := s.cronWorkflowIsV2(workspace, workflow)
	if err != nil {
		if _, ok := err.(*cronTriggerNotFoundError); ok {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	var key string
	if isV2 {
		if s.cronSchedulerV2 == nil {
			http.Error(w, "Cron scheduler not available", http.StatusServiceUnavailable)
			return
		}
		key, err = s.cronSchedulerV2.manualTrigger(workspace, workflow)
	} else {
		if s.cronScheduler == nil {
			http.Error(w, "Cron scheduler not available", http.StatusServiceUnavailable)
			return
		}
		key, err = s.cronScheduler.manualTrigger(workspace, workflow)
	}
	if err != nil {
		if _, ok := err.(*cronTriggerNotFoundError); ok {
			http.Error(w, err.Error(), http.StatusNotFound)
		} else if _, ok := err.(*cronTriggerDisabledError); ok {
			http.Error(w, err.Error(), http.StatusForbidden)
		} else if _, ok := err.(*cronTriggerSkippedError); ok {
			http.Error(w, err.Error(), http.StatusConflict)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":   "triggered",
		"workflow": key,
	})
}

// handleCronWorkflowRuns handles GET /api/workspaces/{workspace}/workflows/{workflow}/cron/runs
// Returns the run history for a cron workflow. Queries both v1 and v2 history
// so runs are returned even if the workspace configuration has been removed.
func (s *Server) handleCronWorkflowRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	workspace := r.PathValue("workspace")
	workflow := r.PathValue("workflow")

	if s.cronScheduler == nil && s.cronSchedulerV2 == nil {
		http.Error(w, "Cron scheduler not available", http.StatusServiceUnavailable)
		return
	}

	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if limitStr != "" {
		if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
			limit = n
		}
	}

	var runs []types.WorkflowRun
	if s.cronScheduler != nil {
		v1Runs, err := s.cronScheduler.getRunHistory(workspace, workflow, limit)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to get run history: %v", err), http.StatusInternalServerError)
			return
		}
		runs = append(runs, v1Runs...)
	}
	if s.cronSchedulerV2 != nil {
		v2Runs, err := s.cronSchedulerV2.getRunHistory(workspace, workflow, limit)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to get run history: %v", err), http.StatusInternalServerError)
			return
		}
		runs = append(runs, v2Runs...)
	}

	sort.Slice(runs, func(i, j int) bool {
		return runs[i].CreatedAt.After(runs[j].CreatedAt)
	})
	if len(runs) > limit {
		runs = runs[:limit]
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"runs":  runs,
		"count": len(runs),
	})
}

// handleCronWorkflowRun handles GET /api/workspaces/{workspace}/workflows/{workflow}/cron/runs/{runId}
// Returns a single workflow run by ID. Queries both v1 and v2 history.
func (s *Server) handleCronWorkflowRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	workspace := r.PathValue("workspace")
	workflow := r.PathValue("workflow")
	runID := r.PathValue("runId")

	if s.cronScheduler == nil && s.cronSchedulerV2 == nil {
		http.Error(w, "Cron scheduler not available", http.StatusServiceUnavailable)
		return
	}

	var run *types.WorkflowRun
	var err error
	if s.cronScheduler != nil {
		run, err = s.cronScheduler.getRunByID(workspace, workflow, runID)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to get run: %v", err), http.StatusInternalServerError)
			return
		}
	}
	if run == nil && s.cronSchedulerV2 != nil {
		run, err = s.cronSchedulerV2.getRunByID(workspace, workflow, runID)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to get run: %v", err), http.StatusInternalServerError)
			return
		}
	}
	if run == nil {
		http.Error(w, "Run not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(run)
}

// handleCronWorkflowNextRun handles GET /api/workspaces/{workspace}/workflows/{workflow}/cron/next
// Returns the next scheduled run time for a cron workflow. Dispatches to v1 or v2 scheduler.
func (s *Server) handleCronWorkflowNextRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	workspace := r.PathValue("workspace")
	workflow := r.PathValue("workflow")

	isV2, err := s.cronWorkflowIsV2(workspace, workflow)
	if err != nil {
		if _, ok := err.(*cronTriggerNotFoundError); ok {
			http.Error(w, "Workflow not found or not cron-triggered", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var nextRuns map[string]time.Time
	if isV2 {
		if s.cronSchedulerV2 == nil {
			http.Error(w, "Cron scheduler not available", http.StatusServiceUnavailable)
			return
		}
		nextRuns = s.cronSchedulerV2.getNextRuns()
	} else {
		if s.cronScheduler == nil {
			http.Error(w, "Cron scheduler not available", http.StatusServiceUnavailable)
			return
		}
		nextRuns = s.cronScheduler.getNextRuns()
	}
	key := workspace + "/" + workflow

	nextRun, ok := nextRuns[key]
	if !ok {
		http.Error(w, "Workflow not found or not cron-triggered", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"workflow": key,
		"next_run": nextRun.Format(time.RFC3339),
	})
}

// cronWorkflowIsV2 returns true if the named workflow exists and is a v2
// document. It is used by the cron endpoints to dispatch to the correct
// scheduler. A non-existent workflow is reported as a *cronTriggerNotFoundError.
func (s *Server) cronWorkflowIsV2(workspaceName, workflowName string) (bool, error) {
	workspaces, err := s.loadAllWorkspaces()
	if err != nil {
		return false, err
	}
	for _, ws := range workspaces {
		if ws.Name != workspaceName {
			continue
		}
		for _, wf := range ws.Workflows {
			if wf == nil || wf.Name != workflowName {
				continue
			}
			return isWorkflowV2(wf), nil
		}
	}
	return false, &cronTriggerNotFoundError{msg: fmt.Sprintf("workflow %s/%s not found", workspaceName, workflowName)}
}
