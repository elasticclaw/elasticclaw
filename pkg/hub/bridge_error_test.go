package hub

import (
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// The two turn bodies claw 1572c4e4 produced during NEXT-725, verbatim. Both
// clear the initial-plan gate's 120-character floor, which is why the hub kept
// answering them with another correction.
const (
	incidentENOSPCTurn   = "⚠️ claw-bridge error: ENOSPC: no space left on device, open '/home/daytona/.openclaw/agents/main/sessions/....jsonl.lock'"
	incidentRecoveryTurn = "⚠️ claw-bridge error: context deadline exceeded; session recovery failed: sessions.create failed: ENOSPC: no space left on device, open '/home/daytona/.openclaw/agents/main/sessions/....jsonl.lock'"
)

func TestBridgeTransportErrorRecognisesDeployedBridgeReplies(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
		want    bool
	}{
		{
			name:    "live turn prefix from the incident",
			content: incidentENOSPCTurn,
			wantErr: "ENOSPC: no space left on device, open '/home/daytona/.openclaw/agents/main/sessions/....jsonl.lock'",
			want:    true,
		},
		{
			name:    "session recovery turn from the incident",
			content: incidentRecoveryTurn,
			wantErr: "context deadline exceeded; session recovery failed: sessions.create failed: ENOSPC: no space left on device, open '/home/daytona/.openclaw/agents/main/sessions/....jsonl.lock'",
			want:    true,
		},
		{
			name:    "replay path prefix",
			content: "⚠️ error: LLM request failed: network connection error",
			wantErr: "LLM request failed: network connection error",
			want:    true,
		},
		{
			name:    "leading whitespace still opens the message",
			content: "\n  " + incidentENOSPCTurn,
			wantErr: "ENOSPC: no space left on device, open '/home/daytona/.openclaw/agents/main/sessions/....jsonl.lock'",
			want:    true,
		},
		{
			name:    "agent quoting the error mid-sentence is the agent talking",
			content: "The build log ends with ⚠️ claw-bridge error: ENOSPC: no space left on device, so I will clean the cache and retry.",
		},
		{
			name:    "ordinary agent turn",
			content: "I read the failing test and I am about to change the matcher.",
		},
		{name: "empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotErr, got := types.BridgeTransportError(tt.content)
			if got != tt.want {
				t.Fatalf("BridgeTransportError() ok = %v, want %v", got, tt.want)
			}
			if gotErr != tt.wantErr {
				t.Fatalf("BridgeTransportError() err = %q, want %q", gotErr, tt.wantErr)
			}
		})
	}
}

// The incident bodies must stay above the plan gate's length floor, otherwise
// the regression tests below would pass for the wrong reason.
func TestIncidentTurnsClearInitialPlanLengthFloor(t *testing.T) {
	for _, content := range []string{incidentENOSPCTurn, incidentRecoveryTurn} {
		if len(strings.TrimSpace(content)) < 120 {
			t.Fatalf("incident turn is only %d bytes; it no longer exercises the gate", len(content))
		}
	}
}

func planGateClaw(t *testing.T, s *Server, clawID string, correctionSent bool) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", clawID, `[]`); err != nil {
		t.Fatalf("insert claw %s: %v", clawID, err)
	}
	s.insertSystemMarker(clawID, "test-tenant-id", initialPlanRequiredMarker)
	if correctionSent {
		s.insertSystemMarker(clawID, "test-tenant-id", initialPlanCorrectionSentMarker)
	}
}

func correctionCount(t *testing.T, s *Server, clawID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='hub' AND content=?`,
		clawID, initialPlanCorrectionContent).Scan(&n); err != nil {
		t.Fatalf("count corrections for %s: %v", clawID, err)
	}
	return n
}

// The loop itself: a bridge error must never be answered with the plan
// correction, while a genuine (incomplete) response still is.
func TestHandleInitialPlanResponseIgnoresBridgeErrors(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")

	tests := []struct {
		name            string
		clawID          string
		correctionSent  bool
		content         string
		wantCorrections int
		wantAccepted    bool
	}{
		{
			name:            "bridge error before any correction stays silent",
			clawID:          "claw-bridge-error-first",
			content:         incidentENOSPCTurn,
			wantCorrections: 0,
		},
		{
			name:            "bridge error after a correction does not re-send it",
			clawID:          "claw-bridge-error-repeat",
			correctionSent:  true,
			content:         incidentRecoveryTurn,
			wantCorrections: 0,
		},
		{
			name:            "real incomplete response still gets the correction",
			clawID:          "claw-real-incomplete",
			content:         "Good, build passes. Now let me read the existing test files.",
			wantCorrections: 1,
		},
		{
			name:           "real incomplete response is re-nudged after a correction",
			clawID:         "claw-real-renudge",
			correctionSent: true,
			content: "I looked at the repository and the CI configuration for a while and I am still deciding " +
				"how to approach this, so here is a long update that carries no plan whatsoever yet.",
			wantCorrections: 1,
		},
		{
			name:           "substantial second attempt is still soft-accepted",
			clawID:         "claw-soft-accept",
			correctionSent: true,
			content: strings.Repeat("Rough plan: add the lint workflow, wire pnpm cache, and run typecheck. ", 8) +
				"Verification: run lint and typecheck in CI and confirm the PR checks go green.",
			wantAccepted: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			planGateClaw(t, s, tt.clawID, tt.correctionSent)
			s.handleInitialPlanResponse(tt.clawID, "test-tenant-id", tt.content)
			if got := correctionCount(t, s, tt.clawID); got != tt.wantCorrections {
				t.Fatalf("corrections sent = %d, want %d", got, tt.wantCorrections)
			}
			if got := s.hasSystemMarker(tt.clawID, initialPlanAcceptedMarker); got != tt.wantAccepted {
				t.Fatalf("plan accepted = %v, want %v", got, tt.wantAccepted)
			}
			// A bridge error must also leave the gate armed, so the next real
			// turn is judged exactly as it would have been.
			if _, isBridgeError := types.BridgeTransportError(tt.content); isBridgeError {
				if s.hasSystemMarker(tt.clawID, initialPlanCorrectionSentMarker) != tt.correctionSent {
					t.Fatalf("bridge error changed the correction-sent marker")
				}
			}
		})
	}
}

func bridgeErrorClaw(t *testing.T, s *Server, clawID string) *clawConn {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO claws(id, tenant_id, name, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", clawID, "connected", "ci_passed"); err != nil {
		t.Fatalf("insert claw %s: %v", clawID, err)
	}
	cc := &clawConn{id: clawID, tenantID: "test-tenant-id"}
	s.mu.Lock()
	s.claws[clawID] = cc
	s.mu.Unlock()
	return cc
}

func pauseNotices(t *testing.T, s *Server, clawID string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT content FROM messages WHERE claw_id=? AND role='hub' AND content LIKE '%Automatic continuation paused:%' ORDER BY rowid`, clawID)
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

func clawPaused(t *testing.T, s *Server, clawID string) bool {
	t.Helper()
	var paused bool
	if err := s.db.QueryRow(`SELECT COALESCE(no_progress_paused,0) != 0 FROM claws WHERE id=?`, clawID).Scan(&paused); err != nil {
		t.Fatalf("read pause latch: %v", err)
	}
	return paused
}

// Two consecutive bridge errors pause automatic continuation exactly once, and
// the notice carries the real error — "no space left on device" is what turns
// an hour-long mystery into a one-minute fix.
func TestConsecutiveBridgeErrorsPauseOnceAndSurfaceTheError(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	const clawID = "claw-bridge-streak"
	cc := bridgeErrorClaw(t, s, clawID)

	errText, _ := types.BridgeTransportError(incidentENOSPCTurn)
	if s.observeBridgeErrorTurn(cc, clawID, errText, true) {
		t.Fatal("a single bridge error paused the claw; one error is transient")
	}
	if clawPaused(t, s, clawID) {
		t.Fatal("a single bridge error latched the pause")
	}
	if got := len(pauseNotices(t, s, clawID)); got != 0 {
		t.Fatalf("notices after one error = %d, want 0", got)
	}

	recoveryErr, _ := types.BridgeTransportError(incidentRecoveryTurn)
	if !s.observeBridgeErrorTurn(cc, clawID, recoveryErr, true) {
		t.Fatalf("%d consecutive bridge errors did not pause the claw", bridgeErrorPauseThreshold)
	}
	if !clawPaused(t, s, clawID) {
		t.Fatal("pause was not persisted on the shared no_progress latch")
	}
	notices := pauseNotices(t, s, clawID)
	if len(notices) != 1 {
		t.Fatalf("notices after the pause = %d, want 1", len(notices))
	}
	if !strings.Contains(notices[0], "no space left on device") {
		t.Fatalf("notice does not carry the real error: %q", notices[0])
	}

	// A third error must not notify again: the claw is already stopped.
	if !s.observeBridgeErrorTurn(cc, clawID, recoveryErr, true) {
		t.Fatal("a further bridge error unpaused the claw")
	}
	if got := len(pauseNotices(t, s, clawID)); got != 1 {
		t.Fatalf("notices after a third error = %d, want 1", got)
	}

	// A human resuming clears both the latch and the streak, so the next
	// pause costs the full threshold again.
	s.resumeNoProgressAfterUserInput(clawID)
	if clawPaused(t, s, clawID) {
		t.Fatal("resume did not lift the bridge-error pause")
	}
	if s.observeBridgeErrorTurn(cc, clawID, errText, true) {
		t.Fatal("the streak was not reset by the resume")
	}
}

// One real turn between two bridge errors means the transport is working; the
// streak must start over rather than accumulate across hours of healthy work.
func TestRealTurnResetsBridgeErrorStreak(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	const clawID = "claw-bridge-reset"
	cc := bridgeErrorClaw(t, s, clawID)

	if paused, bridgeErr := s.observeTurnOutcome(cc, clawID, "turn-1", incidentENOSPCTurn); paused || !bridgeErr {
		t.Fatalf("first bridge error: paused=%v bridgeErr=%v", paused, bridgeErr)
	}
	if paused, bridgeErr := s.observeTurnOutcome(cc, clawID, "turn-2", "I fixed the matcher and pushed the commit."); paused || bridgeErr {
		t.Fatalf("real turn: paused=%v bridgeErr=%v", paused, bridgeErr)
	}
	if paused, _ := s.observeTurnOutcome(cc, clawID, "turn-3", incidentRecoveryTurn); paused {
		t.Fatal("a bridge error after a real turn paused the claw on its own")
	}
	if clawPaused(t, s, clawID) {
		t.Fatal("pause latched despite a real turn between the two errors")
	}

	// The bridge-error turns must also stay out of the no-progress watchdog's
	// evidence: they are not agent outcomes, and admitting them would reset
	// the repeated-outcome chain it is counting.
	var observations int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM claw_turn_observations WHERE claw_id=? AND response LIKE '%claw bridge error%'`, clawID).Scan(&observations); err != nil {
		t.Fatal(err)
	}
	if observations != 0 {
		t.Fatalf("bridge-error turns recorded %d no-progress observations, want 0", observations)
	}
}

// A transport error must never be able to complete or tear down a claw whose
// turn never ran, however the gateway folds text into its error string.
func TestBridgeErrorTurnCannotSignal(t *testing.T) {
	echoed := incidentENOSPCTurn + "\n[DONE] https://github.com/example/repo/pull/1"
	tests := []struct {
		name         string
		content      string
		token        string
		bridgeErr    bool
		wantSignaled bool
	}{
		{name: "bridge error echoing DONE", content: echoed, token: doneSignalToken, bridgeErr: true},
		{name: "bridge error echoing TERMINATE", content: incidentENOSPCTurn + "\n[TERMINATE]", token: terminateSignalToken, bridgeErr: true},
		{name: "agent DONE still signals", content: "Work finished.\n[DONE] https://github.com/example/repo/pull/1", token: doneSignalToken, wantSignaled: true},
		{name: "agent TERMINATE still signals", content: "[TERMINATE]", token: terminateSignalToken, wantSignaled: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := turnMaySignal(tt.content, tt.token, tt.bridgeErr); got != tt.wantSignaled {
				t.Fatalf("turnMaySignal() = %v, want %v", got, tt.wantSignaled)
			}
		})
	}
}

// The pause must reach an operator, not just the claw's own chat: the incident
// was invisible precisely because nothing left the chat.
func TestBridgeErrorPauseNotifiesOperator(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	const clawID = "claw-bridge-notify"
	if _, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, status, pipeline_stage, task_run_id, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", clawID, "connected", "ci_passed", "run-bridge"); err != nil {
		t.Fatal(err)
	}
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-bridge", AttemptID: "attempt-run-bridge", ClawID: clawID, TenantID: "test-tenant-id",
		OwnerType: taskRunOwnerFactory, Factory: "bugfix", Phase: taskRunPhaseAgentRunning, StartedAt: int64(1760000000000),
	})
	cc := &clawConn{id: clawID, tenantID: "test-tenant-id"}
	s.mu.Lock()
	s.claws[clawID] = cc
	s.mu.Unlock()

	errText, _ := types.BridgeTransportError(incidentENOSPCTurn)
	for i := 0; i < bridgeErrorPauseThreshold; i++ {
		s.observeBridgeErrorTurn(cc, clawID, errText, true)
	}

	var detail string
	if err := db.QueryRow(`SELECT detail FROM task_run_events WHERE run_id='run-bridge' AND event_type=?`, taskRunEventAgentIdle).Scan(&detail); err != nil {
		t.Fatalf("no operator notification event was recorded: %v", err)
	}
	if !strings.Contains(detail, "no space left on device") {
		t.Fatalf("notification detail does not carry the real error: %q", detail)
	}
}

// The rendered notification must lead with the error, not with "agent stalled".
func TestBridgeErrorNotificationRendersTheRealError(t *testing.T) {
	ev := lifecycleEventRow{
		EventType: taskRunEventAgentIdle,
		Detail: map[string]any{
			"bridgeError":      "ENOSPC: no space left on device, open '/home/daytona/.openclaw/agents/main/sessions/....jsonl.lock'",
			"bridgeErrorTurns": 2,
			"noProgressPaused": true,
		},
	}
	msg := buildLifecycleMessage(ev, lifecycleRunContext{Repo: "example/repo", IssueID: "NEXT-725", ClawID: "1572c4e4"})
	if !strings.Contains(msg.Body, "no space left on device") {
		t.Fatalf("body does not carry the real error: %q", msg.Body)
	}
	if !strings.Contains(msg.Body, "2 consecutive turns") {
		t.Fatalf("body does not say how many turns failed: %q", msg.Body)
	}
	if !strings.Contains(strings.Join(msg.Summary, " "), "no space left on device") {
		t.Fatalf("push summary does not carry the real error: %q", msg.Summary)
	}
	// A real idle event must keep its own wording.
	idle := buildLifecycleMessage(lifecycleEventRow{EventType: taskRunEventAgentIdle, Detail: map[string]any{"idleMinutes": 9}}, lifecycleRunContext{})
	if !strings.Contains(idle.Body, "No agent activity") {
		t.Fatalf("plain idle message changed: %q", idle.Body)
	}
}

// The shared latch must be a latch: a second caller (the no-progress observer,
// a racing tick) finds it already set, reports that it did not pause, and does
// not publish a second notice into the claw's chat.
func TestPauseAutomaticContinuationLatchesOnce(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	const clawID = "claw-pause-latch"
	cc := bridgeErrorClaw(t, s, clawID)

	if !s.pauseAutomaticContinuation(clawID, "[hub] Automatic continuation paused: first.") {
		t.Fatal("first pause did not latch")
	}
	if s.pauseAutomaticContinuation(clawID, "[hub] Automatic continuation paused: second.") {
		t.Fatal("second pause claimed to latch an already-paused claw")
	}
	notices := pauseNotices(t, s, clawID)
	if len(notices) != 1 {
		t.Fatalf("notices = %d, want 1", len(notices))
	}
	cc.mu.RLock()
	inMemory := cc.noProgressPaused
	cc.mu.RUnlock()
	if !inMemory || !clawPaused(t, s, clawID) {
		t.Fatalf("pause not visible: memory=%v db=%v", inMemory, clawPaused(t, s, clawID))
	}
	// The no-progress observer must agree the claw is held rather than start
	// its own count — the two stops share one latch, they do not compete.
	if !s.observeCompletedTurn(clawID, "turn-1", "still waiting for CI") {
		t.Fatal("observeCompletedTurn did not report the claw as already paused")
	}
}

// The alarm must not wait for the pause threshold. Reaching it needs a SECOND
// turn, and nothing guarantees one once the plan gate stops answering: the next
// turn only exists if the idle auto-resume fires, which is off when
// liveness.idle_resume is disabled, when the claw is ineligible, or once its
// per-work-unit resume cap is spent. Tied to that, a dead transport could sit silent
// forever — the exact failure this change exists to end.
func TestFirstBridgeErrorNotifiesWithoutPausing(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	const clawID = "claw-bridge-first"
	if _, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, status, pipeline_stage, task_run_id, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", clawID, "connected", "working", "run-bridge-first"); err != nil {
		t.Fatal(err)
	}
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-bridge-first", AttemptID: "attempt-first", ClawID: clawID, TenantID: "test-tenant-id",
		OwnerType: taskRunOwnerFactory, Factory: "bugfix", Phase: taskRunPhaseAgentRunning, StartedAt: int64(1760000000000),
	})
	cc := &clawConn{id: clawID, tenantID: "test-tenant-id"}
	s.mu.Lock()
	s.claws[clawID] = cc
	s.mu.Unlock()

	errText, _ := types.BridgeTransportError(incidentENOSPCTurn)
	if s.observeBridgeErrorTurn(cc, clawID, errText, true) {
		t.Fatal("the first error paused the claw; one error is transient")
	}
	if clawPaused(t, s, clawID) {
		t.Fatal("the first error latched the pause")
	}

	var detail string
	if err := db.QueryRow(`SELECT detail FROM task_run_events WHERE run_id='run-bridge-first' AND event_type=?`,
		taskRunEventAgentIdle).Scan(&detail); err != nil {
		t.Fatalf("the first bridge error produced no operator notification: %v", err)
	}
	if !strings.Contains(detail, "no space left on device") {
		t.Fatalf("first-error notification does not carry the real error: %q", detail)
	}
	if strings.Contains(detail, `"noProgressPaused":true`) {
		t.Fatalf("first-error notification claims the claw is paused: %q", detail)
	}
}

// The generic replay label is short enough for an agent to open a turn with it
// while reporting a build failure. It still silences the plan gate — a turn
// opening with an error is not a plan — but it must not count toward stopping
// the claw, or a working agent gets paused and its operator gets told a lie
// about the cause.
func TestGenericErrorPrefixDoesNotCountTowardThePause(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	const clawID = "claw-bridge-generic"
	cc := bridgeErrorClaw(t, s, clawID)

	errText, definite, ok := types.BridgeTransportErrorIsDefinite("⚠️ error: the build failed on step 3")
	if !ok {
		t.Fatal("the generic prefix must still be recognised so the plan gate stays quiet")
	}
	if definite {
		t.Fatal("the generic prefix must not be treated as the bridge's own label")
	}
	for i := 0; i < bridgeErrorPauseThreshold+2; i++ {
		if s.observeBridgeErrorTurn(cc, clawID, errText, definite) {
			t.Fatalf("generic-prefix turn %d paused the claw", i+1)
		}
	}
	if clawPaused(t, s, clawID) {
		t.Fatal("generic-prefix turns latched the pause")
	}
}
