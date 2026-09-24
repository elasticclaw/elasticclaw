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
	f.s.noteSessionLoss(f.cc, f.clawID, "key-2", "session_rotated", types.SessionRecoveryEdge{Reason: types.SessionLossReasonTurnTimeout}, "")
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
				f.cc.streamingMsgID = "interrupted-output"
				f.cc.mu.Unlock()
				if _, err := f.db.Exec(`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,'test-tenant-id','claw',?,?) ON CONFLICT(id) DO UPDATE SET content=excluded.content, created_at=excluded.created_at`, "interrupted-output", f.clawID, fmt.Sprintf("Partial output %d", i), now()); err != nil {
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
			cc := &clawConn{id: id, tenantID: "test-tenant-id", streamingMsgID: "partial"}
			s.claws[id] = cc
			f := sessionLossFixture{s: s, db: db, cc: cc, clawID: id}
			f.output(t, "Initial completed step", now().Add(-time.Hour))
			for i := 1; i <= 3; i++ {
				if _, err := db.Exec(`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,'test-tenant-id','claw',?,?) ON CONFLICT(id) DO UPDATE SET content=excluded.content, created_at=excluded.created_at`, cc.streamingMsgID, id, fmt.Sprintf("Partial output %d", i), now()); err != nil {
					t.Fatal(err)
				}
				if finalized && i == 3 {
					f.output(t, "Completed next step", now().Add(-time.Minute))
				}
				f.expireThrottle(t)
				s.enqueueSessionPreservedContinuation(id, types.SessionRecoveryEdge{Reason: types.SessionLossReasonTurnTimeout}, cc.streamingMsgID)
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
				s.enqueueSessionLostResume(id, prefix, "fail-open", types.SessionRecoveryEdge{}, "")
			} else {
				prefix = sessionPreservedContinuationPrefix
				s.enqueueSessionPreservedContinuation(id, types.SessionRecoveryEdge{}, "")
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
	for _, path := range []string{"rotated", "preserved", "guard"} {
		t.Run(path, func(t *testing.T) {
			s, db, id := newSessionResumeTestServer(t)
			cc := &clawConn{id: id, tenantID: "test-tenant-id", lastTurnFinishedAt: now()}
			s.claws[id] = cc
			if !s.pauseAutomaticContinuation(id, "[hub] Automatic continuation paused for test") {
				t.Fatal("pause failed")
			}
			switch path {
			case "rotated":
				s.noteSessionLoss(cc, id, "new-key", "session_rotated", types.SessionRecoveryEdge{}, "")
			case "preserved":
				s.enqueueSessionPreservedContinuation(id, types.SessionRecoveryEdge{}, "")
			case "guard":
				if !s.sessionLossLoopGuard(id, types.SessionLossReasonUnknown, "") {
					t.Fatal("paused guard allowed resume")
				}
			}
			var notice string
			if err := db.QueryRow(`SELECT pending_session_loss_notice FROM claws WHERE id=?`, id).Scan(&notice); err != nil || notice != s.sessionLossPendingNoticeFor(id) {
				t.Fatalf("notice=%q err=%v", notice, err)
			}
			f := sessionLossFixture{s: s, db: db, clawID: id}
			if f.count(t, sessionRotatedResumePrefix)+f.count(t, sessionPreservedContinuationPrefix) != 0 {
				t.Fatal("paused claw received a resume")
			}
		})
	}
}
