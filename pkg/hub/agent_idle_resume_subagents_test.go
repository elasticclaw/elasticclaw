package hub

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

func lastIdleResumeMessage(t *testing.T, db *sql.DB, clawID string) string {
	t.Helper()
	var content string
	if err := db.QueryRow(`SELECT content FROM messages WHERE claw_id=? AND role='hub' AND content LIKE ? ORDER BY rowid DESC LIMIT 1`, clawID, agentIdleResumePrefix+"%").Scan(&content); err != nil {
		t.Fatalf("read resume message for %s: %v", clawID, err)
	}
	return content
}

func subagentResumeTestClaw(t *testing.T, db *sql.DB, clawID string, parentIdle time.Duration) *clawConn {
	t.Helper()
	insertSlackTestClaw(t, db, clawID, "connected", 0, "", oldEnough)
	setClawPipelineStage(t, db, clawID, "implement")
	cc := idleTestConn(clawID, parentIdle)
	cc.turnBoundarySeen = true
	return cc
}

// A parent parked on sessions_yield is waiting on work, not stalled: the
// resume must leave it alone while its subagents run, then fire
// idle_resume_after past their end with a prompt that says they finished.
func TestAgentIdleResumeWaitsForRunningSubagents(t *testing.T) {
	s, db := newIdleResumeTestServer(t, nil)
	const clawID = "resume-subagents"
	cc := subagentResumeTestClaw(t, db, clawID, 30*time.Minute)
	cc.subagentsActiveAt = time.Now()
	cc.subagentsActiveSince = time.Now().Add(-25 * time.Minute)
	cc.subagentsLastSeenAt = cc.subagentsActiveAt

	s.checkAgentIdleResume(time.Now(), clawID, cc)
	if got := idleResumeMessages(t, db, clawID); got != 0 {
		t.Fatalf("resumed a parent whose subagents are running: %d messages", got)
	}

	// The subagents finish; the completion does not wake the parent.
	inactive, zero := false, 0
	cc.mu.Lock()
	cc.applySubagentHeartbeatLocked(&inactive, &zero)
	cc.mu.Unlock()
	s.checkAgentIdleResume(time.Now(), clawID, cc)
	if got := idleResumeMessages(t, db, clawID); got != 0 {
		t.Fatalf("resumed the moment subagents ended: %d messages", got)
	}

	s.checkAgentIdleResume(time.Now().Add(defaultIdleResumeAfter), clawID, cc)
	if got := idleResumeMessages(t, db, clawID); got != 1 {
		t.Fatalf("injected %d resume messages after subagents ended, want 1", got)
	}
	msg := lastIdleResumeMessage(t, db, clawID)
	for _, want := range []string{"subagents finished", "Collect their results", "do not spawn them again"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("resume message missing %q: %s", want, msg)
		}
	}
}

// A subagent that hangs with an active run must not park its parent forever:
// past the ceiling the resume fires and tells the agent to check on them.
func TestAgentIdleResumeFiresPastSubagentCeiling(t *testing.T) {
	s, db := newIdleResumeTestServer(t, nil)
	const clawID = "resume-subagents-stuck"
	cc := subagentResumeTestClaw(t, db, clawID, agentIdleResumeSubagentCeiling+5*time.Minute)
	cc.subagentsActiveAt = time.Now()
	cc.subagentsActiveSince = time.Now().Add(-agentIdleResumeSubagentCeiling - time.Minute)
	cc.subagentsLastSeenAt = cc.subagentsActiveAt

	s.checkAgentIdleResume(time.Now(), clawID, cc)
	if got := idleResumeMessages(t, db, clawID); got != 1 {
		t.Fatalf("injected %d resume messages past the subagent ceiling, want 1", got)
	}
	if msg := lastIdleResumeMessage(t, db, clawID); !strings.Contains(msg, "have been running for over") || !strings.Contains(msg, "degradation path") {
		t.Fatalf("stuck-subagent resume message: %s", msg)
	}
}

// Past the ceiling the parent itself must still have been idle for
// idle_resume_after: subagents running long while the parent just finished a
// turn is not a stall.
func TestAgentIdleResumeCeilingStillNeedsAnIdleParent(t *testing.T) {
	s, db := newIdleResumeTestServer(t, nil)
	const clawID = "resume-subagents-busy-parent"
	cc := subagentResumeTestClaw(t, db, clawID, time.Minute)
	cc.connectedAt = time.Now().Add(-2 * time.Hour)
	cc.subagentsActiveAt = time.Now()
	cc.subagentsActiveSince = time.Now().Add(-agentIdleResumeSubagentCeiling - time.Minute)

	s.checkAgentIdleResume(time.Now(), clawID, cc)
	if got := idleResumeMessages(t, db, clawID); got != 0 {
		t.Fatalf("resumed a parent idle for a minute: %d messages", got)
	}
}

// The heartbeat keeps the start of an unbroken run of activity, restarts it
// after a gap, and remembers the last activity past a zero report.
func TestApplySubagentHeartbeatTracksActivityRun(t *testing.T) {
	cc := &clawConn{}
	active, inactive, one, zero := true, false, 1, 0

	cc.applySubagentHeartbeatLocked(&active, &one)
	since := cc.subagentsActiveSince
	if since.IsZero() || !cc.subagentsLastSeenAt.Equal(cc.subagentsActiveAt) {
		t.Fatalf("first report: since=%v lastSeen=%v at=%v", since, cc.subagentsLastSeenAt, cc.subagentsActiveAt)
	}

	cc.applySubagentHeartbeatLocked(&active, &one)
	if !cc.subagentsActiveSince.Equal(since) {
		t.Fatal("a renewal restarted the activity run")
	}

	cc.subagentsActiveAt = time.Now().Add(-subagentsActiveFreshFor - time.Second)
	cc.applySubagentHeartbeatLocked(&active, &one)
	if !cc.subagentsActiveSince.After(since) {
		t.Fatal("a report after a gap did not start a new activity run")
	}

	lastSeen := cc.subagentsLastSeenAt
	cc.applySubagentHeartbeatLocked(&inactive, &zero)
	if !cc.subagentsActiveSince.IsZero() || !cc.subagentsActiveAt.IsZero() {
		t.Fatal("zero report left the activity run open")
	}
	if !cc.subagentsLastSeenAt.Equal(lastSeen) {
		t.Fatal("zero report forgot when subagents were last seen")
	}
}
