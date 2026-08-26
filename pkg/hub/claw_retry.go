package hub

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
	"nhooyr.io/websocket"
)

const maxClawAttempts = 3

const prepareClawRetryAttempts = 3

var clawRetryBackoff = []time.Duration{0, 30 * time.Second, 2 * time.Minute}

var clawStopRevaluationDelays = []time.Duration{time.Second, 5 * time.Second}

type clawRetryDisposition int

const (
	clawRetryNotApplicable clawRetryDisposition = iota
	clawRetryScheduled
	clawRetryAlreadyHandled
	clawRetryIndeterminate
)

type clawRetryPlan struct {
	tenantID    string
	attempt     int
	failureType string
}

func retryableAgentFailure(kind agentFailureKind) bool {
	switch kind {
	case agentFailureProvisioning, agentFailureBootstrap, agentFailureSandboxTerminated,
		agentFailureRestore, agentFailureWorkspaceReadiness, agentFailureWorkspaceFiles,
		agentFailureProviderLost:
		return true
	default:
		return false
	}
}

func taskRunFailureTypeForAgentFailure(kind agentFailureKind) string {
	switch kind {
	case agentFailureProvisioning:
		return taskRunFailureProvisionFailed
	case agentFailureSandboxTerminated, agentFailureProviderLost:
		return taskRunFailureProviderLost
	case agentFailureBootstrap, agentFailureRestore, agentFailureWorkspaceReadiness, agentFailureWorkspaceFiles:
		return taskRunFailureBootstrapFailed
	case agentFailureGitHubCredentials:
		return taskRunFailurePermissionOrAuth
	default:
		return taskRunFailureUnknown
	}
}

func isProtectedClawStatus(status string) bool {
	return status == "idle" || status == "completed" || status == "deleted"
}

type watchdogHealthAction int

const (
	watchdogHealthNone watchdogHealthAction = iota
	watchdogHealthWarn
	watchdogHealthEscalate
)

func heartbeatShouldEscalate(unhealthyCount, gatewayUnhealthyMax int, status string, bootstrapOK bool) bool {
	return unhealthyCount == gatewayUnhealthyMax && status == "connected" && bootstrapOK
}

func watchdogAction(nowAt time.Time, status string, bootstrapOK, gatewayReady bool, lastStatusAt, lastUserMessageAt, warnedAt time.Time, silentDeathMax time.Duration) watchdogHealthAction {
	if status != "connected" || !bootstrapOK || !gatewayReady {
		return watchdogHealthNone
	}
	silentFor := nowAt.Sub(lastStatusAt)
	if userSilentFor := nowAt.Sub(lastUserMessageAt); userSilentFor < silentFor {
		silentFor = userSilentFor
	}
	if warnedAt.IsZero() && silentFor > 5*time.Minute {
		return watchdogHealthWarn
	}
	if !warnedAt.IsZero() && silentFor > silentDeathMax && nowAt.Sub(warnedAt) >= 5*time.Minute {
		return watchdogHealthEscalate
	}
	return watchdogHealthNone
}

func (s *Server) escalateClawHealthFailure(clawID, reason string) {
	var status string
	var bootstrapOK int
	if err := s.db.QueryRow(`SELECT status, bootstrap_ok FROM claws WHERE id=?`, clawID).Scan(&status, &bootstrapOK); err != nil {
		return
	}
	if status != "connected" || bootstrapOK == 0 || isProtectedClawStatus(status) {
		return
	}
	s.stopAgentWithReason(clawID, reason, false)
}

func (s *Server) escalateGatewayHealthFailure(clawID string) {
	unhealthyMax := s.livenessSettings().gatewayUnhealthyMax
	if s.gatewayUnhealthyCount(clawID) < unhealthyMax {
		return
	}
	// Report the check count rather than a duration: the threshold is configurable
	// via gateway_unhealthy_checks, and the heartbeat cadence that would turn it into
	// a duration lives in the bridge, not here. Keep the "workspace unresponsive"
	// prefix — classifyAgentFailure matches on it.
	s.escalateClawHealthFailure(clawID, fmt.Sprintf("workspace unresponsive: gateway unhealthy for %d consecutive checks", unhealthyMax))
}

// escalateIdleResumeFailure is the escalation checkAgentIdleResume falls back
// to once repeated auto-resume prompts have not unstuck a claw (see
// defaultIdleResumeEscalateAfter / idle_resume_escalate_after). It shares the
// exact tear-down-and-replace path escalateGatewayHealthFailure already uses
// for a wedged gateway: stopAgentWithReason, which retries through
// scheduleClawRetry when the failure is retryable and otherwise settles the
// claw terminally, rather than leaving it parked for a human to notice.
//
// Safety: the caller (checkAgentIdleResume) only reaches here after
// confirming isBusyLocked() is false AND that a turn boundary has actually
// been observed on this connection (or the blind-resume grace has elapsed) —
// the same boundary care agent_idle.go's comment block demands before any
// resume is sent. This never fires against a live turn.
func (s *Server) escalateIdleResumeFailure(clawID string, resumeCount int, idleFor time.Duration) {
	reason := fmt.Sprintf("workspace unresponsive: agent unresponsive after %d auto-resume attempts (%d minutes idle) with no progress", resumeCount, int(idleFor.Minutes()))
	log.Printf("[agent-idle] claw %s had %d failed auto-resumes; escalating instead of resuming again", shortID(clawID), resumeCount)
	s.escalateClawHealthFailure(clawID, reason)
}

// prepareClawRetry atomically fails the current attempt and creates its
// successor. Updating the claw to error in the same transaction keeps another
// detector from consuming a second attempt while replacement is waiting.
func (s *Server) prepareClawRetry(clawID, reason string) (clawRetryDisposition, clawRetryPlan, error) {
	var disposition clawRetryDisposition
	var plan clawRetryPlan
	var err error
	for attempt := 0; attempt < prepareClawRetryAttempts; attempt++ {
		disposition, plan, err = s.prepareClawRetryOnce(clawID, reason)
		if err == nil || !isSQLiteBusy(err) {
			return disposition, plan, err
		}
		if attempt+1 < prepareClawRetryAttempts {
			time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
		}
	}
	return disposition, plan, err
}

func (s *Server) prepareClawRetryOnce(clawID, reason string) (clawRetryDisposition, clawRetryPlan, error) {
	failure := classifyAgentFailure(reason)
	if !retryableAgentFailure(failure.Kind) {
		return clawRetryNotApplicable, clawRetryPlan{}, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return clawRetryNotApplicable, clawRetryPlan{}, err
	}
	defer tx.Rollback()

	var tenantID, status, provider, runID string
	if err := tx.QueryRow(`
		SELECT tenant_id, status, COALESCE(provider,''), COALESCE(task_run_id,'')
		  FROM claws WHERE id=?`, clawID).Scan(&tenantID, &status, &provider, &runID); err != nil {
		if err == sql.ErrNoRows {
			return clawRetryAlreadyHandled, clawRetryPlan{}, nil
		}
		return clawRetryNotApplicable, clawRetryPlan{}, err
	}
	if isProtectedClawStatus(status) {
		return clawRetryNotApplicable, clawRetryPlan{}, nil
	}
	if status == "error" {
		return clawRetryAlreadyHandled, clawRetryPlan{}, nil
	}
	if provider == "" || runID == "" {
		return clawRetryNotApplicable, clawRetryPlan{}, nil
	}

	var currentAttemptID, triggerID string
	var attemptCount int
	if err := tx.QueryRow(`
		SELECT current_attempt_id, attempt_count, COALESCE(trigger_id,'')
		  FROM task_runs WHERE id=? AND claw_id=?`, runID, clawID).Scan(&currentAttemptID, &attemptCount, &triggerID); err != nil {
		if err == sql.ErrNoRows {
			return clawRetryNotApplicable, clawRetryPlan{}, nil
		}
		return clawRetryNotApplicable, clawRetryPlan{}, err
	}
	if attemptCount >= maxClawAttempts {
		return clawRetryNotApplicable, clawRetryPlan{}, nil
	}

	ts := epochMillis(now())
	failureType := taskRunFailureTypeForAgentFailure(failure.Kind)
	res, err := tx.Exec(`
		UPDATE task_run_attempts
		   SET status='failed', failure_type=?, finished_at=?, updated_at=?
		 WHERE id=? AND run_id=? AND status='running'`, failureType, ts, ts, currentAttemptID, runID)
	if err != nil {
		return clawRetryNotApplicable, clawRetryPlan{}, err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return clawRetryNotApplicable, clawRetryPlan{}, nil
	}

	safeReason := firstUsefulFailureLines(sanitizeFailureDetails(reason), 4)
	res, err = tx.Exec(`
		UPDATE claws
		   SET status='error', bootstrap_status='', bootstrap_diagnostic=?
		 WHERE id=? AND tenant_id=? AND status=?`, safeReason, clawID, tenantID, status)
	if err != nil {
		return clawRetryNotApplicable, clawRetryPlan{}, err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return clawRetryAlreadyHandled, clawRetryPlan{}, nil
	}

	nextAttempt := attemptCount + 1
	nextAttemptID := uuid.New().String()
	if _, err := tx.Exec(`
		INSERT INTO task_run_attempts(
			id, tenant_id, run_id, attempt_id, attempt_number, trigger_id, claw_id,
			status, failure_type, started_at, created_at, updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		nextAttemptID, tenantID, runID, nextAttemptID, nextAttempt, triggerID, clawID,
		"running", "", ts, ts, ts); err != nil {
		return clawRetryNotApplicable, clawRetryPlan{}, err
	}
	if _, err := tx.Exec(`
		UPDATE task_runs SET current_attempt_id=?, attempt_count=?, updated_at=? WHERE id=?`,
		nextAttemptID, nextAttempt, ts, runID); err != nil {
		return clawRetryNotApplicable, clawRetryPlan{}, err
	}
	if err := recordTaskRunEventTx(tx, TaskRunEvent{
		TenantID:        tenantID,
		RunID:           runID,
		AttemptID:       nextAttemptID,
		EventKey:        "retry:" + clawID + ":" + strconv.Itoa(nextAttempt),
		Source:          taskRunSourceHub,
		EventType:       taskRunEventProvisionStarted,
		ActorType:       taskRunActorSystem,
		InteractionRole: taskRunInteractionNeutral,
		Detail:          map[string]any{"attempt": nextAttempt, "reason": safeReason, "previous_failure_type": failureType},
		OccurredAt:      now(),
	}); err != nil {
		return clawRetryNotApplicable, clawRetryPlan{}, err
	}
	if err := tx.Commit(); err != nil {
		return clawRetryNotApplicable, clawRetryPlan{}, err
	}
	return clawRetryScheduled, clawRetryPlan{tenantID: tenantID, attempt: nextAttempt, failureType: failureType}, nil
}

// resolveClawStopDisposition re-evaluates an indeterminate retry decision a
// bounded number of times. Indeterminate means retry preparation rolled back
// (DB error / SQLite busy), so no replacement of ours exists; re-evaluating
// resolves the common cases (a concurrent detector won → alreadyHandled, or
// the DB recovered → scheduled). If it stays indeterminate, we return
// notApplicable so the caller proceeds to the terminal path: a run stuck
// forever is worse than the tiny residual race with a concurrent detector.
func resolveClawStopDisposition(evaluate func() clawRetryDisposition, sleep func(time.Duration)) clawRetryDisposition {
	disposition := evaluate()
	for _, delay := range clawStopRevaluationDelays {
		if disposition != clawRetryIndeterminate {
			return disposition
		}
		sleep(delay)
		disposition = evaluate()
	}
	if disposition == clawRetryIndeterminate {
		return clawRetryNotApplicable
	}
	return disposition
}

func (s *Server) scheduleClawRetry(clawID, reason string) clawRetryDisposition {
	disposition, plan, err := s.prepareClawRetry(clawID, reason)
	if err != nil {
		log.Printf("[claw-retry] failed to prepare retry for %s: %v", shortID(clawID), err)
		return clawRetryIndeterminate
	}
	if disposition != clawRetryScheduled {
		return disposition
	}

	message := fmt.Sprintf("🔄 Agent recovery is starting (attempt %d/%d).", plan.attempt, maxClawAttempts)
	s.broadcastToUsers(plan.tenantID, types.WSMessage{
		Type: "message",
		Payload: map[string]interface{}{
			"role": "system", "content": message, "claw_id": clawID,
		},
	})

	delay := retryDelay(clawRetryBackoff, plan.attempt-2)
	go func() {
		err := retryOperation(retryOptions{
			Label:    "claw instance replacement",
			Attempts: 1,
			Delays:   []time.Duration{delay},
			Run: func() error {
				return s.replaceClawInstance(context.Background(), plan.tenantID, clawID, reason, plan.attempt)
			},
		})
		if err != nil {
			log.Printf("[claw-retry] replacement failed for %s: %v", shortID(clawID), err)
			s.stopAgentTerminalWithReason(clawID, fmt.Sprintf("Restore failed: %v", err), false)
		}
	}()
	return clawRetryScheduled
}

// replaceClawInstance tears down the corrupted provider instance and starts a
// fresh one. It restores the newest ready checkpoint unless the previous failed
// attempt used that exact checkpoint, in which case it provisions cleanly.
func (s *Server) replaceClawInstance(ctx context.Context, tenantID, clawID, reason string, attempt int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	checkpointID, err := s.retryCheckpointBeforeTermination(tenantID, clawID, attempt)
	if err != nil {
		return err
	}

	var provider, providerID string
	if err := s.db.QueryRow(`
		SELECT COALESCE(provider,''), COALESCE(provider_id,'') FROM claws WHERE id=? AND tenant_id=?`,
		clawID, tenantID).Scan(&provider, &providerID); err != nil {
		return err
	}

	s.mu.Lock()
	if cc, ok := s.claws[clawID]; ok {
		if cc.conn != nil {
			_ = cc.conn.Close(websocket.StatusNormalClosure, "replacing corrupted instance")
		}
		delete(s.claws, clawID)
	}
	s.mu.Unlock()
	if providerID != "" {
		go func() {
			s.captureGatewayLog(clawID, provider, providerID)
			s.terminateVM(provider, providerID)
		}()
	}

	bootstrapStatus := fmt.Sprintf("retrying (attempt %d/%d)", attempt, maxClawAttempts)
	reset, err := s.resetClawForRetry(tenantID, clawID, checkpointID, bootstrapStatus)
	if err != nil {
		return err
	}
	if !reset {
		// The claw left 'error'/'offline' during the backoff (e.g. deleted or
		// manually restored), so no VM will be provisioned for the successor
		// attempt; settle it so the run does not report 'running' forever.
		ts := epochMillis(now())
		if _, err := s.db.Exec(`
			UPDATE task_run_attempts SET status='failed', failure_type=?, finished_at=?, updated_at=?
			 WHERE run_id=(SELECT task_run_id FROM claws WHERE id=?) AND attempt_number=? AND status='running'`,
			taskRunFailureUnknown, ts, ts, clawID, attempt); err != nil {
			return err
		}
		log.Printf("[claw-retry] replacement skipped for %s: claw changed state during backoff (attempt %d/%d)", shortID(clawID), attempt, maxClawAttempts)
		return nil
	}
	if _, err := s.db.Exec(`
		UPDATE task_run_attempts SET restored_checkpoint_id=?, updated_at=?
		 WHERE id=(
			SELECT tr.current_attempt_id FROM task_runs tr
			JOIN claws c ON c.task_run_id=tr.id WHERE c.id=?
		 )`,
		nullIfEmpty(checkpointID), epochMillis(now()), clawID); err != nil {
		return err
	}

	s.broadcastToUsers(tenantID, types.WSMessage{
		Type: "claw_status",
		Payload: map[string]string{
			"claw_id": clawID, "status": "provisioning", "bootstrap_status": bootstrapStatus,
		},
	})
	log.Printf("[claw-retry] replacing %s after %s (attempt %d/%d, checkpoint=%q)", shortID(clawID), sanitizeFailureDetails(reason), attempt, maxClawAttempts, checkpointID)
	go s.provisionStoredClaw(clawID)
	return nil
}

// retryCheckpointBeforeTermination fixes the restore choice at the failure
// boundary. A best-effort termination checkpoint is retained for diagnostics,
// but is never selected as the state used to recover that same failure.
func (s *Server) retryCheckpointBeforeTermination(tenantID, clawID string, attempt int) (string, error) {
	checkpointID, err := s.retryCheckpointID(tenantID, clawID, attempt)
	if err != nil {
		return "", err
	}
	s.checkpointBeforeTermination(clawID, "automatic-retry")
	return checkpointID, nil
}

func (s *Server) retryCheckpointID(tenantID, clawID string, attempt int) (string, error) {
	var previousCheckpointID string
	_ = s.db.QueryRow(`
		SELECT COALESCE(restored_checkpoint_id,'')
		  FROM task_run_attempts
		 WHERE run_id=(SELECT task_run_id FROM claws WHERE id=?) AND attempt_number=?`, clawID, attempt-1).Scan(&previousCheckpointID)

	var checkpointID, checkpointReason string
	err := s.db.QueryRow(`
		SELECT id, COALESCE(reason,'') FROM claw_checkpoints
		 WHERE tenant_id=? AND claw_id=? AND status='ready' AND manifest_path != ''
		 ORDER BY created_at DESC LIMIT 1`, tenantID, clawID).Scan(&checkpointID, &checkpointReason)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if checkpointID == previousCheckpointID {
		return "", nil
	}
	// A 'bootstrap' checkpoint captures state zero — it is taken seconds after
	// provisioning, before the agent has done anything. Restoring it over a
	// claw that has already made progress rolls the workspace back and
	// discards real work (observed on NEXT-801/NEXT-790). Fall back to the
	// newest non-bootstrap checkpoint instead — an earlier attempt's periodic
	// checkpoint can hold hours of recoverable work even when a later
	// attempt's bootstrap checkpoint sorted newest — and only when none exists
	// provision the successor cleanly and let the reconnect re-brief point the
	// fresh agent at the live PR/workspace state.
	if checkpointReason == "bootstrap" && s.clawProgressedPastBootstrap(tenantID, clawID) {
		err := s.db.QueryRow(`
			SELECT id FROM claw_checkpoints
			 WHERE tenant_id=? AND claw_id=? AND status='ready' AND manifest_path != ''
			   AND COALESCE(reason,'') != 'bootstrap'
			 ORDER BY created_at DESC LIMIT 1`, tenantID, clawID).Scan(&checkpointID)
		if err == sql.ErrNoRows {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		// Same guard as above: never restore the checkpoint the previous
		// attempt already failed on.
		if checkpointID == previousCheckpointID {
			return "", nil
		}
	}
	return checkpointID, nil
}

// clawProgressedPastBootstrap reports whether the claw shows any signal of
// real work beyond the freshly-provisioned state a 'bootstrap' checkpoint
// captures. Kept deliberately simple and readable: any one signal is enough.
func (s *Server) clawProgressedPastBootstrap(tenantID, clawID string) bool {
	// A registered PR is unambiguous progress.
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id=?`, clawID).Scan(&count); err == nil && count > 0 {
		return true
	}
	// A non-bootstrap ready checkpoint means the agent worked long enough for
	// a later checkpoint to exist, even if the bootstrap one sorted newest.
	if err := s.db.QueryRow(`
		SELECT COUNT(*) FROM claw_checkpoints
		 WHERE tenant_id=? AND claw_id=? AND status='ready' AND reason != 'bootstrap'`,
		tenantID, clawID).Scan(&count); err == nil && count > 0 {
		return true
	}
	// The pipeline moved past its entry stage. The entry stage itself is set
	// right after initialization, before any work happens, so it does not
	// count as progress.
	stageID := s.getPipelineStage(clawID)
	if stageID == "" {
		return false
	}
	ctx, ok := s.findPipelineContextForClaw(clawID)
	if !ok {
		return false
	}
	pl := parsePipelineForContext(ctx)
	if pl == nil {
		return false
	}
	entry := pl.EntryStage()
	return entry != nil && entry.ID != stageID
}

func (s *Server) resetClawForRetry(tenantID, clawID, checkpointID, bootstrapStatus string) (bool, error) {
	// rebrief_pending=1 marks that the successor sandbox starts with a brand-new
	// OpenClaw session: the checkpoint restore brings back workspace files but
	// not the conversation, so the reconnect path must re-brief the agent with
	// its task context. This UPDATE is the only place that arms the flag — a
	// normal reconnect/bridge flap must never set it.
	res, err := s.db.Exec(`
		UPDATE claws
		   SET status='provisioning', bootstrap_ok=0, bootstrap_status=?, bootstrap_diagnostic='',
		       provider_id='', ssh_host='', ssh_port=0, ssh_user='', restore_checkpoint_id=?,
		       rebrief_pending=1
		 WHERE id=? AND tenant_id=? AND status IN ('error','offline')`,
		bootstrapStatus, checkpointID, clawID, tenantID)
	if err != nil {
		return false, err
	}
	rows, _ := res.RowsAffected()
	return rows > 0, nil
}

func nullIfEmpty(value string) interface{} {
	if value == "" {
		return nil
	}
	return value
}
