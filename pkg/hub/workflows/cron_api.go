package workflows

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/httpserver"
)

// handleCronWorkflowTrigger handles POST /api/workspaces/{workspace}/workflows/{workflow}/cron/trigger
// Manually triggers a cron workflow run.
func (s *Service) handleCronWorkflowTrigger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpserver.WriteErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}

	workspace := r.PathValue("workspace")
	workflow := r.PathValue("workflow")

	if s.cronScheduler() == nil {
		httpserver.WriteErr(w, http.StatusServiceUnavailable, "unavailable", "Cron scheduler not available")
		return
	}

	key, err := s.cronScheduler().manualTrigger(workspace, workflow)
	if err != nil {
		if _, ok := err.(*cronTriggerNotFoundError); ok {
			httpserver.WriteErr(w, http.StatusNotFound, "not_found", err.Error())
		} else if _, ok := err.(*cronTriggerDisabledError); ok {
			httpserver.WriteErr(w, http.StatusForbidden, "forbidden", err.Error())
		} else if _, ok := err.(*cronTriggerSkippedError); ok {
			httpserver.WriteErr(w, http.StatusConflict, "conflict", err.Error())
		} else {
			httpserver.WriteErr(w, http.StatusInternalServerError, "internal", err.Error())
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
// Returns the run history for a cron workflow.
func (s *Service) handleCronWorkflowRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpserver.WriteErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}

	workspace := r.PathValue("workspace")
	workflow := r.PathValue("workflow")

	if s.cronScheduler() == nil {
		httpserver.WriteErr(w, http.StatusServiceUnavailable, "unavailable", "Cron scheduler not available")
		return
	}

	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if limitStr != "" {
		if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
			limit = n
		}
	}

	runs, err := s.cronScheduler().getRunHistory(workspace, workflow, limit)
	if err != nil {
		httpserver.WriteErr(w, http.StatusInternalServerError, "internal", fmt.Sprintf("Failed to get run history: %v", err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"runs":  runs,
		"count": len(runs),
	})
}

// handleCronWorkflowNextRun handles GET /api/workspaces/{workspace}/workflows/{workflow}/cron/next
// Returns the next scheduled run time for a cron workflow.
func (s *Service) handleCronWorkflowNextRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpserver.WriteErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}

	workspace := r.PathValue("workspace")
	workflow := r.PathValue("workflow")

	if s.cronScheduler() == nil {
		httpserver.WriteErr(w, http.StatusServiceUnavailable, "unavailable", "Cron scheduler not available")
		return
	}

	nextRuns := s.cronScheduler().getNextRuns()
	key := workspace + "/" + workflow

	nextRun, ok := nextRuns[key]
	if !ok {
		httpserver.WriteErr(w, http.StatusNotFound, "not_found", "Workflow not found or not cron-triggered")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"workflow": key,
		"next_run": nextRun.Format(time.RFC3339),
	})
}
