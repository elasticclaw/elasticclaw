package hub

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// ---------------------------------------------------------------------------
// Trees are referenced, files are listed once per tree
// ---------------------------------------------------------------------------

// planTreeFixture writes n aged file blobs and the tree blob listing them, and
// returns the plan a bridge would send for that workspace.
func planTreeFixture(t *testing.T, n int) (rootSHA string, fileSHAs []string, plan []types.CheckpointFile) {
	t.Helper()
	var entries []types.CheckpointFile
	for i := 0; i < n; i++ {
		sha := writeRetentionBlob(t, []byte(fmt.Sprintf("workspace file %d", i)))
		ageBlob(t, sha)
		fileSHAs = append(fileSHAs, sha)
		entries = append(entries, types.CheckpointFile{Path: fmt.Sprintf("workspace/%d.txt", i), SHA256: sha, Size: 16})
	}
	tree, _ := json.Marshal(entries)
	rootSHA = writeRetentionBlob(t, tree)
	ageBlob(t, rootSHA)
	plan = append(entries, types.CheckpointFile{Path: ".checkpoint/tree.json", SHA256: rootSHA, Size: int64(len(tree))})
	return rootSHA, fileSHAs, plan
}

// The production hub holds 5,211 checkpoints over 331 distinct trees. A second
// checkpoint of a tree the hub already knows must cost one edge and zero
// expansion rows, still keep every file, and the expansion must outlive the
// first holder and go with the last.
func TestTreeExpansionIsWrittenOncePerDistinctTree(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Now()
	insertRetentionClaw(t, s, "claw", reference)
	rootSHA, fileSHAs, plan := planTreeFixture(t, 3)

	for _, id := range []string{"cp-first", "cp-second"} {
		insertRetentionCheckpoint(t, s, retentionCheckpoint{
			id: id, clawID: "claw", status: "creating", createdAt: reference, noBlobRefs: true})
	}
	if err := s.recordCheckpointBlobRefs("cp-first", rootSHA, plan); err != nil {
		t.Fatalf("plan cp-first: %v", err)
	}
	if got := treeBlobRefCount(t, s, rootSHA); got != len(fileSHAs) {
		t.Fatalf("expansion of a new tree has %d rows, want %d (one per file; the tree does not list itself)", got, len(fileSHAs))
	}
	if got := checkpointBlobRefCount(t, s, "cp-first"); got != 1 {
		t.Fatalf("checkpoint holds %d edges after its plan, want 1 (the root tree)", got)
	}

	var rowsBefore int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tree_blob_refs`).Scan(&rowsBefore); err != nil {
		t.Fatal(err)
	}
	if err := s.recordCheckpointBlobRefs("cp-second", rootSHA, plan); err != nil {
		t.Fatalf("plan cp-second: %v", err)
	}
	var rowsAfter int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tree_blob_refs`).Scan(&rowsAfter); err != nil {
		t.Fatal(err)
	}
	if rowsAfter != rowsBefore {
		t.Fatalf("a second checkpoint of a known tree wrote %d expansion rows, want 0", rowsAfter-rowsBefore)
	}
	if got := checkpointBlobRefCount(t, s, "cp-second"); got != 1 {
		t.Fatalf("second checkpoint holds %d edges, want 1", got)
	}

	assertFiles := func(wantPresent bool, why string) {
		t.Helper()
		for _, sha := range fileSHAs {
			_, err := os.Stat(checkpointBlobPath(sha))
			if present := err == nil; present != wantPresent {
				t.Fatalf("file blob %s present = %v, want %v (%s)", shortID(sha), present, wantPresent, why)
			}
		}
	}
	if removed, _, _, err := s.sweepCheckpointBlobs(false, nil); err != nil || removed != 0 {
		t.Fatalf("sweep removed %d blobs (err %v) with two checkpoints on the tree", removed, err)
	}
	assertFiles(true, "both checkpoints reference the tree")

	if err := s.failCheckpoint("cp-first", "bridge went away"); err != nil {
		t.Fatal(err)
	}
	if removed, _, _, err := s.sweepCheckpointBlobs(false, nil); err != nil || removed != 0 {
		t.Fatalf("sweep removed %d blobs (err %v) with one checkpoint still on the tree", removed, err)
	}
	if _, err := s.pruneUnreferencedTreeBlobRefs(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got := treeBlobRefCount(t, s, rootSHA); got != len(fileSHAs) {
		t.Fatalf("expansion has %d rows after one of two holders released, want %d", got, len(fileSHAs))
	}
	assertFiles(true, "one checkpoint still references the tree")

	if err := s.failCheckpoint("cp-second", "bridge went away"); err != nil {
		t.Fatal(err)
	}
	if removed, _, _, err := s.sweepCheckpointBlobs(false, nil); err != nil || removed != len(fileSHAs)+1 {
		t.Fatalf("sweep removed %d blobs (err %v) after the last holder released, want %d files plus the tree", removed, err, len(fileSHAs)+1)
	}
	assertFiles(false, "nothing references the tree any more")
	if n, err := s.pruneUnreferencedTreeBlobRefs(time.Time{}); err != nil || n != int64(len(fileSHAs)) {
		t.Fatalf("tree gc removed %d rows (err %v), want %d", n, err, len(fileSHAs))
	}
	if got := treeBlobRefCount(t, s, rootSHA); got != 0 {
		t.Fatalf("expansion has %d rows after gc, want 0", got)
	}
}

// Finalize adds what the plan could not know -- the message blob and the
// manifest digest -- and nothing else. Re-inserting every file under the
// checkpoint is the per-file model coming back through the side door.
func TestFinalizeAddsOnlyTheRowDigests(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Now()
	insertRetentionClaw(t, s, "claw", reference)
	rootSHA, _, plan := planTreeFixture(t, 4)
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "creating", createdAt: reference, noBlobRefs: true})
	if err := s.recordCheckpointBlobRefs("cp", rootSHA, plan); err != nil {
		t.Fatal(err)
	}
	if err := s.finalizeCheckpoint("cp", "tenant", "claw", rootSHA); err != nil {
		t.Fatal(err)
	}
	if got := checkpointBlobRefCount(t, s, "cp"); got != 3 {
		t.Fatalf("published checkpoint holds %d edges, want 3 (root tree, message blob, manifest)", got)
	}
	var status, msgSHA, manifestSHA string
	if err := s.db.QueryRow(`SELECT status, message_tree_sha256, manifest_sha256 FROM claw_checkpoints WHERE id='cp'`).
		Scan(&status, &msgSHA, &manifestSHA); err != nil {
		t.Fatal(err)
	}
	referenced, err := s.referencedBlobDigests(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, sha := range []string{rootSHA, msgSHA, manifestSHA} {
		if _, ok := referenced[sha]; !ok {
			t.Errorf("no reference names %s", shortID(sha))
		}
	}
}

// The sweep cycle itself garbage-collects dead expansions, after the sweep and
// without ever standing in its way.
func TestCycleCollectsExpansionsNoCheckpointReferences(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s.nowFunc = func() time.Time { return reference }
	s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{Enabled: boolPtr(true)}}
	insertRetentionClaw(t, s, "claw", reference)
	rootSHA, fileSHAs, _ := planTreeFixture(t, 2)
	// A checkpoint past the retention window: expiry deletes the row, its
	// edges go with it, and the tree it named is then nobody's.
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "ready", createdAt: reference.Add(-400 * 24 * time.Hour),
		rootTree: rootSHA, writeManifest: true})
	if got := treeBlobRefCount(t, s, rootSHA); got != len(fileSHAs) {
		t.Fatalf("fixture seeded %d expansion rows, want %d", got, len(fileSHAs))
	}

	out := captureRetentionLog(t, s.retentionSweepOnce)
	if got := treeBlobRefCount(t, s, rootSHA); got != 0 {
		t.Fatalf("expansion has %d rows after the cycle, want 0; log:\n%s", got, out)
	}
	if !strings.Contains(out, fmt.Sprintf("tree_refs=%d", len(fileSHAs))) {
		t.Fatalf("the summary line does not report the tree rows collected; log:\n%s", out)
	}
	for _, sha := range fileSHAs {
		if _, err := os.Stat(checkpointBlobPath(sha)); !os.IsNotExist(err) {
			t.Errorf("file %s of the expired checkpoint survived the cycle", shortID(sha))
		}
	}
}

// ---------------------------------------------------------------------------
// The backfill does not complete over what it could not read
// ---------------------------------------------------------------------------

// A tree that is MISSING is permanent and the backfill records what it can. A
// tree that could not be READ -- EIO, EMFILE, a store that is not mounted -- is
// transient, and writing the marker over it would record the checkpoint as
// holding nothing: the next sweep would then unlink blobs the cycle after could
// have protected.
func TestBackfillDoesNotCompleteOverTransientReadFailures(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(t *testing.T, rootSHA string)
		why    string
	}{
		{
			name: "an unreadable tree blob",
			break_: func(t *testing.T, rootSHA string) {
				// A directory in the blob's place: ReadFile fails with EISDIR,
				// which is neither ENOENT nor a parse error, on every platform
				// and as every user.
				path := checkpointBlobPath(rootSHA)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o750); err != nil {
					t.Fatal(err)
				}
			},
			why: "the files it lists could be recorded next cycle",
		},
		{
			name: "a blob store that is not there",
			break_: func(t *testing.T, rootSHA string) {
				if err := os.RemoveAll(filepath.Join(checkpointsRoot(), "blobs")); err != nil {
					t.Fatal(err)
				}
			},
			why: "an unmounted store is not 'every blob is missing'",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRetentionTestServer(t)
			if _, err := s.db.Exec(`DELETE FROM hub_migrations WHERE name=?`, checkpointBlobRefsBackfillMigration); err != nil {
				t.Fatal(err)
			}
			reference := time.Now()
			insertRetentionClaw(t, s, "claw", reference)
			rootSHA, _, _ := planTreeFixture(t, 1)
			insertRetentionCheckpoint(t, s, retentionCheckpoint{
				id: "cp", clawID: "claw", status: "ready", createdAt: reference,
				rootTree: rootSHA, writeManifest: true, noBlobRefs: true})
			tc.break_(t, rootSHA)

			if err := s.backfillCheckpointBlobRefs(); err == nil {
				t.Fatalf("the backfill reported success over a read failure (%s)", tc.why)
			}
			done, err := s.checkpointBlobRefsBackfilled()
			if err != nil {
				t.Fatal(err)
			}
			if done {
				t.Fatalf("the completion marker was written over a read failure (%s)", tc.why)
			}
		})
	}
}

// A missing tree, with the store present, is permanent: the backfill says so,
// records everything else, and completes.
func TestBackfillCompletesOverAGenuinelyMissingTree(t *testing.T) {
	s := newRetentionTestServer(t)
	if _, err := s.db.Exec(`DELETE FROM hub_migrations WHERE name=?`, checkpointBlobRefsBackfillMigration); err != nil {
		t.Fatal(err)
	}
	reference := time.Now()
	insertRetentionClaw(t, s, "claw", reference)
	rootSHA, _, _ := planTreeFixture(t, 1)
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "ready", createdAt: reference,
		rootTree: rootSHA, writeManifest: true, noBlobRefs: true})
	if err := os.Remove(checkpointBlobPath(rootSHA)); err != nil {
		t.Fatal(err)
	}
	out := captureRetentionLog(t, func() {
		if err := s.backfillCheckpointBlobRefs(); err != nil {
			t.Fatalf("backfill: %v", err)
		}
	})
	if !strings.Contains(out, "missing_trees=1") {
		t.Fatalf("the missing tree was not reported; log:\n%s", out)
	}
	if done, _ := s.checkpointBlobRefsBackfilled(); !done {
		t.Fatal("a permanent omission must not hold the sweep off forever")
	}
}

// The DECLINED state has to be visible in the cycle summary, with the amount of
// work outstanding, or a backfill that will never fit on the disk reads as a
// quiet cycle with blobs=0.
func TestCycleSummaryShowsADeclinedSweep(t *testing.T) {
	s := newRetentionTestServer(t)
	if _, err := s.db.Exec(`DELETE FROM hub_migrations WHERE name=?`, checkpointBlobRefsBackfillMigration); err != nil {
		t.Fatal(err)
	}
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s.nowFunc = func() time.Time { return reference }
	s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{Enabled: boolPtr(true)}}
	insertRetentionClaw(t, s, "claw", reference)
	rootSHA, _, _ := planTreeFixture(t, 2)
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "ready", createdAt: reference,
		rootTree: rootSHA, writeManifest: true, noBlobRefs: true})
	if _, err := s.db.Exec(`UPDATE claw_checkpoints SET files_count=2 WHERE id='cp'`); err != nil {
		t.Fatal(err)
	}
	// Make the backfill fail so the gate stays closed through the cycle.
	if err := os.RemoveAll(filepath.Join(checkpointsRoot(), "blobs")); err != nil {
		t.Fatal(err)
	}

	out := captureRetentionLog(t, s.retentionSweepOnce)
	if !strings.Contains(out, "blobs=DECLINED") {
		t.Fatalf("the summary line hides the declined sweep; log:\n%s", out)
	}
	if !strings.Contains(out, "1 checkpoint(s), 1 unexpanded tree(s), ~5 edge(s)") {
		t.Fatalf("the declined line does not estimate the outstanding work; log:\n%s", out)
	}
}
