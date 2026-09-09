package hub

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// newNudgeTestServer builds a server with one claw, optionally attached to a
// factory whose pipeline declares its own message_contains tokens.
func newNudgeTestServer(t *testing.T, clawID string, factories []*types.FactoryConfig) *Server {
	t.Helper()
	cfg := &types.HubConfig{Token: "test-token", Factories: factories}
	s, db := NewTestServerWithConfig(t, cfg, "", "", "")
	tags := "[]"
	if len(factories) > 0 {
		tags = `["factory:` + factories[0].Name + `"]`
	}
	if _, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, tags, linear_issue_id, pipeline_stage, created_at)
		 VALUES(?,?,?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "nudge-claw", "elasticclaw", "connected", tags, "ELA-1", "working",
	); err != nil {
		t.Fatalf("insert claw: %v", err)
	}
	return s
}

// nudgeMessages returns the unanchored-signal nudges injected for a claw.
func nudgeMessages(t *testing.T, s *Server, clawID string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT content FROM messages WHERE claw_id=? AND role='hub' ORDER BY rowid`, clawID)
	if err != nil {
		t.Fatalf("query messages: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if strings.Contains(c, "not at the start of a line") {
			out = append(out, c)
		}
	}
	if err := rows.Err(); err != nil && err != sql.ErrNoRows {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// TestNudgeUnanchoredSignal is the recoverability half of the anchoring change.
// Anchoring trades a spurious transition for a missed one, and a missed signal
// is a run that hangs until a human notices. Without this nudge the agent has
// no way to learn its signal was dropped, so these cases matter as much as the
// matcher's.
func TestNudgeUnanchoredSignal(t *testing.T) {
	t.Run("stray mention produces exactly one nudge, ever", func(t *testing.T) {
		const clawID = "claw-nudge-once"
		s := newNudgeTestServer(t, clawID, nil)

		s.nudgeUnanchoredSignal(clawID, "Implementation complete, so [DONE].")
		got := nudgeMessages(t, s, clawID)
		if len(got) != 1 {
			t.Fatalf("first turn produced %d nudges, want 1: %v", len(got), got)
		}
		if !strings.Contains(got[0], "[DONE]") {
			t.Fatalf("nudge does not name the token: %q", got[0])
		}

		// Same token quoted again on later turns must not re-nudge: an agent
		// that mentions [DONE] every turn would otherwise be nudged every turn,
		// which is the spam this is designed to avoid.
		s.nudgeUnanchoredSignal(clawID, "As I said, [DONE] is premature.")
		s.nudgeUnanchoredSignal(clawID, "Still not sending [DONE].")
		if got := nudgeMessages(t, s, clawID); len(got) != 1 {
			t.Fatalf("repeat mentions produced %d nudges, want 1: %v", len(got), got)
		}
	})

	t.Run("anchored signal suppresses the nudge", func(t *testing.T) {
		const clawID = "claw-nudge-anchored"
		s := newNudgeTestServer(t, clawID, nil)

		// The agent signalled correctly; the second mention is commentary.
		s.nudgeUnanchoredSignal(clawID, "[DONE] https://github.com/org/repo/pull/1\n\nI sent [DONE] as instructed.")
		if got := nudgeMessages(t, s, clawID); len(got) != 0 {
			t.Fatalf("nudged despite a correct signal: %v", got)
		}
	})

	t.Run("a different anchored token also suppresses the nudge", func(t *testing.T) {
		const clawID = "claw-nudge-other-anchored"
		s := newNudgeTestServer(t, clawID, nil)

		s.nudgeUnanchoredSignal(clawID, "[TERMINATE]\n\nI never reached [DONE].")
		if got := nudgeMessages(t, s, clawID); len(got) != 0 {
			t.Fatalf("nudged despite an anchored [TERMINATE]: %v", got)
		}
	})

	t.Run("at most one nudge per turn", func(t *testing.T) {
		const clawID = "claw-nudge-one-per-turn"
		s := newNudgeTestServer(t, clawID, nil)

		s.nudgeUnanchoredSignal(clawID, "Neither [DONE] nor [TERMINATE] applies yet.")
		if got := nudgeMessages(t, s, clawID); len(got) != 1 {
			t.Fatalf("two stray tokens produced %d nudges, want 1: %v", len(got), got)
		}
	})

	t.Run("no token means no nudge", func(t *testing.T) {
		const clawID = "claw-nudge-none"
		s := newNudgeTestServer(t, clawID, nil)

		s.nudgeUnanchoredSignal(clawID, "Still working through the migration.")
		if got := nudgeMessages(t, s, clawID); len(got) != 0 {
			t.Fatalf("nudged a message with no signal token: %v", got)
		}
	})

	t.Run("pipeline tokens are nudged too", func(t *testing.T) {
		// The dominant real case: an inject that only said "say
		// [READY_TO_COMMIT]" and an agent that obliged mid-sentence. Without the
		// pipeline's own tokens in the set, that message would freeze the run in
		// silence.
		const clawID = "claw-nudge-pipeline"
		s := newNudgeTestServer(t, clawID, []*types.FactoryConfig{{
			Name:        "nudge-factory",
			Integration: "linear",
			Workspace:   "test-workspace",
			PipelineYAML: `
stages:
  - id: working
    entry: true
  - id: pre_commit
    triggers:
      - message_contains: "[READY_TO_COMMIT]"
`,
		}})

		s.nudgeUnanchoredSignal(clawID, "Implementation complete, so [READY_TO_COMMIT].")
		got := nudgeMessages(t, s, clawID)
		if len(got) != 1 {
			t.Fatalf("pipeline token produced %d nudges, want 1: %v", len(got), got)
		}
		if !strings.Contains(got[0], "[READY_TO_COMMIT]") {
			t.Fatalf("nudge does not name the pipeline token: %q", got[0])
		}
	})

	t.Run("a token whose stage is already behind us is not nudged", func(t *testing.T) {
		// The inject that leaves such a stage usually tells the agent not to
		// emit the token again. Nudging "resend it at the start of a line"
		// would be talking it into a backwards transition, because
		// message_contains triggers have no visited-stage guard of their own.
		const clawID = "claw-nudge-visited"
		s := newNudgeTestServer(t, clawID, []*types.FactoryConfig{{
			Name:        "nudge-factory",
			Integration: "linear",
			Workspace:   "test-workspace",
			PipelineYAML: `
stages:
  - id: working
    entry: true
  - id: pre_commit
    triggers:
      - message_contains: "[READY_TO_COMMIT]"
`,
		}})
		if _, err := s.db.Exec(
			`INSERT INTO pipeline_stage_history(claw_id, stage_id, created_at) VALUES(?,?,datetime('now'))`,
			clawID, "pre_commit",
		); err != nil {
			t.Fatalf("mark stage visited: %v", err)
		}

		s.nudgeUnanchoredSignal(clawID, "I will not emit [READY_TO_COMMIT] again.")
		if got := nudgeMessages(t, s, clawID); len(got) != 0 {
			t.Fatalf("nudged a token whose stage already passed: %v", got)
		}
	})
}
