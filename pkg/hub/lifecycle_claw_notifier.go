package hub

import (
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// The claw pass extends the Slack notifier to ad-hoc claws: claws spawned
// outside a task run (empty claws.task_run_id), which produce no task_run_events
// and are therefore invisible to the task-run pass. It derives the same three
// notification kinds from claw state instead:
//
//   - agent started: the claw is in status 'connected' and past the bootstrap
//     gate its provider actually uses (see lifecycleClawStateCondition — only the
//     VM providers ever set bootstrap_ok)
//   - pr opened:     a new claw_prs row for the claw
//   - failure:       the claw reached a terminal failure: status 'error'
//     (stopAgentWithReason/finishClawTerminalTx), or status 'deleted' with the
//     latest workflow run failed (the pipeline-terminal and factory-trigger
//     failure paths). bootstrap_diagnostic carries the reason when set.
//
// Ownership rule (the no-double-notification invariant): a claw with a
// non-empty task_run_id is handled exclusively by the task-run pass; a claw
// with an empty task_run_id exclusively by this pass. Because task_run_id is
// populated by ensureTaskRunForClaw shortly AFTER the claw row is inserted, a
// freshly created task-run claw briefly looks ad-hoc. Two defenses close that
// race:
//
//  1. Grace period: a claw is only classified as ad-hoc once it is at least
//     lifecycleClawAdhocGrace old — orders of magnitude longer than the
//     insert-to-attach window, which happens within the same request.
//  2. Re-check before send: task_run_id is re-read immediately before every
//     claw-pass send; a claw that acquired a task run since selection is
//     skipped without recording anything, leaving it to the task-run pass.
//
// Dedupe reuses slack_notification_deliveries with namespaced synthetic keys
// ("claw:<id>:agent_started", "claw:<id>:failure", "claw:<id>:pr:<url>") that
// can never collide with task_run_events ids. Threading reuses
// slack_run_threads with the namespaced key "claw:<id>" in the run_id column,
// so all events for one ad-hoc claw share one thread and existing run threads
// keep working unchanged.
const (
	// lifecycleStateClawBaselineKey marks that the claw pass has recorded the
	// current claw/PR state as history. Set on the first enabled run so
	// enabling the feature (or deploying it) never replays pre-existing claws
	// or claw_prs rows — the same rule the task-run watermark follows.
	lifecycleStateClawBaselineKey = "claw_baseline_done"

	// lifecycleStateClawPRWatermarkKey stores the claw_prs rowid the claw pass has
	// processed up to (insertion-ordered, like the task-run event watermark).
	lifecycleStateClawPRWatermarkKey = "claw_prs_watermark_rowid"

	// lifecycleClawAdhocGrace is how old a claw must be before the claw pass will
	// classify it as ad-hoc. See the race note above. Every ensureTaskRunForClaw
	// caller attaches the run in the same request that inserts the claw row, so
	// the insert-to-attach window is tiny; the grace only needs to comfortably
	// cover it (the pre-send re-check in deliverLifecycleClawEvent is the real
	// defense). Keep it short: the state pass samples current claw state, so an
	// oversized grace would swallow states that end before it expires (e.g. an
	// ad-hoc claw that errors and is deleted within a couple of minutes).
	lifecycleClawAdhocGrace = 10 * time.Second

	// notificationDeliveryStatusSkipped marks delivery rows seeded to suppress a
	// notification (baseline at first enable, parking while disabled) rather
	// than recording a real send.
	notificationDeliveryStatusSkipped = "skipped"
)

// Claw-pass event kinds (states scanned by selectLifecycleClawStateCandidates).
const (
	lifecycleClawKindStarted = "agent_started"
	lifecycleClawKindFailure = "failure"
)

func lifecycleClawStartedKey(clawID string) string { return "claw:" + clawID + ":agent_started" }
func lifecycleClawFailureKey(clawID string) string { return "claw:" + clawID + ":failure" }
func lifecycleClawPRKey(clawID, prURL string) string {
	return "claw:" + clawID + ":pr:" + prURL
}

// lifecycleClawThreadKey namespaces the claw id for the slack_run_threads run_id
// column so claw threads can never collide with task run ids.
func lifecycleClawThreadKey(clawID string) string { return "claw:" + clawID }

// lifecycleClawRow is the claws-table context the claw pass renders messages from.
type lifecycleClawRow struct {
	ID                  string
	TenantID            string
	Name                string
	IssueTitle          string
	LinearIssueID       string
	GitHubIssueID       string
	ShortcutStoryID     string
	JiraIssueID         string
	FactoryName         string
	Model               string
	BootstrapDiagnostic string
}

const lifecycleClawSelectColumns = `c.id, c.tenant_id, c.name, c.issue_title, c.linear_issue_id, c.github_issue_id,
	       c.shortcut_story_id, c.jira_issue_id, c.factory_name, c.default_model, c.bootstrap_diagnostic`

func scanLifecycleClawRow(scan func(dest ...any) error, claw *lifecycleClawRow) error {
	return scan(&claw.ID, &claw.TenantID, &claw.Name, &claw.IssueTitle, &claw.LinearIssueID,
		&claw.GitHubIssueID, &claw.ShortcutStoryID, &claw.JiraIssueID, &claw.FactoryName,
		&claw.Model, &claw.BootstrapDiagnostic)
}

// lifecycleClawRunContext adapts a claw row to the run context the shared Block
// Kit renderers consume.
func lifecycleClawRunContext(claw lifecycleClawRow) lifecycleRunContext {
	issueID := firstNonEmpty(claw.LinearIssueID, claw.GitHubIssueID, claw.ShortcutStoryID, claw.JiraIssueID)
	title := claw.IssueTitle
	if issueID == "" && title == "" {
		// Ad-hoc claws usually carry no issue; use the claw name as the
		// subject so messages read "Agent started — local-agent".
		title = claw.Name
	}
	return lifecycleRunContext{
		IssueID:     issueID,
		IssueTitle:  title,
		FactoryName: claw.FactoryName,
		ClawID:      claw.ID,
		Model:       claw.Model,
	}
}

// lifecycleClawKindsEnabled maps the existing config toggles onto the claw-pass
// kinds. The claw pass intentionally shares the task-run toggles: an operator
// muting pr_opened mutes it for both sources.
func lifecycleClawKindsEnabled(cfg *types.LifecycleNotificationsConfig) (agentStarted, prOpened, failures bool) {
	agentStarted, prOpened, failures = true, true, true
	if cfg != nil && cfg.Events != nil {
		if cfg.Events.AgentStarted != nil {
			agentStarted = *cfg.Events.AgentStarted
		}
		if cfg.Events.PROpened != nil {
			prOpened = *cfg.Events.PROpened
		}
		if cfg.Events.Failures != nil {
			failures = *cfg.Events.Failures
		}
	}
	return
}

// lifecycleClawPass is the claw-level counterpart of slackTaskRunPass, covering
// only claws with an empty task_run_id.
func (s *Server) lifecycleClawPass(d lifecycleDelivery) {
	if _, found, err := s.notifierStateInt64(lifecycleStateClawBaselineKey); err != nil {
		log.Printf("[notify] read claw baseline state: %v", err)
		return
	} else if !found {
		// First enabled run of the claw pass: mark everything that already
		// happened as handled so pre-existing claws and claw_prs rows are not
		// replayed into the channel.
		if err := s.seedLifecycleClawBaseline(); err != nil {
			log.Printf("[notify] seed claw baseline: %v", err)
			return
		}
		s.setNotifierStateInt64(lifecycleStateClawBaselineKey, 1)
		return
	}

	startedOn, prOn, failuresOn := lifecycleClawKindsEnabled(d.lc)
	// Park disabled kinds so re-enabling a toggle behaves like a fresh enable
	// instead of flushing every transition from the muted window.
	if !startedOn {
		s.skipCurrentLifecycleClawState(lifecycleClawKindStarted)
	}
	if !failuresOn {
		s.skipCurrentLifecycleClawState(lifecycleClawKindFailure)
	}
	if !prOn {
		s.parkLifecycleClawPRWatermark()
	}

	if startedOn && !s.sendLifecycleClawStateEvents(d, lifecycleClawKindStarted) {
		return
	}
	if failuresOn && !s.sendLifecycleClawStateEvents(d, lifecycleClawKindFailure) {
		return
	}
	if prOn {
		s.lifecycleClawPRPass(d)
	}
}

// seedLifecycleClawBaseline records the current claw/PR state as already handled.
func (s *Server) seedLifecycleClawBaseline() error {
	if err := s.skipCurrentLifecycleClawState(lifecycleClawKindStarted); err != nil {
		return err
	}
	if err := s.skipCurrentLifecycleClawState(lifecycleClawKindFailure); err != nil {
		return err
	}
	var maxRow int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(rowid), 0) FROM claw_prs`).Scan(&maxRow); err != nil {
		return err
	}
	s.setNotifierStateInt64(lifecycleStateClawPRWatermarkKey, maxRow)
	return nil
}

// parkLifecycleClawState is the claw-pass analog of parkSlackWatermark: while
// Slack is disabled the current claw state keeps being recorded as handled,
// so a later re-enable behaves like a fresh enable instead of flushing the
// transitions accumulated during the disabled window.
func (s *Server) parkLifecycleClawState() {
	if _, found, err := s.notifierStateInt64(lifecycleStateClawBaselineKey); err != nil || !found {
		// Never enabled (or state unreadable): nothing to park.
		return
	}
	_ = s.skipCurrentLifecycleClawState(lifecycleClawKindStarted)
	_ = s.skipCurrentLifecycleClawState(lifecycleClawKindFailure)
	s.parkLifecycleClawPRWatermark()
}

// skipCurrentLifecycleClawState seeds "skipped" delivery rows for every ad-hoc
// claw currently matching the kind's state, suppressing notifications for
// transitions that already happened. Idempotent (ON CONFLICT DO NOTHING), so
// claws that were genuinely notified keep their "sent" rows.
func (s *Server) skipCurrentLifecycleClawState(kind string) error {
	cond, suffix := lifecycleClawStateCondition(kind)
	_, err := s.db.Exec(`
		INSERT INTO slack_notification_deliveries(event_id, run_id, delivered_at, message_ts, status)
		SELECT 'claw:' || c.id || ?, 'claw:' || c.id, ?, '', ?
		  FROM claws c
		 WHERE c.task_run_id = '' AND `+cond+`
		ON CONFLICT(event_id) DO NOTHING`,
		suffix, epochMillis(now()), notificationDeliveryStatusSkipped)
	if err != nil {
		log.Printf("[notify] seed skipped claw %s deliveries: %v", kind, err)
	}
	return err
}

func (s *Server) parkLifecycleClawPRWatermark() {
	var maxRow int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(rowid), 0) FROM claw_prs`).Scan(&maxRow); err != nil {
		log.Printf("[notify] park claw PR watermark: %v", err)
		return
	}
	wm, found, err := s.notifierStateInt64(lifecycleStateClawPRWatermarkKey)
	if err != nil {
		log.Printf("[notify] park claw PR watermark: %v", err)
		return
	}
	if !found || maxRow > wm {
		s.setNotifierStateInt64(lifecycleStateClawPRWatermarkKey, maxRow)
	}
}

// lifecycleClawStateCondition returns the SQL state condition and the delivery-key
// suffix for a claw-pass state kind.
func lifecycleClawStateCondition(kind string) (cond, keySuffix string) {
	if kind == lifecycleClawKindFailure {
		// Two terminal failure shapes exist in the hub:
		//   - status='error' (stopAgentWithReason → finishClawTerminalTx), and
		//   - status='deleted' with the claw's latest workflow run 'failed' —
		//     the pipeline-terminal and factory-trigger failure paths call
		//     finishClawTerminalTx(claw, "deleted", "", "failed", ...).
		// The workflow-run verdict keys the second shape because success paths
		// also end at status='deleted' (with runs 'completed'/'canceled'), so
		// the claw status alone cannot distinguish failure from success there.
		// A missing workflow run yields NULL, which never equals 'failed'.
		return `(c.status = 'error' OR (c.status = 'deleted' AND (
			SELECT w.status FROM workflow_runs w
			 WHERE w.claw_id = c.id
			 ORDER BY w.created_at DESC, w.rowid DESC LIMIT 1
		) = 'failed'))`, ":failure"
	}
	// Mirror allowWakeBeforeBootstrap: only the VM providers gate readiness on
	// bootstrap_ok — docker/local/noop claws are upserted straight to
	// 'connected' by bridge registration with bootstrap_ok still 0, and
	// requiring bootstrap_ok=1 for them would mean ad-hoc claws (the very case
	// this pass exists for) never produce an agent_started notification.
	return `(c.status = 'connected' AND (c.bootstrap_ok = 1
		OR c.provider NOT IN ('daytona', 'replicated', 'exedev')))`, ":agent_started"
}

// selectLifecycleClawStateCandidates returns ad-hoc claws currently in the kind's
// state that have no delivery row yet. The dedupe happens in SQL (like the
// task-run pass) so already-handled claws never consume the LIMIT.
func (s *Server) selectLifecycleClawStateCandidates(kind string) ([]lifecycleClawRow, error) {
	cond, suffix := lifecycleClawStateCondition(kind)
	cutoff := now().Add(-lifecycleClawAdhocGrace).Unix()
	rows, err := s.db.Query(`
		SELECT `+lifecycleClawSelectColumns+`
		  FROM claws c
		 WHERE c.task_run_id = '' AND `+cond+`
		   AND CAST(strftime('%s', c.created_at) AS INTEGER) <= ?
		   AND NOT EXISTS (
			SELECT 1 FROM slack_notification_deliveries d WHERE d.event_id = 'claw:' || c.id || ?
		   )
		 ORDER BY c.created_at
		 LIMIT `+strconv.Itoa(lifecycleBatchSize), cutoff, suffix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var claws []lifecycleClawRow
	for rows.Next() {
		var claw lifecycleClawRow
		if err := scanLifecycleClawRow(rows.Scan, &claw); err != nil {
			return nil, err
		}
		claws = append(claws, claw)
	}
	return claws, rows.Err()
}

// lifecycleClawStateEvent renders the synthetic event row for a state kind.
func lifecycleClawStateEvent(kind string, claw lifecycleClawRow) (lifecycleEventRow, string) {
	if kind == lifecycleClawKindFailure {
		ev := lifecycleEventRow{
			EventType:   taskRunEventAgentStopped,
			FailureType: taskRunFailureAgentStopped,
		}
		if claw.BootstrapDiagnostic != "" {
			ev.Detail = map[string]any{"reason": claw.BootstrapDiagnostic}
		}
		return ev, lifecycleClawFailureKey(claw.ID)
	}
	return lifecycleEventRow{EventType: taskRunEventAgentStarted}, lifecycleClawStartedKey(claw.ID)
}

// sendLifecycleClawStateEvents delivers one state kind. Returns false when the
// claw pass must stop for this tick (config-level or transient Slack failure).
func (s *Server) sendLifecycleClawStateEvents(d lifecycleDelivery, kind string) bool {
	claws, err := s.selectLifecycleClawStateCandidates(kind)
	if err != nil {
		log.Printf("[notify] select claw %s candidates: %v", kind, err)
		return false
	}
	for _, claw := range claws {
		ev, deliveryKey := lifecycleClawStateEvent(kind, claw)
		if !s.deliverLifecycleClawEvent(d, claw, ev, lifecycleClawRunContext(claw), deliveryKey) {
			return false
		}
	}
	return true
}

// deliverLifecycleClawEvent posts one claw-sourced event with the shared error
// policy: config errors pause everything, permanent errors are recorded as
// failed (and count as handled), transient errors stop the pass for this
// tick. Returns true when the event is handled (sent, permanently failed, or
// ceded to the task-run path) and the caller may move past it.
func (s *Server) deliverLifecycleClawEvent(d lifecycleDelivery, claw lifecycleClawRow, ev lifecycleEventRow, runCtx lifecycleRunContext, deliveryKey string) bool {
	// Re-check ownership immediately before sending: task_run_id may have been
	// populated after this claw was selected, and a claw that belongs to a
	// task run must be notified exclusively by the task-run pass. Nothing is
	// recorded for a ceded claw — the task-run pass uses its own event keys.
	var taskRunID string
	if err := s.db.QueryRow(`SELECT task_run_id FROM claws WHERE id=?`, claw.ID).Scan(&taskRunID); err != nil {
		log.Printf("[notify] re-check task_run_id for claw %s: %v", claw.ID, err)
		return false
	}
	if taskRunID != "" {
		return true
	}

	threadKey := lifecycleClawThreadKey(claw.ID)
	err := s.postLifecycleEvent(d, ev, runCtx, threadKey, deliveryKey, claw.TenantID)
	if err == nil {
		s.clearPollWarning(lifecycleSendWarningKey)
		return true
	}
	// Same send-failure policy as the task-run pass: config errors pause,
	// permanent errors are burned as failed, transient errors stop the pass.
	handled, _ := s.handleLifecycleSendError(err, "claw event "+deliveryKey, deliveryKey, threadKey)
	return handled
}

type lifecycleClawPRRow struct {
	RowID         int64
	Repo          string
	PRNumber      int
	PRURL         string
	TaskRunID     string
	ClawCreatedAt int64 // unix seconds
	Claw          lifecycleClawRow
}

// lifecycleClawPRPass scans claw_prs behind the rowid watermark for PRs opened by
// ad-hoc claws.
func (s *Server) lifecycleClawPRPass(d lifecycleDelivery) {
	watermark, found, err := s.notifierStateInt64(lifecycleStateClawPRWatermarkKey)
	if err != nil {
		log.Printf("[notify] read claw PR watermark: %v", err)
		return
	}
	if !found {
		// Defensive: the baseline normally initializes this. Initialize at the
		// end of the stream rather than replaying history.
		s.parkLifecycleClawPRWatermark()
		return
	}

	rows, err := s.db.Query(`
		SELECT p.rowid, p.repo, p.pr_number, p.pr_url, c.task_run_id,
		       CAST(strftime('%s', c.created_at) AS INTEGER),
		       `+lifecycleClawSelectColumns+`
		  FROM claw_prs p JOIN claws c ON c.id = p.claw_id
		 WHERE p.rowid > ?
		   AND NOT EXISTS (
			SELECT 1 FROM slack_notification_deliveries d
			 WHERE d.event_id = 'claw:' || p.claw_id || ':pr:' || p.pr_url
		   )
		 ORDER BY p.rowid
		 LIMIT `+strconv.Itoa(lifecycleBatchSize), watermark)
	if err != nil {
		log.Printf("[notify] select claw PR candidates: %v", err)
		return
	}
	var prs []lifecycleClawPRRow
	for rows.Next() {
		var pr lifecycleClawPRRow
		var createdAt sql.NullInt64
		if err := rows.Scan(&pr.RowID, &pr.Repo, &pr.PRNumber, &pr.PRURL, &pr.TaskRunID, &createdAt,
			&pr.Claw.ID, &pr.Claw.TenantID, &pr.Claw.Name, &pr.Claw.IssueTitle, &pr.Claw.LinearIssueID,
			&pr.Claw.GitHubIssueID, &pr.Claw.ShortcutStoryID, &pr.Claw.JiraIssueID, &pr.Claw.FactoryName,
			&pr.Claw.Model, &pr.Claw.BootstrapDiagnostic); err != nil {
			rows.Close()
			log.Printf("[notify] scan claw PR candidate: %v", err)
			return
		}
		pr.ClawCreatedAt = createdAt.Int64
		prs = append(prs, pr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Printf("[notify] select claw PR candidates: %v", err)
		return
	}

	cutoff := now().Add(-lifecycleClawAdhocGrace).Unix()
	maxHandled := watermark
	for _, pr := range prs {
		if pr.TaskRunID != "" {
			// Owned by the task-run pass; move past it.
			maxHandled = pr.RowID
			continue
		}
		if pr.ClawCreatedAt > cutoff {
			// The claw is too young to classify as ad-hoc (its task_run_id may
			// still be on the way). Defer this row — and everything after it,
			// so the watermark cannot advance past a PR that may still need a
			// notification.
			break
		}
		ev := lifecycleEventRow{
			EventType:   taskRunEventPROpened,
			TargetURL:   pr.PRURL,
			TargetLabel: fmt.Sprintf("%s#%d", pr.Repo, pr.PRNumber),
		}
		runCtx := lifecycleClawRunContext(pr.Claw)
		runCtx.Repo = pr.Repo
		if !s.deliverLifecycleClawEvent(d, pr.Claw, ev, runCtx, lifecycleClawPRKey(pr.Claw.ID, pr.PRURL)) {
			break
		}
		maxHandled = pr.RowID
	}
	if maxHandled > watermark {
		s.setNotifierStateInt64(lifecycleStateClawPRWatermarkKey, maxHandled)
	}
}

// lifecycleClawByID loads the claw context for the manual test endpoint.
func (s *Server) lifecycleClawByID(clawID string) (lifecycleClawRow, bool, error) {
	var claw lifecycleClawRow
	err := scanLifecycleClawRow(s.db.QueryRow(`
		SELECT `+lifecycleClawSelectColumns+`
		  FROM claws c WHERE c.id = ?`, clawID).Scan, &claw)
	if err == sql.ErrNoRows {
		return claw, false, nil
	}
	if err != nil {
		return claw, false, err
	}
	return claw, true, nil
}
