package hub

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// TaskRun analytics helpers

func (s *Server) createTaskRun(tenantID, runKind, ownerType, workspace, workflow, factory, ownerID, ownerDisplay, integration, integrationWorkspace, triggerID, externalTriggerID, issueID string, tags []string, analyticsEnabled, requiresPR bool) (string, string, error) {
	runID := uuid.New().String()
	attemptID := uuid.New().String()
	nowMs := time.Now().UTC().UnixMilli()

	tagsJSON, _ := json.Marshal(tags)
	enabled := 0
	if analyticsEnabled {
		enabled = 1
	}
	reqPR := 0
	if requiresPR {
		reqPR = 1
	}

	tx, err := s.db.Begin()
	if err != nil {
		return "", "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		`INSERT INTO task_runs(id,tenant_id,initial_attempt_id,current_attempt_id,attempt_count,run_kind,owner_type,workspace_name,workflow_name,factory_name,owner_id,owner_display_name,integration,integration_workspace,trigger_id,external_trigger_id,issue_id,tags,analytics_enabled,requires_pr,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		runID, tenantID, attemptID, attemptID, 1, runKind, ownerType, workspace, workflow, factory, ownerID, ownerDisplay, integration, integrationWorkspace, triggerID, externalTriggerID, issueID, string(tagsJSON), enabled, reqPR, nowMs, nowMs,
	)
	if err != nil {
		return "", "", fmt.Errorf("insert task_run: %w", err)
	}

	_, err = tx.Exec(
		`INSERT INTO task_run_attempts(id,tenant_id,run_id,attempt_id,attempt_number,status,started_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		attemptID, tenantID, runID, attemptID, 1, "running", nowMs, nowMs, nowMs,
	)
	if err != nil {
		return "", "", fmt.Errorf("insert task_run_attempt: %w", err)
	}

	_, err = tx.Exec(
		`INSERT INTO task_run_summaries(tenant_id,run_id,initial_attempt_id,current_attempt_id,attempt_count,run_kind,owner_type,workspace_name,workflow_name,factory_name,owner_id,owner_display_name,integration,issue_id,analytics_enabled,requires_pr,status,phase,started_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		tenantID, runID, attemptID, attemptID, 1, runKind, ownerType, workspace, workflow, factory, ownerID, ownerDisplay, integration, issueID, enabled, reqPR, "running", "claimed", nowMs, nowMs,
	)
	if err != nil {
		return "", "", fmt.Errorf("insert task_run_summary: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", "", fmt.Errorf("commit: %w", err)
	}
	return runID, attemptID, nil
}

func (s *Server) logTaskRunEvent(tenantID, runID, attemptID, eventKey, source, eventType string, eventTime, observedAt int64, actorType, actorSource, actorID, actorLogin, actorDisplay, interactionRole, targetType, targetID, targetURL string, detail map[string]any) error {
	detailJSON, _ := json.Marshal(detail)
	if len(detailJSON) > 8192 {
		detailJSON = detailJSON[:8192]
	}
	nowMs := time.Now().UTC().UnixMilli()
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO task_run_events(id,tenant_id,run_id,attempt_id,event_key,source,event_type,event_time,observed_at,actor_type,actor_source,actor_id,actor_login,actor_display_name,interaction_role,target_type,target_id,target_url,detail,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		uuid.New().String(), tenantID, runID, attemptID, eventKey, source, eventType, eventTime, observedAt, actorType, actorSource, actorID, actorLogin, actorDisplay, interactionRole, targetType, targetID, targetURL, string(detailJSON), nowMs,
	)
	if err != nil {
		log.Printf("[analytics] failed to log event %s for run %s: %v", eventType, runID, err)
	}
	return err
}

func (s *Server) materializeTaskRunSummary(tenantID, runID string) error {
	nowMs := time.Now().UTC().UnixMilli()

	// Count attempts
	var attemptCount int
	var currentAttemptID string
	row := s.db.QueryRow(`SELECT COUNT(*), MAX(id) FROM task_run_attempts WHERE tenant_id=? AND run_id=?`, tenantID, runID)
	_ = row.Scan(&attemptCount, &currentAttemptID)

	// Aggregate events
	rows, err := s.db.Query(
		`SELECT event_type, interaction_role, COUNT(*) FROM task_run_events WHERE tenant_id=? AND run_id=? GROUP BY event_type, interaction_role`,
		tenantID, runID,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	var humanInteractionCount int
	warningTypes := make(map[string]bool)
	var phase string
	var status string
	var failureType string
	var prCount, openPRCount, mergedPRCount int
	var mergedAt, prOpenedAt, finishedAt, lastEventAt int64
	var primaryPRURL string

	for rows.Next() {
		var et, ir string
		var cnt int
		if err := rows.Scan(&et, &ir, &cnt); err != nil {
			continue
		}
		switch et {
		case "run_claimed":
			phase = "claimed"
		case "run_queued":
			phase = "queued"
		case "provision_started":
			phase = "provisioning"
		case "claw_created", "agent_started":
			phase = "agent_running"
		case "pr_opened", "pr_associated":
			phase = "pr_opened"
			prCount += cnt
			if prOpenedAt == 0 {
				prOpenedAt = nowMs
			}
		case "pr_merged":
			mergedPRCount += cnt
			if mergedAt == 0 {
				mergedAt = nowMs
			}
		case "pr_closed_unmerged":
			openPRCount -= cnt
		case "creation_failed", "provision_failed", "bootstrap_failed", "agent_stopped", "manual_stop_before_delivery", "provider_lost", "done_without_pr", "permission_or_auth_failed", "timeout", "unknown_failure":
			failureType = et
			status = "failed"
			phase = "terminal"
			finishedAt = nowMs
		}
		if ir == "warning" || ir == "terminal" {
			humanInteractionCount += cnt
		}
		if ir == "warning" {
			warningTypes[et] = true
		}
		lastEventAt = nowMs
	}
	_ = rows.Err()

	// PR state from task_run_prs
	prRows, err := s.db.Query(`SELECT state, merged, pr_url FROM task_run_prs WHERE tenant_id=? AND run_id=?`, tenantID, runID)
	if err == nil {
		defer prRows.Close()
		prCount = 0
		openPRCount = 0
		mergedPRCount = 0
		for prRows.Next() {
			var st string
			var merged int
			var url string
			if err := prRows.Scan(&st, &merged, &url); err != nil {
				continue
			}
			prCount++
			if st == "open" {
				openPRCount++
			}
			if merged == 1 {
				mergedPRCount++
				if primaryPRURL == "" {
					primaryPRURL = url
				}
			}
		}
		_ = prRows.Err()
	}

	// Determine terminal status
	if status == "" {
		if mergedPRCount > 0 && openPRCount == 0 {
			if humanInteractionCount > 0 {
				status = "warning_success"
			} else {
				status = "clean_success"
			}
			phase = "terminal"
			finishedAt = nowMs
		} else if prCount > 0 && openPRCount == 0 && mergedPRCount == 0 {
			status = "failed"
			failureType = "pr_closed_unmerged"
			phase = "terminal"
			finishedAt = nowMs
		} else if prCount > 0 {
			phase = "waiting_for_merge"
		}
	}

	wtList := make([]string, 0, len(warningTypes))
	for w := range warningTypes {
		wtList = append(wtList, w)
	}
	wtJSON, _ := json.Marshal(wtList)

	_, err = s.db.Exec(
		`UPDATE task_run_summaries SET current_attempt_id=?,attempt_count=?,status=?,phase=?,primary_pr_url=?,pr_count=?,open_pr_count=?,merged_pr_count=?,failure_type=?,warning_types=?,human_interaction_count=?,pr_opened_at=?,merged_at=?,finished_at=?,last_event_at=?,updated_at=? WHERE tenant_id=? AND run_id=?`,
		currentAttemptID, attemptCount, status, phase, primaryPRURL, prCount, openPRCount, mergedPRCount, failureType, string(wtJSON), humanInteractionCount, prOpenedAt, mergedAt, finishedAt, lastEventAt, nowMs, tenantID, runID,
	)
	if err != nil {
		log.Printf("[analytics] failed to materialize summary for run %s: %v", runID, err)
	}
	return err
}

func (s *Server) linkClawToTaskRun(clawID, runID string) {
	_, _ = s.db.Exec(`UPDATE claws SET task_run_id=? WHERE id=?`, runID, clawID)
}

func (s *Server) linkFactoryTriggerToTaskRun(triggerID, runID string) {
	_, _ = s.db.Exec(`UPDATE factory_triggers SET task_run_id=? WHERE id=?`, runID, triggerID)
}

// API handlers

func (s *Server) handleAnalyticsSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	fromStr := q.Get("from")
	toStr := q.Get("to")
	if fromStr == "" || toStr == "" {
		http.Error(w, "from and to required", http.StatusBadRequest)
		return
	}
	fromMs, _ := strconv.ParseInt(fromStr, 10, 64)
	toMs, _ := strconv.ParseInt(toStr, 10, 64)

	workspace := q.Get("workspace")
	ownerType := q.Get("ownerType")
	workflow := q.Get("workflow")
	factory := q.Get("factory")
	status := q.Get("status")
	model := q.Get("model")
	repo := q.Get("repo")
	integration := q.Get("integration")
	issueID := q.Get("issueId")
	clawID := q.Get("clawId")
	search := q.Get("q")

	where := "WHERE tenant_id='default' AND analytics_enabled=1 AND started_at >= ? AND started_at <= ?"
	args := []any{fromMs, toMs}

	if workspace != "" {
		where += " AND workspace_name = ?"
		args = append(args, workspace)
	}
	if ownerType != "" {
		where += " AND owner_type = ?"
		args = append(args, ownerType)
	}
	if workflow != "" {
		where += " AND workflow_name = ?"
		args = append(args, workflow)
	}
	if factory != "" {
		where += " AND factory_name = ?"
		args = append(args, factory)
	}
	if status != "" {
		where += " AND status = ?"
		args = append(args, status)
	}
	if model != "" {
		where += " AND model = ?"
		args = append(args, model)
	}
	if repo != "" {
		where += " AND repo = ?"
		args = append(args, repo)
	}
	if integration != "" {
		where += " AND integration = ?"
		args = append(args, integration)
	}
	if issueID != "" {
		where += " AND issue_id = ?"
		args = append(args, issueID)
	}
	if clawID != "" {
		where += " AND claw_id = ?"
		args = append(args, clawID)
	}
	if search != "" {
		where += " AND (run_id LIKE ? OR issue_id LIKE ? OR factory_name LIKE ? OR workflow_name LIKE ?)"
		like := "%" + search + "%"
		args = append(args, like, like, like, like)
	}

	var totalRuns, terminalRuns, runningRuns, cleanSuccess, warningSuccess, failed, prOpened, prMerged int
	row := s.db.QueryRow(
		fmt.Sprintf(`SELECT COUNT(*), SUM(CASE WHEN status IN ('clean_success','warning_success','failed') THEN 1 ELSE 0 END), SUM(CASE WHEN status='running' THEN 1 ELSE 0 END), SUM(CASE WHEN status='clean_success' THEN 1 ELSE 0 END), SUM(CASE WHEN status='warning_success' THEN 1 ELSE 0 END), SUM(CASE WHEN status='failed' THEN 1 ELSE 0 END), SUM(CASE WHEN pr_count > 0 THEN 1 ELSE 0 END), SUM(CASE WHEN merged_pr_count > 0 THEN 1 ELSE 0 END) FROM task_run_summaries %s`, where),
		args...,
	)
	_ = row.Scan(&totalRuns, &terminalRuns, &runningRuns, &cleanSuccess, &warningSuccess, &failed, &prOpened, &prMerged)

	excludedRuns := 0
	excludedNonPR := 0
	s.db.QueryRow(fmt.Sprintf(`SELECT COUNT(*), SUM(CASE WHEN excluded_reason='requires_pr_not_configured' OR excluded_reason='non_pr_workflow' THEN 1 ELSE 0 END) FROM task_run_summaries %s AND analytics_enabled=0`, where), args...).Scan(&excludedRuns, &excludedNonPR)

	var cleanRate, warningRate, failureRate, prOpenRate, prMergeRate *float64
	if terminalRuns > 0 {
		c := float64(cleanSuccess) / float64(terminalRuns) * 100
		w := float64(warningSuccess) / float64(terminalRuns) * 100
		f := float64(failed) / float64(terminalRuns) * 100
		cleanRate, warningRate, failureRate = &c, &w, &f
	}
	if totalRuns > 0 {
		po := float64(prOpened) / float64(totalRuns) * 100
		pm := float64(prMerged) / float64(totalRuns) * 100
		prOpenRate, prMergeRate = &po, &pm
	}

	// Children breakdown by workspace
	children := []map[string]any{}
	childRows, err := s.db.Query(
		fmt.Sprintf(`SELECT workspace_name, COUNT(*), SUM(CASE WHEN status='clean_success' THEN 1 ELSE 0 END), SUM(CASE WHEN status='warning_success' THEN 1 ELSE 0 END), SUM(CASE WHEN status='failed' THEN 1 ELSE 0 END) FROM task_run_summaries %s GROUP BY workspace_name ORDER BY COUNT(*) DESC`, where),
		args...,
	)
	if err == nil {
		defer childRows.Close()
		for childRows.Next() {
			var ws string
			var runs, cs, wsuc, fl int
			if err := childRows.Scan(&ws, &runs, &cs, &wsuc, &fl); err != nil {
				continue
			}
			var cR, wR, fR *float64
			term := cs + wsuc + fl
			if term > 0 {
				c := float64(cs) / float64(term) * 100
				w := float64(wsuc) / float64(term) * 100
				f := float64(fl) / float64(term) * 100
				cR, wR, fR = &c, &w, &f
			}
			children = append(children, map[string]any{
				"id": ws, "label": ws, "kind": "workspace",
				"totals": map[string]int{"runs": runs, "terminalRuns": term, "cleanSuccess": cs, "warningSuccess": wsuc, "failed": fl},
				"rates": map[string]any{"cleanSuccessRate": cR, "warningSuccessRate": wR, "failureRate": fR},
				"href": "/settings/analytics?workspace=" + ws,
			})
		}
		_ = childRows.Err()
	}

	// Metadata
	var dataStartsAt, lastMat int64
	var coverageWarnings string
	row = s.db.QueryRow(`SELECT data_starts_at, last_materialized_at, coverage_warnings FROM task_run_analytics_metadata WHERE tenant_id='default'`)
	_ = row.Scan(&dataStartsAt, &lastMat, &coverageWarnings)

	jsonOK(w, map[string]any{
		"scope": "global",
		"range": map[string]int64{"from": fromMs, "to": toMs},
		"coverage": map[string]any{
			"dataStartsAt":              dataStartsAt,
			"partialHistory":            dataStartsAt > 0 && fromMs < dataStartsAt,
			"eventsAvailableSince":      dataStartsAt,
			"eventsExpireAfter":         "P1Y",
			"timelineUnavailableBefore": nil,
			"stale":                     false,
			"lastMaterializedAt":        lastMat,
			"coverageWarnings":          json.RawMessage(coverageWarnings),
		},
		"totals": map[string]int{
			"runs": totalRuns, "excludedRuns": excludedRuns, "excludedNonPrRuns": excludedNonPR,
			"terminalRuns": terminalRuns, "runningRuns": runningRuns,
			"cleanSuccess": cleanSuccess, "warningSuccess": warningSuccess, "failed": failed,
			"prOpened": prOpened, "prMerged": prMerged,
		},
		"rates": map[string]any{
			"cleanSuccessRate": cleanRate, "warningSuccessRate": warningRate, "failureRate": failureRate,
			"prOpenedRate": prOpenRate, "prMergedRate": prMergeRate,
		},
		"children": children,
	})
}

func (s *Server) handleAnalyticsRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	fromStr := q.Get("from")
	toStr := q.Get("to")
	if fromStr == "" || toStr == "" {
		http.Error(w, "from and to required", http.StatusBadRequest)
		return
	}
	fromMs, _ := strconv.ParseInt(fromStr, 10, 64)
	toMs, _ := strconv.ParseInt(toStr, 10, 64)

	limit := 50
	if l := q.Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
		if limit < 1 {
			limit = 50
		}
		if limit > 200 {
			limit = 200
		}
	}

	workspace := q.Get("workspace")
	ownerType := q.Get("ownerType")
	workflow := q.Get("workflow")
	factory := q.Get("factory")
	status := q.Get("status")
	model := q.Get("model")
	repo := q.Get("repo")
	integration := q.Get("integration")
	issueID := q.Get("issueId")
	clawID := q.Get("clawId")
	search := q.Get("q")

	where := "WHERE tenant_id='default' AND analytics_enabled=1 AND started_at >= ? AND started_at <= ?"
	args := []any{fromMs, toMs}

	if workspace != "" {
		where += " AND workspace_name = ?"
		args = append(args, workspace)
	}
	if ownerType != "" {
		where += " AND owner_type = ?"
		args = append(args, ownerType)
	}
	if workflow != "" {
		where += " AND workflow_name = ?"
		args = append(args, workflow)
	}
	if factory != "" {
		where += " AND factory_name = ?"
		args = append(args, factory)
	}
	if status != "" {
		where += " AND status = ?"
		args = append(args, status)
	}
	if model != "" {
		where += " AND model = ?"
		args = append(args, model)
	}
	if repo != "" {
		where += " AND repo = ?"
		args = append(args, repo)
	}
	if integration != "" {
		where += " AND integration = ?"
		args = append(args, integration)
	}
	if issueID != "" {
		where += " AND issue_id = ?"
		args = append(args, issueID)
	}
	if clawID != "" {
		where += " AND claw_id = ?"
		args = append(args, clawID)
	}
	if search != "" {
		where += " AND (run_id LIKE ? OR issue_id LIKE ? OR factory_name LIKE ? OR workflow_name LIKE ?)"
		like := "%" + search + "%"
		args = append(args, like, like, like, like)
	}

	rows, err := s.db.Query(
		fmt.Sprintf(`SELECT run_id,initial_attempt_id,current_attempt_id,attempt_count,run_kind,owner_type,workspace_name,workflow_name,factory_name,integration,issue_id,claw_id,status,phase,model,llm_key,primary_pr_url,pr_count,open_pr_count,merged_pr_count,failure_type,warning_types,human_interaction_count,started_at,finished_at,last_event_at FROM task_run_summaries %s ORDER BY started_at DESC, run_id DESC LIMIT ?`, where),
		append(args, limit)...,
	)
	if err != nil {
		log.Printf("[analytics] runs query error: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	items := []map[string]any{}
	for rows.Next() {
		var ri, ia, ca, rk, ot, ws, wf, fa, ig, isid, cid, st, ph, mo, lk, ppr, ft, wt string
		var ac, pc, opc, mpc, hic int
		var sa, faT, lea int64
		if err := rows.Scan(&ri, &ia, &ca, &ac, &rk, &ot, &ws, &wf, &fa, &ig, &isid, &cid, &st, &ph, &mo, &lk, &ppr, &pc, &opc, &mpc, &ft, &wt, &hic, &sa, &faT, &lea); err != nil {
			continue
		}
		var wta []string
		_ = json.Unmarshal([]byte(wt), &wta)
		items = append(items, map[string]any{
			"runId": ri, "initialAttemptId": ia, "currentAttemptId": ca, "attemptCount": ac,
			"runKind": rk, "ownerType": ot, "workspaceName": ws, "workflowName": wf, "factoryName": fa,
			"integration": ig, "issueId": isid, "clawId": cid, "status": st, "phase": ph,
			"model": mo, "llmKey": lk, "primaryPrUrl": ppr, "prCount": pc, "openPrCount": opc,
			"mergedPrCount": mpc, "failureType": ft, "warningTypes": wta, "humanInteractionCount": hic,
			"startedAt": sa, "finishedAt": faT, "lastEventAt": lea,
		})
	}
	_ = rows.Err()

	jsonOK(w, map[string]any{
		"items": items,
		"pagination": map[string]any{"nextCursor": nil, "hasMore": false, "limit": limit},
	})
}

func (s *Server) handleAnalyticsRunAttempts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	runID := r.PathValue("id")
	if runID == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	rows, err := s.db.Query(`SELECT attempt_id,attempt_number,trigger_id,claw_id,status,failure_type,started_at,finished_at FROM task_run_attempts WHERE tenant_id='default' AND run_id=? ORDER BY attempt_number`, runID)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var ai, trid, cid, st, ft string
		var an int
		var sa, fa int64
		if err := rows.Scan(&ai, &an, &trid, &cid, &st, &ft, &sa, &fa); err != nil {
			continue
		}
		items = append(items, map[string]any{
			"attemptId": ai, "attemptNumber": an, "triggerId": trid, "clawId": cid,
			"status": st, "failureType": ft, "startedAt": sa, "finishedAt": fa,
		})
	}
	_ = rows.Err()
	jsonOK(w, items)
}

func (s *Server) handleAnalyticsRunEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	runID := r.PathValue("id")
	if runID == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	limit := 200
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
		if limit < 1 {
			limit = 200
		}
		if limit > 500 {
			limit = 500
		}
	}
	rows, err := s.db.Query(
		`SELECT id,event_type,event_time,observed_at,source,actor_type,actor_login,interaction_role,target_type,target_url,detail FROM task_run_events WHERE tenant_id='default' AND run_id=? ORDER BY event_time ASC, observed_at ASC, event_key ASC LIMIT ?`,
		runID, limit,
	)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, et, src, at, al, ir, tt, tu, det string
		var etime, oa int64
		if err := rows.Scan(&id, &et, &etime, &oa, &src, &at, &al, &ir, &tt, &tu, &det); err != nil {
			continue
		}
		var detail map[string]any
		_ = json.Unmarshal([]byte(det), &detail)
		items = append(items, map[string]any{
			"id": id, "eventType": et, "eventTime": etime, "observedAt": oa,
			"source": src, "actor": map[string]string{"type": at, "login": al},
			"interactionRole": ir, "target": map[string]string{"type": tt, "url": tu},
			"detail": detail,
		})
	}
	_ = rows.Err()
	jsonOK(w, items)
}

func (s *Server) handleAnalyticsRunPRs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	runID := r.PathValue("id")
	if runID == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	rows, err := s.db.Query(`SELECT repo,pr_number,pr_url,head_branch,head_sha,state,merged,opened_at,closed_at,merged_at,merged_by_login FROM task_run_prs WHERE tenant_id='default' AND run_id=?`, runID)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var repo, url, hb, hs, st, mbl string
		var pn int
		var merged int
		var oa, ca, ma int64
		if err := rows.Scan(&repo, &pn, &url, &hb, &hs, &st, &merged, &oa, &ca, &ma, &mbl); err != nil {
			continue
		}
		items = append(items, map[string]any{
			"repo": repo, "prNumber": pn, "url": url, "headBranch": hb, "headSha": hs,
			"state": st, "merged": merged == 1, "openedAt": oa, "closedAt": ca,
			"mergedAt": ma, "mergedByLogin": mbl,
		})
	}
	_ = rows.Err()
	jsonOK(w, items)
}

func (s *Server) handleAnalyticsFilterOptions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	dimension := r.URL.Query().Get("dimension")
	if dimension == "" {
		http.Error(w, "dimension required", http.StatusBadRequest)
		return
	}
	var col string
	switch dimension {
	case "model":
		col = "model"
	case "status":
		col = "status"
	case "warningType":
		jsonOK(w, []map[string]any{})
		return
	case "failureType":
		col = "failure_type"
	case "integration":
		col = "integration"
	case "repo":
		col = "repo"
	case "runKind":
		col = "run_kind"
	case "phase":
		col = "phase"
	default:
		http.Error(w, "unknown dimension", http.StatusBadRequest)
		return
	}
	rows, err := s.db.Query(fmt.Sprintf(`SELECT %s, COUNT(*) FROM task_run_summaries WHERE tenant_id='default' AND analytics_enabled=1 AND %s != '' GROUP BY %s ORDER BY COUNT(*) DESC, %s ASC LIMIT 100`, col, col, col, col))
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var val string
		var cnt int
		if err := rows.Scan(&val, &cnt); err != nil {
			continue
		}
		items = append(items, map[string]any{"value": val, "label": val, "count": cnt})
	}
	_ = rows.Err()
	jsonOK(w, items)
}

func (s *Server) handleAnalyticsRunDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	runID := r.PathValue("id")
	if runID == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var ri, ia, ca, rk, ot, ws, wf, fa, ig, isid, cid, st, ph, mo, lk, ppr, ft, wt string
	var ac, pc, opc, mpc, hic int
	var sa, faT, lea int64
	err := s.db.QueryRow(
		`SELECT run_id,initial_attempt_id,current_attempt_id,attempt_count,run_kind,owner_type,workspace_name,workflow_name,factory_name,integration,issue_id,claw_id,status,phase,model,llm_key,primary_pr_url,pr_count,open_pr_count,merged_pr_count,failure_type,warning_types,human_interaction_count,started_at,finished_at,last_event_at FROM task_run_summaries WHERE tenant_id='default' AND run_id=?`,
		runID,
	).Scan(&ri, &ia, &ca, &ac, &rk, &ot, &ws, &wf, &fa, &ig, &isid, &cid, &st, &ph, &mo, &lk, &ppr, &pc, &opc, &mpc, &ft, &wt, &hic, &sa, &faT, &lea)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	var wta []string
	_ = json.Unmarshal([]byte(wt), &wta)
	jsonOK(w, map[string]any{
		"runId": ri, "initialAttemptId": ia, "currentAttemptId": ca, "attemptCount": ac,
		"runKind": rk, "ownerType": ot, "workspaceName": ws, "workflowName": wf, "factoryName": fa,
		"integration": ig, "issueId": isid, "clawId": cid, "status": st, "phase": ph,
		"model": mo, "llmKey": lk, "primaryPrUrl": ppr, "prCount": pc, "openPrCount": opc,
		"mergedPrCount": mpc, "failureType": ft, "warningTypes": wta, "humanInteractionCount": hic,
		"startedAt": sa, "finishedAt": faT, "lastEventAt": lea,
	})
}

// instrument helpers used by linear.go, server.go, etc.

func (s *Server) startTaskRunFromFactory(factoryName, workspaceName, workflowName, issueID, triggerID, externalTriggerID, integration, integrationWorkspace string, tags []string) (string, string, error) {
	return s.createTaskRun("default", "code_task", "factory", workspaceName, workflowName, factoryName, "", factoryName, integration, integrationWorkspace, triggerID, externalTriggerID, issueID, tags, true, true)
}

func (s *Server) startTaskRunFromWorkflow(workspaceName, workflowName, runKind, ownerType, issueID, triggerID string, tags []string) (string, string, error) {
	return s.createTaskRun("default", runKind, ownerType, workspaceName, workflowName, "", "", workflowName, "", "", triggerID, "", issueID, tags, true, true)
}

func (s *Server) recordTaskRunEvent(tenantID, runID, attemptID, eventKey, source, eventType string, eventTime int64, actorType, actorLogin, interactionRole, targetType, targetURL string, detail map[string]any) {
	observedAt := time.Now().UTC().UnixMilli()
	if eventTime == 0 {
		eventTime = observedAt
	}
	_ = s.logTaskRunEvent(tenantID, runID, attemptID, eventKey, source, eventType, eventTime, observedAt, actorType, source, "", actorLogin, actorLogin, interactionRole, targetType, "", targetURL, detail)
	_ = s.materializeTaskRunSummary(tenantID, runID)
}

func (s *Server) recordTaskRunTerminal(tenantID, runID, attemptID, failureType, source string) {
	s.recordTaskRunEvent(tenantID, runID, attemptID, fmt.Sprintf("terminal:%s:%d", failureType, time.Now().Unix()), source, failureType, 0, "system", "", "terminal", "", "", map[string]any{"failureType": failureType})
	_, _ = s.db.Exec(`UPDATE task_run_attempts SET status='failed',failure_type=?,finished_at=?,updated_at=? WHERE tenant_id=? AND id=?`, failureType, time.Now().UTC().UnixMilli(), time.Now().UTC().UnixMilli(), tenantID, attemptID)
}

func (s *Server) recordTaskRunPR(tenantID, runID, repo string, prNumber int, prURL, headBranch, headSHA string, state string, merged bool, mergedBy string) {
	nowMs := time.Now().UTC().UnixMilli()
	mergedInt := 0
	if merged {
		mergedInt = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO task_run_prs(id,tenant_id,run_id,repo,pr_number,pr_url,head_branch,head_sha,state,merged,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(tenant_id,run_id,repo,pr_number) DO UPDATE SET state=excluded.state, merged=excluded.merged, updated_at=excluded.updated_at`,
		uuid.New().String(), tenantID, runID, repo, prNumber, prURL, headBranch, headSHA, state, mergedInt, nowMs, nowMs,
	)
	if err != nil {
		log.Printf("[analytics] failed to record PR %s#%d for run %s: %v", repo, prNumber, runID, err)
	}
	if merged {
		_, _ = s.db.Exec(`UPDATE task_run_prs SET merged_at=?, merged_by_login=? WHERE tenant_id=? AND run_id=? AND repo=? AND pr_number=?`, nowMs, mergedBy, tenantID, runID, repo, prNumber)
	}
	_ = s.materializeTaskRunSummary(tenantID, runID)
}

func (s *Server) recordTaskRunClawCreated(tenantID, runID, attemptID, clawID string) {
	s.recordTaskRunEvent(tenantID, runID, attemptID, "claw_created", "hub", "claw_created", 0, "system", "", "neutral", "claw", clawID, map[string]any{"clawId": clawID})
	_, _ = s.db.Exec(`UPDATE task_runs SET claw_id=?, updated_at=? WHERE tenant_id=? AND id=?`, clawID, time.Now().UTC().UnixMilli(), tenantID, runID)
	_, _ = s.db.Exec(`UPDATE task_run_summaries SET claw_id=?, updated_at=? WHERE tenant_id=? AND run_id=?`, clawID, time.Now().UTC().UnixMilli(), tenantID, runID)
}

func (s *Server) recordTaskRunDoneSignal(tenantID, runID, attemptID string, prCount int) {
	s.recordTaskRunEvent(tenantID, runID, attemptID, fmt.Sprintf("done_signal:%d", time.Now().Unix()), "agent", "done_signal", 0, "agent", "", "neutral", "", "", map[string]any{"prCount": prCount})
}

func (s *Server) recordTaskRunModelSelected(tenantID, runID, attemptID, model, llmKey string) {
	s.recordTaskRunEvent(tenantID, runID, attemptID, "model_selected", "hub", "model_selected", 0, "system", "", "neutral", "", "", map[string]any{"model": model, "llmKey": llmKey})
	_, _ = s.db.Exec(`UPDATE task_run_summaries SET model=?, llm_key=?, updated_at=? WHERE tenant_id=? AND run_id=?`, model, llmKey, time.Now().UTC().UnixMilli(), tenantID, runID)
}
