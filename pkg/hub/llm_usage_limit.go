package hub

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// Provider usage limits (see types.ParseLLMUsageLimit for how one is
// recognised).
//
// A capped account is the one failure mode the hub can wait out by itself. It
// is not broken — nothing needs fixing, nothing needs a human — and the
// provider even names the instant it lifts. What it needs is the opposite of
// the bridge-error response: do not retry, do not nag the agent, do not ask an
// operator to go look, and come back exactly once, when the wall is gone.
//
// Two things follow from that, and they are the whole design:
//
// The latch is per KEY, not per claw. On 2026-08-31 four Faster claws stopped
// within seconds of each other because they share one Anthropic key. A
// per-claw latch would have meant four separate discoveries, four
// notifications, and three claws still spending turns against a wall the hub
// had already found.
//
// The latch is NOT no_progress_paused. That one is deliberately lifted by any
// user message (resumeNoProgressAfterUserInput), which is right when a human
// is unblocking a stuck agent and wrong here: the account is still capped, so
// the message only buys another failed turn and another hour. This latch
// outlives user input. It is cleared by the clock, or by an operator who says
// so explicitly.
const (
	// llmLimitFallbackRetry is how long to wait when the provider reports a
	// limit but names no deadline (an exhausted prepaid balance, for
	// instance). Retrying costs one turn and re-latches cleanly if the wall is
	// still there, so this is tuned for "notice recovery reasonably soon"
	// rather than for saving requests.
	llmLimitFallbackRetry = time.Hour

	// llmLimitRelatchBackoff is added when the very first turn after a release
	// hits the same limit again. Clock skew and a provider whose reset lands a
	// little late both look like this, and both are fixed by waiting a bit
	// more rather than by hammering.
	llmLimitRelatchBackoff = 15 * time.Minute

	// llmLimitMaxAutoRetries caps the release/re-latch cycle. Past this the
	// deadline the provider gave is demonstrably not the whole story — the cap
	// was never raised, the balance was never topped up — and continuing to
	// retry on a timer only hides that from the operator. Stop, and leave it
	// for the human who can actually change the account.
	llmLimitMaxAutoRetries = 3

	// llmLimitScheduleInterval is the release poll. A one-minute granularity
	// on an hours-long wait is imperceptible, and it matches the other hub
	// loops rather than inventing a wake-at-instant timer that a hub restart
	// would drop.
	llmLimitScheduleInterval = time.Minute

	// llmLimitEpisodeMemory is how long a released limit is remembered. It is
	// what makes the backoff climb: a cap that lifts and is hit again ten
	// minutes later is the SAME episode, and forgetting that immediately would
	// reset the retry counter and loop at the base delay forever. Long enough
	// to cover a provider's reset window, short enough that next month's limit
	// starts from a clean slate.
	llmLimitEpisodeMemory = 6 * time.Hour
)

// llmUsageLimitRecord is one capped key.
type llmUsageLimitRecord struct {
	KeyID          string
	Provider       string
	Reason         string
	Message        string
	RegainAt       time.Time // what the provider said; zero if it said nothing
	RetryAt        time.Time // when the hub will actually try again
	Retries        int
	DetectedAt     time.Time
	DetectedClawID string
	ReleasedAt     time.Time // zero while the limit is holding claws back
}

// Active reports whether this limit is currently parking claws.
func (r llmUsageLimitRecord) Active() bool { return r.ReleasedAt.IsZero() }

// Exhausted reports whether the automatic release cycle has given up.
func (r llmUsageLimitRecord) Exhausted() bool { return r.Retries >= llmLimitMaxAutoRetries }

// noteLLMUsageLimit records a provider limit discovered on one claw and parks
// every claw sharing its key until the limit lifts.
//
// It reports whether this call opened a NEW episode. A limit that is already
// latched returns false so a second claw hitting the same wall does not
// re-notify: the operator has one problem, not four.
func (s *Server) noteLLMUsageLimit(clawID string, limit types.LLMUsageLimit) bool {
	keyID, provider := s.resolveClawLLMKey(clawID)
	nowAt := now()

	retryAt := limit.RegainAt
	if retryAt.IsZero() {
		retryAt = nowAt.Add(llmLimitFallbackRetry)
	}

	s.llmLimitMu.Lock()
	existing, had := s.loadLLMUsageLimit(keyID)
	if had && existing.Active() {
		// Same episode, second claw. Keep the original detection and retry
		// schedule — in particular do NOT let a re-report reset a backoff that
		// a failed release already earned.
		s.llmLimitMu.Unlock()
		s.latchClawsForLLMLimit(keyID, existing)
		return false
	}
	record := llmUsageLimitRecord{
		KeyID:          keyID,
		Provider:       provider,
		Reason:         limit.Reason,
		Message:        limit.Message,
		RegainAt:       limit.RegainAt,
		RetryAt:        retryAt,
		DetectedAt:     nowAt,
		DetectedClawID: clawID,
	}
	if err := s.storeLLMUsageLimit(record); err != nil {
		s.llmLimitMu.Unlock()
		log.Printf("[llm-limit] store limit for key %q: %v", keyID, err)
		return false
	}
	s.llmLimitMu.Unlock()

	latched := s.latchClawsForLLMLimit(keyID, record)
	log.Printf("[llm-limit] key %q capped (%s), %d claw(s) parked until %s: %s",
		keyID, record.Reason, len(latched), formatLLMLimitDeadline(record.RetryAt), record.Message)
	s.recordLLMLimitEvent(clawID, record, true)
	return true
}

// latchClawsForLLMLimit parks every claw on the key and returns their IDs.
func (s *Server) latchClawsForLLMLimit(keyID string, record llmUsageLimitRecord) []string {
	clawIDs := s.clawsForLLMKey(keyID)
	if len(clawIDs) == 0 {
		return nil
	}
	until := epochMillis(record.RetryAt)
	notice := llmLimitNotice(record)
	for _, clawID := range clawIDs {
		res, err := s.db.Exec(`UPDATE claws SET llm_limited_until=? WHERE id=? AND COALESCE(llm_limited_until,0)<>?`, until, clawID, until)
		if err != nil {
			log.Printf("[llm-limit] latch claw %s: %v", shortID(clawID), err)
			continue
		}
		s.mu.RLock()
		cc := s.claws[clawID]
		s.mu.RUnlock()
		if cc != nil {
			cc.mu.Lock()
			cc.llmLimitedUntil = record.RetryAt
			cc.llmLimitNoticedAt = time.Time{}
			cc.mu.Unlock()
		}
		if changed, _ := res.RowsAffected(); changed > 0 {
			s.publishHubNotice(clawID, notice)
		}
	}
	return clawIDs
}

// releaseLLMUsageLimit lifts the latch for a key.
//
// reason is logged and, when announce is set, told to each claw's operator.
// The claws are woken through the ordinary delivery path so a message that
// queued during the block goes out first, exactly as it would have without
// the block.
func (s *Server) releaseLLMUsageLimit(keyID, reason string, announce bool) {
	s.llmLimitMu.Lock()
	if _, err := s.db.Exec(`UPDATE llm_usage_limits SET released_at=? WHERE key_id=?`, epochMillis(now()), keyID); err != nil {
		s.llmLimitMu.Unlock()
		log.Printf("[llm-limit] clear limit for key %q: %v", keyID, err)
		return
	}
	s.llmLimitMu.Unlock()

	clawIDs := s.clawsForLLMKey(keyID)
	for _, clawID := range clawIDs {
		if _, err := s.db.Exec(`UPDATE claws SET llm_limited_until=0 WHERE id=?`, clawID); err != nil {
			log.Printf("[llm-limit] release claw %s: %v", shortID(clawID), err)
			continue
		}
		s.mu.RLock()
		cc := s.claws[clawID]
		s.mu.RUnlock()
		if cc != nil {
			cc.mu.Lock()
			cc.llmLimitedUntil = time.Time{}
			cc.llmLimitNoticedAt = time.Time{}
			cc.mu.Unlock()
		}
		if announce {
			s.publishHubNotice(clawID, "[hub] "+reason+" Automatic continuation resumed.")
		}
		if cc != nil {
			s.resumeClawAfterLLMLimit(cc, clawID)
		}
	}
	log.Printf("[llm-limit] key %q released (%s), %d claw(s) resumed", keyID, reason, len(clawIDs))
}

// resumeClawAfterLLMLimit restarts a parked claw.
//
// A queued message goes first: it is what the human asked for, and delivering
// it also ends the turn-less stretch. With nothing queued the claw needs a
// push of its own — the block swallowed the turn it was going to take, and
// nothing else will offer another.
func (s *Server) resumeClawAfterLLMLimit(cc *clawConn, clawID string) {
	if cc == nil || cc.conn == nil {
		// No live socket to push into. The claw is between connections, and
		// the ordinary post-registration drain delivers anything queued; a
		// write here would only reserve a turn and then abort it.
		return
	}
	var pending int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND delivered_at IS NULL`, clawID).Scan(&pending); err != nil {
		log.Printf("[llm-limit] count pending for claw %s: %v", shortID(clawID), err)
		return
	}
	if pending > 0 {
		s.sendNextQueuedMessage(cc)
		return
	}
	s.sendWakeMessage(cc, clawID)
}

// startLLMUsageLimitScheduler releases limits whose deadline has passed.
func (s *Server) startLLMUsageLimitScheduler() {
	go func() {
		ticker := time.NewTicker(llmLimitScheduleInterval)
		defer ticker.Stop()
		for range ticker.C {
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[llm-limit] scheduler tick panic: %v", r)
					}
				}()
				s.releaseDueLLMUsageLimits()
			}()
		}
	}()
}

func (s *Server) releaseDueLLMUsageLimits() {
	nowAt := now()
	for _, record := range s.llmUsageLimitRecords() {
		if !record.Active() {
			if nowAt.Sub(record.ReleasedAt) > llmLimitEpisodeMemory {
				if _, err := s.db.Exec(`DELETE FROM llm_usage_limits WHERE key_id=? AND released_at<>0`, record.KeyID); err != nil {
					log.Printf("[llm-limit] prune released limit for key %q: %v", record.KeyID, err)
				}
			}
			continue
		}
		// Exhausted limits stay latched on purpose: the block is real, the hub
		// just stopped guessing at when it ends. Only a human clears these.
		if record.Exhausted() || nowAt.Before(record.RetryAt) {
			continue
		}
		s.releaseLLMUsageLimit(record.KeyID, llmLimitReleaseReason(record), true)
	}
}

// handleLLMUsageLimit routes one provider-limit turn.
//
// A limit deliberately does NOT touch the bridge-error streak — it clears it.
// The transport carried the rejection perfectly; counting it as a transport
// failure would pause the claw with a notice blaming the sandbox, which is the
// exact wrong thing to hand an operator whose real problem is a billing cap.
func (s *Server) handleLLMUsageLimit(cc *clawConn, clawID string, limit types.LLMUsageLimit) {
	if cc != nil {
		cc.mu.Lock()
		cc.bridgeErrorStreak = 0
		cc.mu.Unlock()
	}
	keyID, _ := s.resolveClawLLMKey(clawID)
	if record, had := s.loadLLMUsageLimit(keyID); had && !record.Active() {
		// We released this key and the wall is still up.
		s.relatchLLMUsageLimit(clawID, limit)
		return
	}
	s.noteLLMUsageLimit(clawID, limit)
}

// relatchLLMUsageLimit is the answer to "we released, and the wall is still
// there": back off and try once more, up to the cap.
func (s *Server) relatchLLMUsageLimit(clawID string, limit types.LLMUsageLimit) {
	keyID, provider := s.resolveClawLLMKey(clawID)
	s.llmLimitMu.Lock()
	record, had := s.loadLLMUsageLimit(keyID)
	if !had {
		s.llmLimitMu.Unlock()
		s.noteLLMUsageLimit(clawID, limit)
		return
	}
	record.ReleasedAt = time.Time{}
	record.Retries++
	record.Message = limit.Message
	record.Provider = provider
	record.RegainAt = limit.RegainAt
	base := limit.RegainAt
	if base.IsZero() || !base.After(now()) {
		base = now()
	}
	record.RetryAt = base.Add(time.Duration(record.Retries) * llmLimitRelatchBackoff)
	if err := s.storeLLMUsageLimit(record); err != nil {
		s.llmLimitMu.Unlock()
		log.Printf("[llm-limit] re-latch key %q: %v", keyID, err)
		return
	}
	s.llmLimitMu.Unlock()

	s.latchClawsForLLMLimit(keyID, record)
	if record.Exhausted() {
		log.Printf("[llm-limit] key %q still capped after %d automatic retries, leaving it for an operator: %s",
			keyID, record.Retries, record.Message)
		for _, id := range s.clawsForLLMKey(keyID) {
			s.publishHubNotice(id, fmt.Sprintf("[hub] The provider still reports a usage limit after %d automatic retries, so the hub stopped retrying. Raise the account limit and clear the block to resume. Last provider message: %s", record.Retries, record.Message))
		}
		s.recordLLMLimitEvent(clawID, record, false)
		return
	}
	log.Printf("[llm-limit] key %q still capped, retry %d scheduled for %s",
		keyID, record.Retries, formatLLMLimitDeadline(record.RetryAt))
}

// clawLLMLimitBlock reports the deadline a claw is parked until, if it is.
func (s *Server) clawLLMLimitBlock(clawID string) (time.Time, bool) {
	var until int64
	if err := s.db.QueryRow(`SELECT COALESCE(llm_limited_until,0) FROM claws WHERE id=?`, clawID).Scan(&until); err != nil || until == 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(until).UTC(), true
}

// noticeLLMLimitToUser answers a human who wrote to a parked claw.
//
// This is the "validate before continuing" half of the block: the message is
// still persisted and still queued, so nothing is lost, but the person is told
// now — rather than an hour later via a failed turn — that it will not be read
// until the account has allowance again. Once per episode, so a conversation
// does not turn into a wall of identical notices.
func (s *Server) noticeLLMLimitToUser(clawID string) bool {
	until, limited := s.clawLLMLimitBlock(clawID)
	if !limited {
		return false
	}
	s.mu.RLock()
	cc := s.claws[clawID]
	s.mu.RUnlock()
	if cc != nil {
		cc.mu.Lock()
		already := cc.llmLimitNoticedAt.Equal(until)
		cc.llmLimitNoticedAt = until
		cc.mu.Unlock()
		if already {
			return true
		}
	}
	s.publishHubNotice(clawID, fmt.Sprintf("[hub] This agent's model provider is out of allowance, so your message was queued instead of delivered. It goes out automatically when access returns on %s.", formatLLMLimitDeadline(until)))
	return true
}

// llmUsageLimitRecords returns every active limit.
func (s *Server) llmUsageLimitRecords() []llmUsageLimitRecord {
	rows, err := s.db.Query(`SELECT key_id, provider, reason, message, regain_at, retry_at, retries, detected_at, detected_claw_id, released_at FROM llm_usage_limits`)
	if err != nil {
		log.Printf("[llm-limit] list limits: %v", err)
		return nil
	}
	defer rows.Close()
	var out []llmUsageLimitRecord
	for rows.Next() {
		record, err := scanLLMUsageLimit(rows)
		if err != nil {
			log.Printf("[llm-limit] scan limit: %v", err)
			continue
		}
		out = append(out, record)
	}
	return out
}

func (s *Server) loadLLMUsageLimit(keyID string) (llmUsageLimitRecord, bool) {
	row := s.db.QueryRow(`SELECT key_id, provider, reason, message, regain_at, retry_at, retries, detected_at, detected_claw_id, released_at FROM llm_usage_limits WHERE key_id=?`, keyID)
	record, err := scanLLMUsageLimit(row)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Printf("[llm-limit] load limit for key %q: %v", keyID, err)
		}
		return llmUsageLimitRecord{}, false
	}
	return record, true
}

func (s *Server) storeLLMUsageLimit(record llmUsageLimitRecord) error {
	_, err := s.db.Exec(`INSERT INTO llm_usage_limits(key_id, provider, reason, message, regain_at, retry_at, retries, detected_at, detected_claw_id, released_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(key_id) DO UPDATE SET provider=excluded.provider, reason=excluded.reason, message=excluded.message,
			regain_at=excluded.regain_at, retry_at=excluded.retry_at, retries=excluded.retries,
			detected_at=excluded.detected_at, detected_claw_id=excluded.detected_claw_id, released_at=excluded.released_at`,
		record.KeyID, record.Provider, record.Reason, record.Message,
		epochMillisOrZero(record.RegainAt), epochMillisOrZero(record.RetryAt), record.Retries,
		epochMillisOrZero(record.DetectedAt), record.DetectedClawID, epochMillisOrZero(record.ReleasedAt))
	return err
}

type rowScanner interface{ Scan(...any) error }

func scanLLMUsageLimit(row rowScanner) (llmUsageLimitRecord, error) {
	var (
		record                                  llmUsageLimitRecord
		regainAt, retryAt, detectedAt, released int64
		provider, reason, message, clawID       string
	)
	if err := row.Scan(&record.KeyID, &provider, &reason, &message, &regainAt, &retryAt, &record.Retries, &detectedAt, &clawID, &released); err != nil {
		return llmUsageLimitRecord{}, err
	}
	record.ReleasedAt = timeFromEpochMillis(released)
	record.Provider, record.Reason, record.Message, record.DetectedClawID = provider, reason, message, clawID
	record.RegainAt = timeFromEpochMillis(regainAt)
	record.RetryAt = timeFromEpochMillis(retryAt)
	record.DetectedAt = timeFromEpochMillis(detectedAt)
	return record, nil
}

func timeFromEpochMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// clawsForLLMKey lists the claws that would hit the same wall.
//
// Deleted and errored claws are excluded — parking a claw nobody will run
// again only adds noise to the dashboard.
func (s *Server) clawsForLLMKey(keyID string) []string {
	rows, err := s.db.Query(`SELECT id FROM claws WHERE COALESCE(llm_key,'')=? AND status NOT IN ('deleted','error')`, keyID)
	if err != nil {
		log.Printf("[llm-limit] list claws for key %q: %v", keyID, err)
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		out = append(out, id)
	}
	return out
}

// resolveClawLLMKey maps a claw to the key it spends and that key's provider.
//
// The key name is the identity — the empty name is the hub's default key and
// is a perfectly good identity too, which is why it is stored as-is rather
// than folded into a placeholder that a real key could collide with.
func (s *Server) resolveClawLLMKey(clawID string) (keyID, provider string) {
	var defaultModel string
	_ = s.db.QueryRow(`SELECT COALESCE(llm_key,''), COALESCE(default_model,'') FROM claws WHERE id=?`, clawID).Scan(&keyID, &defaultModel)

	s.mu.RLock()
	cfg := s.hubCfg
	s.mu.RUnlock()
	if cfg != nil {
		for _, key := range cfg.LLMKeys {
			if key == nil {
				continue
			}
			if (keyID != "" && key.Name == keyID) || (keyID == "" && key.Default) {
				provider = strings.TrimSpace(key.Provider)
				break
			}
		}
	}
	if provider == "" {
		provider = providerFromModel(defaultModel)
	}
	if provider == "" && cfg != nil {
		provider = providerFromModel(cfg.DefaultModel)
	}
	return keyID, provider
}

func llmLimitNotice(record llmUsageLimitRecord) string {
	return fmt.Sprintf("[hub] Automatic continuation paused: the model provider reports this account is out of allowance, so turns are not reaching the agent. Nothing here is broken — the hub resumes on its own at %s. Provider message: %s",
		formatLLMLimitDeadline(record.RetryAt), record.Message)
}

func llmLimitReleaseReason(record llmUsageLimitRecord) string {
	if record.Retries > 0 {
		return fmt.Sprintf("Provider allowance retry %d is due.", record.Retries+1)
	}
	return "The provider's usage limit window has ended."
}

// formatLLMLimitDeadline renders an instant the way the provider stated it:
// UTC, to the minute. Anything else invites the operator to compare it against
// the wrong clock.
func formatLLMLimitDeadline(at time.Time) string {
	if at.IsZero() {
		return "an unknown time"
	}
	return at.UTC().Format("2006-01-02 15:04 UTC")
}

// recordLLMLimitEvent tells the operator, reusing agent_idle for the same
// reason notifyBridgeErrorPause does: the operator-visible fact is "this agent
// is not moving", and that event already carries the toggle, the severity and
// the delivery passes.
func (s *Server) recordLLMLimitEvent(clawID string, record llmUsageLimitRecord, opening bool) {
	kind := "exhausted"
	if opening {
		kind = "opened"
	}
	if err := s.recordTaskRunEventForClaw(clawID, TaskRunEvent{
		EventKey:        taskRunEventAgentIdle + ":llm-limit:" + kind + ":" + strconv.FormatInt(epochMillis(record.DetectedAt), 10),
		Source:          taskRunSourceHub,
		EventType:       taskRunEventAgentIdle,
		ActorType:       taskRunActorSystem,
		InteractionRole: taskRunInteractionNeutral,
		Detail: map[string]any{
			"llmUsageLimit":         record.Message,
			"llmUsageLimitReason":   record.Reason,
			"llmUsageLimitRetryAt":  formatLLMLimitDeadline(record.RetryAt),
			"llmUsageLimitRetries":  record.Retries,
			"llmUsageLimitProvider": record.Provider,
			"noProgressPaused":      true,
		},
		OccurredAt: now(),
	}); err != nil {
		log.Printf("[llm-limit] record notification event for claw %s: %v", shortID(clawID), err)
	}
}

// handleClawLLMLimit exposes the block on a claw, and clears it.
//
// GET answers "is this claw parked, and until when" for the dashboard.
// DELETE is the operator's override: they raised the cap in the provider's
// console, so the deadline the provider quoted is now wrong and waiting for it
// wastes the rest of the window. It clears the whole key, because the key is
// what was capped — releasing one claw of four would leave the other three
// parked on a limit that no longer exists.
func (s *Server) handleClawLLMLimit(w http.ResponseWriter, r *http.Request, clawID string) {
	tenantID := tenantFromCtx(r)
	var exists int
	if err := s.db.QueryRow(`SELECT 1 FROM claws WHERE id=? AND tenant_id=?`, clawID, tenantID).Scan(&exists); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodGet:
		until, limited := s.clawLLMLimitBlock(clawID)
		payload := map[string]any{"limited": limited}
		if limited {
			payload["retry_at"] = until.Format(time.RFC3339)
		}
		keyID, _ := s.resolveClawLLMKey(clawID)
		if record, ok := s.loadLLMUsageLimit(keyID); ok && record.Active() {
			payload["reason"] = record.Reason
			payload["message"] = record.Message
			payload["retries"] = record.Retries
			payload["provider"] = record.Provider
			if !record.RegainAt.IsZero() {
				payload["regain_at"] = record.RegainAt.Format(time.RFC3339)
			}
		}
		jsonOK(w, payload)
	case http.MethodDelete:
		keyID, _ := s.resolveClawLLMKey(clawID)
		s.releaseLLMUsageLimit(keyID, "An operator cleared the provider usage limit.", true)
		jsonOK(w, map[string]any{"limited": false})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// optionalTime turns a stored epoch-millis latch into the pointer the API
// exposes, so "not limited" is an absent field rather than a zero timestamp a
// client would have to know to ignore.
func optionalTime(ms int64) *time.Time {
	if ms == 0 {
		return nil
	}
	at := time.UnixMilli(ms).UTC()
	return &at
}
