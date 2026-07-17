package hub

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
)

const (
	taskRunKindCodeTask = "code_task"
	taskRunKindPRTask   = "pr_task"

	taskRunStatusRunning        = "running"
	taskRunStatusCleanSuccess   = "clean_success"
	taskRunStatusWarningSuccess = "warning_success"
	taskRunStatusFailed         = "failed"

	taskRunPhaseClaimed         = "claimed"
	taskRunPhaseQueued          = "queued"
	taskRunPhaseProvisioning    = "provisioning"
	taskRunPhaseAgentRunning    = "agent_running"
	taskRunPhasePROpened        = "pr_opened"
	taskRunPhaseWaitingForMerge = "waiting_for_merge"
	taskRunPhaseTerminal        = "terminal"

	taskRunOwnerFactory  = "factory"
	taskRunOwnerWorkflow = "workflow"
	taskRunOwnerManual   = "manual"

	taskRunWarningHumanPRComment        = "human_pr_comment"
	taskRunWarningHumanReviewComment    = "human_review_comment"
	taskRunWarningHumanRequestedChanges = "human_requested_changes"
	taskRunWarningHumanManualCodePush   = "human_manual_code_push"
	taskRunWarningHumanTrackerUpdate    = "human_tracker_update"
	taskRunWarningHumanDashboardMessage = "human_dashboard_message"
	taskRunWarningHumanManualStopResume = "human_manual_stop_or_resume"
	taskRunWarningHumanSettingsChange   = "human_settings_or_status_change"
	taskRunWarningUnknownHuman          = "unknown_human_interaction"
	taskRunWarningPRReplaced            = "pr_replaced"

	taskRunFailureDoneWithoutPR      = "done_without_pr"
	taskRunFailureNoPR               = "no_pr"
	taskRunFailurePRClosedUnmerged   = "pr_closed_unmerged"
	taskRunFailureManualStopDelivery = "manual_stop_before_delivery"
	taskRunFailureAgentStopped       = "agent_stopped"
	taskRunFailureProvisionFailed    = "provision_failed"
	taskRunFailureBootstrapFailed    = "bootstrap_failed"
	taskRunFailureProviderLost       = "provider_lost"
	taskRunFailurePermissionOrAuth   = "permission_or_auth_failed"
	taskRunFailureTimeout            = "timeout"
	taskRunFailureUnknown            = "unknown"

	taskRunEventTaskStart                = "task_start"
	taskRunEventTaskCompleted            = "task_completed"
	taskRunEventRunQueued                = "run_queued"
	taskRunEventProvisionStarted         = "provision_started"
	taskRunEventAgentStarted             = "agent_started"
	taskRunEventClawCreated              = "claw_created"
	taskRunEventPRAssociated             = "pr_associated"
	taskRunEventPROpened                 = "pr_opened"
	taskRunEventPRMerged                 = "pr_merged"
	taskRunEventPRClosedUnmerged         = "pr_closed_unmerged"
	taskRunEventDoneWithoutPR            = "done_without_pr"
	taskRunEventNoPR                     = "no_pr"
	taskRunEventHumanPRComment           = "human_pr_comment"
	taskRunEventHumanReviewComment       = "human_review_comment"
	taskRunEventHumanRequestedChanges    = "human_requested_changes"
	taskRunEventHumanDashboardMessage    = "human_dashboard_message"
	taskRunEventManualStopBeforeDelivery = "manual_stop_before_delivery"
	taskRunEventAgentStopped             = "agent_stopped"
	taskRunEventManualResume             = "human_manual_stop_or_resume"
	taskRunEventManualRetry              = "human_manual_stop_or_resume"
	taskRunEventSettingsChanged          = "human_settings_or_status_change"

	taskRunActorSystem  = "system"
	taskRunActorHuman   = "human"
	taskRunActorAgent   = "agent"
	taskRunActorBot     = "bot"
	taskRunActorUnknown = "unknown"

	taskRunSourceHub         = "hub"
	taskRunSourceGitHub      = "github"
	taskRunSourceDashboard   = "elasticclaw"
	taskRunSourceFactory     = "elasticclaw"
	taskRunSourceWorkflow    = "elasticclaw"
	taskRunSourceIntegration = "elasticclaw"
	taskRunSourcePRWatcher   = "hub"

	taskRunInteractionAllowedStart    = "allowed_start"
	taskRunInteractionAllowedApproval = "allowed_approval"
	taskRunInteractionAllowedMerge    = "allowed_merge"
	taskRunInteractionWarning         = "warning"
	taskRunInteractionNeutral         = "neutral"
	taskRunInteractionTerminal        = "terminal"

	taskRunPRStateOpen    = "open"
	taskRunPRStateClosed  = "closed"
	taskRunPRStateUnknown = "unknown"

	taskRunDetailSchemaVersion = 1
	taskRunMaxDetailBytes      = 16 * 1024
)

type TaskRunStart struct {
	TenantID             string
	RunKind              string
	OwnerType            string
	OwnerName            string
	OwnerID              string
	OwnerDisplayName     string
	WorkspaceName        string
	WorkflowName         string
	FactoryName          string
	Integration          string
	IntegrationWorkspace string
	TriggerID            string
	ExternalTriggerID    string
	IssueID              string
	IssueCreatedAt       time.Time
	Model                string
	LLMKey               string
	Source               string
	AnalyticsEnabled     bool
	RequiresPR           bool
	ExcludedReason       string
	TimeoutAt            time.Time
	EventKey             string
	Tags                 []string
	StartedAt            time.Time
}

type TaskRunEvent struct {
	TenantID         string
	RunID            string
	AttemptID        string
	EventKey         string
	Source           string
	SourceEventID    string
	SourceDeliveryID string
	EventType        string
	EventTime        time.Time
	ObservedAt       time.Time
	ActorType        string
	ActorID          string
	ActorLogin       string
	ActorDisplayName string
	InteractionRole  string
	TargetType       string
	TargetID         string
	TargetURL        string
	WarningType      string
	FailureType      string
	Detail           map[string]any
	OccurredAt       time.Time
}

type TaskRunPR struct {
	TenantID      string
	RunID         string
	Repo          string
	PRNumber      int
	URL           string
	HeadSHA       string
	HeadBranch    string
	BaseBranch    string
	State         string
	Merged        bool
	MergedByLogin string
	OccurredAt    time.Time
}

func epochMillis(t time.Time) int64 {
	if t.IsZero() {
		t = now()
	}
	return t.UTC().UnixNano() / int64(time.Millisecond)
}

func rfc3339FromEpochMillis(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

func (s *Server) ensureTaskRunForClaw(clawID string, opts TaskRunStart) (string, string, error) {
	if s == nil || s.db == nil {
		return "", "", fmt.Errorf("ensure task run: missing server db")
	}
	if strings.TrimSpace(clawID) == "" {
		return "", "", fmt.Errorf("ensure task run: missing claw id")
	}
	if opts.StartedAt.IsZero() {
		opts.StartedAt = now()
	}
	ts := epochMillis(opts.StartedAt)

	tx, err := s.db.Begin()
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()

	var tenantID, existingRunID, rawTags, clawModel, clawLLMKey, clawFactory, externalTriggerID, linearIssue, githubIssue, shortcutStory, jiraIssue, clawStatus string
	err = tx.QueryRow(`
		SELECT tenant_id, COALESCE(task_run_id,''), COALESCE(tags,'[]'), COALESCE(default_model,''),
		       COALESCE(llm_key,''), COALESCE(factory_name,''), COALESCE(external_trigger_id,''),
		       COALESCE(linear_issue_id,''), COALESCE(github_issue_id,''), COALESCE(shortcut_story_id,''), COALESCE(jira_issue_id,''), COALESCE(status,'')
		  FROM claws
		 WHERE id=?`, clawID).Scan(&tenantID, &existingRunID, &rawTags, &clawModel, &clawLLMKey, &clawFactory, &externalTriggerID, &linearIssue, &githubIssue, &shortcutStory, &jiraIssue, &clawStatus)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", "", fmt.Errorf("ensure task run: claw %s not found", clawID)
		}
		return "", "", err
	}
	normalizeTaskRunStart(&opts, tenantID, clawModel, clawLLMKey, clawFactory, externalTriggerID, firstNonEmpty(githubIssue, linearIssue, shortcutStory, jiraIssue))

	if existingRunID != "" {
		attemptID, err := ensureTaskRunAttemptTx(tx, existingRunID, opts.TenantID, clawID, opts.TriggerID, ts)
		if err != nil {
			return "", "", err
		}
		if err := recordTaskRunEventTx(tx, TaskRunEvent{
			TenantID:        opts.TenantID,
			RunID:           existingRunID,
			AttemptID:       attemptID,
			EventKey:        defaultString(opts.EventKey, "task_start:"+existingRunID),
			Source:          defaultString(opts.Source, taskRunSourceHub),
			EventType:       taskRunEventTaskStart,
			EventTime:       opts.StartedAt,
			ObservedAt:      opts.StartedAt,
			ActorType:       taskRunActorSystem,
			InteractionRole: taskRunInteractionAllowedStart,
		}); err != nil {
			return "", "", err
		}
		if err := recordTaskRunPhaseEventTx(tx, opts.TenantID, existingRunID, attemptID, clawStatus, opts.StartedAt); err != nil {
			return "", "", err
		}
		if err := materializeTaskRunTx(tx, existingRunID); err != nil {
			return "", "", err
		}
		return existingRunID, attemptID, tx.Commit()
	}

	runID := uuid.New().String()
	attemptID := uuid.New().String()
	tags, err := mergeTaskRunTags(rawTags, opts.Tags)
	if err != nil {
		return "", "", err
	}
	analyticsEnabled := boolInt(opts.AnalyticsEnabled)
	requiresPR := boolInt(opts.RequiresPR)
	timeoutAt := epochMillisOrZero(opts.TimeoutAt)

	if _, err := tx.Exec(`
		INSERT INTO task_runs(
			id, tenant_id, initial_attempt_id, current_attempt_id, attempt_count, run_kind,
			owner_type, workspace_name, workflow_name, factory_name, owner_id, owner_display_name,
			integration, integration_workspace, trigger_id, external_trigger_id, issue_id, issue_created_at, claw_id, model, llm_key, tags,
			analytics_enabled, requires_pr, excluded_reason, timeout_at, created_at, updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		runID, opts.TenantID, attemptID, attemptID, 1, opts.RunKind, opts.OwnerType, opts.WorkspaceName,
		opts.WorkflowName, opts.FactoryName, opts.OwnerID, opts.OwnerDisplayName, opts.Integration, opts.IntegrationWorkspace,
		opts.TriggerID, opts.ExternalTriggerID, opts.IssueID, epochMillisOrZero(opts.IssueCreatedAt), clawID, opts.Model, opts.LLMKey, tags, analyticsEnabled, requiresPR,
		opts.ExcludedReason, timeoutAt, ts, ts,
	); err != nil {
		return "", "", err
	}
	if _, err := tx.Exec(`
		INSERT INTO task_run_attempts(
			id, tenant_id, run_id, attempt_id, attempt_number, trigger_id, claw_id, status, failure_type,
			started_at, created_at, updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		attemptID, opts.TenantID, runID, attemptID, 1, opts.TriggerID, clawID, "running", "", ts, ts, ts,
	); err != nil {
		return "", "", err
	}
	if _, err := tx.Exec(`UPDATE claws SET task_run_id=? WHERE id=?`, runID, clawID); err != nil {
		return "", "", err
	}
	if opts.TriggerID != "" {
		if _, err := tx.Exec(`UPDATE factory_triggers SET task_run_id=? WHERE id=?`, runID, opts.TriggerID); err != nil {
			return "", "", err
		}
	}
	if err := recordTaskRunEventTx(tx, TaskRunEvent{
		TenantID:        opts.TenantID,
		RunID:           runID,
		AttemptID:       attemptID,
		EventKey:        defaultString(opts.EventKey, "task_start:"+runID),
		Source:          defaultString(opts.Source, taskRunSourceHub),
		EventType:       taskRunEventTaskStart,
		EventTime:       opts.StartedAt,
		ObservedAt:      opts.StartedAt,
		ActorType:       taskRunActorSystem,
		InteractionRole: taskRunInteractionAllowedStart,
	}); err != nil {
		return "", "", err
	}
	if err := recordTaskRunPhaseEventTx(tx, opts.TenantID, runID, attemptID, clawStatus, opts.StartedAt); err != nil {
		return "", "", err
	}
	if err := materializeTaskRunTx(tx, runID); err != nil {
		return "", "", err
	}
	if err := tx.Commit(); err != nil {
		return "", "", err
	}
	return runID, attemptID, nil
}

func (s *Server) recordTaskRunEvent(input TaskRunEvent) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("record task run event: missing server db")
	}
	if input.RunID == "" {
		return fmt.Errorf("record task run event: missing run id")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	runTenantID, err := loadTaskRunTenantTx(tx, input.RunID)
	if err != nil {
		return err
	}
	if input.TenantID == "" {
		input.TenantID = runTenantID
	} else if input.TenantID != runTenantID {
		return fmt.Errorf("record task run event: tenant mismatch for run %s", input.RunID)
	}
	if err := recordTaskRunEventTx(tx, input); err != nil {
		return err
	}
	if err := applyTaskRunPREventTx(tx, input); err != nil {
		return err
	}
	if err := materializeTaskRunTx(tx, input.RunID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Server) associateTaskRunPR(input TaskRunPR) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("associate task run PR: missing server db")
	}
	if input.RunID == "" || input.Repo == "" || input.PRNumber <= 0 {
		return fmt.Errorf("associate task run PR: missing run, repo, or PR number")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	runTenantID, err := loadTaskRunTenantTx(tx, input.RunID)
	if err != nil {
		return err
	}
	if input.TenantID == "" {
		input.TenantID = runTenantID
	} else if input.TenantID != runTenantID {
		return fmt.Errorf("associate task run PR: tenant mismatch for run %s", input.RunID)
	}
	if input.State == "" {
		input.State = taskRunPRStateOpen
	}
	// A caller-supplied OccurredAt is an authoritative provider timestamp and
	// may overwrite an earlier detection-time opened_at; the now() fallback is
	// write-once so it never clobbers an authoritative value.
	authoritativeOpen := input.State == taskRunPRStateOpen && !input.OccurredAt.IsZero()
	if input.OccurredAt.IsZero() {
		input.OccurredAt = now()
	}
	at := epochMillis(input.OccurredAt)
	openedAt, closedAt, mergedAt := int64(0), int64(0), int64(0)
	if input.State == taskRunPRStateOpen {
		openedAt = at
	}
	if input.State == taskRunPRStateClosed {
		closedAt = at
	}
	if input.Merged {
		mergedAt = at
		closedAt = at
	}
	if _, err := tx.Exec(`
		INSERT INTO task_run_prs(
			id, tenant_id, run_id, repo, pr_number, pr_url, head_sha, head_branch, base_branch,
			state, merged, opened_at, closed_at, merged_at, merged_by_login, created_at, updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(tenant_id, run_id, repo, pr_number) DO UPDATE SET
			pr_url=excluded.pr_url,
			head_sha=excluded.head_sha,
			head_branch=excluded.head_branch,
			base_branch=excluded.base_branch,
			state=CASE WHEN task_run_prs.merged = 1 OR task_run_prs.state = 'closed' THEN task_run_prs.state ELSE excluded.state END,
			merged=CASE WHEN excluded.merged = 1 THEN 1 ELSE task_run_prs.merged END,
			opened_at=CASE WHEN excluded.opened_at != 0 AND (task_run_prs.opened_at = 0 OR ?) THEN excluded.opened_at ELSE task_run_prs.opened_at END,
			closed_at=CASE WHEN excluded.closed_at != 0 THEN excluded.closed_at ELSE task_run_prs.closed_at END,
			merged_at=CASE WHEN excluded.merged_at != 0 THEN excluded.merged_at ELSE task_run_prs.merged_at END,
			merged_by_login=CASE WHEN excluded.merged_by_login != '' THEN excluded.merged_by_login ELSE task_run_prs.merged_by_login END,
			updated_at=excluded.updated_at`,
		uuid.New().String(), input.TenantID, input.RunID, input.Repo, input.PRNumber, input.URL,
		input.HeadSHA, input.HeadBranch, input.BaseBranch, input.State, boolInt(input.Merged),
		openedAt, closedAt, mergedAt, input.MergedByLogin, at, at,
		boolInt(authoritativeOpen),
	); err != nil {
		return err
	}
	detail := map[string]any{"repo": input.Repo, "prNumber": input.PRNumber, "url": input.URL}
	keySuffix := sanitizeTaskRunEventKey(input.Repo + "#" + strconv.Itoa(input.PRNumber))
	if err := recordTaskRunEventTx(tx, TaskRunEvent{
		TenantID:        input.TenantID,
		RunID:           input.RunID,
		EventKey:        "pr_associated:" + keySuffix,
		Source:          taskRunSourceHub,
		EventType:       taskRunEventPRAssociated,
		EventTime:       input.OccurredAt,
		ObservedAt:      input.OccurredAt,
		ActorType:       taskRunActorSystem,
		InteractionRole: taskRunInteractionNeutral,
		TargetType:      "pull_request",
		TargetURL:       input.URL,
		Detail:          detail,
	}); err != nil {
		return err
	}
	if input.State == taskRunPRStateOpen {
		if err := recordTaskRunEventTx(tx, TaskRunEvent{
			TenantID:        input.TenantID,
			RunID:           input.RunID,
			EventKey:        "pr_opened:" + keySuffix,
			Source:          taskRunSourceGitHub,
			EventType:       taskRunEventPROpened,
			EventTime:       input.OccurredAt,
			ObservedAt:      input.OccurredAt,
			ActorType:       taskRunActorSystem,
			InteractionRole: taskRunInteractionNeutral,
			TargetType:      "pull_request",
			TargetURL:       input.URL,
			Detail:          detail,
		}); err != nil {
			return err
		}
	}
	if err := materializeTaskRunTx(tx, input.RunID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Server) materializeTaskRun(runID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("materialize task run: missing server db")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := materializeTaskRunTx(tx, runID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Server) taskRunContextForClaw(clawID string) (tenantID, runID, attemptID string, ok bool, err error) {
	if s == nil || s.db == nil {
		return "", "", "", false, fmt.Errorf("task run context: missing server db")
	}
	if strings.TrimSpace(clawID) == "" {
		return "", "", "", false, fmt.Errorf("task run context: missing claw id")
	}
	err = s.db.QueryRow(`
		SELECT c.tenant_id, COALESCE(c.task_run_id,''), COALESCE(tr.current_attempt_id,'')
		  FROM claws c
		  LEFT JOIN task_runs tr ON tr.id = c.task_run_id
		 WHERE c.id=?`, clawID).Scan(&tenantID, &runID, &attemptID)
	if err == sql.ErrNoRows {
		return "", "", "", false, nil
	}
	if err != nil {
		return "", "", "", false, err
	}
	if runID == "" {
		return tenantID, "", "", false, nil
	}
	return tenantID, runID, attemptID, true, nil
}

// recordTaskRunIssueCreatedAt backfills the authoritative issue-creation time
// on the task run for a claw and refreshes its summary. Best-effort: analytics
// must never fail the calling flow.
func (s *Server) recordTaskRunIssueCreatedAt(clawID string, issueCreatedAt time.Time) {
	if issueCreatedAt.IsZero() {
		return
	}
	_, runID, _, ok, err := s.taskRunContextForClaw(clawID)
	if err != nil || !ok {
		return
	}
	if _, err := s.db.Exec(`UPDATE task_runs SET issue_created_at=? WHERE id=? AND issue_created_at=0`, epochMillis(issueCreatedAt), runID); err != nil {
		log.Printf("[task-run-analytics] failed to record issue creation time for claw %s: %v", clawID, err)
		return
	}
	if err := s.materializeTaskRun(runID); err != nil {
		log.Printf("[task-run-analytics] failed to materialize run %s after issue creation backfill: %v", runID, err)
	}
}

func loadTaskRunTenantTx(tx *sql.Tx, runID string) (string, error) {
	var tenantID string
	if err := tx.QueryRow(`SELECT tenant_id FROM task_runs WHERE id=?`, runID).Scan(&tenantID); err != nil {
		return "", err
	}
	return tenantID, nil
}

func (s *Server) recordTaskRunEventForClaw(clawID string, input TaskRunEvent) error {
	tenantID, runID, attemptID, ok, err := s.taskRunContextForClaw(clawID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if input.TenantID == "" {
		input.TenantID = tenantID
	}
	input.RunID = runID
	if input.AttemptID == "" {
		input.AttemptID = attemptID
	}
	return s.recordTaskRunEvent(input)
}

func (s *Server) recordTaskRunHumanEventForClaw(clawID, eventType, eventKey, actorLogin, targetURL string, detail map[string]any) {
	if strings.TrimSpace(clawID) == "" {
		return
	}
	if eventKey == "" {
		eventKey = eventType + ":" + sanitizeTaskRunEventKey(actorLogin) + ":" + sanitizeTaskRunEventKey(targetURL)
	}
	if err := s.recordTaskRunEventForClaw(clawID, TaskRunEvent{
		EventKey:        eventKey,
		Source:          taskRunSourceGitHub,
		EventType:       eventType,
		ActorType:       taskRunActorHuman,
		ActorLogin:      actorLogin,
		InteractionRole: taskRunInteractionWarning,
		TargetType:      "pull_request",
		TargetURL:       targetURL,
		WarningType:     warningTypeForTaskRunEventType(eventType),
		Detail:          detail,
		OccurredAt:      now(),
	}); err != nil {
		log.Printf("[task-run-analytics] failed to record human event %s for claw %s: %v", eventType, clawID, err)
	}
}

func (s *Server) recordTaskRunDashboardMessage(clawID, actorLogin, messageID string) {
	if strings.TrimSpace(clawID) == "" {
		return
	}
	if err := s.recordTaskRunEventForClaw(clawID, TaskRunEvent{
		EventKey:        "human_dashboard_message:" + defaultString(messageID, uuid.New().String()),
		Source:          taskRunSourceDashboard,
		EventType:       taskRunEventHumanDashboardMessage,
		ActorType:       taskRunActorHuman,
		ActorLogin:      actorLogin,
		InteractionRole: taskRunInteractionWarning,
		TargetType:      "claw",
		TargetID:        clawID,
		WarningType:     taskRunWarningHumanDashboardMessage,
		Detail:          map[string]any{"message_id": messageID},
		OccurredAt:      now(),
	}); err != nil {
		log.Printf("[task-run-analytics] failed to record dashboard message for claw %s: %v", clawID, err)
	}
}

func (s *Server) recordTaskRunManualStopBeforeDelivery(clawID, actorLogin string) {
	if strings.TrimSpace(clawID) == "" {
		return
	}
	if err := s.recordTaskRunEventForClaw(clawID, TaskRunEvent{
		EventKey:        "manual_stop_before_delivery:" + clawID,
		Source:          taskRunSourceDashboard,
		EventType:       taskRunEventManualStopBeforeDelivery,
		ActorType:       taskRunActorHuman,
		ActorLogin:      actorLogin,
		InteractionRole: taskRunInteractionTerminal,
		TargetType:      "claw",
		TargetID:        clawID,
		WarningType:     taskRunWarningHumanManualStopResume,
		FailureType:     taskRunFailureManualStopDelivery,
		Detail:          map[string]any{"reason": "dashboard_delete"},
		OccurredAt:      now(),
	}); err != nil {
		log.Printf("[task-run-analytics] failed to record manual stop for claw %s: %v", clawID, err)
	}
}

func taskRunKindForFactory(factory *types.FactoryConfig) string {
	if factory != nil && factory.RunKind != "" {
		return factory.RunKind
	}
	if factory != nil && factory.Integration == "github" && factory.Trigger != nil && factory.Trigger.On == "pull_request" {
		return taskRunKindPRTask
	}
	return taskRunKindCodeTask
}

func taskRunKindForWorkflow(workflow *types.WorkflowConfig) string {
	if workflow != nil && workflow.RunKind != "" {
		return workflow.RunKind
	}
	return taskRunKindCodeTask
}

func taskRunAnalyticsContractForFactory(factory *types.FactoryConfig) (enabled bool, requiresPR bool, excludedReason string) {
	if factory != nil && factory.AnalyticsEnabled != nil {
		enabled = *factory.AnalyticsEnabled
	} else {
		enabled = isTaskRunAnalyticsIntegration(factoryIntegration(factory))
	}
	if factory != nil && factory.RequiresPR != nil {
		requiresPR = *factory.RequiresPR
	} else {
		requiresPR = enabled
	}
	if !enabled {
		return false, requiresPR, "analytics_disabled"
	}
	if !requiresPR {
		return true, false, "non_pr_producing"
	}
	return true, true, ""
}

func taskRunAnalyticsContractForWorkflow(workflow *types.WorkflowConfig) (enabled bool, requiresPR bool, excludedReason string) {
	if workflow != nil && workflow.AnalyticsEnabled != nil {
		enabled = *workflow.AnalyticsEnabled
	} else {
		enabled = isTaskRunAnalyticsIntegration(workflowIntegration(workflow))
	}
	if workflow != nil && workflow.RequiresPR != nil {
		requiresPR = *workflow.RequiresPR
	} else {
		requiresPR = enabled
	}
	if !enabled {
		return false, requiresPR, "analytics_disabled"
	}
	if !requiresPR {
		return true, false, "non_pr_producing"
	}
	return true, true, ""
}

func recordTaskRunPhaseEventTx(tx *sql.Tx, tenantID, runID, attemptID, clawStatus string, eventTime time.Time) error {
	eventType := taskRunPhaseEventForClawStatus(clawStatus)
	if eventType == "" {
		return nil
	}
	return recordTaskRunEventTx(tx, TaskRunEvent{
		TenantID:        tenantID,
		RunID:           runID,
		AttemptID:       attemptID,
		EventKey:        eventType + ":" + runID,
		Source:          taskRunSourceHub,
		EventType:       eventType,
		EventTime:       eventTime,
		ObservedAt:      eventTime,
		ActorType:       taskRunActorSystem,
		InteractionRole: taskRunInteractionNeutral,
	})
}

func taskRunPhaseEventForClawStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "pending":
		return taskRunEventRunQueued
	case "provisioning", "starting", "creating":
		return taskRunEventProvisionStarted
	case "running", "online", "offline", "idle", "watching":
		return taskRunEventAgentStarted
	default:
		return ""
	}
}

func (s *Server) taskRunRequiresPRForClaw(clawID string) bool {
	if s == nil || s.db == nil || strings.TrimSpace(clawID) == "" {
		return true
	}
	var requiresPR int
	err := s.db.QueryRow(`
		SELECT tr.requires_pr
		  FROM task_runs tr
		  JOIN claws c ON c.task_run_id = tr.id
		 WHERE c.id=?`, clawID).Scan(&requiresPR)
	if err != nil {
		return true
	}
	return requiresPR == 1
}

func isTaskRunAnalyticsIntegration(integration string) bool {
	switch integration {
	case "linear", "shortcut", "github-issues", "jira", "github":
		return true
	default:
		return false
	}
}

func factoryIntegration(factory *types.FactoryConfig) string {
	if factory == nil {
		return ""
	}
	return factory.Integration
}

func workflowIntegration(workflow *types.WorkflowConfig) string {
	if workflow == nil {
		return ""
	}
	return workflow.Integration
}

func recordTaskRunEventTx(tx *sql.Tx, input TaskRunEvent) error {
	if input.RunID == "" || input.EventType == "" {
		return fmt.Errorf("record task run event: missing run id or event type")
	}
	if input.EventTime.IsZero() {
		input.EventTime = firstNonZeroTime(input.OccurredAt, now())
	}
	if input.ObservedAt.IsZero() {
		input.ObservedAt = now()
	}
	if input.ActorType == "" {
		input.ActorType = taskRunActorSystem
	}
	if input.Source == "" {
		input.Source = taskRunSourceHub
	}
	if input.InteractionRole == "" {
		input.InteractionRole = taskRunInteractionNeutral
	}
	if input.EventKey == "" {
		input.EventKey = input.EventType + ":" + strconv.FormatInt(epochMillis(input.EventTime), 10) + ":" + uuid.New().String()[:8]
	}
	if input.TenantID == "" {
		if err := tx.QueryRow(`SELECT tenant_id FROM task_runs WHERE id=?`, input.RunID).Scan(&input.TenantID); err != nil {
			return err
		}
	}
	if isHumanWarningEvent(input) && input.WarningType == "" {
		input.WarningType = warningTypeForTaskRunInput(input)
	}
	if input.EventType == taskRunEventDoneWithoutPR && input.FailureType == "" {
		input.FailureType = taskRunFailureDoneWithoutPR
	}
	if input.EventType == taskRunEventNoPR && input.FailureType == "" {
		input.FailureType = taskRunFailureNoPR
	}
	if input.EventType == taskRunEventManualStopBeforeDelivery && input.FailureType == "" {
		input.FailureType = taskRunFailureManualStopDelivery
	}
	if input.EventType == taskRunEventAgentStopped && input.FailureType == "" {
		input.FailureType = taskRunFailureAgentStopped
	}
	detail, err := sanitizeTaskRunDetail(input.Detail)
	if err != nil {
		return err
	}
	eventTime := epochMillis(input.EventTime)
	observedAt := epochMillis(input.ObservedAt)
	_, err = tx.Exec(`
		INSERT INTO task_run_events(
			id, tenant_id, run_id, attempt_id, event_key, source, source_event_id, source_delivery_id,
			event_type, event_time, observed_at, actor_type, actor_source, actor_id, actor_login,
			actor_display_name, actor_classification_reason, interaction_role, target_type, target_id,
			target_url, target_label, warning_type, failure_type, detail, created_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(tenant_id, run_id, event_key) DO NOTHING`,
		uuid.New().String(), input.TenantID, input.RunID, input.AttemptID, input.EventKey, input.Source,
		input.SourceEventID, input.SourceDeliveryID, input.EventType, eventTime, observedAt, input.ActorType,
		"", input.ActorID, input.ActorLogin, input.ActorDisplayName, "", input.InteractionRole, input.TargetType,
		input.TargetID, input.TargetURL, "", input.WarningType, input.FailureType, detail, observedAt,
	)
	return err
}

func applyTaskRunPREventTx(tx *sql.Tx, input TaskRunEvent) error {
	if input.EventType != taskRunEventPRMerged && input.EventType != taskRunEventPRClosedUnmerged {
		return nil
	}
	repo, prNumber := detailPR(input.Detail)
	if repo == "" || prNumber <= 0 {
		return nil
	}
	eventTime := epochMillis(firstNonZeroTime(input.EventTime, input.OccurredAt, now()))
	if input.EventType == taskRunEventPRMerged {
		_, err := tx.Exec(`
			INSERT INTO task_run_prs(id, tenant_id, run_id, repo, pr_number, state, merged, closed_at, merged_at, created_at, updated_at)
			VALUES(?,?,?,?,?,'closed',1,?,?,?,?)
			ON CONFLICT(tenant_id, run_id, repo, pr_number) DO UPDATE SET
				state='closed',
				merged=1,
				closed_at=CASE WHEN task_run_prs.closed_at = 0 THEN excluded.closed_at ELSE task_run_prs.closed_at END,
				merged_at=CASE WHEN excluded.merged_at != 0 THEN excluded.merged_at ELSE task_run_prs.merged_at END,
				updated_at=excluded.updated_at`,
			uuid.New().String(), input.TenantID, input.RunID, repo, prNumber, eventTime, eventTime, eventTime, eventTime,
		)
		return err
	}
	_, err := tx.Exec(`
		INSERT INTO task_run_prs(id, tenant_id, run_id, repo, pr_number, state, merged, closed_at, created_at, updated_at)
		VALUES(?,?,?,?,?,'closed',0,?,?,?)
		ON CONFLICT(tenant_id, run_id, repo, pr_number) DO UPDATE SET
			state=CASE WHEN task_run_prs.merged = 1 THEN task_run_prs.state ELSE 'closed' END,
			closed_at=CASE WHEN excluded.closed_at != 0 THEN excluded.closed_at ELSE task_run_prs.closed_at END,
			updated_at=excluded.updated_at`,
		uuid.New().String(), input.TenantID, input.RunID, repo, prNumber, eventTime, eventTime, eventTime,
	)
	return err
}

func materializeTaskRunTx(tx *sql.Tx, runID string) error {
	if runID == "" {
		return fmt.Errorf("materialize task run: missing run id")
	}
	meta, err := readTaskRunMetaTx(tx, runID)
	if err != nil {
		return err
	}
	prs, err := readTaskRunPRsTx(tx, runID)
	if err != nil {
		return err
	}
	events, err := readTaskRunEventsTx(tx, runID)
	if err != nil {
		return err
	}

	warnings := map[string]bool{}
	humanInteractions := 0
	for _, event := range events {
		if warning := normalizeTaskRunWarningType(event.warningType); warning != "" {
			warnings[warning] = true
		}
		if warning := warningTypeForEvent(event); warning != "" {
			humanInteractions++
			warnings[warning] = true
		}
	}

	counts := taskRunPRCounts(prs)
	phaseTimes := taskRunPhaseTimes(events)
	// task_run_prs.opened_at may carry GitHub's authoritative created_at
	// (backfilled by the PR watcher or webhook), while pr_opened events are
	// stamped at detection time and never updated. Use the earliest known time.
	for _, pr := range prs {
		if pr.openedAt > 0 && (phaseTimes.prOpenedAt == 0 || pr.openedAt < phaseTimes.prOpenedAt) {
			phaseTimes.prOpenedAt = pr.openedAt
		}
	}
	if counts.merged > 0 && counts.closed > 0 {
		warnings[taskRunWarningPRReplaced] = true
	}

	definitiveFailure, definitiveFailureAt := definitiveFailure(events)
	completedAt := taskCompletedAt(events)
	prePRFailure, prePRFailureAt := prePRFailure(events)
	status, phase, failureType, finishedAt := computeTaskRunStatus(
		meta, counts, warnings, phaseTimes, definitiveFailure, definitiveFailureAt, completedAt, prePRFailure, prePRFailureAt,
	)
	if finishedAt == 0 && phase == taskRunPhaseTerminal {
		finishedAt = epochMillis(now())
	}
	lastEventAt := meta.updatedAt
	for _, event := range events {
		if event.eventTime > lastEventAt {
			lastEventAt = event.eventTime
		}
	}
	warningTypes := encodeWarningTypes(warnings)
	primaryPRURL := ""
	for _, pr := range prs {
		if pr.merged && pr.url != "" {
			primaryPRURL = pr.url
			break
		}
		if primaryPRURL == "" && pr.url != "" {
			primaryPRURL = pr.url
		}
	}
	materializedAt := epochMillis(now())
	updatedAt := maxInt64(meta.updatedAt, materializedAt)

	_, err = tx.Exec(`
		INSERT INTO task_run_summaries(
			id, tenant_id, run_id, initial_attempt_id, current_attempt_id, status, phase,
			attempt_count, owner_type, workspace_name, workflow_name, factory_name, owner_id,
			owner_display_name, run_kind, integration, integration_workspace, issue_id, issue_created_at, claw_id,
			model, llm_key, repo, primary_pr_url,
			pr_count, open_pr_count, merged_pr_count, closed_pr_count, warning_types, failure_type,
			human_interaction_count, started_at, queued_at, provision_started_at, agent_started_at,
			pr_opened_at, merged_at, finished_at, timeout_at, last_event_at, materialized_at, updated_at,
			analytics_enabled, requires_pr, excluded_reason
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(run_id) DO UPDATE SET
			initial_attempt_id=excluded.initial_attempt_id,
			current_attempt_id=excluded.current_attempt_id,
			status=excluded.status,
			phase=excluded.phase,
			attempt_count=excluded.attempt_count,
			owner_type=excluded.owner_type,
			workspace_name=excluded.workspace_name,
			workflow_name=excluded.workflow_name,
			factory_name=excluded.factory_name,
			owner_id=excluded.owner_id,
			owner_display_name=excluded.owner_display_name,
			run_kind=excluded.run_kind,
			integration=excluded.integration,
			integration_workspace=excluded.integration_workspace,
			issue_id=excluded.issue_id,
			issue_created_at=excluded.issue_created_at,
			claw_id=excluded.claw_id,
			model=excluded.model,
			llm_key=excluded.llm_key,
			repo=excluded.repo,
			primary_pr_url=excluded.primary_pr_url,
			pr_count=excluded.pr_count,
			open_pr_count=excluded.open_pr_count,
			merged_pr_count=excluded.merged_pr_count,
			closed_pr_count=excluded.closed_pr_count,
			warning_types=excluded.warning_types,
			failure_type=excluded.failure_type,
			human_interaction_count=excluded.human_interaction_count,
			queued_at=excluded.queued_at,
			provision_started_at=excluded.provision_started_at,
			agent_started_at=excluded.agent_started_at,
			pr_opened_at=excluded.pr_opened_at,
			merged_at=excluded.merged_at,
			finished_at=excluded.finished_at,
			timeout_at=excluded.timeout_at,
			last_event_at=excluded.last_event_at,
			materialized_at=excluded.materialized_at,
			updated_at=excluded.updated_at,
			analytics_enabled=excluded.analytics_enabled,
			requires_pr=excluded.requires_pr,
			excluded_reason=excluded.excluded_reason`,
		uuid.New().String(), meta.tenantID, runID, meta.initialAttemptID, meta.currentAttemptID, status, phase, meta.attemptCount, meta.ownerType,
		meta.workspaceName, meta.workflowName, meta.factoryName, meta.ownerID, meta.ownerDisplayName,
		meta.runKind, meta.integration, meta.integrationWorkspace, meta.issueID, meta.issueCreatedAt, meta.clawID,
		meta.model, meta.llmKey, counts.primaryRepo, primaryPRURL, counts.total, counts.open, counts.merged, counts.closed,
		warningTypes, failureType, humanInteractions, meta.createdAt, phaseTimes.queuedAt, phaseTimes.provisionStartedAt,
		phaseTimes.agentStartedAt, phaseTimes.prOpenedAt, counts.latestMergedAt, finishedAt, meta.timeoutAt,
		lastEventAt, materializedAt, updatedAt, meta.analyticsEnabled, meta.requiresPR, meta.excludedReason,
	)
	if err != nil {
		return err
	}
	attemptStatus := "running"
	if phase == taskRunPhaseTerminal {
		attemptStatus = "succeeded"
		if status == taskRunStatusFailed {
			attemptStatus = "failed"
		}
	}
	if _, err := tx.Exec(`
		UPDATE task_run_attempts
		   SET status=?, failure_type=?, finished_at=CASE WHEN ? != 0 THEN ? ELSE finished_at END, updated_at=?
		 WHERE run_id=? AND id=?`,
		attemptStatus, failureType, finishedAt, finishedAt, updatedAt, runID, meta.currentAttemptID,
	); err != nil {
		return err
	}
	_, err = tx.Exec(`
		UPDATE task_runs
		   SET updated_at=?
		 WHERE id=?`, updatedAt, runID)
	return err
}

func computeTaskRunStatus(
	meta taskRunMeta,
	counts prCountSummary,
	warnings map[string]bool,
	phaseTimes taskRunPhaseTimeSummary,
	definitiveFailure string,
	definitiveFailureAt int64,
	taskCompletedAt int64,
	prePRFailure string,
	prePRFailureAt int64,
) (status, phase, failureType string, finishedAt int64) {
	status, phase = taskRunStatusRunning, initialTaskRunPhase(meta, phaseTimes)
	if meta.analyticsEnabled == 0 {
		// Excluded runs are not part of PR-scoped analytics; failed/unknown is a sentinel summary state.
		return taskRunStatusFailed, taskRunPhaseTerminal, taskRunFailureUnknown, meta.updatedAt
	} else if definitiveFailure != "" {
		status, phase, failureType, finishedAt = taskRunStatusFailed, taskRunPhaseTerminal, definitiveFailure, definitiveFailureAt
	} else if meta.requiresPR == 0 && taskCompletedAt != 0 {
		if len(warnings) > 0 {
			status = taskRunStatusWarningSuccess
		} else {
			status = taskRunStatusCleanSuccess
		}
		phase = taskRunPhaseTerminal
		finishedAt = taskCompletedAt
	} else if counts.merged > 0 && counts.open == 0 {
		if len(warnings) > 0 {
			status = taskRunStatusWarningSuccess
		} else {
			status = taskRunStatusCleanSuccess
		}
		phase = taskRunPhaseTerminal
		finishedAt = counts.latestTerminalAt
	} else if counts.merged > 0 && counts.open > 0 {
		status, phase = taskRunStatusRunning, taskRunPhaseWaitingForMerge
	} else if prePRFailure != "" {
		status, phase, failureType, finishedAt = taskRunStatusFailed, taskRunPhaseTerminal, prePRFailure, prePRFailureAt
	} else if counts.total > 0 {
		status = taskRunStatusRunning
		if counts.open > 0 {
			phase = taskRunPhaseWaitingForMerge
		} else {
			status, phase, failureType, finishedAt = taskRunStatusFailed, taskRunPhaseTerminal, taskRunFailurePRClosedUnmerged, counts.latestTerminalAt
		}
	}
	return status, phase, failureType, finishedAt
}

func initialTaskRunPhase(meta taskRunMeta, phaseTimes taskRunPhaseTimeSummary) string {
	if phaseTimes.agentStartedAt != 0 {
		return taskRunPhaseAgentRunning
	}
	if phaseTimes.provisionStartedAt != 0 {
		return taskRunPhaseProvisioning
	}
	if phaseTimes.queuedAt != 0 {
		return taskRunPhaseQueued
	}
	switch strings.ToLower(strings.TrimSpace(meta.clawStatus)) {
	case "pending":
		return taskRunPhaseQueued
	case "provisioning", "starting", "creating":
		return taskRunPhaseProvisioning
	case "running", "online", "offline", "idle", "watching":
		return taskRunPhaseAgentRunning
	default:
		return taskRunPhaseClaimed
	}
}

type taskRunMeta struct {
	tenantID             string
	clawID               string
	clawStatus           string
	runKind              string
	initialAttemptID     string
	currentAttemptID     string
	attemptCount         int
	ownerType            string
	workspaceName        string
	workflowName         string
	factoryName          string
	ownerID              string
	ownerDisplayName     string
	integration          string
	integrationWorkspace string
	issueID              string
	issueCreatedAt       int64
	model                string
	llmKey               string
	analyticsEnabled     int
	requiresPR           int
	excludedReason       string
	timeoutAt            int64
	createdAt            int64
	updatedAt            int64
}

func readTaskRunMetaTx(tx *sql.Tx, runID string) (taskRunMeta, error) {
	var meta taskRunMeta
	err := tx.QueryRow(`
		SELECT tr.tenant_id, tr.claw_id, tr.run_kind, tr.initial_attempt_id, tr.current_attempt_id,
		       tr.attempt_count, tr.owner_type, tr.workspace_name,
		       tr.workflow_name, tr.factory_name, tr.owner_id, tr.owner_display_name,
		       tr.integration,
		       tr.integration_workspace, tr.issue_id, tr.issue_created_at, COALESCE(c.status,''), COALESCE(NULLIF(tr.model,''), c.default_model, ''),
		       COALESCE(NULLIF(tr.llm_key,''), c.llm_key, ''), tr.analytics_enabled, tr.requires_pr, tr.excluded_reason,
		       tr.timeout_at, tr.created_at, tr.updated_at
		  FROM task_runs tr
		  LEFT JOIN claws c ON c.id = tr.claw_id
		 WHERE tr.id=?`, runID).Scan(
		&meta.tenantID, &meta.clawID, &meta.runKind, &meta.initialAttemptID, &meta.currentAttemptID,
		&meta.attemptCount, &meta.ownerType,
		&meta.workspaceName, &meta.workflowName, &meta.factoryName, &meta.ownerID, &meta.ownerDisplayName,
		&meta.integration, &meta.integrationWorkspace, &meta.issueID, &meta.issueCreatedAt, &meta.clawStatus, &meta.model, &meta.llmKey,
		&meta.analyticsEnabled, &meta.requiresPR, &meta.excludedReason,
		&meta.timeoutAt, &meta.createdAt, &meta.updatedAt,
	)
	return meta, err
}

type taskRunPRProjection struct {
	repo     string
	url      string
	state    string
	merged   bool
	openedAt int64
	closedAt int64
	mergedAt int64
}

func readTaskRunPRsTx(tx *sql.Tx, runID string) ([]taskRunPRProjection, error) {
	rows, err := tx.Query(`
		SELECT repo, pr_url, state, merged, opened_at, closed_at, merged_at
		  FROM task_run_prs
		 WHERE run_id=?
		 ORDER BY opened_at, pr_number`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var prs []taskRunPRProjection
	for rows.Next() {
		var pr taskRunPRProjection
		var merged int
		if err := rows.Scan(&pr.repo, &pr.url, &pr.state, &merged, &pr.openedAt, &pr.closedAt, &pr.mergedAt); err != nil {
			return nil, err
		}
		pr.merged = merged == 1
		prs = append(prs, pr)
	}
	return prs, rows.Err()
}

type taskRunEventProjection struct {
	eventType   string
	actorType   string
	warningType string
	failureType string
	eventTime   int64
}

func readTaskRunEventsTx(tx *sql.Tx, runID string) ([]taskRunEventProjection, error) {
	rows, err := tx.Query(`
		SELECT event_type, actor_type, warning_type, failure_type, event_time
		  FROM task_run_events
		 WHERE run_id=?
		 ORDER BY event_time, id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []taskRunEventProjection
	for rows.Next() {
		var event taskRunEventProjection
		if err := rows.Scan(&event.eventType, &event.actorType, &event.warningType, &event.failureType, &event.eventTime); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

type prCountSummary struct {
	total            int
	open             int
	merged           int
	closed           int
	latestTerminalAt int64
	latestMergedAt   int64
	primaryRepo      string
}

func taskRunPRCounts(prs []taskRunPRProjection) prCountSummary {
	var counts prCountSummary
	for _, pr := range prs {
		counts.total++
		if counts.primaryRepo == "" && pr.repo != "" {
			counts.primaryRepo = pr.repo
		}
		if pr.merged {
			counts.merged++
			if pr.mergedAt > counts.latestTerminalAt {
				counts.latestTerminalAt = pr.mergedAt
			}
			if pr.mergedAt > counts.latestMergedAt {
				counts.latestMergedAt = pr.mergedAt
				counts.primaryRepo = pr.repo
			}
			continue
		}
		switch pr.state {
		case taskRunPRStateOpen:
			counts.open++
		case taskRunPRStateClosed:
			counts.closed++
			if pr.closedAt > counts.latestTerminalAt {
				counts.latestTerminalAt = pr.closedAt
			}
		}
	}
	return counts
}

func definitiveFailure(events []taskRunEventProjection) (string, int64) {
	agentStoppedAt := int64(0)
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		switch event.failureType {
		case taskRunFailureManualStopDelivery, taskRunFailureTimeout:
			return event.failureType, event.eventTime
		case taskRunFailureAgentStopped:
			if agentStoppedAt == 0 {
				agentStoppedAt = event.eventTime
			}
		case taskRunFailureDoneWithoutPR, taskRunFailureNoPR, taskRunFailurePRClosedUnmerged:
			return "", 0
		}
	}
	if agentStoppedAt > 0 {
		return taskRunFailureAgentStopped, agentStoppedAt
	}
	return "", 0
}

func taskCompletedAt(events []taskRunEventProjection) int64 {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].eventType == taskRunEventTaskCompleted {
			return events[i].eventTime
		}
	}
	return 0
}

func prePRFailure(events []taskRunEventProjection) (string, int64) {
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		switch event.failureType {
		case taskRunFailureDoneWithoutPR, taskRunFailureNoPR, taskRunFailurePRClosedUnmerged:
			return event.failureType, event.eventTime
		}
	}
	return "", 0
}

type taskRunPhaseTimeSummary struct {
	queuedAt           int64
	provisionStartedAt int64
	agentStartedAt     int64
	prOpenedAt         int64
}

func taskRunPhaseTimes(events []taskRunEventProjection) taskRunPhaseTimeSummary {
	var summary taskRunPhaseTimeSummary
	for _, event := range events {
		switch event.eventType {
		case taskRunEventRunQueued:
			if summary.queuedAt == 0 || event.eventTime < summary.queuedAt {
				summary.queuedAt = event.eventTime
			}
		case taskRunEventProvisionStarted:
			if summary.provisionStartedAt == 0 || event.eventTime < summary.provisionStartedAt {
				summary.provisionStartedAt = event.eventTime
			}
		case taskRunEventAgentStarted:
			if summary.agentStartedAt == 0 || event.eventTime < summary.agentStartedAt {
				summary.agentStartedAt = event.eventTime
			}
		case taskRunEventPROpened:
			if summary.prOpenedAt == 0 || event.eventTime < summary.prOpenedAt {
				summary.prOpenedAt = event.eventTime
			}
		}
	}
	return summary
}

func ensureTaskRunAttemptTx(tx *sql.Tx, runID, tenantID, clawID, triggerID string, startedAt int64) (string, error) {
	var attemptID string
	err := tx.QueryRow(`
		SELECT id
		  FROM task_run_attempts
		 WHERE run_id=?
		 ORDER BY attempt_number DESC
		 LIMIT 1`, runID).Scan(&attemptID)
	if err == nil {
		return attemptID, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	attemptID = uuid.New().String()
	_, err = tx.Exec(`
		INSERT INTO task_run_attempts(
			id, tenant_id, run_id, attempt_id, attempt_number, trigger_id, claw_id, status, failure_type,
			started_at, created_at, updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		attemptID, tenantID, runID, attemptID, 1, triggerID, clawID, "running", "", startedAt, startedAt, startedAt,
	)
	return attemptID, err
}

func normalizeTaskRunStart(opts *TaskRunStart, tenantID, model, llmKey, factoryName, externalTriggerID, issueID string) {
	if opts.TenantID == "" {
		opts.TenantID = tenantID
	}
	if opts.RunKind == "" {
		opts.RunKind = taskRunKindPRTask
	}
	if opts.OwnerType == "" {
		opts.OwnerType = taskRunOwnerFactory
	}
	if opts.FactoryName == "" {
		opts.FactoryName = factoryName
	}
	if opts.OwnerName == "" {
		opts.OwnerName = firstNonEmpty(opts.OwnerDisplayName, opts.FactoryName, opts.WorkflowName)
	}
	if opts.OwnerDisplayName == "" {
		opts.OwnerDisplayName = opts.OwnerName
	}
	if opts.Model == "" {
		opts.Model = model
	}
	if opts.LLMKey == "" {
		opts.LLMKey = llmKey
	}
	if opts.ExternalTriggerID == "" {
		opts.ExternalTriggerID = externalTriggerID
	}
	if opts.IssueID == "" {
		opts.IssueID = issueID
	}
	if opts.Integration == "" {
		opts.Integration = firstNonEmpty(opts.Source, opts.FactoryName, opts.WorkflowName)
	}
}

func sanitizeTaskRunDetail(detail map[string]any) (string, error) {
	clean := make(map[string]any, len(detail)+1)
	for k, v := range detail {
		clean[k] = sanitizeTaskRunDetailValue(k, v)
	}
	if _, ok := clean["schemaVersion"]; !ok {
		clean["schemaVersion"] = taskRunDetailSchemaVersion
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return "", fmt.Errorf("sanitize task run detail: %w", err)
	}
	if len(b) <= taskRunMaxDetailBytes {
		return string(b), nil
	}
	b, err = json.Marshal(map[string]any{
		"schemaVersion": taskRunDetailSchemaVersion,
		"truncated":     true,
		"originalBytes": len(b),
	})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func sanitizeTaskRunDetailValue(key string, value any) any {
	if isTaskRunSensitiveTextKey(key) {
		if text, ok := value.(string); ok {
			return map[string]any{
				"redacted": true,
				"bytes":    len(text),
			}
		}
		return map[string]any{"redacted": true}
	}
	switch typed := value.(type) {
	case map[string]any:
		clean := make(map[string]any, len(typed))
		for childKey, childValue := range typed {
			clean[childKey] = sanitizeTaskRunDetailValue(childKey, childValue)
		}
		return clean
	case []any:
		clean := make([]any, 0, len(typed))
		for _, item := range typed {
			clean = append(clean, sanitizeTaskRunDetailValue("", item))
		}
		return clean
	default:
		return value
	}
}

func isTaskRunSensitiveTextKey(key string) bool {
	switch strings.ToLower(key) {
	case "body", "comment", "content", "message", "text", "raw", "payload", "description":
		return true
	default:
		return false
	}
}

func mergeTaskRunTags(raw string, extra []string) (string, error) {
	seen := map[string]bool{}
	tags := []string{}
	var decoded any
	if strings.TrimSpace(raw) != "" && json.Unmarshal([]byte(raw), &decoded) == nil {
		if list, ok := decoded.([]any); ok {
			for _, item := range list {
				if tag, ok := item.(string); ok && tag != "" && !seen[tag] {
					seen[tag] = true
					tags = append(tags, tag)
				}
			}
		}
	}
	for _, tag := range extra {
		if tag != "" && !seen[tag] {
			seen[tag] = true
			tags = append(tags, tag)
		}
	}
	b, err := json.Marshal(tags)
	return string(b), err
}

func encodeWarningTypes(warnings map[string]bool) string {
	if len(warnings) == 0 {
		return "[]"
	}
	values := make([]string, 0, len(warnings))
	for warning := range warnings {
		if warning != "" {
			values = append(values, warning)
		}
	}
	sort.Strings(values)
	b, _ := json.Marshal(values)
	return string(b)
}

func isHumanWarningEvent(input TaskRunEvent) bool {
	return input.ActorType == taskRunActorHuman ||
		input.EventType == taskRunEventHumanPRComment ||
		input.EventType == taskRunEventHumanDashboardMessage ||
		input.InteractionRole == taskRunInteractionWarning
}

func warningTypeForEvent(event taskRunEventProjection) string {
	if warning := normalizeTaskRunWarningType(event.warningType); warning != "" {
		return warning
	}
	switch event.eventType {
	case taskRunEventHumanPRComment:
		return taskRunWarningHumanPRComment
	case "human_review_comment":
		return taskRunWarningHumanReviewComment
	case "human_requested_changes":
		return taskRunWarningHumanRequestedChanges
	case "human_manual_code_push":
		return taskRunWarningHumanManualCodePush
	case "human_tracker_update":
		return taskRunWarningHumanTrackerUpdate
	case taskRunEventHumanDashboardMessage:
		return taskRunWarningHumanDashboardMessage
	case "human_manual_stop_or_resume":
		return taskRunWarningHumanManualStopResume
	case "human_settings_or_status_change":
		return taskRunWarningHumanSettingsChange
	case "unknown_human_interaction":
		return taskRunWarningUnknownHuman
	}
	if event.actorType == taskRunActorHuman {
		return taskRunWarningUnknownHuman
	}
	return ""
}

func warningTypeForTaskRunInput(input TaskRunEvent) string {
	return warningTypeForEvent(taskRunEventProjection{
		eventType:   input.EventType,
		actorType:   input.ActorType,
		warningType: input.WarningType,
	})
}

func warningTypeForTaskRunEventType(eventType string) string {
	return warningTypeForEvent(taskRunEventProjection{
		eventType: eventType,
		actorType: taskRunActorHuman,
	})
}

func normalizeTaskRunWarningType(value string) string {
	switch value {
	case taskRunWarningHumanPRComment,
		taskRunWarningHumanReviewComment,
		taskRunWarningHumanRequestedChanges,
		taskRunWarningHumanManualCodePush,
		taskRunWarningHumanTrackerUpdate,
		taskRunWarningHumanDashboardMessage,
		taskRunWarningHumanManualStopResume,
		taskRunWarningHumanSettingsChange,
		taskRunWarningUnknownHuman,
		taskRunWarningPRReplaced:
		return value
	default:
		return ""
	}
}

func detailPR(detail map[string]any) (string, int) {
	repo, _ := detail["repo"].(string)
	if repo == "" {
		repo, _ = detail["repository"].(string)
	}
	return repo, intFromDetail(detail, "prNumber", "pr_number")
}

func intFromDetail(detail map[string]any, keys ...string) int {
	for _, key := range keys {
		switch v := detail[key].(type) {
		case int:
			return v
		case int64:
			return int(v)
		case float64:
			return int(v)
		case json.Number:
			n, _ := v.Int64()
			return int(n)
		}
	}
	return 0
}

func sanitizeTaskRunEventKey(s string) string {
	replacer := strings.NewReplacer(" ", "-", "/", ":", "\n", "-", "\t", "-")
	return strings.ToLower(replacer.Replace(strings.TrimSpace(s)))
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func epochMillisOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return epochMillis(t)
}

func firstNonZeroTime(values ...time.Time) time.Time {
	for _, value := range values {
		if !value.IsZero() {
			return value
		}
	}
	return time.Time{}
}

func defaultString(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
