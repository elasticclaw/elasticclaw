package hub

import (
	"testing"
	"time"
)

// deferTestConn is an idle connection whose turns the hub has watched end, so
// the auto-resume is not blind to it.
func deferTestConn(clawID string, idleFor time.Duration) *clawConn {
	cc := idleTestConn(clawID, idleFor)
	cc.turnBoundarySeen = true
	return cc
}

// With the auto-resume on (default 10m), the alert waits for it: a stretch
// shorter than resume + idle_after is the resume's to fix, and only a stretch
// that outlives an unanswered resume alerts.
func TestAgentIdleAlertDefersToAutoResume(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	const clawID = "idle-defer"
	insertSlackTestClaw(t, db, clawID, "connected", 0, "", oldEnough)
	setClawPipelineStage(t, db, clawID, "implement")
	seedIdleTestBaseline(t, s)
	backdateAgentIdleBaseline(t, s)
	setSlackWatermark(t, s, 0)
	d := testLifecycleDelivery(t, s)

	s.checkAgentIdle(time.Now(), clawID, deferTestConn(clawID, 12*time.Minute))
	s.lifecycleClawPass(d)
	if got := clawIdleSince(t, db, clawID); got != 0 || fake.count() != 0 {
		t.Fatalf("alert inside the resume window: idle_since=%d sent=%d, want neither", got, fake.count())
	}

	s.checkAgentIdle(time.Now(), clawID, deferTestConn(clawID, 16*time.Minute))
	s.lifecycleClawPass(d)
	if fake.count() != 1 {
		t.Fatalf("stretch past resume + idle_after sent %d messages, want 1", fake.count())
	}
}

// When the resume cannot help, the alert fires at idle_after as before.
func TestAgentIdleAlertDoesNotDeferWhenResumeCannotHelp(t *testing.T) {
	cases := map[string]func(t *testing.T, s *Server, clawID string, cc *clawConn){
		"no-progress paused": func(t *testing.T, s *Server, clawID string, cc *clawConn) {
			cc.noProgressPaused = true
		},
		"resume cap reached": func(t *testing.T, s *Server, clawID string, cc *clawConn) {
			if _, err := s.db.Exec(`UPDATE claws SET idle_resume_count=? WHERE id=?`, agentIdleResumeMaxAttempts, clawID); err != nil {
				t.Fatal(err)
			}
		},
		"resume disabled": func(t *testing.T, s *Server, clawID string, cc *clawConn) {
			disableIdleResume(s)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			fake := newFakeSlackServer(t)
			s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
			const clawID = "idle-nodefer"
			insertSlackTestClaw(t, db, clawID, "connected", 0, "", oldEnough)
			setClawPipelineStage(t, db, clawID, "implement")
			seedIdleTestBaseline(t, s)
			backdateAgentIdleBaseline(t, s)
			setSlackWatermark(t, s, 0)
			cc := deferTestConn(clawID, 6*time.Minute)
			setup(t, s, clawID, cc)

			s.checkAgentIdle(time.Now(), clawID, cc)
			s.lifecycleClawPass(testLifecycleDelivery(t, s))
			if fake.count() != 1 {
				t.Fatalf("sent %d messages, want 1", fake.count())
			}
		})
	}
}
