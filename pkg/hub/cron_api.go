package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// handleCronWorkflowTrigger handles POST /api/workspaces/{workspace}/workflows/{workflow}/cron/trigger
// Manually triggers a cron workflow run.
func (s *Server) handleCronWorkflowTrigger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	workspace := r.PathValue("workspace")
	workflow := r.PathValue("workflow")

	if s.cronScheduler == nil {
		http.Error(w, "Cron scheduler not available", http.StatusServiceUnavailable)
		return
	}

	key, err := s.cronScheduler.manualTrigger(workspace, workflow)
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
// Returns the run history for a cron workflow.
func (s *Server) handleCronWorkflowRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	workspace := r.PathValue("workspace")
	workflow := r.PathValue("workflow")

	if s.cronScheduler == nil {
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

	runs, err := s.cronScheduler.getRunHistory(workspace, workflow, limit)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get run history: %v", err), http.StatusInternalServerError)
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
func (s *Server) handleCronWorkflowNextRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	workspace := r.PathValue("workspace")
	workflow := r.PathValue("workflow")

	if s.cronScheduler == nil {
		http.Error(w, "Cron scheduler not available", http.StatusServiceUnavailable)
		return
	}

	nextRuns := s.cronScheduler.getNextRuns()
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
