package hub

import (
	"fmt"
	"sync"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestSimilarTurnOutcomes(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{name: "exact short trigger", a: "[CI RETRY]", b: "[CI RETRY]", want: true},
		{
			name: "minor wording change",
			a:    "Depot passed and the pull request is ready. I checked every Greptile conversation, review, and inline thread and found no actionable feedback remaining.",
			b:    "Depot passed; the pull request is ready. I checked all Greptile conversation, review, and inline threads and found no actionable feedback remaining.",
			want: true,
		},
		{
			name: "new outcome",
			a:    "Depot passed and the pull request is ready. I checked every Greptile conversation, review, and inline thread and found no actionable feedback remaining.",
			b:    "Greptile found a race in message delivery. I changed the queue implementation, added a regression test, pushed a new commit, and requested CI again.",
			want: false,
		},
		{name: "different short messages", a: "waiting for review", b: "review completed", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := similarTurnOutcomes(normalizeTurnOutcome(tt.a), normalizeTurnOutcome(tt.b)); got != tt.want {
				t.Fatalf("similarTurnOutcomes() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestObserveCompletedTurnPausesOnlyUnchangedRepeatedOutcomes(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	const clawID = "claw-no-progress"
	if _, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,datetime('now'))`, clawID, "test-tenant-id", "AMB-12", "connected", "ci_passed"); err != nil {
		t.Fatal(err)
	}
	s.claws[clawID] = &clawConn{id: clawID, tenantID: "test-tenant-id"}

	if s.observeCompletedTurn(clawID, "turn-1", "[CI RETRY]") {
		t.Fatal("first repeated outcome paused the claw")
	}
	if s.observeCompletedTurn(clawID, "turn-2", "[CI RETRY]") {
		t.Fatal("second repeated outcome paused the claw")
	}
	// A changed workflow output is material progress and starts a new epoch,
	// even though the model emitted the same control signal.
	if _, err := db.Exec(`INSERT INTO pipeline_outputs(claw_id, stage_id, output_name, exit_code, stdout, stderr, parsed_json, created_at) VALUES(?,?,?,?,?,?,?,datetime('now'))`, clawID, "depot_ci", "depot_ci", 0, `{"status":"passed","head":"abc1234"}`, "", `{"status":"passed","head":"abc1234"}`); err != nil {
		t.Fatal(err)
	}
	if s.observeCompletedTurn(clawID, "turn-3", "[CI RETRY]") {
		t.Fatal("changed pipeline output did not reset repeated outcomes")
	}
	if s.observeCompletedTurn(clawID, "turn-4", "[CI RETRY]") {
		t.Fatal("second outcome in the new epoch paused the claw")
	}
	if !s.observeCompletedTurn(clawID, "turn-5", "[CI RETRY]") {
		t.Fatal("third unchanged repeated outcome did not pause the claw")
	}

	var paused bool
	if err := db.QueryRow(`SELECT no_progress_paused != 0 FROM claws WHERE id=?`, clawID).Scan(&paused); err != nil {
		t.Fatal(err)
	}
	if !paused {
		t.Fatal("pause was not persisted")
	}
	var notices int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='hub' AND content LIKE '%Automatic continuation paused:%' AND delivered_at IS NOT NULL`, clawID).Scan(&notices); err != nil {
		t.Fatal(err)
	}
	if notices != 1 {
		t.Fatalf("pause notices = %d, want 1", notices)
	}

	s.resumeNoProgressAfterUserInput(clawID)
	var observations int
	if err := db.QueryRow(`SELECT no_progress_paused != 0, (SELECT COUNT(*) FROM claw_turn_observations WHERE claw_id=?) FROM claws WHERE id=?`, clawID, clawID).Scan(&paused, &observations); err != nil {
		t.Fatal(err)
	}
	if paused || observations != 0 {
		t.Fatalf("resume left paused=%v observations=%d", paused, observations)
	}
}

func TestConcurrentResumeWinsOverNoProgressPause(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	const clawID = "claw-no-progress-race"
	if _, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,datetime('now'))`, clawID, "test-tenant-id", "AMB-12", "connected", "ci_passed"); err != nil {
		t.Fatal(err)
	}
	cc := &clawConn{id: clawID, tenantID: "test-tenant-id"}
	s.claws[clawID] = cc

	for i := 0; i < 25; i++ {
		s.resumeNoProgressAfterUserInput(clawID)
		if s.observeCompletedTurn(clawID, fmt.Sprintf("turn-%d-1", i), "[CI RETRY]") {
			t.Fatal("first repeated outcome paused the claw")
		}
		if s.observeCompletedTurn(clawID, fmt.Sprintf("turn-%d-2", i), "[CI RETRY]") {
			t.Fatal("second repeated outcome paused the claw")
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			s.observeCompletedTurn(clawID, fmt.Sprintf("turn-%d-3", i), "[CI RETRY]")
		}()
		go func() {
			defer wg.Done()
			<-start
			s.resumeNoProgressAfterUserInput(clawID)
		}()
		close(start)
		wg.Wait()

		var persistedPaused bool
		if err := db.QueryRow(`SELECT no_progress_paused != 0 FROM claws WHERE id=?`, clawID).Scan(&persistedPaused); err != nil {
			t.Fatal(err)
		}
		cc.mu.RLock()
		memoryPaused := cc.noProgressPaused
		cc.mu.RUnlock()
		if persistedPaused || memoryPaused {
			t.Fatalf("iteration %d: concurrent resume left persisted=%v memory=%v", i, persistedPaused, memoryPaused)
		}
	}
}

func TestTurnProgressFingerprintRejectsPartialState(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	const clawID = "claw-partial-fingerprint"
	if _, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,datetime('now'))`, clawID, "test-tenant-id", "AMB-12", "connected", "ci_passed"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE pipeline_outputs RENAME TO pipeline_outputs_valid`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE VIEW pipeline_outputs AS SELECT 'claw-partial-fingerprint' AS claw_id, NULL AS output_name, 0 AS exit_code, '' AS stdout, '' AS stderr, '' AS parsed_json`); err != nil {
		t.Fatal(err)
	}

	if _, err := s.turnProgressFingerprint(clawID, "still waiting"); err == nil {
		t.Fatal("turnProgressFingerprint accepted a partial row after a scan error")
	}
}

func TestResponseProgressMarkersTrackNewCommitsAndPRs(t *testing.T) {
	first := responseProgressMarkers("Pushed 1a2b3c4 to https://github.com/elasticclaw/elasticclaw/pull/100")
	second := responseProgressMarkers("Pushed 9d8e7f6 to https://github.com/elasticclaw/elasticclaw/pull/100")
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("markers = %v and %v, want PR and commit markers", first, second)
	}
	if first[0] == second[0] && first[1] == second[1] {
		t.Fatal("new commit did not change progress markers")
	}
}
