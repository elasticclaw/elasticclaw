package hub

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// newSignalContractTestServer builds a server with one claw sitting in
// pipeline stage "working", entered at enteredAt, wired to a task run so
// recordTaskRunEventForClaw has somewhere to write.
func newSignalContractTestServer(t *testing.T, clawID string, enteredAt time.Time) (*Server, *sql.DB) {
	t.Helper()
	cfg := &types.HubConfig{Token: "test-token"}
	s, db := NewTestServerWithConfig(t, cfg, "", "", "")
	if _, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, tags, pipeline_stage, pipeline_stage_entered_at, created_at)
		 VALUES(?,?,?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "signal-claw", "elasticclaw", "connected", "[]", "working", epochMillis(enteredAt),
	); err != nil {
		t.Fatalf("insert claw: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO task_runs(id,tenant_id,initial_attempt_id,current_attempt_id,run_kind,owner_type,claw_id,created_at,updated_at)
		 VALUES('run-1','test-tenant-id','attempt-1','attempt-1','code_task','factory',?,?,?)`,
		clawID, epochMillis(enteredAt), epochMillis(enteredAt),
	); err != nil {
		t.Fatalf("insert task run: %v", err)
	}
	if _, err := db.Exec(`UPDATE claws SET task_run_id='run-1' WHERE id=?`, clawID); err != nil {
		t.Fatalf("attach task run: %v", err)
	}
	return s, db
}

func signalContractDetail(t *testing.T, db *sql.DB, eventType string) map[string]any {
	t.Helper()
	var raw string
	if err := db.QueryRow(`SELECT detail FROM task_run_events WHERE run_id='run-1' AND event_type=?`, eventType).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		t.Fatal(err)
	}
	return detail
}

func assertSignalContract(t *testing.T, db *sql.DB, cause, emission string) {
	t.Helper()
	if got := signalContractDetail(t, db, taskRunEventSignalAdvanceCause)["cause"]; got != cause {
		t.Fatalf("cause=%q, want %q", got, cause)
	}
	if got := signalContractDetail(t, db, taskRunEventSignalEmission)["emission"]; got != emission {
		t.Fatalf("emission=%q, want %q", got, emission)
	}
}

func insertSignalContractMessage(t *testing.T, db *sql.DB, clawID, role, content, userLogin string, createdAt time.Time) {
	t.Helper()
	var login any
	if userLogin != "" {
		login = userLogin
	}
	if _, err := db.Exec(
		`INSERT INTO messages(id,claw_id,tenant_id,role,content,user_login,created_at) VALUES(?,?,?,?,?,?,?)`,
		content+"-"+role+"-"+createdAt.String(), clawID, "test-tenant-id", role, content, login, createdAt,
	); err != nil {
		t.Fatalf("insert message: %v", err)
	}
}

func signalContractEventTypes(t *testing.T, db *sql.DB, runID string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT event_type FROM task_run_events WHERE run_id=? ORDER BY rowid`, runID)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var et string
		if err := rows.Scan(&et); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, et)
	}
	return out
}

// TestRecordStageSignalContractOutcome is phase-one measurement only: it must
// count how a stage got unstuck without ever nudging, injecting, or otherwise
// touching the claw's conversation.
func TestRecordStageSignalContractOutcome(t *testing.T) {
	t.Run("nudge resolved it: signal_unanchored_nudged", func(t *testing.T) {
		const clawID = "claw-sc-nudged"
		entered := time.Now().UTC().Add(-time.Hour)
		s, db := newSignalContractTestServer(t, clawID, entered)
		insertSignalContractMessage(t, db, clawID, "claw", "I think it's [DONE] time", "", entered.Add(time.Minute))
		insertSignalContractMessage(t, db, clawID, "hub", unanchoredSignalNudgeText(doneSignalToken), "", entered.Add(2*time.Minute))
		insertSignalContractMessage(t, db, clawID, "claw", "[DONE]\nhttps://github.com/org/repo/pull/1", "", entered.Add(3*time.Minute))

		s.recordStageSignalContractOutcome(clawID, "working", entered)

		got := signalContractEventTypes(t, db, "run-1")
		if len(got) != 2 || got[0] != taskRunEventSignalAdvanceCause || got[1] != taskRunEventSignalEmission {
			t.Fatalf("events = %v, want cause and emission", got)
		}
		assertSignalContract(t, db, "hub_nag", "anchored")
	})

	t.Run("human dashboard message resolved it: signal_human_rescue", func(t *testing.T) {
		const clawID = "claw-sc-human"
		entered := time.Now().UTC().Add(-time.Hour)
		s, db := newSignalContractTestServer(t, clawID, entered)
		insertSignalContractMessage(t, db, clawID, "claw", "Still working on it, will signal soon", "", entered.Add(time.Minute))
		insertSignalContractMessage(t, db, clawID, "user", "please open the PR", "reviewer", entered.Add(2*time.Minute))

		s.recordStageSignalContractOutcome(clawID, "working", entered)

		got := signalContractEventTypes(t, db, "run-1")
		if len(got) != 2 || got[0] != taskRunEventSignalAdvanceCause || got[1] != taskRunEventSignalEmission {
			t.Fatalf("events = %v, want cause and emission", got)
		}
		assertSignalContract(t, db, "human_message", "absent")
	})

	t.Run("token never emitted at all: signal_missed", func(t *testing.T) {
		const clawID = "claw-sc-missed"
		entered := time.Now().UTC().Add(-time.Hour)
		s, db := newSignalContractTestServer(t, clawID, entered)
		insertSignalContractMessage(t, db, clawID, "claw", "Implementation looks complete, ready to move on", "", entered.Add(time.Minute))

		s.recordStageSignalContractOutcome(clawID, "working", entered)

		got := signalContractEventTypes(t, db, "run-1")
		if len(got) != 2 {
			t.Fatalf("events = %v, want two dimensions", got)
		}
		assertSignalContract(t, db, "self", "absent")
	})

	t.Run("system-injected user message does not count as a human rescue", func(t *testing.T) {
		const clawID = "claw-sc-injected"
		entered := time.Now().UTC().Add(-time.Hour)
		s, db := newSignalContractTestServer(t, clawID, entered)
		// role='user' with no user_login is the hub's own injectUserMessage,
		// not a real person — must not be mistaken for a human rescue, and
		// with no token ever mentioned this is a missed signal instead.
		insertSignalContractMessage(t, db, clawID, "user", "[hub] reminder to keep going", "", entered.Add(time.Minute))

		s.recordStageSignalContractOutcome(clawID, "working", entered)

		got := signalContractEventTypes(t, db, "run-1")
		if len(got) != 2 {
			t.Fatalf("events = %v, want two dimensions", got)
		}
		assertSignalContract(t, db, "self", "absent")
	})

	t.Run("clean anchored signal with no intervention: nothing recorded", func(t *testing.T) {
		const clawID = "claw-sc-clean"
		entered := time.Now().UTC().Add(-time.Hour)
		s, db := newSignalContractTestServer(t, clawID, entered)
		insertSignalContractMessage(t, db, clawID, "claw", "[DONE]\nhttps://github.com/org/repo/pull/1", "", entered.Add(time.Minute))

		s.recordStageSignalContractOutcome(clawID, "working", entered)

		got := signalContractEventTypes(t, db, "run-1")
		if len(got) != 2 {
			t.Fatalf("events = %v, want two dimensions", got)
		}
		assertSignalContract(t, db, "self", "anchored")
	})

	t.Run("zero entered-at records nothing", func(t *testing.T) {
		const clawID = "claw-sc-zero"
		entered := time.Now().UTC().Add(-time.Hour)
		s, db := newSignalContractTestServer(t, clawID, entered)

		s.recordStageSignalContractOutcome(clawID, "working", time.Time{})

		got := signalContractEventTypes(t, db, "run-1")
		if len(got) != 0 {
			t.Fatalf("events = %v, want none", got)
		}
	})
}
