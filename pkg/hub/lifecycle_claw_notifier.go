package hub

import (
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"
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
// can never collide with task_run_events ids.
const (
	// lifecycleStateClawBaselineKey marks that the claw pass has recorded the
	// current claw/PR state as history. Set on the first enabled run so
	// enabling the feature (or deploying it) never replays pre-existing claws
	// or claw_prs rows — the same rule the task-run watermark follows.
	lifecycleStateClawBaselineKey = "claw_baseline_done"

	// lifecycleClawRouteBaselinePrefix namespaces one route's own baseline.
	// The claw pass has no cursor, so a route added to an existing config
	// would otherwise find every currently connected claw (and every open
	// claw_prs row) missing its delivery rows and replay the lot into the new
	// channel. Seeding the route's history on first sight is the per-route
	// analog of the task-run pass's first-run watermark.
	lifecycleClawRouteBaselinePrefix = lifecycleStateClawBaselineKey + ":"

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

// lifecycleClawRunKey keeps synthetic claw delivery rows distinct from
// task-run rows.
func lifecycleClawRunKey(clawID string) string { return "claw:" + clawID }

// lifecycleClawIdleKey keys one idle stretch of one ad-hoc claw. The latch
// value (claws.idle_since, the stretch's start in millis) is part of the key
// so a claw that goes idle, works, and goes idle again gets a fresh key — and
// therefore a fresh notification — per stretch, while re-scans of the same
// stretch dedupe on the delivery row.
func lifecycleClawIdleKey(clawID string, idleSince int64) string {
	return "claw:" + clawID + ":idle:" + strconv.FormatInt(idleSince, 10)
}

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
func lifecycleClawKindsEnabled(cfg *types.LifecycleNotificationsConfig) (agentStarted, prOpened, failures, agentIdle bool) {
	agentStarted, prOpened, failures, agentIdle = true, true, true, true
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
		if cfg.Events.AgentIdle != nil {
			agentIdle = *cfg.Events.AgentIdle
		}
	}
	return
}

func lifecycleClawRouteBaselineKey(notifier string) string {
	return lifecycleClawRouteBaselinePrefix + notifier
}

// lifecycleRouteAccepts reports whether a route's allow-list covers an event
// type. An empty allow-list means "every type".
func lifecycleRouteAccepts(route lifecycleRouteDelivery, eventType string) bool {
	return len(route.events) == 0 || route.events[eventType]
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
		for _, route := range d.lc.EffectiveRoutes() {
			// The shared baseline just recorded the current state for every
			// route, so none of them needs its own seeding pass. Stamp every
			// CONFIGURED route, not only the ones that built this tick: a route
			// whose notifier was unavailable here would otherwise arrive with no
			// baseline key and be seeded on recovery, burying exactly the claw
			// events the tick promises are "still pending for it".
			if via := strings.TrimSpace(route.Via); via != "" {
				s.setNotifierStateInt64(lifecycleClawRouteBaselineKey(via), 1)
			}
		}
		return
	}

	startedOn, prOn, failuresOn, idleOn := lifecycleClawKindsEnabled(d.lc)
	// Park disabled kinds so re-enabling a toggle behaves like a fresh enable
	// instead of flushing every transition from the muted window.
	if !startedOn {
		s.skipCurrentLifecycleClawState(lifecycleClawKindStarted)
	}
	if !failuresOn {
		s.skipCurrentLifecycleClawState(lifecycleClawKindFailure)
	}
	if !idleOn {
		s.skipCurrentLifecycleClawIdle()
	}
	if !prOn {
		s.skipCurrentLifecycleClawPRs()
	}

	// Every route selects and delivers on its own. The claw pass has no cursor,
	// so a claw stays a candidate until it has a delivery row — and a shared
	// candidate set means a route that cannot deliver (archived channel,
	// unresolvable token secret) keeps its claws in that set forever and, at
	// `ORDER BY created_at LIMIT 200`, eventually locks every newer claw out of
	// the batch for the HEALTHY routes too. Per-route selection confines that
	// debt to the route that owes it.
	for _, route := range d.effectiveRoutes() {
		if d.routePaused(route.notifier) {
			continue
		}
		if !s.ensureLifecycleClawRouteBaseline(d, route) {
			continue
		}
		s.lifecycleClawRoutePass(d, route, startedOn, prOn, failuresOn, idleOn)
	}
}

// lifecycleClawRoutePass runs the four claw-sourced kinds for one route. A kind
// the route's allow-list rejects is never scanned, so those claws cannot
// consume the route's batch either — but it IS parked, exactly as the global
// toggles park a kind they mute. The claw pass has no cursor, so without a
// delivery row every claw still connected (and every claw_prs row still open)
// since the route's baseline would be replayed into the channel the moment the
// operator adds that event type to the route's allow-list.
func (s *Server) lifecycleClawRoutePass(d lifecycleDelivery, route lifecycleRouteDelivery, startedOn, prOn, failuresOn, idleOn bool) {
	if startedOn {
		if !lifecycleRouteAccepts(route, taskRunEventAgentStarted) {
			_ = s.skipCurrentLifecycleClawRouteState(route.notifier, lifecycleClawKindStarted)
		} else if !s.sendLifecycleClawStateEvents(d, route, lifecycleClawKindStarted) {
			return
		}
	}
	if failuresOn {
		if !lifecycleRouteAccepts(route, taskRunEventAgentStopped) {
			_ = s.skipCurrentLifecycleClawRouteState(route.notifier, lifecycleClawKindFailure)
		} else if !s.sendLifecycleClawStateEvents(d, route, lifecycleClawKindFailure) {
			return
		}
	}
	if idleOn {
		if !lifecycleRouteAccepts(route, taskRunEventAgentIdle) {
			_ = s.skipCurrentLifecycleClawRouteIdle(route.notifier)
		} else if !s.sendLifecycleClawIdleEvents(d, route) {
			return
		}
	}
	if prOn {
		if !lifecycleRouteAccepts(route, taskRunEventPROpened) {
			_ = s.skipCurrentLifecycleClawRoutePRs(route.notifier)
		} else {
			s.lifecycleClawPRPass(d, route)
		}
	}
}

// ensureLifecycleClawRouteBaseline records a newly configured route's history
// and reports whether the route may deliver this tick. The legacy single-`via`
// shape needs no seeding: it keeps writing the shared legacy rows, which
// already fence every claw it has handled.
func (s *Server) ensureLifecycleClawRouteBaseline(d lifecycleDelivery, route lifecycleRouteDelivery) bool {
	key := lifecycleClawRouteBaselineKey(route.notifier)
	if d.singleRoute() {
		// Stamp the incumbent's baseline key anyway (best effort — an
		// unreadable state must never hold up the legacy shape's delivery).
		// Migrating this config to multi-route would otherwise present the
		// incumbent as a newly added route and seed its current claw state as
		// "skipped", destroying the backlog it never delivered — for example
		// the claws that reached 'connected' while its token secret was
		// missing. Its history lives in the legacy delivery table, so the key
		// is stamped WITHOUT seeding any rows.
		if route.notifier != "" {
			if _, found, err := s.notifierStateInt64(key); err == nil && !found {
				s.setNotifierStateInt64(key, 1)
			}
		}
		return true
	}
	_, found, err := s.notifierStateInt64(key)
	if err != nil {
		// Unreadable state must not be mistaken for "already baselined": that
		// would replay the current claw list into the channel.
		log.Printf("[notify] read claw baseline state for %q: %v", route.notifier, err)
		return false
	}
	if found {
		return true
	}
	if err := s.seedLifecycleClawRouteBaseline(route.notifier); err != nil {
		log.Printf("[notify] seed claw baseline for %q: %v", route.notifier, err)
		return false
	}
	s.setNotifierStateInt64(key, 1)
	return false
}

// seedLifecycleClawRouteBaseline records the current claw/PR state as already
// handled for one route, mirroring seedLifecycleClawBaseline into the per-route
// table.
func (s *Server) seedLifecycleClawRouteBaseline(notifier string) error {
	if err := s.skipCurrentLifecycleClawRouteState(notifier, lifecycleClawKindStarted); err != nil {
		return err
	}
	if err := s.skipCurrentLifecycleClawRouteState(notifier, lifecycleClawKindFailure); err != nil {
		return err
	}
	if err := s.skipCurrentLifecycleClawRouteIdle(notifier); err != nil {
		return err
	}
	return s.skipCurrentLifecycleClawRoutePRs(notifier)
}

// lifecycleClawAdhocCutoff is the created_at bound (unix seconds) every
// claw-pass statement shares: a claw younger than the ad-hoc grace is not yet
// classified as ad-hoc, so it must be neither selected nor fenced.
func lifecycleClawAdhocCutoff() int64 { return now().Add(-lifecycleClawAdhocGrace).Unix() }

// lifecycleClawRouteSkip records claw-pass events as already handled for ONE
// route — the per-route analog of the skipCurrent* helpers, which write the
// shared legacy rows that fence every route at once. Two callers share it: the
// baseline of a newly added route, and the per-tick parking of a kind the
// route's allow-list rejects.
func (s *Server) lifecycleClawRouteSkip(notifier, what, selectSQL string, args ...any) error {
	_, err := s.db.Exec(`
		INSERT INTO slack_notification_deliveries_v2(event_id, notifier, run_id, delivered_at, message_ts, status)`+
		selectSQL+`
		ON CONFLICT(event_id, notifier) DO NOTHING`, args...)
	if err != nil {
		log.Printf("[notify] seed skipped claw %s deliveries for %q: %v", what, notifier, err)
	}
	return err
}

func (s *Server) skipCurrentLifecycleClawRouteState(notifier, kind string) error {
	cond, suffix := lifecycleClawStateCondition(kind)
	return s.lifecycleClawRouteSkip(notifier, kind, `
		SELECT 'claw:' || c.id || ?, ?, 'claw:' || c.id, ?, '', ?
		  FROM claws c
		 WHERE c.task_run_id = '' AND `+cond+`
		   AND CAST(strftime('%s', c.created_at) AS INTEGER) <= ?`,
		suffix, notifier, epochMillis(now()), notificationDeliveryStatusSkipped, lifecycleClawAdhocCutoff())
}

func (s *Server) skipCurrentLifecycleClawRouteIdle(notifier string) error {
	return s.lifecycleClawRouteSkip(notifier, "idle", `
		SELECT 'claw:' || c.id || ':idle:' || c.idle_since, ?, 'claw:' || c.id, ?, '', ?
		  FROM claws c
		 WHERE c.task_run_id = '' AND c.idle_since > 0
		   AND CAST(strftime('%s', c.created_at) AS INTEGER) <= ?`,
		notifier, epochMillis(now()), notificationDeliveryStatusSkipped, lifecycleClawAdhocCutoff())
}

func (s *Server) skipCurrentLifecycleClawRoutePRs(notifier string) error {
	return s.lifecycleClawRouteSkip(notifier, "PR", `
		SELECT 'claw:' || p.claw_id || ':pr:' || p.pr_url, ?, 'claw:' || p.claw_id, ?, '', ?
		  FROM claw_prs p JOIN claws c ON c.id = p.claw_id
		 WHERE CAST(strftime('%s', c.created_at) AS INTEGER) <= ?`,
		notifier, epochMillis(now()), notificationDeliveryStatusSkipped, lifecycleClawAdhocCutoff())
}

// seedLifecycleClawBaseline records the current claw/PR state as already handled.
func (s *Server) seedLifecycleClawBaseline() error {
	if err := s.skipCurrentLifecycleClawState(lifecycleClawKindStarted); err != nil {
		return err
	}
	if err := s.skipCurrentLifecycleClawState(lifecycleClawKindFailure); err != nil {
		return err
	}
	if err := s.skipCurrentLifecycleClawIdle(); err != nil {
		return err
	}
	return s.skipCurrentLifecycleClawPRs()
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
	_ = s.skipCurrentLifecycleClawIdle()
	_ = s.skipCurrentLifecycleClawPRs()
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

// skipCurrentLifecycleClawPRs seeds "skipped" delivery rows for every claw_prs
// row that exists right now, suppressing notifications for PRs that predate
// the baseline or arrived while pr_opened was muted. The PR pass deliberately
// has NO rowid watermark: claw_prs rows are routinely deleted (PR closed
// without merge, terminal pipeline stages, claw teardown) and the table has no
// AUTOINCREMENT, so SQLite reuses freed rowids — a max-rowid cursor would
// permanently skip whichever PR next reused a rowid at or below it. The
// delivery rows keyed by (claw, pr_url) are the only dedupe. Idempotent
// (ON CONFLICT DO NOTHING), so delivered PRs keep their "sent" rows.
func (s *Server) skipCurrentLifecycleClawPRs() error {
	// The WHERE clause is load-bearing: without it SQLite parses ON CONFLICT
	// as the start of a join clause and rejects the statement.
	_, err := s.db.Exec(`
		INSERT INTO slack_notification_deliveries(event_id, run_id, delivered_at, message_ts, status)
		SELECT 'claw:' || p.claw_id || ':pr:' || p.pr_url, 'claw:' || p.claw_id, ?, '', ?
		  FROM claw_prs p
		 WHERE true
		ON CONFLICT(event_id) DO NOTHING`,
		epochMillis(now()), notificationDeliveryStatusSkipped)
	if err != nil {
		log.Printf("[notify] seed skipped claw PR deliveries: %v", err)
	}
	return err
}

// skipCurrentLifecycleClawIdle seeds "skipped" delivery rows for every ad-hoc
// claw currently latched idle (claws.idle_since set by the status watchdog),
// so muting agent_idle — or the whole feature — parks the stretch instead of
// replaying it on re-enable. Idempotent per (claw, idle_since) key.
func (s *Server) skipCurrentLifecycleClawIdle() error {
	_, err := s.db.Exec(`
		INSERT INTO slack_notification_deliveries(event_id, run_id, delivered_at, message_ts, status)
		SELECT 'claw:' || c.id || ':idle:' || c.idle_since, 'claw:' || c.id, ?, '', ?
		  FROM claws c
		 WHERE c.task_run_id = '' AND c.idle_since > 0
		ON CONFLICT(event_id) DO NOTHING`,
		epochMillis(now()), notificationDeliveryStatusSkipped)
	if err != nil {
		log.Printf("[notify] seed skipped claw idle deliveries: %v", err)
	}
	return err
}

// selectLifecycleClawIdleCandidates returns ad-hoc claws whose idle latch has
// no delivery row yet for this route — the shared legacy row (baseline,
// parking, or a legacy single-`via` send) fences every route, the v2 row only
// its own. The exclusion conditions mirror agentIdleEligible for
// the ad-hoc kind — status 'connected', no unresolved DELIVERED tracked PR,
// still driven by a pipeline stage or a live workflow run — so a claw that
// opened a PR, died, or lost its automatic driver between latch and delivery
// is not alerted on stale grounds. Mention-only rows are excluded, matching
// clawOpenPRCount and agentIdleHasClawPRs: a PR the agent merely linked is
// not "PR out, awaiting humans", and suppressing the idle notification on it
// would hide a hung claw the watcher will also never finalize.
func (s *Server) selectLifecycleClawIdleCandidates(notifier string) ([]lifecycleClawRow, []int64, error) {
	cutoff := lifecycleClawAdhocCutoff()
	rows, err := s.db.Query(`
		SELECT `+lifecycleClawSelectColumns+`, c.idle_since
		  FROM claws c
		 WHERE c.task_run_id = '' AND c.idle_since > 0 AND c.status = 'connected'
		   AND NOT EXISTS (SELECT 1 FROM claw_prs p WHERE p.claw_id = c.id AND p.state NOT IN ('merged','closed') AND p.mention_only = 0)
		   AND (c.pipeline_stage != '' OR EXISTS (
			SELECT 1 FROM workflow_runs w WHERE w.claw_id = c.id AND w.status IN ('pending','running')
		   ))
		   AND CAST(strftime('%s', c.created_at) AS INTEGER) <= ?
		   AND NOT EXISTS (
			SELECT 1 FROM slack_notification_deliveries d
			 WHERE d.event_id = 'claw:' || c.id || ':idle:' || c.idle_since
		   )
		   AND NOT EXISTS (
			SELECT 1 FROM slack_notification_deliveries_v2 v
			 WHERE v.event_id = 'claw:' || c.id || ':idle:' || c.idle_since AND v.notifier = ?
		   )
		 ORDER BY c.created_at
		 LIMIT `+strconv.Itoa(lifecycleBatchSize), cutoff, notifier)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var claws []lifecycleClawRow
	var idleSinces []int64
	for rows.Next() {
		var claw lifecycleClawRow
		var idleSince int64
		scan := func(dest ...any) error { return rows.Scan(append(dest, &idleSince)...) }
		if err := scanLifecycleClawRow(scan, &claw); err != nil {
			return nil, nil, err
		}
		claws = append(claws, claw)
		idleSinces = append(idleSinces, idleSince)
	}
	return claws, idleSinces, rows.Err()
}

// sendLifecycleClawIdleEvents delivers the idle kind for ad-hoc claws. Returns
// false when the claw pass must stop for this tick.
func (s *Server) sendLifecycleClawIdleEvents(d lifecycleDelivery, route lifecycleRouteDelivery) bool {
	claws, idleSinces, err := s.selectLifecycleClawIdleCandidates(route.notifier)
	if err != nil {
		log.Printf("[notify] select claw idle candidates: %v", err)
		return false
	}
	for i, claw := range claws {
		idleSince := idleSinces[i]
		detail := map[string]any{"idleSince": idleSince}
		if minutes := int(now().Sub(time.UnixMilli(idleSince)).Minutes()); minutes > 0 {
			detail["idleMinutes"] = minutes
		}
		ev := lifecycleEventRow{EventType: taskRunEventAgentIdle, Detail: detail}
		if !s.deliverLifecycleClawEvent(d, route, claw, ev, lifecycleClawRunContext(claw), lifecycleClawIdleKey(claw.ID, idleSince)) {
			return false
		}
	}
	return true
}

// lifecycleClawStateCondition returns the SQL state condition and the delivery-key
// suffix for a claw-pass state kind.
func lifecycleClawStateCondition(kind string) (cond, keySuffix string) {
	// Two terminal failure shapes exist in the hub:
	//   - status='error' (stopAgentWithReason → finishClawTerminalTx), and
	//   - status='deleted' with the claw's latest workflow run 'failed' —
	//     the pipeline-terminal and factory-trigger failure paths call
	//     finishClawTerminalTx(claw, "deleted", "", "failed", ...).
	// The workflow-run verdict keys the second shape because success paths
	// also end at status='deleted' (with runs 'completed'/'canceled'), so
	// the claw status alone cannot distinguish failure from success there.
	// A missing workflow run yields NULL, which never equals 'failed'.
	const failureCond = `(c.status = 'error' OR (c.status = 'deleted' AND (
		SELECT w.status FROM workflow_runs w
		 WHERE w.claw_id = c.id
		 ORDER BY w.created_at DESC, w.rowid DESC LIMIT 1
	) = 'failed'))`
	if kind == lifecycleClawKindFailure {
		return failureCond, ":failure"
	}
	// Mirror allowWakeBeforeBootstrap: only the VM providers gate readiness on
	// bootstrap_ok — docker/local/noop claws are upserted straight to
	// 'connected' by bridge registration with bootstrap_ok still 0, and
	// requiring bootstrap_ok=1 for them would mean ad-hoc claws (the very case
	// this pass exists for) never produce an agent_started notification.
	const bootstrapGate = `(c.bootstrap_ok = 1
		OR c.provider NOT IN ('daytona', 'replicated', 'exedev'))`
	// The state pass samples CURRENT state, so a claw whose whole connected
	// life fits inside the ad-hoc grace would never match "connected" once it
	// becomes eligible — its agent_started would be lost and only the failure
	// would fire. The second branch recovers it: a claw that reached a
	// terminal failure but demonstrably came up (last_seen is written by
	// bridge registration only, and the bootstrap gate still applies) did
	// start, so both notifications are delivered — started first, threading
	// the failure under it, in the same tick.
	return `((c.status = 'connected' OR (c.last_seen IS NOT NULL AND ` + failureCond + `))
		AND ` + bootstrapGate + `)`, ":agent_started"
}

// selectLifecycleClawStateCandidates returns ad-hoc claws currently in the kind's
// state that this route has no delivery row for. The dedupe happens in SQL (like
// the task-run pass) so already-handled claws never consume the LIMIT — and it
// is per route, so a route that cannot deliver keeps its debt to itself instead
// of holding every other route's claws in a shared, oldest-first batch.
func (s *Server) selectLifecycleClawStateCandidates(kind, notifier string) ([]lifecycleClawRow, error) {
	cond, suffix := lifecycleClawStateCondition(kind)
	cutoff := lifecycleClawAdhocCutoff()
	rows, err := s.db.Query(`
		SELECT `+lifecycleClawSelectColumns+`
		  FROM claws c
		 WHERE c.task_run_id = '' AND `+cond+`
		   AND CAST(strftime('%s', c.created_at) AS INTEGER) <= ?
		   AND NOT EXISTS (
			SELECT 1 FROM slack_notification_deliveries d WHERE d.event_id = 'claw:' || c.id || ?
		   )
		   AND NOT EXISTS (
			SELECT 1 FROM slack_notification_deliveries_v2 v
			 WHERE v.event_id = 'claw:' || c.id || ? AND v.notifier = ?
		   )
		 ORDER BY c.created_at
		 LIMIT `+strconv.Itoa(lifecycleBatchSize), cutoff, suffix, suffix, notifier)
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
func (s *Server) sendLifecycleClawStateEvents(d lifecycleDelivery, route lifecycleRouteDelivery, kind string) bool {
	claws, err := s.selectLifecycleClawStateCandidates(kind, route.notifier)
	if err != nil {
		log.Printf("[notify] select claw %s candidates: %v", kind, err)
		return false
	}
	for _, claw := range claws {
		ev, deliveryKey := lifecycleClawStateEvent(kind, claw)
		if !s.deliverLifecycleClawEvent(d, route, claw, ev, lifecycleClawRunContext(claw), deliveryKey) {
			return false
		}
	}
	return true
}

// deliverLifecycleClawEvent posts one claw-sourced event through one route with
// the shared error policy: config errors park the route, permanent errors are
// recorded as failed (and count as handled), transient errors stop this route's
// pass for the tick. Returns true when the event is handled (sent, permanently
// failed, or ceded to the task-run path) and the caller may move past it.
func (s *Server) deliverLifecycleClawEvent(d lifecycleDelivery, route lifecycleRouteDelivery, claw lifecycleClawRow, ev lifecycleEventRow, runCtx lifecycleRunContext, deliveryKey string) bool {
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

	runKey := lifecycleClawRunKey(claw.ID)
	err := s.postLifecycleEventRoute(d, route, buildLifecycleMessage(ev, runCtx), runKey, deliveryKey)
	if err == nil {
		return true
	}
	// Same send-failure policy as the task-run pass: config errors park the
	// route, permanent errors are burned as failed, transient errors park the
	// route until the next tick. Parking one destination instead of the whole
	// pass is what keeps a single broken channel from muting the healthy ones;
	// the claw keeps no delivery row for the parked route, so it is re-selected
	// and retried later — by that route's own pass only.
	if handled, _ := s.handleLifecycleSendError(err, "claw event "+deliveryKey, deliveryKey, runKey, route.notifier, d.singleRoute()); handled {
		return true
	}
	d.pauseRoute(route.notifier)
	return false
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

// lifecycleClawPRPass scans claw_prs for undelivered PRs opened by ad-hoc
// claws. There is intentionally no rowid cursor (claw_prs rowids are reused
// after deletes — see skipCurrentLifecycleClawPRs); the delivery-row anti-join
// alone decides what is new, so every handled row must end up with a delivery
// record or it would be re-selected forever and consume the LIMIT.
func (s *Server) lifecycleClawPRPass(d lifecycleDelivery, route lifecycleRouteDelivery) {
	rows, err := s.db.Query(`
		SELECT p.rowid, p.repo, p.pr_number, p.pr_url, c.task_run_id,
		       CAST(strftime('%s', c.created_at) AS INTEGER),
		       `+lifecycleClawSelectColumns+`
		  FROM claw_prs p JOIN claws c ON c.id = p.claw_id
		 WHERE NOT EXISTS (
			SELECT 1 FROM slack_notification_deliveries d
			 WHERE d.event_id = 'claw:' || p.claw_id || ':pr:' || p.pr_url
		   )
		   AND NOT EXISTS (
			SELECT 1 FROM slack_notification_deliveries_v2 v
			 WHERE v.event_id = 'claw:' || p.claw_id || ':pr:' || p.pr_url AND v.notifier = ?
		   )
		 ORDER BY p.rowid
		 LIMIT `+strconv.Itoa(lifecycleBatchSize), route.notifier)
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

	cutoff := lifecycleClawAdhocCutoff()
	for _, pr := range prs {
		if pr.TaskRunID != "" {
			// Owned by the task-run pass. Record a skipped row under this
			// pass's key so the anti-join stops re-selecting it (there is no
			// cursor to move past it); the task-run pass dedupes under its
			// own event ids, so the keys never collide.
			s.recordNotificationDelivery(lifecycleClawPRKey(pr.Claw.ID, pr.PRURL),
				lifecycleClawRunKey(pr.Claw.ID), "", notificationDeliveryStatusSkipped)
			continue
		}
		if pr.ClawCreatedAt > cutoff {
			// The claw is too young to classify as ad-hoc (its task_run_id may
			// still be on the way). Leave the row undelivered; it is
			// re-selected once the grace expires.
			continue
		}
		ev := lifecycleEventRow{
			EventType:   taskRunEventPROpened,
			TargetURL:   pr.PRURL,
			TargetLabel: fmt.Sprintf("%s#%d", pr.Repo, pr.PRNumber),
		}
		runCtx := lifecycleClawRunContext(pr.Claw)
		runCtx.Repo = pr.Repo
		if !s.deliverLifecycleClawEvent(d, route, pr.Claw, ev, runCtx, lifecycleClawPRKey(pr.Claw.ID, pr.PRURL)) {
			break
		}
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
