package hub

import (
	"context"
	"encoding/json"
	"strings"
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
	if !s.noteLLMUsageLimit("claw-limit-due-a", types.LLMUsageLimit{
		Reason:   types.LLMLimitUsage,
		RegainAt: now().Add(-time.Minute),
		Message:  "You have reached your specified API usage limits.",
	}) {
		t.Fatal("noteLLMUsageLimit did not open an episode")
	}
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
	s.noteLLMUsageLimit("claw-limit-no-deadline", types.LLMUsageLimit{
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
	s.noteLLMUsageLimit("claw-limit-relatch", limit)

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
	s.noteLLMUsageLimit("claw-limit-badge", types.LLMUsageLimit{
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
