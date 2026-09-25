package hub

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

type sessionLossFixture struct {
	s      *Server
	db     *sql.DB
	cc     *clawConn
	conn   *websocket.Conn
	clawID string
}

func newSessionLossFixture(t *testing.T, max *int) sessionLossFixture {
	t.Helper()
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token", Liveness: &types.LivenessConfig{SessionLossMaxConsecutive: max}}, "", "", "")
	id := "session-loss-" + uuid.NewString()
	conn := watchdogClaw(t, s, id)
	cc := watchdogClawConn(t, s, id)
	if _, err := db.Exec(`UPDATE claws SET status='connected', bootstrap_ok=1, pipeline_stage='implement' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	f := sessionLossFixture{s, db, cc, conn, id}
	f.output(t, "Initial plan", now().Add(-time.Hour))
	return f
}

func (f sessionLossFixture) output(t *testing.T, content string, at time.Time) {
	t.Helper()
	f.s.recordSessionLossCompletedTurn(f.clawID, content)
	if _, err := f.db.Exec(`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)`, uuid.NewString(), f.clawID, "test-tenant-id", "claw", content, at); err != nil {
		t.Fatal(err)
	}
}

func (f sessionLossFixture) expireThrottle(t *testing.T) {
	t.Helper()
	if _, err := f.db.Exec(`UPDATE messages SET created_at=? WHERE claw_id=? AND role='hub' AND (content LIKE ? OR content LIKE ?)`, now().Add(-sessionRotatedResumeThrottle-time.Minute), f.clawID, sessionRotatedResumePrefix+"%", sessionPreservedContinuationPrefix+"%"); err != nil {
		t.Fatal(err)
	}
}

func (f sessionLossFixture) state(t *testing.T) (streak int, mark string, paused bool) {
	t.Helper()
	if err := f.db.QueryRow(`SELECT session_loss_streak,session_loss_progress_mark,no_progress_paused != 0 FROM claws WHERE id=?`, f.clawID).Scan(&streak, &mark, &paused); err != nil {
		t.Fatal(err)
	}
	return
}

func (f sessionLossFixture) count(t *testing.T, prefix string) int {
	t.Helper()
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='hub' AND content LIKE ?`, f.clawID, prefix+"%").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func (f sessionLossFixture) send(t *testing.T, kind string, n, wantStreak, wantPrompts int) {
	t.Helper()
	f.cc.mu.Lock()
	f.cc.streamingStartedAt = now()
	f.cc.mu.Unlock()
	if err := wsjson.Write(context.Background(), f.conn, types.WSMessage{Type: kind, Payload: types.SessionRecoveryEdge{SessionKey: fmt.Sprintf("key-%d", n), Reason: types.SessionLossReasonTurnTimeout}}); err != nil {
		t.Fatal(err)
	}
	prefix := sessionRotatedResumePrefix
	if kind == "session_preserved" {
		prefix = sessionPreservedContinuationPrefix
	}
	eventuallyWatchdog(t, func() bool {
		streak, _, paused := f.state(t)
		return streak == wantStreak && f.count(t, prefix) == wantPrompts && (wantStreak < 3 || paused)
	}, "session-loss edge processed")
}

func TestSessionLossLoopGuardPausesAfterThreeLossesWithoutProgress(t *testing.T) {
	f := newSessionLossFixture(t, nil)
	for i := 1; i <= 3; i++ {
		f.expireThrottle(t)
		f.send(t, "session_rotated", i, i, min(i, 2))
	}
	eventuallyWatchdog(t, func() bool {
		return f.count(t, "[hub] Automatic continuation paused: the agent's session was lost 3 times") == 1
	}, "session-loss pause notice")
	if f.count(t, sessionRotatedResumePrefix) != 2 {
		t.Fatal("third loss injected a resume")
	}
}

func TestSessionLossLoopGuardResetsOnNewClawOutput(t *testing.T) {
	f := newSessionLossFixture(t, nil)
	for i := 1; i <= 2; i++ {
		f.expireThrottle(t)
		f.send(t, "session_rotated", i, i, i)
	}
	f.output(t, "Implemented the first bounded step", now())
	f.expireThrottle(t)
	f.send(t, "session_rotated", 3, 1, 3)
	if _, _, paused := f.state(t); paused {
		t.Fatal("new output did not prevent pause")
	}
}

func TestSessionLossLoopGuardResetsOnStageTransition(t *testing.T) {
	f := newSessionLossFixture(t, nil)
	f.send(t, "session_rotated", 1, 1, 1)
	if f.s.claimPipelineStageTransition(f.clawID, "implement") {
		t.Fatal("same stage claimed")
	}
	if streak, _, _ := f.state(t); streak != 1 {
		t.Fatal("same-stage entry reset streak")
	}
	if !f.s.claimPipelineStageTransition(f.clawID, "review") {
		t.Fatal("stage transition failed")
	}
	if streak, mark, _ := f.state(t); streak != 0 || mark != "" {
		t.Fatalf("stage transition left streak=%d mark=%q", streak, mark)
	}
}

func TestSessionLossLoopGuardResetsOnUserInput(t *testing.T) {
	f := newSessionLossFixture(t, nil)
	for i := 1; i <= 3; i++ {
		f.expireThrottle(t)
		f.send(t, "session_rotated", i, i, min(i, 2))
	}
	f.s.resumeNoProgressAfterUserInput(f.clawID)
	if streak, mark, paused := f.state(t); streak != 0 || mark != "" || paused {
		t.Fatalf("human input: streak=%d mark=%q paused=%v", streak, mark, paused)
	}
	f.cc.mu.RLock()
	paused := f.cc.noProgressPaused
	f.cc.mu.RUnlock()
	if paused {
		t.Fatal("connection remains paused")
	}
	f.expireThrottle(t)
	f.send(t, "session_rotated", 4, 1, 3)
}

func TestSessionLossLoopGuardIgnoresThrottledLosses(t *testing.T) {
	f := newSessionLossFixture(t, nil)
	f.send(t, "session_rotated", 1, 1, 1)
	// Synchronous call makes the negative assertion independent of websocket timing.
	f.s.noteSessionLoss(f.cc, f.clawID, "key-2", "session_rotated", types.SessionRecoveryEdge{Reason: types.SessionLossReasonTurnTimeout})
	if streak, _, paused := f.state(t); streak != 1 || paused || f.count(t, sessionRotatedResumePrefix) != 1 {
		t.Fatalf("throttled loss counted: streak=%d paused=%v", streak, paused)
	}
}

func TestSessionLossLoopGuardDisabledByZero(t *testing.T) {
	zero := 0
	f := newSessionLossFixture(t, &zero)
	for i := 1; i <= 5; i++ {
		f.expireThrottle(t)
		f.send(t, "session_rotated", i, 0, i)
	}
	if _, _, paused := f.state(t); paused {
		t.Fatal("disabled guard paused claw")
	}
}

func TestSessionLossLoopGuardCountsPreservedContinuations(t *testing.T) {
	f := newSessionLossFixture(t, nil)
	for i := 1; i <= 3; i++ {
		f.expireThrottle(t)
		f.send(t, "session_preserved", i, i, min(i, 2))
	}
	eventuallyWatchdog(t, func() bool { return f.count(t, "[hub] Automatic continuation paused:") == 1 }, "preserved loop pause")
}

func TestLivenessSessionLossMaxSettings(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured *int
		want       int
	}{
		{"nil", nil, 3}, {"negative", sessionLossIntPtr(-1), 3}, {"three", sessionLossIntPtr(3), 3}, {"zero", sessionLossIntPtr(0), 0}, {"custom", sessionLossIntPtr(5), 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newIdleResumeTestServer(t, &types.LivenessConfig{SessionLossMaxConsecutive: tc.configured})
			if got := s.livenessSettings().sessionLossMax; got != tc.want {
				t.Fatalf("max=%d want %d", got, tc.want)
			}
		})
	}
}

func TestSessionLossLoopGuardResetsOnRetry(t *testing.T) {
	f := newSessionLossFixture(t, nil)
	f.send(t, "session_rotated", 1, 1, 1)
	if _, err := f.db.Exec(`UPDATE claws SET status='offline' WHERE id=?`, f.clawID); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.s.resetClawForRetry("test-tenant-id", f.clawID, "", "pending", ""); err != nil || !ok {
		t.Fatalf("retry reset: ok=%v err=%v", ok, err)
	}
	if streak, mark, _ := f.state(t); streak != 0 || mark != "" {
		t.Fatalf("retry left streak=%d mark=%q", streak, mark)
	}
}

func TestSessionLossProgressMarkDistinguishesOutputWithSameTimestamp(t *testing.T) {
	f := newSessionLossFixture(t, nil)
	at := now()
	f.output(t, "First step", at)
	before := f.s.sessionLossProgressMark(f.clawID)
	f.output(t, "Second step", at)
	after := f.s.sessionLossProgressMark(f.clawID)
	if before == after {
		t.Fatal("new output sharing a timestamp was ignored")
	}
	for _, content := range []string{"  ", types.BridgeErrorPrefix + " failed", types.BridgeReplayErrorPrefix + " failed"} {
		f.output(t, content, at.Add(time.Second))
	}
	if got := f.s.sessionLossProgressMark(f.clawID); got != after {
		t.Fatalf("non-substantive output counted: %q != %q", got, after)
	}
	if !strings.HasPrefix(after, "implement|") {
		t.Fatalf("missing stage: %q", after)
	}
}

func sessionLossIntPtr(value int) *int { return &value }

func TestSessionLossLoopGuardRecordsOperatorEvent(t *testing.T) {
	f := newSessionLossFixture(t, nil)
	if _, err := f.db.Exec(`UPDATE claws SET task_run_id='run-session-loss' WHERE id=?`, f.clawID); err != nil {
		t.Fatal(err)
	}
	insertTaskRunAnalyticsAPIRun(t, f.db, apiRunFixture{RunID: "run-session-loss", AttemptID: "attempt-session-loss", ClawID: f.clawID, TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory, Factory: "generic", StartedAt: epochMillis(now())})
	for i := 1; i <= 3; i++ {
		f.expireThrottle(t)
		f.send(t, "session_rotated", i, i, min(i, 2))
	}
	eventuallyWatchdog(t, func() bool {
		var detail string
		err := f.db.QueryRow(`SELECT detail FROM task_run_events WHERE run_id='run-session-loss' AND event_type=? AND event_key LIKE ?`, taskRunEventAgentIdle, taskRunEventAgentIdle+":session-loss-loop:%").Scan(&detail)
		return err == nil && strings.Contains(detail, `"sessionLossStreak":3`) && strings.Contains(detail, `"sessionLossReason":"turn_timeout"`) && strings.Contains(detail, `"pipelineStage":"implement"`) && strings.Contains(detail, `"noProgressPaused":true`)
	}, "session-loss operator event")
	// Further losses while paused neither advance the streak nor notify twice.
	if !f.s.sessionLossLoopGuard(f.clawID, types.SessionLossReasonTurnTimeout, "") {
		t.Fatal("paused guard allowed injection")
	}
	if streak, _, paused := f.state(t); streak != 3 || !paused {
		t.Fatalf("paused guard changed state: %d %v", streak, paused)
	}
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM task_run_events WHERE run_id='run-session-loss' AND event_type=?`, taskRunEventAgentIdle).Scan(&n); err != nil || n != 1 {
		t.Fatalf("operator event count=%d err=%v", n, err)
	}
}

func TestSessionLossLoopGuardState(t *testing.T) {
	for _, scenario := range []string{"pause", "new output", "same timestamp", "bridge error", "stage transition", "human input", "retry", "disabled", "persisted", "operator event"} {
		t.Run(scenario, func(t *testing.T) {
			max := 3
			if scenario == "disabled" {
				max = 0
			}
			s, db := NewTestServerWithConfig(t, &types.HubConfig{Liveness: &types.LivenessConfig{SessionLossMaxConsecutive: &max}}, "", "", "")
			id := "session-loss-unit"
			if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,status,bootstrap_ok,pipeline_stage,created_at) VALUES(?,?,?,?,?,?,?)`, id, "test-tenant-id", id, "connected", 1, "implement", now()); err != nil {
				t.Fatal(err)
			}
			f := sessionLossFixture{s: s, db: db, clawID: id}
			at := now()
			f.output(t, "Initial work", at)
			if scenario == "operator event" {
				if _, err := db.Exec(`UPDATE claws SET task_run_id='run-session-unit' WHERE id=?`, id); err != nil {
					t.Fatal(err)
				}
				insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: "run-session-unit", AttemptID: "attempt-session-unit", ClawID: id, TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory, Factory: "generic", StartedAt: epochMillis(now())})
			}
			for i := 0; i < 2; i++ {
				if s.sessionLossLoopGuard(id, types.SessionLossReasonTurnTimeout, "") {
					t.Fatal("guard paused too early")
				}
			}
			wantPause := true
			wantStreak := 3
			switch scenario {
			case "new output":
				f.output(t, "Completed next step", at.Add(time.Second))
				wantPause = false
				wantStreak = 1
			case "same timestamp":
				f.output(t, "Completed next step", at)
				wantPause = false
				wantStreak = 1
			case "bridge error":
				f.output(t, types.BridgeErrorPrefix+" failed", at.Add(time.Second))
			case "stage transition":
				if !s.claimPipelineStageTransition(id, "review") {
					t.Fatal("transition failed")
				}
				wantPause = false
				wantStreak = 1
			case "human input":
				s.resumeNoProgressAfterUserInput(id)
				wantPause = false
				wantStreak = 1
			case "retry":
				if _, err := db.Exec(`UPDATE claws SET status='offline' WHERE id=?`, id); err != nil {
					t.Fatal(err)
				}
				if ok, err := s.resetClawForRetry("test-tenant-id", id, "", "pending", ""); err != nil || !ok {
					t.Fatalf("retry: %v %v", ok, err)
				}
				wantPause = false
				wantStreak = 1
			case "disabled":
				wantPause = false
				wantStreak = 0
			case "persisted":
				// Connection replacement does not carry or reset the durable streak.
				s.mu.Lock()
				s.claws[id] = &clawConn{id: id, tenantID: "test-tenant-id"}
				s.mu.Unlock()
			}
			if got := s.sessionLossLoopGuard(id, types.SessionLossReasonTurnTimeout, ""); got != wantPause {
				t.Fatalf("guard=%v want %v", got, wantPause)
			}
			if streak, mark, paused := f.state(t); streak != wantStreak || paused != wantPause || (wantStreak > 0 && mark == "") {
				t.Fatalf("streak=%d mark=%q paused=%v", streak, mark, paused)
			}
			if scenario == "operator event" {
				var detail string
				if err := db.QueryRow(`SELECT detail FROM task_run_events WHERE run_id='run-session-unit' AND event_type=?`, taskRunEventAgentIdle).Scan(&detail); err != nil {
					t.Fatal(err)
				}
				for _, field := range []string{`"sessionLossStreak":3`, `"sessionLossReason":"turn_timeout"`, `"noProgressPaused":true`} {
					if !strings.Contains(detail, field) {
						t.Fatalf("missing %s in %s", field, detail)
					}
				}
			}
		})
	}
}

func TestSessionLossLoopGuardIgnoresInterruptedOutput(t *testing.T) {
	for _, kind := range []string{"session_rotated", "session_preserved"} {
		t.Run(kind, func(t *testing.T) {
			f := newSessionLossFixture(t, nil)
			for i := 1; i <= 3; i++ {
				f.cc.mu.Lock()
				f.cc.streamingMsgID = fmt.Sprintf("interrupted-output-%d", i)
				f.cc.mu.Unlock()
				if _, err := f.db.Exec(`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,'test-tenant-id','claw',?,?) ON CONFLICT(id) DO UPDATE SET content=excluded.content, created_at=excluded.created_at`, fmt.Sprintf("interrupted-output-%d", i), f.clawID, fmt.Sprintf("Partial output %d", i), now()); err != nil {
					t.Fatal(err)
				}
				f.expireThrottle(t)
				f.send(t, kind, i, i, min(i, 2))
			}
		})
	}
}

func TestSessionLossLoopGuardInterruptedOutputState(t *testing.T) {
	for _, finalized := range []bool{false, true} {
		t.Run(fmt.Sprintf("finalized=%v", finalized), func(t *testing.T) {
			s, db, id := newSessionResumeTestServer(t)
			cc := &clawConn{id: id, tenantID: "test-tenant-id", awaitingResponse: true}
			s.claws[id] = cc
			f := sessionLossFixture{s: s, db: db, cc: cc, clawID: id}
			f.output(t, "Initial completed step", now().Add(-time.Hour))
			for i := 1; i <= 3; i++ {
				cc.streamingMsgID = fmt.Sprintf("partial-%d", i)
				if _, err := db.Exec(`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,'test-tenant-id','claw',?,?) ON CONFLICT(id) DO UPDATE SET content=excluded.content, created_at=excluded.created_at`, cc.streamingMsgID, id, fmt.Sprintf("Partial output %d", i), now()); err != nil {
					t.Fatal(err)
				}
				cc.streamingBuf.WriteString(fmt.Sprintf("Partial output %d", i))
				if err := s.flushStreamingSegment(id, "test-tenant-id", cc); err != nil {
					t.Fatal(err)
				}
				if cc.streamingMsgID != "" {
					t.Fatal("activity did not clear the current streaming ID")
				}
				if finalized && i == 3 {
					f.output(t, "Completed next step", now().Add(-time.Minute))
				}
				f.expireThrottle(t)
				s.enqueueSessionPreservedContinuation(id, types.SessionRecoveryEdge{Reason: types.SessionLossReasonTurnTimeout})
			}
			wantStreak, wantPrompts := 3, 2
			if finalized {
				wantStreak, wantPrompts = 1, 3
			}
			if streak, _, paused := f.state(t); streak != wantStreak || paused == finalized || f.count(t, sessionPreservedContinuationPrefix) != wantPrompts {
				t.Fatalf("streak=%d paused=%v prompts=%d", streak, paused, f.count(t, sessionPreservedContinuationPrefix))
			}
		})
	}
}

func TestSessionLossLoopGuardFailsOpen(t *testing.T) {
	for _, kind := range []string{"rotated", "preserved"} {
		t.Run(kind, func(t *testing.T) {
			s, db, id := newSessionResumeTestServer(t)
			// Fail only the guard's UPDATE, leaving the message queue writable.
			if _, err := db.Exec(`CREATE TRIGGER fail_session_loss_guard BEFORE UPDATE OF session_loss_streak ON claws BEGIN SELECT RAISE(FAIL, 'guard unavailable'); END`); err != nil {
				t.Fatal(err)
			}
			prefix := sessionRotatedResumePrefix
			if kind == "rotated" {
				s.enqueueSessionLostResume(id, prefix, "fail-open", types.SessionRecoveryEdge{})
			} else {
				prefix = sessionPreservedContinuationPrefix
				s.enqueueSessionPreservedContinuation(id, types.SessionRecoveryEdge{})
			}
			if prompt := sessionResumePrompt(t, db, id, prefix); !strings.HasPrefix(prompt, prefix) {
				t.Fatal("guard failure dropped resume")
			}
		})
	}
	t.Run("closed database", func(t *testing.T) {
		s, db, id := newSessionResumeTestServer(t)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if s.sessionLossLoopGuard(id, types.SessionLossReasonUnknown, "") {
			t.Fatal("guard failed closed")
		}
	})
}

func TestPausedSessionLossRecordsPendingNotice(t *testing.T) {
	for _, path := range []string{"rotated", "preserved"} {
		t.Run(path, func(t *testing.T) {
			s, db, id := newSessionResumeTestServer(t)
			cc := &clawConn{id: id, tenantID: "test-tenant-id", lastTurnFinishedAt: now()}
			s.claws[id] = cc
			if !s.pauseAutomaticContinuation(id, "[hub] Automatic continuation paused for test") {
				t.Fatal("pause failed")
			}
			switch path {
			case "rotated":
				s.noteSessionLoss(cc, id, "new-key", "session_rotated", types.SessionRecoveryEdge{})
			case "preserved":
				s.enqueueSessionPreservedContinuation(id, types.SessionRecoveryEdge{})
			}
			var notice string
			if err := db.QueryRow(`SELECT pending_session_loss_notice FROM claws WHERE id=?`, id).Scan(&notice); err != nil || notice == "" {
				t.Fatalf("notice=%q err=%v", notice, err)
			}
			if path == "preserved" {
				if strings.Contains(notice, "no memory") || !strings.Contains(notice, "history are intact") {
					t.Fatalf("incorrect preserved notice: %s", notice)
				}
			} else if !strings.Contains(notice, "no memory") {
				t.Fatalf("missing lost-session notice: %s", notice)
			}
			f := sessionLossFixture{s: s, db: db, clawID: id}
			if f.count(t, sessionRotatedResumePrefix)+f.count(t, sessionPreservedContinuationPrefix) != 0 {
				t.Fatal("paused claw received a resume")
			}
		})
	}
}

// Expire the real pending callback deterministically, without making heartbeat
// tests wait for the production grace period.
func expireHeartbeatSessionLoss(t *testing.T, s *Server, cc *clawConn, clawID string) {
	t.Helper()
	var pending *pendingHeartbeatSessionLoss
	eventuallyWatchdog(t, func() bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		if s.sessionLosses[clawID] == nil {
			return false
		}
		for _, loss := range s.sessionLosses[clawID].pending {
			pending = loss
			return true
		}
		return false
	}, "heartbeat grace scheduled")
	pending.timer.Stop()
	s.finishHeartbeatSessionLoss(cc, clawID, pending)
}

func TestSessionLossHeartbeatEdgeOrdering(t *testing.T) {
	for _, source := range []string{"gateway_session_key", "restart_count"} {
		for _, order := range []string{"heartbeat first", "edge first", "grace expires"} {
			t.Run(source+"/"+order, func(t *testing.T) {
				s, db, id := newSessionResumeTestServer(t)
				cc := &clawConn{id: id, tenantID: "test-tenant-id", awaitingResponse: true, deliveryInFlight: true}
				s.claws[id] = cc
				edge := types.SessionRecoveryEdge{SessionKey: "replacement", Reason: types.SessionLossReasonTurnTimeout, TranscriptPath: "/tmp/lost.jsonl", Digest: &types.SessionDigest{AssistantMessages: []string{"Recovered bounded step"}}}
				f := sessionLossFixture{s: s, db: db, cc: cc, clawID: id}
				var pending *pendingHeartbeatSessionLoss
				if order != "edge first" {
					s.noteSessionLoss(cc, id, edge.SessionKey, source, types.SessionRecoveryEdge{})
					s.mu.RLock()
					pending = s.sessionLosses[id].pending[edge.SessionKey]
					s.mu.RUnlock()
					if pending == nil || f.count(t, restartResumePrefix) != 0 {
						t.Fatal("heartbeat did not wait for edge metadata")
					}
					t.Cleanup(func() { pending.timer.Stop() })
				}
				if order == "heartbeat first" {
					cc.mu.Lock()
					cc.finishTurnLocked()
					cc.mu.Unlock()
					// Another heartbeat detection must not bypass the grace just
					// because the interrupted turn has now sent its empty final.
					s.noteSessionLoss(cc, id, edge.SessionKey, source, types.SessionRecoveryEdge{})
					if f.count(t, restartResumePrefix) != 0 {
						t.Fatal("second heartbeat bypassed the grace")
					}
				}
				if order == "grace expires" {
					// The bridge's empty terminal message may arrive in the window.
					cc.mu.Lock()
					cc.finishTurnLocked()
					cc.mu.Unlock()
					expireHeartbeatSessionLoss(t, s, cc, id)
				}
				s.noteSessionLoss(cc, id, edge.SessionKey, "session_rotated", edge)
				s.noteSessionLoss(cc, id, edge.SessionKey, source, types.SessionRecoveryEdge{})
				if pending != nil {
					// A timer callback already racing with Stop must also be harmless.
					s.finishHeartbeatSessionLoss(cc, id, pending)
				}
				if got := f.count(t, restartResumePrefix) + f.count(t, sessionRotatedResumePrefix); got != 1 {
					t.Fatalf("got %d resumes for one incident", got)
				}
				prefix := sessionRotatedResumePrefix
				if order == "grace expires" {
					prefix = restartResumePrefix
				}
				prompt := sessionResumePrompt(t, db, id, prefix)
				if order != "grace expires" {
					for _, want := range []string{"per-turn run limit", "/tmp/lost.jsonl", "Recovered bounded step"} {
						if !strings.Contains(prompt, want) {
							t.Fatalf("edge metadata missing %q: %s", want, prompt)
						}
					}
				}
			})
		}
	}
}

func TestSessionLossInterruptedStreamingActivityBeforeEdge(t *testing.T) {
	for _, kind := range []string{"session_rotated", "session_preserved"} {
		t.Run(kind, func(t *testing.T) {
			f := newSessionLossFixture(t, nil)
			var previousID string
			for i := 1; i <= 3; i++ {
				f.expireThrottle(t)
				partial := fmt.Sprintf("Partial step %d", i)
				if err := wsjson.Write(context.Background(), f.conn, types.WSMessage{Type: "chunk", Payload: map[string]string{"content": partial}}); err != nil {
					t.Fatal(err)
				}
				var streamID string
				eventuallyWatchdog(t, func() bool {
					return f.db.QueryRow(`SELECT id FROM messages WHERE claw_id=? AND content=?`, f.clawID, partial).Scan(&streamID) == nil
				}, "new partial persisted")
				if streamID == previousID {
					t.Fatal("interrupted turns reused a streaming ID")
				}
				previousID = streamID
				if err := wsjson.Write(context.Background(), f.conn, types.WSMessage{Type: "agent_activity", Payload: map[string]string{"kind": kind, "message": "Turn interrupted"}}); err != nil {
					t.Fatal(err)
				}
				eventuallyWatchdog(t, func() bool {
					f.cc.mu.RLock()
					defer f.cc.mu.RUnlock()
					return f.cc.streamingMsgID == "" && f.cc.streamingSplit
				}, "activity flushed partial before edge")
				f.send(t, kind, i, i, min(i, 2))
				if err := wsjson.Write(context.Background(), f.conn, types.WSMessage{Type: "message", Payload: types.HubMessage{Content: ""}}); err != nil {
					t.Fatal(err)
				}
				eventuallyWatchdog(t, func() bool {
					f.cc.mu.RLock()
					defer f.cc.mu.RUnlock()
					return !f.cc.streamingSplit
				}, "empty final processed")
			}
		})
	}
}

func TestSessionLossCompletedReplyProgress(t *testing.T) {
	for _, outcome := range []string{"completed", "split completed", "empty", "bridge error", "interrupted"} {
		t.Run(outcome, func(t *testing.T) {
			f := newSessionLossFixture(t, nil)
			before := f.s.sessionLossProgressMark(f.clawID)
			f.cc.mu.Lock()
			f.cc.awaitingResponse = true
			f.cc.turnInputMessageID = "input"
			f.cc.mu.Unlock()
			if err := wsjson.Write(context.Background(), f.conn, types.WSMessage{Type: "chunk", Payload: map[string]string{"content": "Streamed work"}}); err != nil {
				t.Fatal(err)
			}
			if outcome == "split completed" {
				if err := wsjson.Write(context.Background(), f.conn, types.WSMessage{Type: "agent_activity", Payload: map[string]string{"kind": "tool", "tool": "exec", "phase": "start"}}); err != nil {
					t.Fatal(err)
				}
			}
			reply := "Completed bounded step"
			switch outcome {
			case "empty":
				reply = ""
			case "bridge error":
				reply = types.BridgeErrorPrefix + " failed"
			case "interrupted":
				if err := wsjson.Write(context.Background(), f.conn, types.WSMessage{Type: "session_preserved", Payload: types.SessionRecoveryEdge{Reason: types.SessionLossReasonTurnTimeout, InterruptedMessageID: "input"}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := wsjson.Write(context.Background(), f.conn, types.WSMessage{Type: "message", Payload: types.HubMessage{Content: reply}}); err != nil {
				t.Fatal(err)
			}
			// A following chunk is processed only after all final-message writes.
			if err := wsjson.Write(context.Background(), f.conn, types.WSMessage{Type: "chunk", Payload: map[string]string{"content": "Next turn sentinel"}}); err != nil {
				t.Fatal(err)
			}
			eventuallyWatchdog(t, func() bool {
				var n int
				return f.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND content='Next turn sentinel'`, f.clawID).Scan(&n) == nil && n == 1
			}, "terminal message processed")
			changed := f.s.sessionLossProgressMark(f.clawID) != before
			if want := outcome == "completed" || outcome == "split completed"; changed != want {
				t.Fatalf("progress changed=%v for %s", changed, outcome)
			}
		})
	}
}

func TestSessionLossGuardPauseRetainsTriggeringRecoveryContext(t *testing.T) {
	for _, preserved := range []bool{false, true} {
		t.Run(fmt.Sprintf("preserved=%v", preserved), func(t *testing.T) {
			s, db, id := newSessionResumeTestServer(t)
			f := sessionLossFixture{s: s, db: db, clawID: id}
			edge := types.SessionRecoveryEdge{Reason: types.SessionLossReasonTurnTimeout, TranscriptPath: "/tmp/triggering-loss.jsonl", Digest: &types.SessionDigest{AssistantMessages: []string{"Triggering loss context"}}}
			seedSessionResumeMessage(t, db, id, "user", "Implement the requested fix", now())
			for i := 0; i < 3; i++ {
				f.expireThrottle(t)
				if preserved {
					s.enqueueSessionPreservedContinuation(id, edge)
				} else {
					s.enqueueSessionLostResume(id, sessionRotatedResumePrefix, fmt.Sprintf("loss-%d", i), edge)
				}
			}
			var notice string
			if err := db.QueryRow(`SELECT pending_session_loss_notice FROM claws WHERE id=?`, id).Scan(&notice); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"per-turn run limit", "/tmp/triggering-loss.jsonl"} {
				if !strings.Contains(notice, want) {
					t.Fatalf("pause omitted triggering context %q: %s", want, notice)
				}
			}
			if preserved {
				if strings.Contains(notice, "no memory") || !strings.Contains(notice, "history are intact") {
					t.Fatal(notice)
				}
			} else if !strings.Contains(notice, "Triggering loss context") || !strings.Contains(notice, "Implement the requested fix") {
				t.Fatal(notice)
			}
			if _, _, paused := f.state(t); !paused || f.count(t, sessionRotatedResumePrefix)+f.count(t, sessionPreservedContinuationPrefix) != 2 {
				t.Fatal("third loss did not pause without injecting")
			}
		})
	}
}

func TestSessionLossOnlyInterruptsObservedTurn(t *testing.T) {
	for _, scenario := range []string{"idle heartbeat", "error ended then edge", "interrupted"} {
		t.Run(scenario, func(t *testing.T) {
			f := newSessionLossFixture(t, nil)
			f.cc.mu.Lock()
			f.cc.resetTurnStateLocked()
			f.cc.lastTurnFinishedAt = time.Time{}
			f.cc.deliveryInFlight = true // Keep recovery prompts queued for this test.
			f.cc.mu.Unlock()
			send := func(kind string, payload any) {
				t.Helper()
				if err := wsjson.Write(context.Background(), f.conn, types.WSMessage{Type: kind, Payload: payload}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "error ended then edge" {
				f.cc.mu.Lock()
				f.cc.awaitingResponse = true
				f.cc.turnInputMessageID = "input"
				f.cc.mu.Unlock()
				send("message", types.HubMessage{Content: types.BridgeErrorPrefix + " gateway disconnected"})
				eventuallyWatchdog(t, func() bool {
					f.cc.mu.RLock()
					defer f.cc.mu.RUnlock()
					return !f.cc.awaitingResponse
				}, "error turn ended")
			}
			if scenario == "interrupted" {
				f.cc.mu.Lock()
				f.cc.awaitingResponse = true
				f.cc.turnInputMessageID = "input"
				f.cc.mu.Unlock()
			}
			if scenario == "idle heartbeat" {
				send("heartbeat", map[string]any{"gateway_healthy": true, "restart_count": 0, "gateway_session_key": "old"})
				send("heartbeat", map[string]any{"gateway_healthy": true, "restart_count": 1, "gateway_session_key": "loss"})
			} else {
				send("session_rotated", types.SessionRecoveryEdge{SessionKey: "loss", InterruptedMessageID: "input"})
			}
			eventuallyWatchdog(t, func() bool {
				f.s.mu.RLock()
				defer f.s.mu.RUnlock()
				losses := f.s.sessionLosses[f.clawID]
				return losses != nil && losses.announced["loss"]
			}, "loss observed")
			before := f.s.sessionLossProgressMark(f.clawID)
			f.cc.mu.Lock()
			f.cc.awaitingResponse = true
			f.cc.turnInputMessageID = "input"
			f.cc.mu.Unlock()
			reply := "Completed the recovery work"
			send("message", types.HubMessage{Content: reply})
			eventuallyWatchdog(t, func() bool {
				var n int
				return f.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND content=?`, f.clawID, reply).Scan(&n) == nil && n == 1
			}, "reply processed")
			if changed := f.s.sessionLossProgressMark(f.clawID) != before; changed != (scenario != "interrupted") {
				t.Fatalf("progress changed=%v for %s", changed, scenario)
			}
		})
	}
}

func TestSessionLossEdgeClaimsAllPendingKeys(t *testing.T) {
	for _, scenario := range []string{"restart reports old key", "second rotation", "heartbeat after terminal"} {
		t.Run(scenario, func(t *testing.T) {
			s, db, id := newSessionResumeTestServer(t)
			cc := &clawConn{id: id, tenantID: "test-tenant-id", awaitingResponse: true, deliveryInFlight: true}
			s.claws[id] = cc
			f := sessionLossFixture{s: s, db: db, cc: cc, clawID: id}
			if scenario == "heartbeat after terminal" {
				cc.finishTurnLocked()
			}
			s.noteSessionLoss(cc, id, "A", "restart_count", types.SessionRecoveryEdge{})
			s.noteSessionLoss(cc, id, "B", "gateway_session_key", types.SessionRecoveryEdge{})
			s.mu.RLock()
			var pending []*pendingHeartbeatSessionLoss
			for _, loss := range s.sessionLosses[id].pending {
				pending = append(pending, loss)
			}
			s.mu.RUnlock()
			if len(pending) != 2 || f.count(t, restartResumePrefix) != 0 {
				t.Fatal("heartbeat did not wait for enriched edge")
			}
			key := "B"
			if scenario == "second rotation" {
				key = "C"
			}
			edge := types.SessionRecoveryEdge{SessionKey: key, Reason: types.SessionLossReasonTurnTimeout, TranscriptPath: "/tmp/lost.jsonl"}
			s.noteSessionLoss(cc, id, key, "session_rotated", edge)
			f.expireThrottle(t) // A duplicate must be rejected without relying on throttling.
			for _, loss := range pending {
				s.finishHeartbeatSessionLoss(cc, id, loss)
				s.noteSessionLoss(cc, id, loss.key, loss.source, types.SessionRecoveryEdge{})
			}
			if streak, _, paused := f.state(t); streak != 1 || paused || f.count(t, restartResumePrefix)+f.count(t, sessionRotatedResumePrefix) != 1 {
				t.Fatalf("incident counted more than once: streak=%d paused=%v", streak, paused)
			}
			if prompt := sessionResumePrompt(t, db, id, sessionRotatedResumePrefix); !strings.Contains(prompt, "/tmp/lost.jsonl") {
				t.Fatal("enriched context lost")
			}
		})
	}
}

func TestSessionLossGraceSurvivesWebSocketReconnect(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired=%v", expired), func(t *testing.T) {
			f := newSessionLossFixture(t, nil)
			f.cc.mu.Lock()
			f.cc.awaitingResponse = true
			f.cc.turnInputMessageID = "input"
			f.cc.deliveryInFlight = true
			f.cc.mu.Unlock()
			f.s.noteSessionLoss(f.cc, f.clawID, "replacement", "gateway_session_key", types.SessionRecoveryEdge{})
			f.s.mu.RLock()
			pending := f.s.sessionLosses[f.clawID].pending["replacement"]
			f.s.mu.RUnlock()
			t.Cleanup(func() { pending.timer.Stop() })
			if expired {
				expireHeartbeatSessionLoss(t, f.s, f.cc, f.clawID)
			}
			_ = f.conn.Close(websocket.StatusNormalClosure, "reconnect")
			eventuallyWatchdog(t, func() bool {
				f.s.mu.RLock()
				removed := f.s.claws[f.clawID] == nil
				f.s.mu.RUnlock()
				var status string
				return removed && f.db.QueryRow(`SELECT status FROM claws WHERE id=?`, f.clawID).Scan(&status) == nil && status == "offline"
			}, "old socket removed")
			if !expired && f.count(t, restartResumePrefix) != 0 {
				t.Fatal("socket close prematurely flushed metadata grace")
			}
			conn := watchdogClaw(t, f.s, f.clawID)
			f.expireThrottle(t)
			edge := types.SessionRecoveryEdge{SessionKey: "replacement", Reason: types.SessionLossReasonTurnTimeout, TranscriptPath: "/tmp/reconnect.jsonl", InterruptedMessageID: "input"}
			if err := wsjson.Write(context.Background(), conn, types.WSMessage{Type: "session_rotated", Payload: edge}); err != nil {
				t.Fatal(err)
			}
			// A later chunk proves the replayed edge was fully processed.
			if err := wsjson.Write(context.Background(), conn, types.WSMessage{Type: "chunk", Payload: map[string]string{"content": "Replay sentinel"}}); err != nil {
				t.Fatal(err)
			}
			eventuallyWatchdog(t, func() bool {
				var n int
				return f.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND content='Replay sentinel'`, f.clawID).Scan(&n) == nil && n == 1
			}, "replayed edge processed")
			f.s.finishHeartbeatSessionLoss(f.cc, f.clawID, pending)
			// When the grace window already expired, the heartbeat itself claimed
			// the key with a metadata-less restart resume (streak untouched, since
			// only edges count toward the streak); the later edge for that same
			// key is then a duplicate and never reaches the loop guard.
			wantStreak := 1
			if expired {
				wantStreak = 0
			}
			if streak, _, paused := f.state(t); streak != wantStreak || paused || f.count(t, restartResumePrefix)+f.count(t, sessionRotatedResumePrefix) != 1 {
				t.Fatalf("reconnect duplicated incident: streak=%d paused=%v", streak, paused)
			}
			if !expired {
				if prompt := sessionResumePrompt(t, f.db, f.clawID, sessionRotatedResumePrefix); !strings.Contains(prompt, "/tmp/reconnect.jsonl") {
					t.Fatal("reconnect dropped enriched context")
				}
			}
		})
	}
}

func TestSessionLossGraceExpiryWhileDisconnectedKeepsIncident(t *testing.T) {
	s, db, id := newSessionResumeTestServer(t)
	cc := &clawConn{id: id, tenantID: "test-tenant-id", awaitingResponse: true, deliveryInFlight: true}
	s.claws[id] = cc
	f := sessionLossFixture{s: s, db: db, cc: cc, clawID: id}
	s.noteSessionLoss(cc, id, "replacement", "gateway_session_key", types.SessionRecoveryEdge{})
	s.mu.Lock()
	pending := s.sessionLosses[id].pending["replacement"]
	delete(s.claws, id)
	s.mu.Unlock()
	pending.timer.Stop()
	if _, err := db.Exec(`UPDATE claws SET status='offline' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	s.finishHeartbeatSessionLoss(cc, id, pending)
	s.mu.RLock()
	retained := s.sessionLosses[id].pending["replacement"] == pending && !s.sessionLosses[id].announced["replacement"]
	s.mu.RUnlock()
	if !retained || f.count(t, restartResumePrefix) != 0 {
		t.Fatal("offline expiry consumed the incident")
	}
	t.Cleanup(func() { pending.timer.Stop() })
	cc = &clawConn{id: id, tenantID: "test-tenant-id", deliveryInFlight: true}
	s.mu.Lock()
	s.claws[id] = cc
	s.mu.Unlock()
	if _, err := db.Exec(`UPDATE claws SET status='connected' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	s.noteSessionLoss(cc, id, "replacement", "session_rotated", types.SessionRecoveryEdge{})
	s.finishHeartbeatSessionLoss(cc, id, pending)
	if streak, _, paused := f.state(t); streak != 1 || paused || f.count(t, sessionRotatedResumePrefix) != 1 {
		t.Fatalf("replayed incident lost or duplicated: streak=%d paused=%v", streak, paused)
	}
}

func TestSessionLossDelayedEdgeDoesNotInterruptNextTurn(t *testing.T) {
	s, db, id := newSessionResumeTestServer(t)
	cc := &clawConn{id: id, tenantID: "test-tenant-id", awaitingResponse: true, turnInputMessageID: "T1", deliveryInFlight: true}
	s.claws[id] = cc
	before := s.sessionLossProgressMark(id)
	// T1's disconnect reply ends the turn, then a queued T2 is delivered.
	s.recordSessionLossCompletedTurn(id, types.BridgeErrorPrefix+" gateway disconnected")
	cc.finishTurnLocked()
	cc.awaitingResponse = true
	cc.turnInputMessageID = "T2"
	cc.streamingStartedAt = time.Now()
	cc.streamingMsgID = "T2-output"
	s.noteSessionLoss(cc, id, "B", "gateway_session_key", types.SessionRecoveryEdge{})
	s.noteSessionLoss(cc, id, "B", "session_rotated", types.SessionRecoveryEdge{
		SessionKey: "B", PreviousSessionKey: "A", InterruptedMessageID: "T1",
	})
	if cc.sessionLossInterrupted {
		t.Fatal("T1's delayed loss interrupted T2")
	}
	// Match the terminal-response path: only non-interrupted replies count.
	completedNormally := !cc.sessionLossInterrupted
	cc.finishTurnLocked()
	if completedNormally {
		s.recordSessionLossCompletedTurn(id, "Completed T2 successfully")
	}
	if s.sessionLossProgressMark(id) == before {
		t.Fatal("T2's healthy reply did not count as progress")
	}
	f := sessionLossFixture{s: s, db: db, cc: cc, clawID: id}
	f.expireThrottle(t)
	s.enqueueSessionRotatedResume(id, types.SessionRecoveryEdge{})
	if streak, _, paused := f.state(t); streak != 1 || paused {
		t.Fatalf("T2 did not reset loss streak: streak=%d paused=%v", streak, paused)
	}
}

func TestInterruptSessionLossTurnUsesIdentity(t *testing.T) {
	observed := time.Now()
	for _, tc := range []struct {
		name, messageID string
		started         time.Time
		busy, want      bool
	}{
		{"matching input", "T2", observed.Add(time.Second), true, true},
		{"earlier input", "T1", observed.Add(-time.Second), true, false},
		{"legacy already open", "", observed.Add(-time.Second), true, false},
		{"legacy later turn", "", observed.Add(time.Second), true, false},
		{"idle", "T2", time.Time{}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cc := &clawConn{turnInputMessageID: "T2", awaitingResponse: tc.busy, streamingStartedAt: tc.started}
			cc.interruptSessionLossTurnLocked(tc.messageID)
			if got := cc.sessionLossInterrupted; got != tc.want {
				t.Fatalf("interrupted=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestSessionLossHeartbeatOnlyThenDistinctEdge(t *testing.T) {
	s, db, id := newSessionResumeTestServer(t)
	cc := &clawConn{id: id, tenantID: "test-tenant-id", awaitingResponse: true, turnInputMessageID: "T1", deliveryInFlight: true}
	s.claws[id] = cc
	f := sessionLossFixture{s: s, db: db, cc: cc, clawID: id}
	// Heartbeat observes A -> B without an enriched edge.
	s.noteSessionLoss(cc, id, "B", "gateway_session_key", types.SessionRecoveryEdge{})
	expireHeartbeatSessionLoss(t, s, cc, id)
	if streak, _, paused := f.state(t); streak != 0 || paused || cc.sessionLossInterrupted || f.count(t, restartResumePrefix) != 1 {
		t.Fatalf("heartbeat affected the loop guard: streak=%d paused=%v", streak, paused)
	}
	// B -> C is a genuine loss, even though B was announced by the fallback.
	edge := types.SessionRecoveryEdge{SessionKey: "C", PreviousSessionKey: "B", InterruptedMessageID: "T1"}
	s.noteSessionLoss(cc, id, "C", "session_rotated", edge)
	if streak, _, paused := f.state(t); streak != 1 || paused || !cc.sessionLossInterrupted || f.count(t, sessionRotatedResumePrefix) != 1 {
		t.Fatalf("distinct edge was lost: streak=%d paused=%v", streak, paused)
	}
	f.expireThrottle(t)
	// Announced keys survive reconnect, and a replay must not interrupt T2.
	fresh := &clawConn{id: id, tenantID: "test-tenant-id", awaitingResponse: true, turnInputMessageID: "T2", deliveryInFlight: true}
	s.claws[id] = fresh
	s.noteSessionLoss(fresh, id, "C", "session_rotated", edge)
	if streak, _, paused := f.state(t); streak != 1 || paused || fresh.sessionLossInterrupted || f.count(t, sessionRotatedResumePrefix) != 1 {
		t.Fatalf("replayed edge was not a no-op: streak=%d paused=%v", streak, paused)
	}
}

func TestSessionLossOldKeyExpiryAfterRegistrationWaitsForReplay(t *testing.T) {
	s, db, id := newSessionResumeTestServer(t)
	cc := &clawConn{id: id, tenantID: "test-tenant-id", awaitingResponse: true, deliveryInFlight: true}
	s.claws[id] = cc
	s.noteSessionLoss(cc, id, "A", "restart_count", types.SessionRecoveryEdge{})
	pending := s.sessionLosses[id].pending["A"]
	pending.timer.Stop()
	t.Cleanup(func() { pending.timer.Stop() })
	// The old socket is replaced before its metadata timer fires.
	fresh := &clawConn{id: id, tenantID: "test-tenant-id", connectedAt: time.Now(), deliveryInFlight: true}
	s.claws[id] = fresh
	s.finishHeartbeatSessionLoss(cc, id, pending)
	f := sessionLossFixture{s: s, db: db, cc: fresh, clawID: id}
	if s.sessionLosses[id].pending["A"] != pending || f.count(t, restartResumePrefix) != 0 {
		t.Fatal("fresh registration consumed the old-key incident before replay")
	}
	s.noteSessionLoss(fresh, id, "B", "session_rotated", types.SessionRecoveryEdge{SessionKey: "B", PreviousSessionKey: "A", TranscriptPath: "/tmp/replayed.jsonl"})
	s.finishHeartbeatSessionLoss(cc, id, pending)
	if streak, _, paused := f.state(t); streak != 1 || paused || f.count(t, sessionRotatedResumePrefix) != 1 {
		t.Fatalf("replayed incident lost or duplicated: streak=%d paused=%v", streak, paused)
	}
	if !strings.Contains(sessionResumePrompt(t, db, id, sessionRotatedResumePrefix), "/tmp/replayed.jsonl") {
		t.Fatal("replayed context lost")
	}
}

func TestSessionLossRepeatedPreviousKeyDoesNotDeduplicate(t *testing.T) {
	s, db, id := newSessionResumeTestServer(t)
	cc := &clawConn{id: id, tenantID: "test-tenant-id", awaitingResponse: true, deliveryInFlight: true}
	s.claws[id] = cc
	f := sessionLossFixture{s: s, db: db, cc: cc, clawID: id}
	s.noteSessionLoss(cc, id, "B", "session_rotated", types.SessionRecoveryEdge{PreviousSessionKey: "A"})
	f.expireThrottle(t)
	// Only the new key identifies a duplicate; the previous key is irrelevant.
	s.noteSessionLoss(cc, id, "C", "session_rotated", types.SessionRecoveryEdge{PreviousSessionKey: "A"})
	if streak, _, paused := f.state(t); streak != 2 || paused || f.count(t, sessionRotatedResumePrefix) != 2 {
		t.Fatalf("different new key was suppressed: streak=%d paused=%v", streak, paused)
	}
}

func TestSessionLossPendingTimerStops(t *testing.T) {
	for _, status := range []string{"offline", "deleted"} {
		t.Run(status, func(t *testing.T) {
			s, db, id := newSessionResumeTestServer(t)
			cc := &clawConn{id: id, awaitingResponse: true, deliveryInFlight: true}
			s.claws[id] = cc
			s.noteSessionLoss(cc, id, "B", "gateway_session_key", types.SessionRecoveryEdge{})
			pending := s.sessionLosses[id].pending["B"]
			pending.timer.Stop()
			delete(s.claws, id)
			if _, err := db.Exec(`UPDATE claws SET status=? WHERE id=?`, status, id); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 4; i++ {
				s.finishHeartbeatSessionLoss(cc, id, pending)
				pending.timer.Stop()
			}
			if losses := s.sessionLosses[id]; losses != nil && (status == "deleted" || len(losses.pending) != 0) {
				t.Fatal("timer retained the deleted claw or kept retrying while offline")
			}
			lastTimer := pending.timer
			s.finishHeartbeatSessionLoss(cc, id, pending)
			if pending.timer != lastTimer {
				t.Fatal("stale callback re-armed the timer")
			}
		})
	}
}

func TestSessionLossPreservedEdgeIdentityAndReplay(t *testing.T) {
	s, db, id := newSessionResumeTestServer(t)
	cc := &clawConn{id: id, tenantID: "test-tenant-id", awaitingResponse: true, turnInputMessageID: "T2", deliveryInFlight: true}
	s.claws[id] = cc
	f := sessionLossFixture{s: s, db: db, cc: cc, clawID: id}
	edge := types.SessionRecoveryEdge{SessionKey: "B", InterruptedMessageID: "T1", Reason: types.SessionLossReasonTurnTimeout}
	s.noteSessionLoss(cc, id, "B", "gateway_session_key", types.SessionRecoveryEdge{})
	s.noteSessionLoss(cc, id, "B", "session_preserved", edge)
	if cc.sessionLossInterrupted || f.count(t, restartResumePrefix) != 0 || f.count(t, sessionPreservedContinuationPrefix) != 1 {
		t.Fatal("preserved edge interrupted T2 or duplicated the heartbeat resume")
	}
	if !strings.Contains(sessionResumePrompt(t, db, id, sessionPreservedContinuationPrefix), "per-turn run limit") {
		t.Fatal("preserved edge metadata was lost")
	}
	f.expireThrottle(t)
	// Even a replay naming the current turn must be a no-op for an announced key.
	edge.InterruptedMessageID = "T2"
	s.noteSessionLoss(cc, id, "B", "session_preserved", edge)
	if streak, _, paused := f.state(t); streak != 1 || paused || cc.sessionLossInterrupted || f.count(t, sessionPreservedContinuationPrefix) != 1 {
		t.Fatalf("preserved replay was not a no-op: streak=%d paused=%v", streak, paused)
	}
	// Without a new key, preserved edges retain their historical throttle behavior.
	edge.SessionKey = ""
	s.noteSessionLoss(cc, id, "", "session_preserved", edge)
	if !cc.sessionLossInterrupted || f.count(t, sessionPreservedContinuationPrefix) != 2 {
		t.Fatal("matching preserved edge did not interrupt its turn")
	}
}
