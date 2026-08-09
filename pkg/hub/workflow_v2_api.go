package hub

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
)

// handleWorkflowV2Run exposes the durable state-machine record used for
// operator inspection. It never reads conversation messages or transcripts.
func (s *Server) handleWorkflowV2Run(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	runID := strings.TrimSpace(r.PathValue("runId"))
	if runID == "" {
		jsonError(w, http.StatusBadRequest, "run id is required")
		return
	}

	var exists int
	err := s.db.QueryRowContext(r.Context(),
		`SELECT 1 FROM workflow_v2_runs WHERE id=? AND tenant_id=?`, runID, tenantFromCtx(r)).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		jsonError(w, http.StatusNotFound, "workflow run not found")
		return
	}
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "inspect workflow run")
		return
	}

	inspection, err := workflowv2.NewStore(s.db).InspectRun(r.Context(), runID)
	if errors.Is(err, sql.ErrNoRows) {
		jsonError(w, http.StatusNotFound, "workflow run not found")
		return
	}
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "inspect workflow run")
		return
	}
	jsonOK(w, inspection)
}
