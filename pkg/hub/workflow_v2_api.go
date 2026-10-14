package hub

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
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

type workflowV2CancelRequest struct {
	Reason string `json:"reason"`
}

func (s *Server) handleWorkflowV2RunCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	runID := strings.TrimSpace(r.PathValue("runId"))
	if runID == "" {
		jsonError(w, http.StatusBadRequest, "run id is required")
		return
	}
	decoder := json.NewDecoder(r.Body)
	var request workflowV2CancelRequest
	if err := decoder.Decode(&request); err != nil && !errors.Is(err, io.EOF) {
		jsonError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			jsonError(w, http.StatusBadRequest, "invalid JSON: multiple values")
		} else {
			jsonError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		}
		return
	}
	var tenantID, status string
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT tenant_id,status FROM workflow_v2_runs WHERE id=?`, runID).Scan(&tenantID, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			jsonError(w, http.StatusNotFound, "workflow run not found")
			return
		}
		jsonError(w, http.StatusInternalServerError, "lookup workflow run")
		return
	}
	if tenantID != tenantFromCtx(r) {
		jsonError(w, http.StatusNotFound, "workflow run not found")
		return
	}
	if status != string(workflowv2.RunActive) && status != string(workflowv2.RunSuspended) {
		jsonError(w, http.StatusConflict, "workflow run is already terminal")
		return
	}
	if err := workflowv2.NewStore(s.db).CancelActivation(r.Context(), runID, request.Reason); err != nil {
		jsonError(w, http.StatusInternalServerError, "cancel workflow run")
		return
	}
	s.maybeFinishWorkflowV2Parent(r.Context(), runID)
	inspection, err := workflowv2.NewStore(s.db).InspectRun(r.Context(), runID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "inspect cancelled workflow run")
		return
	}
	jsonOK(w, inspection)
}
