package hub

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"regexp"
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

// handleLLMUsageLimit routes one provider-limit turn.
//
// A limit deliberately does NOT touch the bridge-error streak — it clears it.
// The transport carried the rejection perfectly; counting it as a transport
// failure would pause the claw with a notice blaming the sandbox, which is the
// exact wrong thing to hand an operator whose real problem is a billing cap.
//
// The decision (new episode? same episode, second claw? released and hit
// again?) and the write that acts on it happen under ONE hold of llmLimitMu.
// They used to be split, and the gap was wide enough to matter: claws sharing
// a key are woken together at release, so their rejections arrive together,
// and every one of them read "released" before any of them wrote. Four claws
// then spent four retries on a single episode and the hub gave up after
// effectively one, each claw collecting a different deadline notice on the way.
func (s *Server) handleLLMUsageLimit(cc *clawConn, clawID string, limit types.LLMUsageLimit) {
	if cc != nil {
		cc.mu.Lock()
		cc.bridgeErrorStreak = 0
		cc.mu.Unlock()
	}
	keyID, provider := s.resolveClawLLMKey(clawID)
	nowAt := now()

	s.llmLimitMu.Lock()
	record, had := s.loadLLMUsageLimit(keyID)
	// Whatever this turn decides, the key is capped now: a release that was
	// still awaiting proof has just been disproved.
	delete(s.llmLimitProbing, keyID)
	opened, relatched := false, false
	switch {
	case had && record.Active():
		// Same episode, another claw. Keep the schedule exactly as it is: a
		// re-report must not reset a backoff an earlier failed release earned,
		// and must not spend a retry that belongs to the episode, not to the
		// claw.
	case had:
		// Released, and the wall is still there. Same episode: back off.
		record.ReleasedAt = time.Time{}
		record.Retries++
		record.Provider, record.Message, record.RegainAt = provider, limit.Message, limit.RegainAt
		base := limit.RegainAt
		if base.IsZero() || !base.After(nowAt) {
			base = nowAt
		}
		record.RetryAt = base.Add(time.Duration(record.Retries) * llmLimitRelatchBackoff)
		relatched = true
	default:
		record = llmUsageLimitRecord{
			KeyID:          keyID,
			Provider:       provider,
			Reason:         limit.Reason,
			Message:        limit.Message,
			RegainAt:       limit.RegainAt,
			RetryAt:        limit.RegainAt,
			DetectedAt:     nowAt,
			DetectedClawID: clawID,
		}
		if record.RetryAt.IsZero() {
			record.RetryAt = nowAt.Add(llmLimitFallbackRetry)
		}
		opened = true
	}
	if opened || relatched {
		if err := s.storeLLMUsageLimit(record); err != nil {
			s.llmLimitMu.Unlock()
			log.Printf("[llm-limit] store limit for key %q: %v", keyID, err)
			return
		}
	}
	latched := s.latchClawsForLLMLimitLocked(keyID, record)
	s.llmLimitMu.Unlock()

	switch {
	case opened:
		log.Printf("[llm-limit] key %q capped (%s), %d claw(s) parked until %s: %s",
			keyID, record.Reason, len(latched), formatLLMLimitDeadline(record.RetryAt), record.Message)
		s.recordLLMLimitEvent(record, len(latched), "provider_limit_opened")
	case relatched && record.Exhausted():
		log.Printf("[llm-limit] key %q still capped after %d automatic retries, leaving it for an operator: %s",
			keyID, record.Retries, record.Message)
		for _, id := range latched {
			s.publishHubNotice(id, fmt.Sprintf("[hub] The provider still reports a usage limit after %d automatic retries, so the hub stopped retrying. Raise the account limit and clear the block to resume. Last provider message: %s", record.Retries, record.Message))
		}
		s.recordLLMLimitEvent(record, len(latched), "provider_limit_exhausted")
	case relatched:
		log.Printf("[llm-limit] key %q still capped, retry %d scheduled for %s",
			keyID, record.Retries, formatLLMLimitDeadline(record.RetryAt))
		// Said out loud: the operator watched the release and must not read
		// silence as recovery. The event key carries the retry number, so
		// each re-latch is its own message.
		s.recordLLMLimitEvent(record, len(latched), "provider_limit_opened")
	}
}

// latchClawsForLLMLimitLocked parks every claw on the key and returns their
// IDs. Callers must hold s.llmLimitMu: the record write and this walk are one
// unit, or a concurrent release can unpark a claw the caller is still parking.
func (s *Server) latchClawsForLLMLimitLocked(keyID string, record llmUsageLimitRecord) []string {
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
		s.setClawLLMLimitMirror(clawID, record.RetryAt)
		if changed, _ := res.RowsAffected(); changed > 0 {
			s.publishHubNotice(clawID, notice)
		}
	}
	return clawIDs
}

// setClawLLMLimitMirror updates the in-memory copy a live connection reads in
// its delivery gate. The DB column is the truth; this only spares every gate a
// query.
func (s *Server) setClawLLMLimitMirror(clawID string, until time.Time) {
	s.mu.RLock()
	cc := s.claws[clawID]
	s.mu.RUnlock()
	if cc == nil {
		return
	}
	cc.mu.Lock()
	cc.llmLimitedUntil = until
	cc.mu.Unlock()
}

// releaseLLMUsageLimit lifts the latch for a key.
//
// The claws are unparked BEFORE the record is marked released, and that order
// is the crash-safety. A hub that dies half way through leaves the record
// active, so the next scheduler tick simply runs the release again; the
// reverse order left claws parked against a record nobody would ever look at
// again, and nothing in the system re-examined a stored deadline.
//
// The unlatch is a single UPDATE across every claw on the key, not a walk of
// the live ones: a claw that errored while parked is still parked, and
// claw_retry can revive it later on the same row.
func (s *Server) releaseLLMUsageLimit(keyID, reason string, announce bool) {
	s.llmLimitMu.Lock()
	record, had := s.loadLLMUsageLimit(keyID)
	if !had || !record.Active() {
		s.llmLimitMu.Unlock()
		return
	}
	if _, err := s.db.Exec(
		`UPDATE claws SET llm_limited_until=0 WHERE COALESCE(llm_key,'')=? AND COALESCE(llm_limited_until,0)<>0 AND status<>'deleted'`,
		keyID); err != nil {
		s.llmLimitMu.Unlock()
		log.Printf("[llm-limit] release claws for key %q: %v", keyID, err)
		return
	}
	clawIDs := s.clawsForLLMKey(keyID)
	for _, clawID := range clawIDs {
		s.setClawLLMLimitMirror(clawID, time.Time{})
	}
	if _, err := s.db.Exec(`UPDATE llm_usage_limits SET released_at=? WHERE key_id=?`, epochMillis(now()), keyID); err != nil {
		s.llmLimitMu.Unlock()
		log.Printf("[llm-limit] mark limit released for key %q: %v", keyID, err)
		return
	}
	// A release is a probe until a turn goes through: the cap may well still
	// be there (clock skew, a late reset, a balance nobody topped up), and
	// the re-latch a minute later would make "Provider cap lifted" a lie the
	// operator reads three times per episode. The lift is announced by
	// confirmLLMLimitLift once proven — an authored turn on the key, or an
	// operator clearing the block. The probe itself is derivable from durable
	// state — a released record with no released event yet — so a restart
	// re-arms it (seedLLMLimitProbes) instead of forgetting it.
	if s.llmLimitProbing == nil {
		s.llmLimitProbing = map[string]bool{}
	}
	s.llmLimitProbing[keyID] = true
	s.llmLimitMu.Unlock()

	// Notices and wakes touch sockets, so they happen outside the lock.
	for _, clawID := range clawIDs {
		if announce {
			s.publishHubNotice(clawID, "[hub] "+reason+" Automatic continuation resumed.")
		}
		s.resumeClawAfterLLMLimit(clawID)
	}
	log.Printf("[llm-limit] key %q released (%s), %d claw(s) resumed", keyID, reason, len(clawIDs))
}

// confirmLLMLimitLift announces a proven release of the key's latch. See
// releaseLLMUsageLimit for why the release itself stays quiet. Idempotent:
// the event key is fixed while the record stays released, so a second proof
// is absorbed by the event store's dedupe.
//
// The read and the event write happen under ONE hold of llmLimitMu. Claws on
// a key are resumed together, so one claw's proving turn and another claw's
// fresh rejection arrive together too; with the write outside the lock, the
// re-latch could land between them and the record this read as released was
// announced as lifted while the key was already capped again.
func (s *Server) confirmLLMLimitLift(keyID string) {
	s.llmLimitMu.Lock()
	defer s.llmLimitMu.Unlock()
	record, had := s.loadLLMUsageLimit(keyID)
	delete(s.llmLimitProbing, keyID)
	if !had || record.Active() {
		return
	}
	s.recordLLMLimitEvent(record, len(s.clawsForLLMKey(keyID)), "provider_limit_released")
}

// observeAuthoredTurnForLLMLimit is the proof a scheduled release waits for:
// a claw on the key got an answer from the provider. Free when nothing is
// probing, which is nearly always.
func (s *Server) observeAuthoredTurnForLLMLimit(clawID string) {
	s.llmLimitMu.Lock()
	probing := len(s.llmLimitProbing) != 0
	s.llmLimitMu.Unlock()
	if !probing {
		return
	}
	keyID, _ := s.resolveClawLLMKey(clawID)
	s.llmLimitMu.Lock()
	waiting := s.llmLimitProbing[keyID]
	s.llmLimitMu.Unlock()
	if waiting {
		s.confirmLLMLimitLift(keyID)
	}
}

// resumeClawAfterLLMLimit restarts a parked claw.
//
// A queued message goes first: it is what the human asked for, and delivering
// it also ends the turn-less stretch. With nothing queued the claw needs a
// push of its own — the block swallowed the turn it was going to take, and
// nothing else will offer another.
//
// A claw with no live socket gets that push as a QUEUED hub message rather
// than a dropped one. The block outlives a disconnect, so the release has to
// as well: a claw that was offline at midnight and reconnects at nine would
// otherwise come back unparked and silent, waiting for a turn nobody will ever
// start.
func (s *Server) resumeClawAfterLLMLimit(clawID string) {
	var pending int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND delivered_at IS NULL`, clawID).Scan(&pending); err != nil {
		log.Printf("[llm-limit] count pending for claw %s: %v", shortID(clawID), err)
		return
	}
	s.mu.RLock()
	cc := s.claws[clawID]
	s.mu.RUnlock()
	live := cc != nil && cc.conn != nil

	if pending > 0 {
		if live {
			s.sendNextQueuedMessage(cc)
		}
		// Not live: the post-registration drain delivers it on reconnect.
		return
	}
	if live {
		s.sendWakeMessage(cc, clawID)
		return
	}
	s.injectHubMessageByID(clawID, llmLimitResumeWake)
}

// llmLimitResumeWake is the durable form of the release wake. It is a hub
// message because a hub message survives the disconnect: it queues, the
// dashboard shows it, and the ordinary drain hands it to the bridge whenever
// the claw comes back.
const llmLimitResumeWake = "[hub] The model provider's allowance is back. Continue where you left off."

// seedLLMLimitProbes re-arms, after a restart, every release that is still
// waiting for its proof. The probe set lives in memory, and losing it between
// the release and the first successful turn would leave the operator with an
// "account capped" alert whose recovery never arrives: the record says
// released, no turn can confirm it, and nothing else looks again. Durable
// state answers the question on its own — a released record whose released
// event has not been recorded is exactly a pending probe.
func (s *Server) seedLLMLimitProbes() {
	for _, record := range s.llmUsageLimitRecords() {
		if record.Active() {
			continue
		}
		var announced int
		if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM infra_events WHERE event_key=?)`,
			llmLimitEventKey(record, "provider_limit_released")).Scan(&announced); err != nil {
			log.Printf("[llm-limit] check released event for key %q: %v", record.KeyID, err)
			continue
		}
		if announced != 0 {
			continue
		}
		s.llmLimitMu.Lock()
		if s.llmLimitProbing == nil {
			s.llmLimitProbing = map[string]bool{}
		}
		s.llmLimitProbing[record.KeyID] = true
		s.llmLimitMu.Unlock()
	}
}

// startLLMUsageLimitScheduler releases limits whose deadline has passed and
// reconciles stored latches against the record table.
func (s *Server) startLLMUsageLimitScheduler() {
	s.seedLLMLimitProbes()
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
				s.reconcileLLMUsageLimitLatches()
			}()
		}
	}()
}

func (s *Server) releaseDueLLMUsageLimits() {
	nowAt := now()
	for _, record := range s.llmUsageLimitRecords() {
		if !record.Active() {
			s.pruneReleasedLLMUsageLimit(record, nowAt)
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

// pruneReleasedLLMUsageLimit forgets a released episode once it is old
// enough (llmLimitEpisodeMemory) — but not while its lift is still unproven.
// The released record is the only durable trace of a pending probe: the
// proving turn reads it (confirmLLMLimitLift) and a restart re-derives the
// probe from it (seedLLMLimitProbes). Pruning it first left the operator's
// channel with an "account capped" alert whose recovery could never arrive:
// the claw that was offline through the release wakes hours later, authors
// its turn, and finds nothing to confirm. An unproven release is not an
// episode that ended, so it keeps its memory until a turn says otherwise.
func (s *Server) pruneReleasedLLMUsageLimit(record llmUsageLimitRecord, nowAt time.Time) {
	if nowAt.Sub(record.ReleasedAt) <= llmLimitEpisodeMemory {
		return
	}
	s.llmLimitMu.Lock()
	defer s.llmLimitMu.Unlock()
	if s.llmLimitProbing[record.KeyID] {
		return
	}
	if _, err := s.db.Exec(`DELETE FROM llm_usage_limits WHERE key_id=? AND released_at<>0`, record.KeyID); err != nil {
		log.Printf("[llm-limit] prune released limit for key %q: %v", record.KeyID, err)
	}
}

// reconcileLLMUsageLimitLatches unparks any claw whose key has no active
// limit.
//
// The per-claw column and the per-key record are two writes, and every way of
// splitting them leaves a window: a hub that dies mid-release, a claw revived
// by claw_retry on a row nobody re-examined, a row updated by hand. Without a
// reconciler, "parked" is a state only one code path can ever leave, and a
// claw that misses that path is parked forever — a permanently silent agent
// whose UI still promises it resumes on its own.
func (s *Server) reconcileLLMUsageLimitLatches() {
	active := map[string]bool{}
	for _, record := range s.llmUsageLimitRecords() {
		if record.Active() {
			active[record.KeyID] = true
		}
	}
	rows, err := s.db.Query(`SELECT id, COALESCE(llm_key,'') FROM claws WHERE COALESCE(llm_limited_until,0)<>0 AND status<>'deleted'`)
	if err != nil {
		log.Printf("[llm-limit] reconcile query: %v", err)
		return
	}
	type stale struct{ id, key string }
	var orphans []stale
	for rows.Next() {
		var row stale
		if err := rows.Scan(&row.id, &row.key); err != nil {
			continue
		}
		if !active[row.key] {
			orphans = append(orphans, row)
		}
	}
	rows.Close()

	for _, orphan := range orphans {
		s.llmLimitMu.Lock()
		res, err := s.db.Exec(`UPDATE claws SET llm_limited_until=0 WHERE id=? AND COALESCE(llm_limited_until,0)<>0`, orphan.id)
		if err == nil {
			s.setClawLLMLimitMirror(orphan.id, time.Time{})
		}
		s.llmLimitMu.Unlock()
		if err != nil {
			log.Printf("[llm-limit] reconcile claw %s: %v", shortID(orphan.id), err)
			continue
		}
		if changed, _ := res.RowsAffected(); changed == 0 {
			continue
		}
		log.Printf("[llm-limit] reconciled stale block on claw %s (key %q has no active limit)", shortID(orphan.id), orphan.key)
		s.resumeClawAfterLLMLimit(orphan.id)
	}
}

// seedLLMLimitForConnection settles whether a freshly registered claw is
// parked, and is the ONLY thing that decides it.
//
// The latch used to be a one-shot sweep over the claws that existed when the
// cap was found, which quietly meant a claw created afterwards on the same
// capped key started unparked and spent a turn discovering the wall for
// itself. Reading the key's record here instead makes the record authoritative
// at the moment it is consulted, so joining late, being revived by claw_retry,
// and reconnecting mid-block all land in the same place.
//
// It must run AFTER the connection is in s.claws and s.mu is released: latch
// and release take s.llmLimitMu and then s.mu, so taking them in the other
// order here would invert the lock order.
func (s *Server) seedLLMLimitForConnection(cc *clawConn, clawID string) {
	keyID, _ := s.resolveClawLLMKey(clawID)

	s.llmLimitMu.Lock()
	record, had := s.loadLLMUsageLimit(keyID)
	until := time.Time{}
	if had && record.Active() {
		until = record.RetryAt
		if _, err := s.db.Exec(`UPDATE claws SET llm_limited_until=? WHERE id=?`, epochMillis(until), clawID); err != nil {
			log.Printf("[llm-limit] park registering claw %s: %v", shortID(clawID), err)
		}
	} else if _, err := s.db.Exec(`UPDATE claws SET llm_limited_until=0 WHERE id=? AND COALESCE(llm_limited_until,0)<>0`, clawID); err != nil {
		log.Printf("[llm-limit] clear stale block on registering claw %s: %v", shortID(clawID), err)
	}
	if cc != nil {
		cc.mu.Lock()
		cc.llmLimitedUntil = until
		cc.mu.Unlock()
	}
	s.llmLimitMu.Unlock()
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
// until the account has allowance again.
//
// Once per block, and the marker is a column rather than a field on the
// connection. In memory it was wrong in three ways at once: a claw with no
// live connection had no marker at all and re-announced on every single
// message, a bridge reconnect reset it, and every latch pass cleared it.
func (s *Server) noticeLLMLimitToUser(clawID string) bool {
	until, limited := s.clawLLMLimitBlock(clawID)
	if !limited {
		return false
	}
	res, err := s.db.Exec(
		`UPDATE claws SET llm_limit_noticed_until=? WHERE id=? AND COALESCE(llm_limit_noticed_until,0)<>?`,
		epochMillis(until), clawID, epochMillis(until))
	if err != nil {
		log.Printf("[llm-limit] mark notice for claw %s: %v", shortID(clawID), err)
		return true
	}
	if changed, _ := res.RowsAffected(); changed == 0 {
		return true
	}
	s.publishHubNotice(clawID, fmt.Sprintf("[hub] This agent's model provider is out of allowance, so your message was queued instead of delivered. It goes out automatically when access returns on %s.", formatLLMLimitDeadline(until)))
	return true
}

// llmUsageLimitRecords returns every limit, released ones included.
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
// Only deleted claws are excluded. An errored claw is deliberately included:
// claw_retry revives one on the same row, and skipping it at release left the
// revived claw parked against a deadline that had already passed, with nothing
// left to clear it.
func (s *Server) clawsForLLMKey(keyID string) []string {
	rows, err := s.db.Query(`SELECT id FROM claws WHERE COALESCE(llm_key,'')=? AND status<>'deleted'`, keyID)
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

// recordLLMLimitEvent records the fleet-wide account fact once per key. Four
// claws sharing a key formerly produced four "Agent stalled" reports, which
// sent operators looking for four agent bugs instead of one billing cap.
func (s *Server) recordLLMLimitEvent(record llmUsageLimitRecord, parked int, eventType string) {
	if err := s.recordInfraEvent(infraEvent{
		EventKey: llmLimitEventKey(record, eventType), EventType: eventType, Subject: record.Provider,
		Detail: map[string]any{
			"provider": record.Provider, "key_id": maskLLMLimitKeyID(record.KeyID), "parked_claws": parked,
			"deadline": formatLLMLimitDeadline(record.RetryAt), "retry_count": record.Retries,
			"message": redactLLMLimitEventMessage(record.Message, record.KeyID),
		}, OccurredAt: now(),
	}); err != nil {
		log.Printf("[llm-limit] record %s event: %v", eventType, err)
	}
}

// llmLimitEventKey names one edge of one episode: the retry number keeps each
// re-latch its own message, and the same key derived at seed time is how a
// restarted hub tells an announced lift from a pending one.
func llmLimitEventKey(record llmUsageLimitRecord, eventType string) string {
	return fmt.Sprintf("%s:%s:%d:%d", eventType, maskLLMLimitKeyID(record.KeyID), epochMillis(record.DetectedAt), record.Retries)
}

func maskLLMLimitKeyID(keyID string) string {
	if keyID == "" {
		return "default"
	}
	sum := sha256.Sum256([]byte(keyID))
	return "key_" + hex.EncodeToString(sum[:6])
}

// llmLimitTokenPatterns match credential-shaped substrings a provider error
// body may quote back at us. The configured key ID is often an alias
// ("anthropic-prod"), so replacing only the literal ID would wave the raw
// token straight through into infra_events and Slack. Over-redaction is the
// safe failure mode here: nothing in a billing-cap message needs a 28-char
// opaque string to be actionable.
var llmLimitTokenPatterns = []*regexp.Regexp{
	// Provider API keys: sk-..., sk-ant-..., sk-proj-... and friends.
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}`),
	// An echoed Authorization header: whatever follows "Bearer" is the
	// credential, however short or oddly spelled.
	regexp.MustCompile(`(?i)\bbearer\s+[^\s;,'"]+`),
	// Any long opaque run that reads as a credential, not prose.
	regexp.MustCompile(`[A-Za-z0-9_-]{28,}`),
}

func redactLLMLimitEventMessage(message, keyID string) string {
	if keyID != "" {
		message = strings.ReplaceAll(message, keyID, "[redacted]")
	}
	for _, pattern := range llmLimitTokenPatterns {
		message = pattern.ReplaceAllString(message, "[redacted]")
	}
	return message
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
		// The operator says the cap is gone; that is the proof.
		s.confirmLLMLimitLift(keyID)
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
