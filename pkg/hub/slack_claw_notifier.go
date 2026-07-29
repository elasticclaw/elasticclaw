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
//     gate its provider actually uses (see slackClawStateCondition — only the
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
//     slackClawAdhocGrace old — orders of magnitude longer than the
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
	// slackStateClawBaselineKey marks that the claw pass has recorded the
	// current claw/PR state as history. Set on the first enabled run so
	// enabling the feature (or deploying it) never replays pre-existing claws
	// or claw_prs rows — the same rule the task-run watermark follows.
	slackStateClawBaselineKey = "claw_baseline_done"

	// slackStateClawPRWatermarkKey stores the claw_prs rowid the claw pass has
	// processed up to (insertion-ordered, like the task-run event watermark).
	slackStateClawPRWatermarkKey = "claw_prs_watermark_rowid"

	// slackClawAdhocGrace is how old a claw must be before the claw pass will
	// classify it as ad-hoc. See the race note above. Every ensureTaskRunForClaw
	// caller attaches the run in the same request that inserts the claw row, so
	// the insert-to-attach window is tiny; the grace only needs to comfortably
	// cover it (the pre-send re-check in deliverSlackClawEvent is the real
	// defense). Keep it short: the state pass samples current claw state, so an
	// oversized grace would swallow states that end before it expires (e.g. an
	// ad-hoc claw that errors and is deleted within a couple of minutes).
	slackClawAdhocGrace = 10 * time.Second

	// slackDeliveryStatusSkipped marks delivery rows seeded to suppress a
	// notification (baseline at first enable, parking while disabled) rather
	// than recording a real send.
	slackDeliveryStatusSkipped = "skipped"
)

// Claw-pass event kinds (states scanned by selectSlackClawStateCandidates).
const (
	slackClawKindStarted = "agent_started"
	slackClawKindFailure = "failure"
)

func slackClawStartedKey(clawID string) string { return "claw:" + clawID + ":agent_started" }
func slackClawFailureKey(clawID string) string { return "claw:" + clawID + ":failure" }
func slackClawPRKey(clawID, prURL string) string {
	return "claw:" + clawID + ":pr:" + prURL
}

// slackClawThreadKey namespaces the claw id for the slack_run_threads run_id
// column so claw threads can never collide with task run ids.
func slackClawThreadKey(clawID string) string { return "claw:" + clawID }

// slackClawRow is the claws-table context the claw pass renders messages from.
type slackClawRow struct {
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

const slackClawSelectColumns = `c.id, c.tenant_id, c.name, c.issue_title, c.linear_issue_id, c.github_issue_id,
	       c.shortcut_story_id, c.jira_issue_id, c.factory_name, c.default_model, c.bootstrap_diagnostic`

func scanSlackClawRow(scan func(dest ...any) error, claw *slackClawRow) error {
	return scan(&claw.ID, &claw.TenantID, &claw.Name, &claw.IssueTitle, &claw.LinearIssueID,
		&claw.GitHubIssueID, &claw.ShortcutStoryID, &claw.JiraIssueID, &claw.FactoryName,
		&claw.Model, &claw.BootstrapDiagnostic)
}

// slackClawRunContext adapts a claw row to the run context the shared Block
// Kit renderers consume.
func slackClawRunContext(claw slackClawRow) slackRunContext {
	issueID := firstNonEmpty(claw.LinearIssueID, claw.GitHubIssueID, claw.ShortcutStoryID, claw.JiraIssueID)
	title := claw.IssueTitle
	if issueID == "" && title == "" {
		// Ad-hoc claws usually carry no issue; use the claw name as the
		// subject so messages read "Agent started — local-agent".
		title = claw.Name
	}
	return slackRunContext{
		IssueID:     issueID,
		IssueTitle:  title,
		FactoryName: claw.FactoryName,
		ClawID:      claw.ID,
		Model:       claw.Model,
	}
}

// slackClawKindsEnabled maps the existing config toggles onto the claw-pass
// kinds. The claw pass intentionally shares the task-run toggles: an operator
// muting pr_opened mutes it for both sources.
func slackClawKindsEnabled(cfg *types.SlackNotificationsConfig) (agentStarted, prOpened, failures bool) {
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

// slackClawPass is the claw-level counterpart of slackTaskRunPass, covering
// only claws with an empty task_run_id.
func (s *Server) slackClawPass(client *slackClient, cfg *types.SlackNotificationsConfig) {
	if _, found, err := s.slackStateInt64(slackStateClawBaselineKey); err != nil {
		log.Printf("[slack] read claw baseline state: %v", err)
		return
	} else if !found {
		// First enabled run of the claw pass: mark everything that already
		// happened as handled so pre-existing claws and claw_prs rows are not
		// replayed into the channel.
		if err := s.seedSlackClawBaseline(); err != nil {
			log.Printf("[slack] seed claw baseline: %v", err)
			return
		}
		s.setSlackStateInt64(slackStateClawBaselineKey, 1)
		return
	}

	startedOn, prOn, failuresOn := slackClawKindsEnabled(cfg)
	// Park disabled kinds so re-enabling a toggle behaves like a fresh enable
	// instead of flushing every transition from the muted window.
	if !startedOn {
		s.skipCurrentSlackClawState(slackClawKindStarted)
	}
	if !failuresOn {
		s.skipCurrentSlackClawState(slackClawKindFailure)
	}
	if !prOn {
		s.parkSlackClawPRWatermark()
	}

	if startedOn && !s.sendSlackClawStateEvents(client, cfg, slackClawKindStarted) {
		return
	}
	if failuresOn && !s.sendSlackClawStateEvents(client, cfg, slackClawKindFailure) {
		return
	}
	if prOn {
		s.slackClawPRPass(client, cfg)
	}
}

// seedSlackClawBaseline records the current claw/PR state as already handled.
func (s *Server) seedSlackClawBaseline() error {
	if err := s.skipCurrentSlackClawState(slackClawKindStarted); err != nil {
		return err
	}
	if err := s.skipCurrentSlackClawState(slackClawKindFailure); err != nil {
		return err
	}
	var maxRow int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(rowid), 0) FROM claw_prs`).Scan(&maxRow); err != nil {
		return err
	}
	s.setSlackStateInt64(slackStateClawPRWatermarkKey, maxRow)
	return nil
}

// parkSlackClawState is the claw-pass analog of parkSlackWatermark: while
// Slack is disabled the current claw state keeps being recorded as handled,
// so a later re-enable behaves like a fresh enable instead of flushing the
// transitions accumulated during the disabled window.
func (s *Server) parkSlackClawState() {
	if _, found, err := s.slackStateInt64(slackStateClawBaselineKey); err != nil || !found {
		// Never enabled (or state unreadable): nothing to park.
		return
	}
	_ = s.skipCurrentSlackClawState(slackClawKindStarted)
	_ = s.skipCurrentSlackClawState(slackClawKindFailure)
	s.parkSlackClawPRWatermark()
}

// skipCurrentSlackClawState seeds "skipped" delivery rows for every ad-hoc
// claw currently matching the kind's state, suppressing notifications for
// transitions that already happened. Idempotent (ON CONFLICT DO NOTHING), so
// claws that were genuinely notified keep their "sent" rows.
func (s *Server) skipCurrentSlackClawState(kind string) error {
	cond, suffix := slackClawStateCondition(kind)
	_, err := s.db.Exec(`
		INSERT INTO slack_notification_deliveries(event_id, run_id, delivered_at, message_ts, status)
		SELECT 'claw:' || c.id || ?, 'claw:' || c.id, ?, '', ?
		  FROM claws c
		 WHERE c.task_run_id = '' AND `+cond+`
		ON CONFLICT(event_id) DO NOTHING`,
		suffix, epochMillis(now()), slackDeliveryStatusSkipped)
	if err != nil {
		log.Printf("[slack] seed skipped claw %s deliveries: %v", kind, err)
	}
	return err
}

func (s *Server) parkSlackClawPRWatermark() {
	var maxRow int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(rowid), 0) FROM claw_prs`).Scan(&maxRow); err != nil {
		log.Printf("[slack] park claw PR watermark: %v", err)
		return
	}
	wm, found, err := s.slackStateInt64(slackStateClawPRWatermarkKey)
	if err != nil {
		log.Printf("[slack] park claw PR watermark: %v", err)
		return
	}
	if !found || maxRow > wm {
		s.setSlackStateInt64(slackStateClawPRWatermarkKey, maxRow)
	}
}

// slackClawStateCondition returns the SQL state condition and the delivery-key
// suffix for a claw-pass state kind.
func slackClawStateCondition(kind string) (cond, keySuffix string) {
	if kind == slackClawKindFailure {
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

// selectSlackClawStateCandidates returns ad-hoc claws currently in the kind's
// state that have no delivery row yet. The dedupe happens in SQL (like the
// task-run pass) so already-handled claws never consume the LIMIT.
func (s *Server) selectSlackClawStateCandidates(kind string) ([]slackClawRow, error) {
	cond, suffix := slackClawStateCondition(kind)
	cutoff := now().Add(-slackClawAdhocGrace).Unix()
	rows, err := s.db.Query(`
		SELECT `+slackClawSelectColumns+`
		  FROM claws c
		 WHERE c.task_run_id = '' AND `+cond+`
		   AND CAST(strftime('%s', c.created_at) AS INTEGER) <= ?
		   AND NOT EXISTS (
			SELECT 1 FROM slack_notification_deliveries d WHERE d.event_id = 'claw:' || c.id || ?
		   )
		 ORDER BY c.created_at
		 LIMIT `+strconv.Itoa(slackBatchSize), cutoff, suffix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var claws []slackClawRow
	for rows.Next() {
		var claw slackClawRow
		if err := scanSlackClawRow(rows.Scan, &claw); err != nil {
			return nil, err
		}
		claws = append(claws, claw)
	}
	return claws, rows.Err()
}

// slackClawStateEvent renders the synthetic event row for a state kind.
func slackClawStateEvent(kind string, claw slackClawRow) (slackEventRow, string) {
	if kind == slackClawKindFailure {
		ev := slackEventRow{
			EventType:   taskRunEventAgentStopped,
			FailureType: taskRunFailureAgentStopped,
		}
		if claw.BootstrapDiagnostic != "" {
			ev.Detail = map[string]any{"reason": claw.BootstrapDiagnostic}
		}
		return ev, slackClawFailureKey(claw.ID)
	}
	return slackEventRow{EventType: taskRunEventAgentStarted}, slackClawStartedKey(claw.ID)
}

// sendSlackClawStateEvents delivers one state kind. Returns false when the
// claw pass must stop for this tick (config-level or transient Slack failure).
func (s *Server) sendSlackClawStateEvents(client *slackClient, cfg *types.SlackNotificationsConfig, kind string) bool {
	claws, err := s.selectSlackClawStateCandidates(kind)
	if err != nil {
		log.Printf("[slack] select claw %s candidates: %v", kind, err)
		return false
	}
	for _, claw := range claws {
		ev, deliveryKey := slackClawStateEvent(kind, claw)
		if !s.deliverSlackClawEvent(client, cfg, claw, ev, slackClawRunContext(claw), deliveryKey) {
			return false
		}
	}
	return true
}

// deliverSlackClawEvent posts one claw-sourced event with the shared error
// policy: config errors pause everything, permanent errors are recorded as
// failed (and count as handled), transient errors stop the pass for this
// tick. Returns true when the event is handled (sent, permanently failed, or
// ceded to the task-run path) and the caller may move past it.
func (s *Server) deliverSlackClawEvent(client *slackClient, cfg *types.SlackNotificationsConfig, claw slackClawRow, ev slackEventRow, runCtx slackRunContext, deliveryKey string) bool {
	// Re-check ownership immediately before sending: task_run_id may have been
	// populated after this claw was selected, and a claw that belongs to a
	// task run must be notified exclusively by the task-run pass. Nothing is
	// recorded for a ceded claw — the task-run pass uses its own event keys.
	var taskRunID string
	if err := s.db.QueryRow(`SELECT task_run_id FROM claws WHERE id=?`, claw.ID).Scan(&taskRunID); err != nil {
		log.Printf("[slack] re-check task_run_id for claw %s: %v", claw.ID, err)
		return false
	}
	if taskRunID != "" {
		return true
	}

	threadKey := slackClawThreadKey(claw.ID)
	err := s.postSlackEvent(client, cfg, ev, runCtx, threadKey, deliveryKey, claw.TenantID)
	if err == nil {
		s.clearPollWarning(slackSendWarningKey)
		return true
	}
	if isSlackConfigError(err) {
		s.logPollWarningOnce(slackSendWarningKey, "[slack] delivery paused until the Slack config is fixed: %v", err)
		return false
	}
	if isPermanentSlackError(err) {
		log.Printf("[slack] permanent failure for claw event %s: %v", deliveryKey, err)
		s.recordSlackDelivery(deliveryKey, threadKey, "", slackDeliveryStatusFailed)
		return true
	}
	log.Printf("[slack] transient failure for claw event %s, will retry: %v", deliveryKey, err)
	return false
}

type slackClawPRRow struct {
	RowID         int64
	Repo          string
	PRNumber      int
	PRURL         string
	TaskRunID     string
	ClawCreatedAt int64 // unix seconds
	Claw          slackClawRow
}

// slackClawPRPass scans claw_prs behind the rowid watermark for PRs opened by
// ad-hoc claws.
func (s *Server) slackClawPRPass(client *slackClient, cfg *types.SlackNotificationsConfig) {
	watermark, found, err := s.slackStateInt64(slackStateClawPRWatermarkKey)
	if err != nil {
		log.Printf("[slack] read claw PR watermark: %v", err)
		return
	}
	if !found {
		// Defensive: the baseline normally initializes this. Initialize at the
		// end of the stream rather than replaying history.
		s.parkSlackClawPRWatermark()
		return
	}

	rows, err := s.db.Query(`
		SELECT p.rowid, p.repo, p.pr_number, p.pr_url, c.task_run_id,
		       CAST(strftime('%s', c.created_at) AS INTEGER),
		       `+slackClawSelectColumns+`
		  FROM claw_prs p JOIN claws c ON c.id = p.claw_id
		 WHERE p.rowid > ?
		   AND NOT EXISTS (
			SELECT 1 FROM slack_notification_deliveries d
			 WHERE d.event_id = 'claw:' || p.claw_id || ':pr:' || p.pr_url
		   )
		 ORDER BY p.rowid
		 LIMIT `+strconv.Itoa(slackBatchSize), watermark)
	if err != nil {
		log.Printf("[slack] select claw PR candidates: %v", err)
		return
	}
	var prs []slackClawPRRow
	for rows.Next() {
		var pr slackClawPRRow
		var createdAt sql.NullInt64
		if err := rows.Scan(&pr.RowID, &pr.Repo, &pr.PRNumber, &pr.PRURL, &pr.TaskRunID, &createdAt,
			&pr.Claw.ID, &pr.Claw.TenantID, &pr.Claw.Name, &pr.Claw.IssueTitle, &pr.Claw.LinearIssueID,
			&pr.Claw.GitHubIssueID, &pr.Claw.ShortcutStoryID, &pr.Claw.JiraIssueID, &pr.Claw.FactoryName,
			&pr.Claw.Model, &pr.Claw.BootstrapDiagnostic); err != nil {
			rows.Close()
			log.Printf("[slack] scan claw PR candidate: %v", err)
			return
		}
		pr.ClawCreatedAt = createdAt.Int64
		prs = append(prs, pr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Printf("[slack] select claw PR candidates: %v", err)
		return
	}

	cutoff := now().Add(-slackClawAdhocGrace).Unix()
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
		ev := slackEventRow{
			EventType:   taskRunEventPROpened,
			TargetURL:   pr.PRURL,
			TargetLabel: fmt.Sprintf("%s#%d", pr.Repo, pr.PRNumber),
		}
		runCtx := slackClawRunContext(pr.Claw)
		runCtx.Repo = pr.Repo
		if !s.deliverSlackClawEvent(client, cfg, pr.Claw, ev, runCtx, slackClawPRKey(pr.Claw.ID, pr.PRURL)) {
			break
		}
		maxHandled = pr.RowID
	}
	if maxHandled > watermark {
		s.setSlackStateInt64(slackStateClawPRWatermarkKey, maxHandled)
	}
}

// slackClawByID loads the claw context for the manual test endpoint.
func (s *Server) slackClawByID(clawID string) (slackClawRow, bool, error) {
	var claw slackClawRow
	err := scanSlackClawRow(s.db.QueryRow(`
		SELECT `+slackClawSelectColumns+`
		  FROM claws c WHERE c.id = ?`, clawID).Scan, &claw)
	if err == sql.ErrNoRows {
		return claw, false, nil
	}
	if err != nil {
		return claw, false, err
	}
	return claw, true, nil
}
