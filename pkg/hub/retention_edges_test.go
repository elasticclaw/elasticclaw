package hub

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// ---------------------------------------------------------------------------
// The backfill and schema-1 manifests
// ---------------------------------------------------------------------------

// schemaOneManifest is the shape the base hub wrote: the tree AND the complete
// file list, side by side. Most of the production hub's manifests look like
// this.
func schemaOneManifest(t *testing.T, id, rootSHA, msgSHA string, files []types.CheckpointFile) []byte {
	t.Helper()
	body, err := json.Marshal(struct {
		checkpointManifest
		Files []types.CheckpointFile `json:"files"`
	}{
		checkpointManifest: checkpointManifest{
			Schema: 1, CheckpointID: id, ClawID: "claw",
			Workspace: checkpointWorkspace{TreeSHA256: rootSHA},
			Messages:  checkpointMessages{BlobSHA256: msgSHA},
		},
		Files: files,
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// A schema-1 manifest inlines every file, and it also names the tree. The tree
// is what gets expanded, once; the inlined list must NOT become one checkpoint
// edge per file -- on the production hub that is the 16.2M rows this design
// exists to avoid. The list is a fallback for a checkpoint whose tree cannot
// be expanded, and nothing else.
func TestBackfillExpandsTheTreeASchemaOneManifestNames(t *testing.T) {
	const fileCount = 4
	cases := []struct {
		name          string
		rowRoot       bool // the row names the tree in root_tree_sha256
		manifestRoot  bool // the manifest names the tree in workspace.tree_sha256
		removeTree    bool
		wantEdges     int
		wantExpansion int
		why           string
	}{
		{
			name: "tree present", rowRoot: true, manifestRoot: true,
			wantEdges: 3, wantExpansion: fileCount,
			why: "root, message and manifest under the checkpoint; the files under the tree, once",
		},
		{
			name: "tree named only by the manifest", rowRoot: false, manifestRoot: true,
			wantEdges: 3, wantExpansion: fileCount,
			why: "the manifest's tree is a tree like any other",
		},
		{
			name: "tree blob missing", rowRoot: true, manifestRoot: true, removeTree: true,
			wantEdges: 3 + fileCount, wantExpansion: 0,
			why: "nothing else records the files, so the inlined list is the fallback",
		},
		{
			name: "no tree named at all", rowRoot: false, manifestRoot: false,
			wantEdges: 2 + fileCount, wantExpansion: 0,
			why: "message and manifest under the checkpoint, plus the inlined files: there is no tree to hang them on",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRetentionTestServer(t)
			reference := time.Now()
			insertRetentionClaw(t, s, "claw", reference)
			rootSHA, fileSHAs, plan := planTreeFixture(t, fileCount)
			msgSHA := writeRetentionBlob(t, []byte(`{"id":"m1"}`))
			ageBlob(t, msgSHA)
			files := plan[:fileCount]

			rowRoot, manifestRoot := "", ""
			if tc.rowRoot {
				rowRoot = rootSHA
			}
			if tc.manifestRoot {
				manifestRoot = rootSHA
			}
			insertRetentionCheckpoint(t, s, retentionCheckpoint{
				id: "cp", clawID: "claw", status: "ready", createdAt: reference,
				rootTree: rowRoot, messageTree: msgSHA, writeManifest: true,
				manifestBody: schemaOneManifest(t, "cp", manifestRoot, msgSHA, files), noBlobRefs: true})
			if tc.removeTree {
				if err := os.Remove(checkpointBlobPath(rootSHA)); err != nil {
					t.Fatal(err)
				}
			}

			if err := s.backfillCheckpointBlobRefs(); err != nil {
				t.Fatalf("backfill: %v", err)
			}
			if got := checkpointBlobRefCount(t, s, "cp"); got != tc.wantEdges {
				t.Fatalf("checkpoint holds %d edges, want %d (%s)", got, tc.wantEdges, tc.why)
			}
			if got := treeBlobRefCount(t, s, rootSHA); got != tc.wantExpansion {
				t.Fatalf("tree expansion has %d rows, want %d (%s)", got, tc.wantExpansion, tc.why)
			}
			referenced, err := s.referencedBlobDigests(nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, sha := range append([]string{msgSHA}, fileSHAs...) {
				if _, ok := referenced[sha]; !ok {
					t.Errorf("the keep set does not cover %s (%s)", shortID(sha), tc.why)
				}
			}
			if unreferenced, _, _ := s.unreferencedCheckpoints(); unreferenced != 0 {
				t.Fatalf("%d checkpoint(s) still without references after the backfill", unreferenced)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Deploy, roll back, roll forward
// ---------------------------------------------------------------------------

// The sequence the one-time backfill could not survive: this build runs and
// sweeps, the hub is rolled back to a build that records no edges, checkpoints
// are created, the hub is rolled forward. Those checkpoints are 'ready' with a
// valid manifest and zero edges. The sweep must decline while they exist, the
// next cycle must give them edges, and their blobs must survive.
func TestRollbackCheckpointsGetReferencesBeforeTheSweepRuns(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Now()
	s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{Enabled: boolPtr(true)}}
	insertRetentionClaw(t, s, "claw", reference)

	// This build: a checkpoint planned and published, then a sweep.
	beforeRoot, beforeFiles, beforePlan := planTreeFixture(t, 2)
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp-before", clawID: "claw", status: "creating", createdAt: reference, noBlobRefs: true})
	if err := s.recordCheckpointBlobRefs("cp-before", beforeRoot, beforePlan); err != nil {
		t.Fatal(err)
	}
	if err := s.finalizeCheckpoint("cp-before", "tenant", "claw", beforeRoot); err != nil {
		t.Fatal(err)
	}
	if err := s.backfillCheckpointBlobRefs(); err != nil {
		t.Fatal(err)
	}
	if result, err := s.sweepCheckpointBlobsResult(false, nil); err != nil || result.declined || result.removed != 0 {
		t.Fatalf("first sweep: removed=%d declined=%v err=%v", result.removed, result.declined, err)
	}

	// The rollback day: a build that writes no edges publishes a checkpoint.
	// Its blobs are older than the grace window by the time we come back.
	rollbackRoot, rollbackFiles, _ := planTreeFixture(t, 3)
	rollbackMsg := writeRetentionBlob(t, []byte(`{"id":"m-rollback"}`))
	ageBlob(t, rollbackMsg)
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp-rollback", clawID: "claw", status: "ready", createdAt: reference,
		rootTree: rollbackRoot, messageTree: rollbackMsg, writeManifest: true, noBlobRefs: true})

	// Roll forward. A sweep that runs before the backfill must decline: the
	// keep set omits cp-rollback entirely.
	out := captureRetentionLog(t, func() {
		result, err := s.sweepCheckpointBlobsResult(false, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !result.declined || result.unreferenced != 1 {
			t.Fatalf("sweep after a rollback: declined=%v unreferenced=%d, want declined with 1", result.declined, result.unreferenced)
		}
		if result.removed != 0 {
			t.Fatalf("the sweep removed %d blob(s) of a checkpoint that had no edges", result.removed)
		}
	})
	if !strings.Contains(out, "DECLINED: 1 checkpoint(s)") || !strings.Contains(out, shortID("cp-rollback")) {
		t.Fatalf("the declined line must name the count and the checkpoint; log was:\n%s", out)
	}

	// The full cycle: the backfill runs first, then the sweep.
	out = captureRetentionLog(t, s.retentionSweepOnce)
	if strings.Contains(out, "DECLINED") {
		t.Fatalf("the cycle still declined after the backfill had run; log was:\n%s", out)
	}
	if got := checkpointBlobRefCount(t, s, "cp-rollback"); got != 3 {
		t.Fatalf("the rollback checkpoint holds %d edges after the cycle, want 3", got)
	}
	if got := treeBlobRefCount(t, s, rollbackRoot); got != len(rollbackFiles) {
		t.Fatalf("the rollback checkpoint's tree has %d expansion rows, want %d", got, len(rollbackFiles))
	}
	for _, sha := range append(append([]string{rollbackRoot, rollbackMsg, beforeRoot}, rollbackFiles...), beforeFiles...) {
		if _, err := os.Stat(checkpointBlobPath(sha)); err != nil {
			t.Errorf("blob %s did not survive the roll-forward cycle: %v", shortID(sha), err)
		}
	}
}

// ---------------------------------------------------------------------------
// Transitions whose omission no other test saw
// ---------------------------------------------------------------------------

// A 'complete' can name a root the plan never mentioned. The root edge alone
// would reference a tree with no expansion behind it, and every file it lists
// would be unprotected; finalize has to expand it.
func TestFinalizeExpandsARootThePlanNeverNamed(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Now()
	insertRetentionClaw(t, s, "claw", reference)
	plannedRoot, _, plan := planTreeFixture(t, 2)
	completedRoot, completedFiles, _ := planTreeFixture(t, 3)
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "creating", createdAt: reference, noBlobRefs: true})
	if err := s.recordCheckpointBlobRefs("cp", plannedRoot, plan); err != nil {
		t.Fatal(err)
	}
	if err := s.finalizeCheckpoint("cp", "tenant", "claw", completedRoot); err != nil {
		t.Fatal(err)
	}
	if got := treeBlobRefCount(t, s, completedRoot); got != len(completedFiles) {
		t.Fatalf("the completed root has %d expansion rows, want %d: finalize did not expand a tree the plan never named", got, len(completedFiles))
	}
	referenced, err := s.referencedBlobDigests(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, sha := range completedFiles {
		if _, ok := referenced[sha]; !ok {
			t.Errorf("file %s of the completed root is not in the keep set", shortID(sha))
		}
	}
}

// Expiry deletes the row before unlinking the manifest, for the same reason
// compaction marks before it unlinks: a failed DELETE after an unlink leaves a
// 'ready' row pointing at a manifest that is gone, and the next cycle can pick
// it as a claw's survivor. A read-only handle makes the DELETE fail and leaves
// every read working, like the disk-full hub.
func TestExpiryDeletesTheRowBeforeUnlinkingTheManifest(t *testing.T) {
	s, path := newFileBackedRetentionServer(t)
	reference := time.Now()
	insertRetentionClaw(t, s, "claw", reference)
	manifest := insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp-old", clawID: "claw", status: "ready",
		createdAt: reference.Add(-100 * 24 * time.Hour), writeManifest: true})

	readOnly, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	readOnly.SetMaxOpenConns(1)
	if _, err := readOnly.Exec(`PRAGMA query_only = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := readOnly.Exec(`DELETE FROM claw_checkpoints WHERE id='none'`); err == nil {
		t.Fatal("the fixture is not read-only, so it cannot make the DELETE fail")
	}
	s.db = readOnly

	removed, _, _, err := s.pruneExpiredCheckpoints(reference.Add(-90*24*time.Hour), false, newRetentionPacer("checkpoints", time.Time{}))
	if err == nil {
		t.Fatal("the failed DELETE was not reported; the fixture did not exercise the failure")
	}
	if len(removed) != 0 {
		t.Fatalf("removed=%v after a failed DELETE, want none", removed)
	}
	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("the manifest was unlinked before the row was deleted; a failed DELETE now leaves a 'ready' row pointing at nothing: %v", err)
	}
	status, manifestPath, _ := retentionCheckpointRow(t, s, "cp-old")
	if status != "ready" || manifestPath != manifest {
		t.Fatalf("row = %q %q after a failed DELETE, want ready %q", status, manifestPath, manifest)
	}
}

// An edge whose checkpoint row is gone -- a crash between a row delete and its
// edge delete in some earlier build -- pins its blobs for the life of the
// database, because the keep set reads edges and nothing reads rows. Boot is
// where it is released.
func TestBootReleasesEdgesWhoseCheckpointIsGone(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Now()
	insertRetentionClaw(t, s, "claw", reference)
	orphaned := writeRetentionBlob(t, []byte("held by an edge with no row"))
	ageBlob(t, orphaned)
	if _, err := s.db.Exec(`INSERT INTO checkpoint_blob_refs(checkpoint_id, sha256) VALUES('cp-gone', ?)`, orphaned); err != nil {
		t.Fatal(err)
	}

	// The orphan pins the blob: that is why it has to be released.
	if removed, _, _, err := s.sweepCheckpointBlobs(false, nil); err != nil || removed != 0 {
		t.Fatalf("sweep with the orphan in place: removed=%d err=%v, want 0 (the fixture would prove nothing)", removed, err)
	}

	s.reconcileCheckpointsOnBoot()
	if got := checkpointBlobRefCount(t, s, "cp-gone"); got != 0 {
		t.Fatalf("boot left %d edge(s) whose checkpoint row does not exist", got)
	}
	if removed, _, _, err := s.sweepCheckpointBlobs(false, nil); err != nil || removed != 1 {
		t.Fatalf("sweep after boot: removed=%d err=%v, want 1", removed, err)
	}
	if _, err := os.Stat(checkpointBlobPath(orphaned)); !os.IsNotExist(err) {
		t.Fatalf("the blob the orphaned edge pinned survived: %v", err)
	}
}

// A skipped row is a genuine holder of the tree it duplicated. The plan
// normally recorded that root already, which is why removing the insert from
// markCheckpointSkipped went unnoticed; a skip against a row with no plan edges
// is where it has to do the work itself.
func TestSkipRecordsTheRootTreeWithoutAPlan(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Now()
	insertRetentionClaw(t, s, "claw", reference)
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "creating", createdAt: reference, noBlobRefs: true})
	if got := checkpointBlobRefCount(t, s, "cp"); got != 0 {
		t.Fatalf("fixture seeded %d edges, want 0", got)
	}
	if err := s.markCheckpointSkipped("cp", testTreeSHA); err != nil {
		t.Fatal(err)
	}
	var sha string
	if err := s.db.QueryRow(`SELECT sha256 FROM checkpoint_blob_refs WHERE checkpoint_id='cp'`).Scan(&sha); err != nil {
		t.Fatalf("the skipped row holds no edge: %v", err)
	}
	if sha != testTreeSHA {
		t.Fatalf("the skipped row holds %s, want its root tree %s", sha, testTreeSHA)
	}
}
