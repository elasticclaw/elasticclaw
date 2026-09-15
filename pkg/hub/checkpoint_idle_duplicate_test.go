package hub

import (
	"testing"
	"time"
)

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

// Revert verified against: checkpointDuplicatesPrevious with BOTH its
// `rootSHA == ""` early return and the non-empty root_tree_sha256 filter of its
// previous-checkpoint query removed. Either alone still answers false here,
// so this test holds the pair, not each: with the filter in place the guard
// is unreachable by behaviour (an empty previous tree is never selected), and
// with the guard in place the filter's only observable effect is the one
// TestDuplicateCheckLooksPastACheckpointWithoutATree covers.
//
// A bridge that reports no workspace tree -- an older build, or a capture
// whose tree hashing failed -- leaves root_tree_sha256 empty on a 'ready' row.
// Two such captures in a row have not been shown to be the same workspace;
// "unknown" equals "unknown" only as strings.
func TestEmptyTreeNeverDuplicatesAPreviousEmptyTree(t *testing.T) {
	s := newCheckpointCompletionTestServer(t)
	insertTestCheckpoint(t, s, "first", "idle-timer")
	if _, err := s.db.Exec(`UPDATE claw_checkpoints SET status='ready', root_tree_sha256='' WHERE id='first'`); err != nil {
		t.Fatal(err)
	}
	insertTestCheckpoint(t, s, "second", "idle-timer")
	if s.checkpointDuplicatesPrevious("second", "claw", "") {
		t.Fatal("an idle checkpoint without a tree was treated as a duplicate of a previous checkpoint without a tree")
	}
}

// Revert verified against: the non-empty root_tree_sha256 filter of the
// previous-checkpoint query removed.
//
// A previous row without a tree says nothing about the workspace; the
// comparison is against the last checkpoint whose tree is known.
func TestDuplicateCheckLooksPastACheckpointWithoutATree(t *testing.T) {
	s := newCheckpointCompletionTestServer(t)
	const tree = "ab12cd34"
	insertTestCheckpoint(t, s, "known", "idle-timer")
	if err := s.markCheckpointSkipped("known", tree); err != nil {
		t.Fatal(err)
	}
	insertTestCheckpoint(t, s, "treeless", "idle-timer")
	// Later than "known" by the same clock the rows were inserted with: the
	// column is a time.Time, and arithmetic on it in SQL would silently turn
	// the value into an integer that sorts BEFORE every text timestamp.
	if _, err := s.db.Exec(`UPDATE claw_checkpoints SET status='ready', root_tree_sha256='', created_at=? WHERE id='treeless'`, now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	insertTestCheckpoint(t, s, "third", "idle-timer")
	if _, err := s.db.Exec(`UPDATE claw_checkpoints SET created_at=? WHERE id='third'`, now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var newest string
	if err := s.db.QueryRow(`SELECT id FROM claw_checkpoints WHERE claw_id='claw' AND id<>'third' ORDER BY created_at DESC LIMIT 1`).Scan(&newest); err != nil {
		t.Fatal(err)
	}
	if newest != "treeless" {
		t.Fatalf("fixture: the newest previous row is %q, not the treeless one; the filter would not be what decides", newest)
	}
	if !s.checkpointDuplicatesPrevious("third", "claw", tree) {
		t.Fatal("the same tree as the last checkpoint that had one was not treated as a duplicate")
	}
}
