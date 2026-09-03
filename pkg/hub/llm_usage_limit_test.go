package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// The rejection the Faster fleet actually received, verbatim, wrapped in the
// bridge's own error label the way claw-bridge sends it.
const incidentUsageLimitTurn = "⚠️ claw-bridge error: LLM request rejected: You have reached your specified API usage limits. You will regain access on 2026-09-01 at 00:00 UTC."

func limitClaw(t *testing.T, s *Server, clawID, llmKey string, register bool) *clawConn {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO claws(id, tenant_id, name, status, llm_key, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", clawID, "connected", llmKey, "ci_passed"); err != nil {
		t.Fatalf("insert claw %s: %v", clawID, err)
	}
	if !register {
		return nil
	}
	cc := &clawConn{id: clawID, tenantID: "test-tenant-id"}
	s.mu.Lock()
	s.claws[clawID] = cc
	s.mu.Unlock()
	return cc
}

func limitedUntil(t *testing.T, s *Server, clawID string) int64 {
	t.Helper()
	var until int64
	if err := s.db.QueryRow(`SELECT COALESCE(llm_limited_until,0) FROM claws WHERE id=?`, clawID).Scan(&until); err != nil {
		t.Fatalf("read llm_limited_until for %s: %v", clawID, err)
	}
	return until
}

func hubNotices(t *testing.T, s *Server, clawID, like string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT content FROM messages WHERE claw_id=? AND role='hub' AND content LIKE ? ORDER BY rowid`, clawID, like)
	if err != nil {
		t.Fatalf("read notices: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// The shape of the incident: one claw discovers the cap, and every claw on the
// same key is parked by it. Three of the four Faster claws never had to spend
// a turn finding out for themselves.
func TestLLMUsageLimitParksEveryClawSharingTheKey(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")

	discoverer := limitClaw(t, s, "claw-limit-discoverer", "faster", true)
	sibling := limitClaw(t, s, "claw-limit-sibling", "faster", true)
	stranger := limitClaw(t, s, "claw-limit-stranger", "other-key", true)

	paused, bridgeErrTurn := s.observeTurnOutcome(discoverer, "claw-limit-discoverer", "msg-1", incidentUsageLimitTurn)
	if !paused || !bridgeErrTurn {
		t.Fatalf("observeTurnOutcome() = (%v, %v), want (true, true)", paused, bridgeErrTurn)
	}

	wantUntil := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	for _, clawID := range []string{"claw-limit-discoverer", "claw-limit-sibling"} {
		if got := limitedUntil(t, s, clawID); got != wantUntil {
			t.Errorf("claw %s limited until %d, want %d (the deadline the provider quoted)", clawID, got, wantUntil)
		}
	}
	if got := limitedUntil(t, s, "claw-limit-stranger"); got != 0 {
		t.Errorf("claw on a different key was parked (until %d); the cap is per key", got)
	}

	for _, cc := range []*clawConn{discoverer, sibling} {
		cc.mu.Lock()
		blocked := cc.deliveryBlockedLocked()
		cc.mu.Unlock()
		if !blocked {
			t.Errorf("claw %s still accepts hub-initiated delivery while capped", cc.id)
		}
	}
	stranger.mu.Lock()
	strangerBlocked := stranger.deliveryBlockedLocked()
	stranger.mu.Unlock()
	if strangerBlocked {
		t.Error("claw on a different key had its delivery blocked")
	}

	// The notice must carry the deadline: "come back later" without a time is
	// exactly the report that sent an operator to the console for two hours.
	notices := hubNotices(t, s, "claw-limit-sibling", "%out of allowance%")
	if len(notices) != 1 {
		t.Fatalf("got %d pause notices, want 1: %v", len(notices), notices)
	}
	if want := "2026-09-01 00:00 UTC"; !strings.Contains(notices[0], want) {
		t.Errorf("notice %q does not state the deadline %q", notices[0], want)
	}
}

// A cap is not a broken transport. Counting it as one would pause the claw
// with a notice blaming the sandbox and start a streak that outlives the
// billing period.
func TestLLMUsageLimitIsNotABridgeTransportError(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	cc := limitClaw(t, s, "claw-limit-not-transport", "faster", true)

	cc.mu.Lock()
	cc.bridgeErrorStreak = 1
	cc.mu.Unlock()

	s.observeTurnOutcome(cc, "claw-limit-not-transport", "msg-1", incidentUsageLimitTurn)

	cc.mu.Lock()
	streak := cc.bridgeErrorStreak
	cc.mu.Unlock()
	if streak != 0 {
		t.Errorf("bridgeErrorStreak = %d, want 0: the transport delivered the rejection just fine", streak)
	}
	if notices := hubNotices(t, s, "claw-limit-not-transport", "%claw-bridge returned a transport error%"); len(notices) != 0 {
		t.Errorf("operator was told the transport is broken: %v", notices)
	}
}

// The reason this latch is not no_progress_paused: a human writing to a capped
// agent must not buy it another failed turn.
func TestLLMUsageLimitSurvivesUserInput(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	cc := limitClaw(t, s, "claw-limit-user-input", "faster", true)
	s.observeTurnOutcome(cc, "claw-limit-user-input", "msg-1", incidentUsageLimitTurn)

	s.resumeNoProgressAfterUserInput("claw-limit-user-input")

	if got := limitedUntil(t, s, "claw-limit-user-input"); got == 0 {
		t.Fatal("a user message cleared the provider limit; the account is still capped")
	}
	cc.mu.Lock()
	blocked := cc.deliveryBlockedLocked()
	cc.mu.Unlock()
	if !blocked {
		t.Error("delivery unblocked by user input while the provider is still capped")
	}

	// The sender is told once, not once per message.
	for i := 0; i < 3; i++ {
		if !s.noticeLLMLimitToUser("claw-limit-user-input") {
			t.Fatal("noticeLLMLimitToUser reported no limit while one is latched")
		}
	}
	notices := hubNotices(t, s, "claw-limit-user-input", "%queued instead of delivered%")
	if len(notices) != 1 {
		t.Fatalf("got %d queue notices, want exactly 1: %v", len(notices), notices)
	}
	if want := "2026-09-01 00:00 UTC"; !strings.Contains(notices[0], want) {
		t.Errorf("queue notice %q does not tell the sender when it goes out", notices[0])
	}
}

// A claw that reconnects during the block must come back parked, or the fresh
// bridge spends a turn rediscovering the same wall.
func TestLLMUsageLimitOutlivesReconnect(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	cc := limitClaw(t, s, "claw-limit-reconnect", "faster", true)
	s.observeTurnOutcome(cc, "claw-limit-reconnect", "msg-1", incidentUsageLimitTurn)

	until, limited := s.clawLLMLimitBlock("claw-limit-reconnect")
	if !limited {
		t.Fatal("clawLLMLimitBlock reports no block right after latching one")
	}
	fresh := &clawConn{id: "claw-limit-reconnect", tenantID: "test-tenant-id", llmLimitedUntil: until}
	fresh.mu.Lock()
	blocked := fresh.deliveryBlockedLocked()
	fresh.mu.Unlock()
	if !blocked {
		t.Error("a reconnected claw accepts delivery while its key is still capped")
	}
}

// Requirement 4, the payoff: the hub comes back on its own at the instant the
// provider named, with nobody watching.
func TestLLMUsageLimitReleasesAtTheDeadline(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	// Offline claws: the release path is being tested, not the socket write.
	limitClaw(t, s, "claw-limit-due-a", "faster", false)
	limitClaw(t, s, "claw-limit-due-b", "faster", false)
	s.handleLLMUsageLimit(nil, "claw-limit-due-a", types.LLMUsageLimit{
		Reason:   types.LLMLimitUsage,
		RegainAt: now().Add(-time.Minute),
		Message:  "You have reached your specified API usage limits.",
	})
	if got := limitedUntil(t, s, "claw-limit-due-b"); got == 0 {
		t.Fatal("sibling claw was not parked")
	}

	s.releaseDueLLMUsageLimits()

	for _, clawID := range []string{"claw-limit-due-a", "claw-limit-due-b"} {
		if got := limitedUntil(t, s, clawID); got != 0 {
			t.Errorf("claw %s still parked (until %d) after its deadline passed", clawID, got)
		}
	}
	if notices := hubNotices(t, s, "claw-limit-due-a", "%Automatic continuation resumed%"); len(notices) != 1 {
		t.Errorf("got %d resume notices, want 1: %v", len(notices), notices)
	}
}

// A limit with no stated deadline must not look due on the very first tick —
// that would spend a turn a minute after the block started, every minute.
func TestLLMUsageLimitWithoutDeadlineWaitsForTheFallback(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	limitClaw(t, s, "claw-limit-no-deadline", "faster", false)
	s.handleLLMUsageLimit(nil, "claw-limit-no-deadline", types.LLMUsageLimit{
		Reason:  types.LLMLimitCredit,
		Message: "Your credit balance is too low to access the Anthropic API.",
	})

	s.releaseDueLLMUsageLimits()

	if got := limitedUntil(t, s, "claw-limit-no-deadline"); got == 0 {
		t.Fatal("a limit with no stated deadline was released immediately")
	}
	record, ok := s.loadLLMUsageLimit("faster")
	if !ok {
		t.Fatal("limit record missing")
	}
	if want := now().Add(llmLimitFallbackRetry); record.RetryAt.Before(want.Add(-time.Minute)) {
		t.Errorf("retryAt = %v, want roughly %v", record.RetryAt, want)
	}
}

// Released and hit again is the SAME episode: the backoff has to climb, and
// after enough tries the hub must stop guessing and leave it to a human.
func TestLLMUsageLimitBacksOffAndThenStopsRetrying(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	limitClaw(t, s, "claw-limit-relatch", "faster", false)
	limit := types.LLMUsageLimit{
		Reason:   types.LLMLimitUsage,
		RegainAt: now().Add(-time.Minute),
		Message:  "You have reached your specified API usage limits.",
	}
	s.handleLLMUsageLimit(nil, "claw-limit-relatch", limit)

	var lastRetryAt time.Time
	for attempt := 1; attempt <= llmLimitMaxAutoRetries; attempt++ {
		s.releaseLLMUsageLimit("faster", "test release", false)
		s.handleLLMUsageLimit(nil, "claw-limit-relatch", limit)

		record, ok := s.loadLLMUsageLimit("faster")
		if !ok {
			t.Fatalf("attempt %d: limit record missing", attempt)
		}
		if record.Retries != attempt {
			t.Fatalf("attempt %d: retries = %d, want %d (a re-hit is the same episode, not a new one)", attempt, record.Retries, attempt)
		}
		if !record.Active() {
			t.Fatalf("attempt %d: re-latch left the limit released", attempt)
		}
		if !lastRetryAt.IsZero() && !record.RetryAt.After(lastRetryAt) {
			t.Fatalf("attempt %d: retryAt %v did not move past %v", attempt, record.RetryAt, lastRetryAt)
		}
		lastRetryAt = record.RetryAt
	}

	record, _ := s.loadLLMUsageLimit("faster")
	if !record.Exhausted() {
		t.Fatalf("retries = %d, want the automatic cycle to have given up", record.Retries)
	}
	// Exhausted stays latched: the wall is demonstrably real.
	s.releaseDueLLMUsageLimits()
	if got := limitedUntil(t, s, "claw-limit-relatch"); got == 0 {
		t.Error("an exhausted limit was auto-released; only an operator should clear it")
	}
	if notices := hubNotices(t, s, "claw-limit-relatch", "%stopped retrying%"); len(notices) == 0 {
		t.Error("operator was never told the hub gave up retrying")
	}
}

// A capped account raises its own badge, NOT the outage badge. The status page
// says "operational" throughout — the service is fine, our account is not — so
// the overlay has to win, and it has to win with a word that sends the operator
// to the billing console rather than to a status page.
func TestLLMUsageLimitRaisesItsOwnBadge(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	limitClaw(t, s, "claw-limit-badge", "faster", false)
	// The key's provider is what maps the limit onto a dependency row.
	if _, err := s.db.Exec(`UPDATE claws SET default_model='anthropic/claude-opus-5' WHERE id=?`, "claw-limit-badge"); err != nil {
		t.Fatalf("set default model: %v", err)
	}

	service := s.dependencyStatus
	service.mu.Lock()
	service.cache = &DependencyStatusResponse{
		Dependencies: []DependencyStatus{{
			ID:        "model:anthropic",
			Name:      "Anthropic",
			Kind:      dependencyKindModel,
			Status:    dependencyStatusOperational,
			CheckedAt: now(),
		}},
		CheckedAt: now(),
	}
	service.mu.Unlock()

	before := service.snapshot(context.Background())
	if before.DowntimeCount != 0 || before.LimitedCount != 0 {
		t.Fatalf("counts before any limit = (%d downtime, %d limited), want (0, 0)", before.DowntimeCount, before.LimitedCount)
	}

	regain := now().Add(4 * time.Hour).Truncate(time.Minute)
	s.handleLLMUsageLimit(nil, "claw-limit-badge", types.LLMUsageLimit{
		Reason:   types.LLMLimitUsage,
		RegainAt: regain,
		Message:  "You have reached your specified API usage limits.",
	})

	after := service.snapshot(context.Background())
	if after.LimitedCount != 1 {
		t.Fatalf("limitedCount = %d, want 1: the badge must fire for a capped account", after.LimitedCount)
	}
	// The distinction is the point: a cap is not an outage, and an operator
	// reading "downtime" would go looking for one that does not exist.
	if after.DowntimeCount != 0 {
		t.Errorf("downtimeCount = %d, want 0: a capped account is not a provider outage", after.DowntimeCount)
	}
	var found *DependencyStatus
	for i := range after.Dependencies {
		if after.Dependencies[i].ID == "model:anthropic" {
			found = &after.Dependencies[i]
		}
	}
	if found == nil {
		t.Fatal("anthropic dependency missing from the snapshot")
	}
	if found.Status != dependencyStatusLimited {
		t.Errorf("status = %q, want %q", found.Status, dependencyStatusLimited)
	}
	// Requirement 4: the badge itself carries the renewal instant.
	if found.RegainAt == nil || !found.RegainAt.Equal(regain) {
		t.Errorf("regainAt = %v, want %v", found.RegainAt, regain)
	}
	if !strings.Contains(found.Message, formatLLMLimitDeadline(regain)) {
		t.Errorf("message %q does not state the deadline", found.Message)
	}

	// And it clears with the limit, without waiting out the status cache.
	s.releaseLLMUsageLimit("faster", "test release", false)
	cleared := service.snapshot(context.Background())
	if cleared.LimitedCount != 0 {
		t.Errorf("limitedCount = %d after release, want 0", cleared.LimitedCount)
	}
}

// omitempty does not skip a zero time.Time, so a pointer is the only thing
// keeping every healthy dependency from claiming it recovers in the year 1.
func TestDependencyStatusOmitsRegainAtWhenUnknown(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	s.dependencyStatus.mu.Lock()
	s.dependencyStatus.cache = &DependencyStatusResponse{
		Dependencies: []DependencyStatus{{
			ID: "sandbox:daytona", Name: "Daytona", Kind: dependencyKindSandbox,
			Status: dependencyStatusDowntime, CheckedAt: now(),
		}},
		CheckedAt: now(),
	}
	s.dependencyStatus.mu.Unlock()

	encoded, err := json.Marshal(s.dependencyStatus.snapshot(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "regainAt") {
		t.Errorf("snapshot ships a regainAt for a dependency with no known deadline: %s", encoded)
	}
}

// One release episode costs ONE retry, however many claws share the key.
//
// The claws on a key are woken together at release, so their rejections arrive
// together. With the note-vs-relatch decision read outside the lock, all four
// saw "released" before any of them wrote, each spent a retry, and the hub
// declared the episode exhausted after effectively one attempt — with four
// different deadlines announced to each claw on the way.
func TestLLMUsageLimitSpendsOneRetryPerEpisodeNotPerClaw(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	ids := []string{"claw-race-a", "claw-race-b", "claw-race-c", "claw-race-d"}
	for _, id := range ids {
		limitClaw(t, s, id, "faster", false)
	}
	limit := types.LLMUsageLimit{
		Reason:   types.LLMLimitUsage,
		RegainAt: now().Add(-time.Minute),
		Message:  "You have reached your specified API usage limits.",
	}
	s.handleLLMUsageLimit(nil, ids[0], limit)
	s.releaseLLMUsageLimit("faster", "test release", false)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, id := range ids {
		wg.Add(1)
		go func(clawID string) {
			defer wg.Done()
			<-start
			s.handleLLMUsageLimit(nil, clawID, limit)
		}(id)
	}
	close(start)
	wg.Wait()

	record, ok := s.loadLLMUsageLimit("faster")
	if !ok {
		t.Fatal("limit record missing")
	}
	if record.Retries != 1 {
		t.Errorf("retries = %d after one release episode, want 1: the budget belongs to the episode, not to each claw", record.Retries)
	}
	if record.Exhausted() {
		t.Error("episode declared exhausted after a single release")
	}
	for _, id := range ids {
		if got := limitedUntil(t, s, id); got == 0 {
			t.Errorf("claw %s not re-parked by the relatch", id)
		}
	}
}

// A claw that errored while parked must still be unparked. claw_retry revives
// one on the same row, and the release used to skip it — leaving it blocked
// against a deadline already in the past, with nothing left to clear it.
func TestLLMUsageLimitReleasesErroredClaws(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	limitClaw(t, s, "claw-errored", "faster", false)
	limitClaw(t, s, "claw-healthy", "faster", false)
	s.handleLLMUsageLimit(nil, "claw-errored", types.LLMUsageLimit{
		Reason: types.LLMLimitUsage, RegainAt: now().Add(time.Hour), Message: "capped",
	})
	if _, err := s.db.Exec(`UPDATE claws SET status='error' WHERE id=?`, "claw-errored"); err != nil {
		t.Fatal(err)
	}

	s.releaseLLMUsageLimit("faster", "test release", false)

	if got := limitedUntil(t, s, "claw-errored"); got != 0 {
		t.Errorf("errored claw still parked (until %d); claw_retry would revive it blocked forever", got)
	}
	if got := limitedUntil(t, s, "claw-healthy"); got != 0 {
		t.Errorf("healthy claw still parked (until %d)", got)
	}
}

// A claw created after the cap was found shares the wall, so it must share the
// block. The latch is a one-shot sweep; the KEY's record is what registration
// consults.
func TestLLMUsageLimitParksAClawThatJoinsLater(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	limitClaw(t, s, "claw-first", "faster", false)
	s.handleLLMUsageLimit(nil, "claw-first", types.LLMUsageLimit{
		Reason: types.LLMLimitUsage, RegainAt: now().Add(time.Hour), Message: "capped",
	})

	latecomer := limitClaw(t, s, "claw-latecomer", "faster", true)
	if got := limitedUntil(t, s, "claw-latecomer"); got != 0 {
		t.Fatalf("test setup: latecomer should start unparked, got %d", got)
	}
	s.seedLLMLimitForConnection(latecomer, "claw-latecomer")

	if got := limitedUntil(t, s, "claw-latecomer"); got == 0 {
		t.Error("a claw registering on a capped key was not parked; it will spend a turn on the same wall")
	}
	latecomer.mu.Lock()
	blocked := latecomer.deliveryBlockedLocked()
	latecomer.mu.Unlock()
	if !blocked {
		t.Error("latecomer accepts hub-initiated delivery on a capped key")
	}
}

// Registration is also where a stale block dies: if the key has no active
// limit, whatever the column says is history.
func TestLLMUsageLimitRegistrationClearsAStaleBlock(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	cc := limitClaw(t, s, "claw-stale", "faster", true)
	if _, err := s.db.Exec(`UPDATE claws SET llm_limited_until=? WHERE id=?`, epochMillis(now().Add(-time.Hour)), "claw-stale"); err != nil {
		t.Fatal(err)
	}

	s.seedLLMLimitForConnection(cc, "claw-stale")

	if got := limitedUntil(t, s, "claw-stale"); got != 0 {
		t.Errorf("stale block survived registration (until %d) with no active limit for the key", got)
	}
	cc.mu.Lock()
	blocked := cc.deliveryBlockedLocked()
	cc.mu.Unlock()
	if blocked {
		t.Error("claw still blocked in memory after a stale block was cleared")
	}
}

// An interrupted release leaves the column set and the record gone. Nothing
// else in the system re-reads a stored deadline, so without reconciliation a
// claw in that state is silent forever while its UI promises a resume.
func TestLLMUsageLimitReconcilesOrphanedBlocks(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	limitClaw(t, s, "claw-orphan", "faster", false)
	if _, err := s.db.Exec(`UPDATE claws SET llm_limited_until=? WHERE id=?`, epochMillis(now().Add(time.Hour)), "claw-orphan"); err != nil {
		t.Fatal(err)
	}

	s.reconcileLLMUsageLimitLatches()

	if got := limitedUntil(t, s, "claw-orphan"); got != 0 {
		t.Errorf("orphaned block survived reconciliation (until %d)", got)
	}
}

// ...and reconciliation must not touch a claw whose key is genuinely capped.
func TestLLMUsageLimitReconciliationSparesLiveBlocks(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	limitClaw(t, s, "claw-live-block", "faster", false)
	s.handleLLMUsageLimit(nil, "claw-live-block", types.LLMUsageLimit{
		Reason: types.LLMLimitUsage, RegainAt: now().Add(time.Hour), Message: "capped",
	})

	s.reconcileLLMUsageLimitLatches()

	if got := limitedUntil(t, s, "claw-live-block"); got == 0 {
		t.Error("reconciliation unparked a claw whose key is still capped")
	}
}

// "Told once" has to survive not having a connection at all — the claw is
// frequently offline while blocked, and that is exactly when the in-memory
// marker announced on every single message.
func TestLLMUsageLimitNoticeIsOncePerBlockWithoutAConnection(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	limitClaw(t, s, "claw-no-conn", "faster", false)
	s.handleLLMUsageLimit(nil, "claw-no-conn", types.LLMUsageLimit{
		Reason: types.LLMLimitUsage, RegainAt: now().Add(time.Hour), Message: "capped",
	})

	for i := 0; i < 4; i++ {
		if !s.noticeLLMLimitToUser("claw-no-conn") {
			t.Fatal("noticeLLMLimitToUser reported no limit while one is latched")
		}
	}

	notices := hubNotices(t, s, "claw-no-conn", "%queued instead of delivered%")
	if len(notices) != 1 {
		t.Fatalf("got %d queue notices for a disconnected claw, want exactly 1: %v", len(notices), notices)
	}
}

// A claw offline at release must still be resumed. The block outlives a
// disconnect, so the release has to as well, or a claw that was down at
// midnight comes back unparked and silent with nothing to start its next turn.
func TestLLMUsageLimitReleaseQueuesAWakeForADisconnectedClaw(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	limitClaw(t, s, "claw-offline-release", "faster", false)
	s.handleLLMUsageLimit(nil, "claw-offline-release", types.LLMUsageLimit{
		Reason: types.LLMLimitUsage, RegainAt: now().Add(-time.Minute), Message: "capped",
	})

	s.releaseDueLLMUsageLimits()

	var pending int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE claw_id=? AND delivered_at IS NULL AND content=?`,
		"claw-offline-release", llmLimitResumeWake).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Errorf("queued resume wakes = %d, want 1: a claw offline at release gets no push otherwise", pending)
	}
}

// An agent REPORTING a provider message must not be able to park the fleet.
// The generic replay prefix is short enough to open a real turn with, which is
// why only the bridge's own label may declare a cap.
func TestLLMUsageLimitIgnoresTheGenericReplayPrefix(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	cc := limitClaw(t, s, "claw-agent-quoting", "faster", true)

	s.observeTurnOutcome(cc, "claw-agent-quoting", "msg-1",
		types.BridgeReplayErrorPrefix+" the provider told me: You have reached your specified API usage limits. You will regain access on 2026-09-02 at 00:00 UTC.")

	if got := limitedUntil(t, s, "claw-agent-quoting"); got != 0 {
		t.Errorf("a turn opening with the generic error prefix parked the key (until %d)", got)
	}
	if _, ok := s.loadLLMUsageLimit("faster"); ok {
		t.Error("a non-definite bridge body created a usage-limit record for the whole key")
	}
}

// redactLLMLimitEventMessage must catch credential-shaped substrings a
// provider error quotes back, not merely the literal configured key id —
// which is often an alias that never appears in the provider's text at all.
func TestRedactLLMLimitEventMessageRedactsTokenShapes(t *testing.T) {
	cases := []struct {
		name    string
		message string
		secret  string
	}{
		{"sk token with unrelated alias key", "invalid key sk-ant-api03-AAAA1111BBBB2222", "sk-ant-api03-AAAA1111BBBB2222"},
		{"long opaque credential", "token ghp_a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4 rejected", "ghp_a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"},
		{"configured key id itself", "key anthropic-prod is over its cap", "anthropic-prod"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := redactLLMLimitEventMessage(tc.message, "anthropic-prod")
			if strings.Contains(out, tc.secret) {
				t.Fatalf("secret survived redaction: %q", out)
			}
			if !strings.Contains(out, "[redacted]") {
				t.Fatalf("nothing was redacted from %q -> %q", tc.message, out)
			}
		})
	}
	const prose = "monthly allowance reached, resets at midnight UTC"
	if out := redactLLMLimitEventMessage(prose, "anthropic-prod"); out != prose {
		t.Fatalf("plain prose was mangled: %q", out)
	}
}

// A scheduled release is a probe, not a recovery: "Provider cap lifted" must
// only be said once a turn proves the wall is gone, and every re-latch must
// tell the operator the hub is still capped instead of staying silent.
func TestLLMUsageLimitReleaseIsAnnouncedOnlyOnceProven(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	limitClaw(t, s, "claw-limit-probe", "faster", false)
	limit := types.LLMUsageLimit{Reason: types.LLMLimitUsage, RegainAt: now().Add(-time.Minute), Message: "capped"}
	s.handleLLMUsageLimit(nil, "claw-limit-probe", limit)

	for attempt := 1; attempt <= llmLimitMaxAutoRetries; attempt++ {
		s.releaseLLMUsageLimit("faster", "test release", false)
		s.handleLLMUsageLimit(nil, "claw-limit-probe", limit)
	}
	want := "provider_limit_opened,provider_limit_opened,provider_limit_opened,provider_limit_exhausted"
	if got := strings.Join(infraEventTypes(t, s), ","); got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
}

// Once a claw on the key authors a turn after a scheduled release, the lift
// is real and is announced exactly once.
func TestLLMUsageLimitAuthoredTurnConfirmsTheLift(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	limitClaw(t, s, "claw-limit-proof", "faster", false)
	limitClaw(t, s, "claw-limit-other-key", "other", false)
	s.handleLLMUsageLimit(nil, "claw-limit-proof", types.LLMUsageLimit{Reason: types.LLMLimitUsage, RegainAt: now().Add(-time.Minute), Message: "capped"})
	s.releaseLLMUsageLimit("faster", "test release", false)
	if got := strings.Join(infraEventTypes(t, s), ","); got != "provider_limit_opened" {
		t.Fatalf("events after a scheduled release = %s, want the release to stay unannounced", got)
	}

	s.observeTurnOutcome(nil, "claw-limit-other-key", "msg-other", "Working on the other key.")
	if got := strings.Join(infraEventTypes(t, s), ","); got != "provider_limit_opened" {
		t.Fatalf("a turn on an unrelated key confirmed the lift: %s", got)
	}
	s.observeTurnOutcome(nil, "claw-limit-proof", "msg-1", "Back to work.")
	s.observeTurnOutcome(nil, "claw-limit-proof", "msg-2", "Still working.")
	if got := strings.Join(infraEventTypes(t, s), ","); got != "provider_limit_opened,provider_limit_released" {
		t.Fatalf("events = %s, want exactly one released after the proving turn", got)
	}
}

// An operator clearing the block is a confirmed lift in itself.
func TestLLMUsageLimitOperatorClearAnnouncesTheLift(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	limitClaw(t, s, "claw-limit-clear", "faster", false)
	s.handleLLMUsageLimit(nil, "claw-limit-clear", types.LLMUsageLimit{Reason: types.LLMLimitUsage, RegainAt: now().Add(time.Hour), Message: "capped"})
	req := httptest.NewRequest(http.MethodDelete, "/api/claws/claw-limit-clear/llm-limit", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxTenantKey{}, "test-tenant-id"))
	rr := httptest.NewRecorder()
	s.handleClawLLMLimit(rr, req, "claw-limit-clear")
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE = %d: %s", rr.Code, rr.Body.String())
	}
	if got := strings.Join(infraEventTypes(t, s), ","); got != "provider_limit_opened,provider_limit_released" {
		t.Fatalf("events = %s, want the operator clear announced", got)
	}
}

// A short bearer token echoed in an Authorization header is a credential too,
// whatever its length or alphabet.
func TestRedactLLMLimitEventMessageRedactsBearerEchoes(t *testing.T) {
	out := redactLLMLimitEventMessage("429: Authorization: Bearer dG9rZW4tdGVzdA==; org org_abc123 over quota", "prod-openai")
	if strings.Contains(out, "dG9rZW4tdGVzdA==") {
		t.Fatalf("bearer token survived redaction: %q", out)
	}
	if !strings.Contains(out, "org_abc123") {
		t.Fatalf("account identifier needed to act on the message was mangled: %q", out)
	}
}

// A restart between the scheduled release and its proving turn must not lose
// the recovery alert: the probe is re-derived from the record and the event
// log, so the first authored turn after boot still announces the lift once.
func TestLLMUsageLimitProbeSurvivesRestart(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	limitClaw(t, s, "claw-limit-reboot", "faster", false)
	s.handleLLMUsageLimit(nil, "claw-limit-reboot", types.LLMUsageLimit{Reason: types.LLMLimitUsage, RegainAt: now().Add(-time.Minute), Message: "capped"})
	s.releaseLLMUsageLimit("faster", "test release", false)

	// The restart: in-memory probes are gone, durable state is what it was.
	s.llmLimitMu.Lock()
	s.llmLimitProbing = nil
	s.llmLimitMu.Unlock()
	s.observeTurnOutcome(nil, "claw-limit-reboot", "msg-0", "Nobody re-armed me.")
	if got := strings.Join(infraEventTypes(t, s), ","); got != "provider_limit_opened" {
		t.Fatalf("events without a reseed = %s, want the lift still pending", got)
	}

	s.seedLLMLimitProbes()
	s.observeTurnOutcome(nil, "claw-limit-reboot", "msg-1", "Back to work.")
	if got := strings.Join(infraEventTypes(t, s), ","); got != "provider_limit_opened,provider_limit_released" {
		t.Fatalf("events after reseed = %s, want exactly one released", got)
	}

	// An already-announced lift is not re-armed by a later restart.
	s.seedLLMLimitProbes()
	s.llmLimitMu.Lock()
	probing := s.llmLimitProbing["faster"]
	s.llmLimitMu.Unlock()
	if probing {
		t.Fatal("reseed re-armed a probe for a lift that was already announced")
	}
}

// The proving turn and a fresh rejection arrive together (claws sharing a
// key are resumed together), so confirming the lift must be atomic with the
// re-latch: whichever wins, the log never says "lifted" after "capped again".
func TestLLMUsageLimitLiftConfirmationIsAtomicWithRelatch(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	limitClaw(t, s, "claw-limit-race-a", "faster", false)
	limitClaw(t, s, "claw-limit-race-b", "faster", false)
	limit := types.LLMUsageLimit{Reason: types.LLMLimitUsage, RegainAt: now().Add(-time.Minute), Message: "capped"}
	eventKeys := func() []string {
		rows, err := s.db.Query(`SELECT event_key FROM infra_events ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				t.Fatal(err)
			}
			out = append(out, key)
		}
		return out
	}
	for i := 0; i < 100; i++ {
		for _, table := range []string{"llm_usage_limits", "infra_events"} {
			if _, err := s.db.Exec(`DELETE FROM ` + table); err != nil {
				t.Fatal(err)
			}
		}
		s.handleLLMUsageLimit(nil, "claw-limit-race-a", limit)
		s.releaseLLMUsageLimit("faster", "test release", false)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); s.observeTurnOutcome(nil, "claw-limit-race-a", "msg-proof", "Back to work.") }()
		go func() { defer wg.Done(); s.handleLLMUsageLimit(nil, "claw-limit-race-b", limit) }()
		wg.Wait()
		relatched := false
		for _, key := range eventKeys() {
			if strings.HasPrefix(key, "provider_limit_opened:") && strings.HasSuffix(key, ":1") {
				relatched = true
			}
			if strings.HasPrefix(key, "provider_limit_released:") && relatched {
				t.Fatalf("run %d announced the lift after the re-latch: %v", i, eventKeys())
			}
		}
	}
}

// A released episode is forgotten after llmLimitEpisodeMemory — unless its
// lift is still unproven. The record is what the proving turn confirms
// against and what a restart re-derives the probe from; pruning it first
// left the channel's "account capped" alert without a recovery forever (the
// only claw on the key was offline through the release and turned hours
// later), and a reseed after that had nothing to find.
func TestLLMUsageLimitUnprovenReleaseOutlivesEpisodeMemory(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	limitClaw(t, s, "claw-limit-late", "faster", false)
	s.handleLLMUsageLimit(nil, "claw-limit-late", types.LLMUsageLimit{Reason: types.LLMLimitUsage, RegainAt: now().Add(-time.Minute), Message: "capped"})
	s.releaseLLMUsageLimit("faster", "test release", false)
	if _, err := s.db.Exec(`UPDATE llm_usage_limits SET released_at=? WHERE key_id='faster'`, epochMillis(now().Add(-llmLimitEpisodeMemory-time.Hour))); err != nil {
		t.Fatal(err)
	}
	s.releaseDueLLMUsageLimits()
	if _, had := s.loadLLMUsageLimit("faster"); !had {
		t.Fatal("the scheduler pruned a released record whose lift was never proven")
	}

	// The same restart-shaped gap: the probe set is gone, the durable state
	// still has to be enough to re-arm it.
	s.llmLimitMu.Lock()
	s.llmLimitProbing = nil
	s.llmLimitMu.Unlock()
	s.seedLLMLimitProbes()
	s.releaseDueLLMUsageLimits()
	s.observeTurnOutcome(nil, "claw-limit-late", "msg-1", "Back to work.")
	if got := strings.Join(infraEventTypes(t, s), ","); got != "provider_limit_opened,provider_limit_released" {
		t.Fatalf("events after the late proving turn = %s, want the lift announced", got)
	}

	// Proven, and older than the memory window: now it is history.
	s.releaseDueLLMUsageLimits()
	if _, had := s.loadLLMUsageLimit("faster"); had {
		t.Fatal("a proven, expired episode was kept")
	}
}
