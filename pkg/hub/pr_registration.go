package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

// handleClawPRRegistration records creation without signaling completion.
func (s *Server) handleClawPRRegistration(w http.ResponseWriter, r *http.Request) {
	clawToken := r.URL.Query().Get("claw_token")
	if clawToken == "" {
		clawToken = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	s.mu.RLock()
	hubClawToken := s.hubCfg.ClawToken
	s.mu.RUnlock()
	if clawToken == "" || clawToken != hubClawToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	clawID := r.PathValue("id")
	var tenantID, status, reposJSON string
	err := s.db.QueryRow(`SELECT tenant_id, status, COALESCE(github_repos,'[]') FROM claws WHERE id=?`, clawID).Scan(&tenantID, &status, &reposJSON)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "claw not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	var report struct {
		URL    string `json:"url"`
		Source string `json:"source"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	if err := decoder.Decode(&report); err != nil || decoder.Decode(new(any)) != io.EOF {
		http.Error(w, "invalid PR report", http.StatusBadRequest)
		return
	}
	match := prURLRegex.FindStringSubmatch(report.URL)
	if len(match) == 0 || match[0] != report.URL || report.Source != "gh_create" {
		http.Error(w, "invalid PR URL or source", http.StatusBadRequest)
		return
	}
	number, err := strconv.Atoi(match[2])
	if err != nil || number <= 0 {
		http.Error(w, "invalid PR number", http.StatusBadRequest)
		return
	}
	// JSON field matching accepts both legacy capitalized and current keys.
	var repos []RepoAccess
	if json.Unmarshal([]byte(reposJSON), &repos) != nil || effectiveRepoAccess(match[1], repos) == nil {
		http.Error(w, "requested repo is not configured on this claw", http.StatusForbidden)
		return
	}
	if status == "deleted" || status == "error" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"registered": false})
		return
	}

	s.mu.RLock()
	cc := s.claws[clawID]
	s.mu.RUnlock()
	if cc == nil {
		cc = &clawConn{id: clawID, tenantID: tenantID}
	}
	var registered bool
	if s.workflowV2OwnsExecution(cc) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		store := workflowv2.NewStore(s.db)
		binding, found, bindingErr := store.ActiveControlBinding(ctx, tenantID, clawID)
		if bindingErr != nil {
			http.Error(w, "failed to resolve workflow V2 attempt", http.StatusInternalServerError)
			return
		}
		if !found {
			http.Error(w, "no active workflow V2 attempt", http.StatusConflict)
			return
		}
		var pr typesv2.VerifiedPullRequest
		pr, registered, err = store.RegisterPullRequest(ctx, binding.RunID, binding.AttemptID,
			typesv2.PullRequestClaim{URL: report.URL}, s.workflowV2PullRequestVerifier())
		if err == nil {
			err = s.associateWorkflowV2TaskRunPR(clawID, pr)
		}
	} else {
		registered, err = s.storePRMention(clawID, match[1], number, report.URL, false, report.Source)
	}
	if err != nil {
		http.Error(w, "failed to register PR", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"registered": registered})
}

func (s *Server) associateWorkflowV2TaskRunPR(clawID string, pr typesv2.VerifiedPullRequest) error {
	_, runID, _, ok, err := s.taskRunContextForClaw(clawID)
	if err != nil || !ok {
		return err
	}
	state := pr.State
	if state == "merged" {
		state = taskRunPRStateClosed
	}
	return s.associateTaskRunPR(TaskRunPR{
		RunID: runID, Repo: pr.Repository, PRNumber: pr.Number, URL: pr.URL,
		HeadSHA: pr.HeadSHA, AgentHeadSHA: true, State: state, Merged: pr.State == "merged",
	})
}
