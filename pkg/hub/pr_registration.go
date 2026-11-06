package hub

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
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
	if clawToken == "" || subtle.ConstantTimeCompare([]byte(clawToken), []byte(hubClawToken)) != 1 {
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

	registered, err := s.registerClawPR(r.Context(), clawID, tenantID, match[1], number, report.URL, report.Source)
	if errors.Is(err, errNoActivePRAttempt) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		log.Printf("[pr-registration] failed to register %s for claw %s: %v", report.URL, shortID(clawID), err)
		http.Error(w, "failed to register PR", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"registered": registered})
}

var errNoActivePRAttempt = errors.New("no active workflow V2 attempt")

// registerClawPR associates a PR without emitting a completion signal.
func (s *Server) registerClawPR(ctx context.Context, clawID, tenantID, repo string, number int, prURL, source string) (bool, error) {
	var err error
	s.mu.RLock()
	cc := s.claws[clawID]
	s.mu.RUnlock()
	if cc == nil {
		cc = &clawConn{id: clawID, tenantID: tenantID}
	}
	var registered bool
	if s.workflowV2OwnsExecution(cc) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		store := workflowv2.NewStore(s.db)
		binding, found, bindingErr := store.ActiveControlBinding(ctx, tenantID, clawID)
		if bindingErr != nil {
			return false, bindingErr
		}
		if !found {
			return false, errNoActivePRAttempt
		}
		var pr typesv2.VerifiedPullRequest
		pr, registered, err = store.RegisterPullRequest(ctx, binding.RunID, binding.AttemptID,
			typesv2.PullRequestClaim{URL: prURL}, s.workflowV2PullRequestVerifier())
		if err == nil {
			err = s.associateWorkflowV2TaskRunPR(clawID, pr)
		}
	} else {
		registered, err = s.storePRRegistration(clawID, repo, number, prURL, source)
	}
	return registered, err
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
