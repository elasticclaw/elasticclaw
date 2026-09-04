package hub

import "testing"

func insertTestCheckpoint(t *testing.T, s *Server, id, reason string) {
	t.Helper()
	if err := s.insertCheckpoint(id, "tenant", "claw", reason, "hub", "local", "provider-id"); err != nil {
		t.Fatalf("insert checkpoint %s: %v", id, err)
	}
}

func checkpointStatus(t *testing.T, s *Server, id string) string {
	t.Helper()
	var status string
	if err := s.db.QueryRow(`SELECT status FROM claw_checkpoints WHERE id=?`, id).Scan(&status); err != nil {
		t.Fatalf("read status of %s: %v", id, err)
	}
	return status
}

// An idle checkpoint that captured the same workspace tree as the previous one
// records an agent that did nothing. It must not be finalized as ready work.
func TestFinalizeSkipsIdleCheckpointWithUnchangedWorkspace(t *testing.T) {
	s := newCheckpointCompletionTestServer(t)
	const tree = "ab12cd34"

	insertTestCheckpoint(t, s, "first", "idle-timer")
	if err := s.markCheckpointSkipped("first", tree); err != nil {
		t.Fatalf("seed previous checkpoint: %v", err)
	}

	insertTestCheckpoint(t, s, "second", "idle-timer")
	if err := s.finalizeCheckpoint("second", "tenant", "claw", tree); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	if got := checkpointStatus(t, s, "second"); got != "skipped" {
		t.Fatalf("status = %q, want skipped", got)
	}
	var manifestPath string
	if err := s.db.QueryRow(`SELECT manifest_path FROM claw_checkpoints WHERE id='second'`).Scan(&manifestPath); err != nil {
		t.Fatal(err)
	}
	if manifestPath != "" {
		t.Errorf("a skipped checkpoint must not write a manifest, got %q", manifestPath)
	}
}

// A changed workspace is real work and must be finalized normally.
func TestFinalizeKeepsIdleCheckpointWhenWorkspaceChanged(t *testing.T) {
	s := newCheckpointCompletionTestServer(t)

	insertTestCheckpoint(t, s, "first", "idle-timer")
	if err := s.markCheckpointSkipped("first", "aaaa1111"); err != nil {
		t.Fatalf("seed previous checkpoint: %v", err)
	}

	insertTestCheckpoint(t, s, "second", "idle-timer")
	if s.checkpointDuplicatesPrevious("second", "claw", "bbbb2222") {
		t.Fatal("a different workspace tree must not be treated as a duplicate")
	}
}

// Lifecycle checkpoints mark a transition and are worth keeping even when the
// workspace is untouched, so only idle-timer is eligible for skipping.
func TestFinalizeNeverSkipsLifecycleCheckpoints(t *testing.T) {
	s := newCheckpointCompletionTestServer(t)
	const tree = "ab12cd34"

	insertTestCheckpoint(t, s, "first", "idle-timer")
	if err := s.markCheckpointSkipped("first", tree); err != nil {
		t.Fatalf("seed previous checkpoint: %v", err)
	}

	for _, reason := range []string{"done", "bootstrap", "termination:pr-merged", "manual"} {
		id := "cp-" + reason
		insertTestCheckpoint(t, s, id, reason)
		if s.checkpointDuplicatesPrevious(id, "claw", tree) {
			t.Errorf("reason %q must never be skipped as a duplicate", reason)
		}
	}
}

// The idle scheduler throttles on the last checkpoint. If a skipped checkpoint
// did not count, the timer would re-request one every cycle forever.
func TestSkippedCheckpointCountsAsRecent(t *testing.T) {
	s := newCheckpointCompletionTestServer(t)

	insertTestCheckpoint(t, s, "first", "idle-timer")
	if err := s.markCheckpointSkipped("first", "ab12cd34"); err != nil {
		t.Fatalf("mark skipped: %v", err)
	}

	if !s.hasRecentCheckpoint("claw", checkpointMinInterval) {
		t.Fatal("a skipped checkpoint must throttle the idle scheduler")
	}
}

// Without a previous checkpoint there is nothing to compare against.
func TestFirstCheckpointIsNeverADuplicate(t *testing.T) {
	s := newCheckpointCompletionTestServer(t)
	insertTestCheckpoint(t, s, "only", "idle-timer")
	if s.checkpointDuplicatesPrevious("only", "claw", "ab12cd34") {
		t.Fatal("the first checkpoint of a claw cannot duplicate a previous one")
	}
	if s.checkpointDuplicatesPrevious("only", "claw", "") {
		t.Fatal("an empty tree must not be treated as a duplicate")
	}
}
