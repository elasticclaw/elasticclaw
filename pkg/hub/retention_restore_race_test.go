package hub

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// ---------------------------------------------------------------------------
// The restore guard is re-evaluated at the write, not only at the list
// ---------------------------------------------------------------------------
//
// compactFinalizedCheckpoints and pruneExpiredCheckpoints each read their
// work list once and then write for up to the cycle budget, pausing after
// every batch. TestCompactionSparesTheClawARestoreIsReading and
// TestExpirySparesTheCheckpointARestoreIsReading cover a restore that began
// BEFORE the list was read. These cover the one that begins after it: the
// retry path in particular sets restore_checkpoint_id only after its backoff
// and a termination checkpoint, minutes into a cycle.

// pauseHook replaces the pacer's pause with fn until fn reports that it has
// acted, so a test can interleave a write between two batches of a phase.
func pauseHook(t *testing.T, fn func() (acted bool)) *bool {
	t.Helper()
	previous := retentionSleep
	acted := false
	retentionSleep = func(time.Duration) {
		if !acted {
			acted = fn()
		}
	}
	t.Cleanup(func() { retentionSleep = previous })
	return &acted
}

func seedTwoStaleClawsWithTwoCheckpoints(t *testing.T, s *Server, reference time.Time) (manifests map[string]string, files map[string][]string) {
	t.Helper()
	manifests = map[string]string{}
	files = map[string][]string{}
	for _, claw := range []string{"claw-a", "claw-b"} {
		insertRetentionClaw(t, s, claw, reference.Add(-400*24*time.Hour))
		oldRoot, oldFiles, _ := planTreeFixtureSeeded(t, 2, claw+" old ")
		newRoot, _, _ := planTreeFixtureSeeded(t, 1, claw+" new ")
		manifests[claw+"-old"] = insertRetentionCheckpoint(t, s, retentionCheckpoint{
			id: claw + "-old", clawID: claw, status: "ready", createdAt: reference.Add(-60 * 24 * time.Hour),
			rootTree: oldRoot, writeManifest: true})
		files[claw+"-old"] = append(oldFiles, oldRoot)
		insertRetentionCheckpoint(t, s, retentionCheckpoint{
			id: claw + "-newest", clawID: claw, status: "ready", createdAt: reference.Add(-30 * 24 * time.Hour),
			rootTree: newRoot, writeManifest: true})
	}
	return manifests, files
}

func assertCheckpointIntact(t *testing.T, s *Server, id, manifest string, blobs []string, out string) {
	t.Helper()
	if status, _, _ := retentionCheckpointRow(t, s, id); status != "ready" {
		t.Fatalf("%s is %q, want ready:\n%s", id, status, out)
	}
	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("the manifest of %s is gone: %v\n%s", id, err, out)
	}
	if n := checkpointBlobRefCount(t, s, id); n == 0 {
		t.Fatalf("%s lost its reference edges\n%s", id, out)
	}
	for _, sha := range blobs {
		if _, err := os.Stat(checkpointBlobPath(sha)); err != nil {
			t.Fatalf("blob %s of %s was swept: %v\n%s", shortID(sha), id, err, out)
		}
	}
}

// Revert verified against: markCheckpointsCompacted without the NOT EXISTS
// guard AND compactClawCheckpoints without the clawFinalized re-check. Either
// alone spares the row here; the two unit tests below hold each on its own.
func TestCompactionSparesARestoreThatBeginsMidPhase(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s.nowFunc = func() time.Time { return reference }
	s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{Enabled: boolPtr(true)}}
	manifests, files := seedTwoStaleClawsWithTwoCheckpoints(t, s, reference)

	// The pause after claw-a's batch: a restore of claw-b's old checkpoint
	// begins now, after the claw list was read and before claw-b's turn.
	acted := pauseHook(t, func() bool {
		if status, _, _ := retentionCheckpointRow(t, s, "claw-a-old"); status != "compacted" {
			return false
		}
		if status, _, _ := retentionCheckpointRow(t, s, "claw-b-old"); status != "ready" {
			t.Fatal("fixture: claw-b was compacted before the pause that follows claw-a; the interleaving is not what the test names")
		}
		if _, err := s.db.Exec(`UPDATE claws SET status='provisioning', restore_checkpoint_id='claw-b-old' WHERE id='claw-b'`); err != nil {
			t.Fatal(err)
		}
		return true
	})

	out := captureRetentionLog(t, s.retentionSweepOnce)
	if !*acted {
		t.Fatalf("fixture: the phase never paused after claw-a, so no restore was interleaved:\n%s", out)
	}
	if status, _, _ := retentionCheckpointRow(t, s, "claw-a-old"); status != "compacted" {
		t.Fatalf("claw-a-old = %q; the phase did not do its job on the claw that was not restoring", status)
	}
	assertCheckpointIntact(t, s, "claw-b-old", manifests["claw-b-old"], files["claw-b-old"], out)
	if status, _, _ := retentionCheckpointRow(t, s, "claw-b-newest"); status != "ready" {
		t.Fatalf("claw-b-newest = %q; the restoring claw's other rows must be untouched too", status)
	}
}

// Revert verified against: compactClawCheckpoints without the clawFinalized
// re-check.
//
// A claw that reconnects mid-phase has no restore id for the UPDATE's guard
// to see; only re-evaluating the predicate for that claw catches it.
func TestCompactionSkipsAClawThatResumedSinceTheListWasRead(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s.nowFunc = func() time.Time { return reference }
	s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{Enabled: boolPtr(true)}}
	manifests, files := seedTwoStaleClawsWithTwoCheckpoints(t, s, reference)

	acted := pauseHook(t, func() bool {
		if status, _, _ := retentionCheckpointRow(t, s, "claw-a-old"); status != "compacted" {
			return false
		}
		if status, _, _ := retentionCheckpointRow(t, s, "claw-b-old"); status != "ready" {
			t.Fatal("fixture: claw-b was compacted before the pause that follows claw-a")
		}
		// claw-b came back: its bridge reconnected and the heartbeat moved
		// last_seen. It is no longer finalized by either arm.
		if _, err := s.db.Exec(`UPDATE claws SET status='connected', last_seen=? WHERE id='claw-b'`, reference); err != nil {
			t.Fatal(err)
		}
		return true
	})

	out := captureRetentionLog(t, s.retentionSweepOnce)
	if !*acted {
		t.Fatalf("fixture: the phase never paused after claw-a:\n%s", out)
	}
	assertCheckpointIntact(t, s, "claw-b-old", manifests["claw-b-old"], files["claw-b-old"], out)
}

// Revert verified against: markCheckpointsCompacted without the NOT EXISTS
// guard.
func TestMarkCheckpointsCompactedSkipsAClawWithARestoreInFlight(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Now()
	insertRetentionClaw(t, s, "claw", reference)
	insertRetentionClaw(t, s, "other", reference)
	root, _, _ := planTreeFixtureSeeded(t, 1, "claw ")
	otherRoot, _, _ := planTreeFixtureSeeded(t, 1, "other ")
	for _, cp := range []struct{ id, claw, root string }{
		{"cp-old", "claw", root}, {"cp-new", "claw", root}, {"other-old", "other", otherRoot},
	} {
		insertRetentionCheckpoint(t, s, retentionCheckpoint{
			id: cp.id, clawID: cp.claw, status: "ready", createdAt: reference, rootTree: cp.root, writeManifest: true})
	}
	if _, err := s.db.Exec(`UPDATE claws SET status='provisioning', restore_checkpoint_id='cp-old' WHERE id='claw'`); err != nil {
		t.Fatal(err)
	}

	compacted, err := s.markCheckpointsCompacted([]string{"cp-old", "cp-new", "other-old"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := compacted["other-old"]; !ok || len(compacted) != 1 {
		t.Fatalf("compacted = %v, want only other-old", compacted)
	}
	for _, id := range []string{"cp-old", "cp-new"} {
		if status, _, _ := retentionCheckpointRow(t, s, id); status != "ready" {
			t.Fatalf("%s = %q under a restore of its claw, want ready", id, status)
		}
		if n := checkpointBlobRefCount(t, s, id); n == 0 {
			t.Fatalf("%s lost its edges under a restore of its claw", id)
		}
	}
	if status, _, _ := retentionCheckpointRow(t, s, "other-old"); status != "compacted" {
		t.Fatalf("other-old = %q, want compacted", status)
	}
}

// Revert verified against: deleteExpiredCheckpoints without the NOT IN guard.
func TestDeleteExpiredCheckpointsSkipsARestoreSource(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Now()
	insertRetentionClaw(t, s, "claw", reference)
	root, _, _ := planTreeFixtureSeeded(t, 1, "claw ")
	for _, id := range []string{"cp-restoring", "cp-other"} {
		insertRetentionCheckpoint(t, s, retentionCheckpoint{
			id: id, clawID: "claw", status: "ready", createdAt: reference, rootTree: root, writeManifest: true})
	}
	if _, err := s.db.Exec(`UPDATE claws SET status='provisioning', restore_checkpoint_id='cp-restoring' WHERE id='claw'`); err != nil {
		t.Fatal(err)
	}

	deleted, err := s.deleteExpiredCheckpoints([]string{"cp-restoring", "cp-other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := deleted["cp-other"]; !ok || len(deleted) != 1 {
		t.Fatalf("deleted = %v, want only cp-other", deleted)
	}
	if status, _, _ := retentionCheckpointRow(t, s, "cp-restoring"); status != "ready" {
		t.Fatalf("the restore source is %q, want ready", status)
	}
	if n := checkpointBlobRefCount(t, s, "cp-restoring"); n == 0 {
		t.Fatal("the restore source lost its edges")
	}
	if n := countRetentionRows(t, s, `SELECT COUNT(*) FROM claw_checkpoints WHERE id='cp-other'`); n != 0 {
		t.Fatal("cp-other was not deleted")
	}
}

// Revert verified against: deleteExpiredCheckpoints without the NOT IN guard.
//
// Expiry batches its victims retentionCheckpointBatch at a time and pauses
// between batches; the restore begins in that pause, for a row in the next
// batch.
func TestExpirySparesARestoreThatBeginsMidPhase(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s.nowFunc = func() time.Time { return reference }
	s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{Enabled: boolPtr(true)}}
	// Live claws: compaction leaves them alone, and expiry applies whatever
	// the claw's status.
	for _, claw := range []string{"claw-a", "claw-b"} {
		insertRetentionClaw(t, s, claw, reference)
		if _, err := s.db.Exec(`UPDATE claws SET status='connected' WHERE id=?`, claw); err != nil {
			t.Fatal(err)
		}
	}
	expired := reference.Add(-400 * 24 * time.Hour)
	// One full batch for claw-a, so the pause after it precedes claw-b's row.
	for i := 0; i < retentionCheckpointBatch; i++ {
		insertRetentionCheckpoint(t, s, retentionCheckpoint{
			id: fmt.Sprintf("claw-a-%03d", i), clawID: "claw-a", status: "ready", createdAt: expired, writeManifest: true})
	}
	root, fileSHAs, _ := planTreeFixtureSeeded(t, 2, "claw-b ")
	manifest := insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "claw-b-old", clawID: "claw-b", status: "ready", createdAt: expired, rootTree: root, writeManifest: true})

	acted := pauseHook(t, func() bool {
		if n := countRetentionRows(t, s, `SELECT COUNT(*) FROM claw_checkpoints WHERE claw_id='claw-a'`); n != 0 {
			return false
		}
		if n := countRetentionRows(t, s, `SELECT COUNT(*) FROM claw_checkpoints WHERE id='claw-b-old'`); n != 1 {
			t.Fatal("fixture: claw-b-old was deleted in claw-a's batch; the interleaving is not what the test names")
		}
		if _, err := s.db.Exec(`UPDATE claws SET status='provisioning', restore_checkpoint_id='claw-b-old' WHERE id='claw-b'`); err != nil {
			t.Fatal(err)
		}
		return true
	})

	out := captureRetentionLog(t, s.retentionSweepOnce)
	if !*acted {
		t.Fatalf("fixture: expiry never paused between its batches:\n%s", out)
	}
	if n := countRetentionRows(t, s, `SELECT COUNT(*) FROM claw_checkpoints WHERE claw_id='claw-a'`); n != 0 {
		t.Fatalf("%d of claw-a's expired rows survived; the phase did not do its job", n)
	}
	assertCheckpointIntact(t, s, "claw-b-old", manifest, append(fileSHAs, root), out)
}
