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
// The backfill and its gate
// ---------------------------------------------------------------------------

// A hub that has just upgraded into reference counting has rows with no edges.
// Read literally, that table says every blob in the store is garbage -- so the
// sweeper must refuse to act on it until the backfill has completed at least
// once, and must say so loudly rather than silently reporting a quiet cycle.
func TestBlobSweepDeclinesUntilTheBackfillHasCompleted(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Now()
	insertRetentionClaw(t, s, "claw", reference)

	fileSHA := writeRetentionBlob(t, []byte("the workspace of a checkpoint that predates refcounting"))
	tree, _ := json.Marshal([]types.CheckpointFile{{Path: "workspace/a.txt", SHA256: fileSHA, Size: 55}})
	treeSHA := writeRetentionBlob(t, tree)
	for _, sha := range []string{fileSHA, treeSHA} {
		ageBlob(t, sha)
	}
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "ready", createdAt: reference,
		rootTree: treeSHA, writeManifest: true, noBlobRefs: true})

	out := captureRetentionLog(t, func() {
		removed, _, _, err := s.sweepCheckpointBlobs(false, nil)
		if err != nil {
			t.Fatalf("sweepCheckpointBlobs: %v", err)
		}
		if removed != 0 {
			t.Fatalf("the sweep removed %d blobs before the backfill completed; a half-populated edge table reads exactly like 'everything is garbage'", removed)
		}
	})
	if !strings.Contains(out, "DECLINED") {
		t.Fatalf("declining to sweep must be loud; log was:\n%s", out)
	}
	for _, sha := range []string{fileSHA, treeSHA} {
		if _, err := os.Stat(checkpointBlobPath(sha)); err != nil {
			t.Fatalf("a blob was deleted while the sweep was supposed to be declining: %v", err)
		}
	}

	// After the backfill the same blobs are referenced and the sweep runs.
	if err := s.backfillCheckpointBlobRefs(); err != nil {
		t.Fatalf("backfillCheckpointBlobRefs: %v", err)
	}
	removed, _, _, err := s.sweepCheckpointBlobs(false, nil)
	if err != nil {
		t.Fatalf("sweepCheckpointBlobs: %v", err)
	}
	if removed != 0 {
		t.Fatalf("the sweep removed %d blobs the backfill had just recorded references for", removed)
	}
	for _, sha := range []string{fileSHA, treeSHA} {
		if _, err := os.Stat(checkpointBlobPath(sha)); err != nil {
			t.Fatalf("backfilled reference did not protect %s: %v", sha, err)
		}
	}
}

// The backfill walks the same manifests and tree blobs the keep set used to
// rebuild every cycle -- once. It must be idempotent, and it must step over a
// file it cannot read: the disk-full hub this feature exists for is guaranteed
// to have some, and one of them aborting the traversal would leave the gate
// closed for the length of the retention window.
func TestBlobReferenceBackfillIsIdempotentAndSurvivesUnreadableFiles(t *testing.T) {
	cases := []struct {
		name         string
		manifestBody []byte
		why          string
	}{
		{
			name: "intact manifest",
			why:  "the ordinary case",
		},
		{
			name:         "truncated manifest",
			manifestBody: []byte("{ truncated by ENOSP"),
			why:          "a manifest a full disk cut short must not abort the traversal, and the row's own digests still get recorded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRetentionTestServer(t)
			reference := time.Now()
			insertRetentionClaw(t, s, "claw", reference)

			fileSHA := writeRetentionBlob(t, []byte("workspace file"))
			tree, _ := json.Marshal([]types.CheckpointFile{{Path: "workspace/a.txt", SHA256: fileSHA, Size: 14}})
			treeSHA := writeRetentionBlob(t, tree)
			msgSHA := writeRetentionBlob(t, []byte(`{"id":"m1"}`))
			insertRetentionCheckpoint(t, s, retentionCheckpoint{
				id: "cp", clawID: "claw", status: "ready", createdAt: reference,
				rootTree: treeSHA, messageTree: msgSHA, writeManifest: true,
				manifestBody: tc.manifestBody, noBlobRefs: true})

			if err := s.backfillCheckpointBlobRefs(); err != nil {
				t.Fatalf("backfill (%s): %v", tc.why, err)
			}
			first := checkpointBlobRefCount(t, s, "cp")
			referenced, err := s.referencedBlobDigests(nil)
			if err != nil {
				t.Fatal(err)
			}
			// The tree is named by the row, so it is recorded whatever the
			// manifest says -- and following it is what recovers the per-file
			// digests, which live nowhere else under schema 2.
			for _, sha := range []string{treeSHA, msgSHA, fileSHA} {
				if _, ok := referenced[sha]; !ok {
					t.Errorf("backfill did not record %s (%s)", sha, tc.why)
				}
			}

			// A second run must write nothing and must not fail: a row with an
			// edge is not on the work list. A row that has LOST its edges is,
			// and gets the same set back -- which is the rollback case, and
			// proves the INSERT OR IGNORE underneath is what makes a re-run safe.
			if err := s.backfillCheckpointBlobRefs(); err != nil {
				t.Fatalf("second backfill: %v", err)
			}
			if got := checkpointBlobRefCount(t, s, "cp"); got != first {
				t.Fatalf("a second backfill changed the reference count from %d to %d", first, got)
			}
			if _, err := s.db.Exec(`DELETE FROM checkpoint_blob_refs WHERE checkpoint_id='cp'`); err != nil {
				t.Fatal(err)
			}
			if err := s.backfillCheckpointBlobRefs(); err != nil {
				t.Fatalf("re-run backfill: %v", err)
			}
			if got := checkpointBlobRefCount(t, s, "cp"); got != first {
				t.Fatalf("re-running the backfill over an edge-less row gave it %d references, want %d; it must be idempotent", got, first)
			}
		})
	}
}

// A crash mid-backfill leaves edges for some checkpoints and none for the rest.
// That is indistinguishable from "most blobs are garbage", so the marker must
// not have been written and the sweep must still decline.
func TestInterruptedBackfillLeavesTheGateClosed(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Now()
	insertRetentionClaw(t, s, "claw", reference)
	covered := writeRetentionBlob(t, []byte("a checkpoint the backfill reached"))
	uncovered := writeRetentionBlob(t, []byte("a checkpoint the crash came before"))
	ageBlob(t, covered)
	ageBlob(t, uncovered)
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp-covered", clawID: "claw", status: "ready", createdAt: reference,
		rootTree: covered, noBlobRefs: true})
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp-uncovered", clawID: "claw", status: "ready", createdAt: reference,
		rootTree: uncovered, noBlobRefs: true})
	// The crash: the first checkpoint's edges landed, the second's never did.
	if err := s.insertCheckpointBlobRefs("cp-covered", []string{covered}); err != nil {
		t.Fatal(err)
	}

	removed, _, _, err := s.sweepCheckpointBlobs(false, nil)
	if err != nil {
		t.Fatalf("sweepCheckpointBlobs: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed %d blobs on a half-populated edge table", removed)
	}
	if _, err := os.Stat(checkpointBlobPath(uncovered)); err != nil {
		t.Fatalf("the blob of the checkpoint the crash came before was swept: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Compaction releases by construction
// ---------------------------------------------------------------------------

// Compaction drops the checkpoint's edges in the same transaction as its status
// change, so the blob is collectable by the time the sweep runs later in the
// SAME cycle. Under the old design compaction cleared digest columns and the
// sweeper had to agree, which it did not for several kinds of row.
func TestCompactionReleasesEdgesAndTheBlobGoesInTheSameCycle(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s.nowFunc = func() time.Time { return reference }
	s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{Enabled: boolPtr(true)}}
	insertRetentionClaw(t, s, "claw", reference)
	insertRetentionPR(t, s, "claw", 1)

	makeCheckpoint := func(id, contents string, createdAt time.Time) (fileSHA, treeSHA string) {
		fileSHA = writeRetentionBlob(t, []byte(contents+" file"))
		tree, _ := json.Marshal([]types.CheckpointFile{{Path: "workspace/a.txt", SHA256: fileSHA, Size: 1}})
		treeSHA = writeRetentionBlob(t, tree)
		ageBlob(t, fileSHA)
		ageBlob(t, treeSHA)
		insertRetentionCheckpoint(t, s, retentionCheckpoint{
			id: id, clawID: "claw", status: "ready", createdAt: createdAt,
			rootTree: treeSHA, writeManifest: true})
		return
	}
	oldFile, oldTree := makeCheckpoint("cp-old", "old", reference.Add(-48*time.Hour))
	newFile, newTree := makeCheckpoint("cp-new", "new", reference.Add(-24*time.Hour))

	s.retentionSweepOnce()

	if got := checkpointBlobRefCount(t, s, "cp-old"); got != 0 {
		t.Fatalf("the compacted checkpoint still holds %d references", got)
	}
	if got := checkpointBlobRefCount(t, s, "cp-new"); got == 0 {
		t.Fatal("the surviving checkpoint released its references")
	}
	for _, sha := range []string{oldFile, oldTree} {
		if _, err := os.Stat(checkpointBlobPath(sha)); !os.IsNotExist(err) {
			t.Errorf("blob %s of the compacted checkpoint survived its own cycle", sha)
		}
	}
	for _, sha := range []string{newFile, newTree} {
		if _, err := os.Stat(checkpointBlobPath(sha)); err != nil {
			t.Errorf("sweep deleted a blob of the kept checkpoint %s: %v", sha, err)
		}
	}
}

// Compaction marks the row BEFORE unlinking the manifest. The other order left
// a window where a failed UPDATE produced a 'ready' row pointing at a file that
// no longer exists -- which the next cycle can pick as the claw's survivor.
//
// A successful pass ends in the same state under either order, so the only way
// to see the difference is to make the UPDATE fail: a read-only handle lets
// every SELECT succeed and every write fail, exactly as the disk-full hub does.
// Mark-then-unlink leaves the manifest on disk and the row 'ready'; unlink-
// then-mark leaves a 'ready' row whose manifest is gone.
func TestCompactionMarksTheRowBeforeUnlinkingTheManifest(t *testing.T) {
	s, path := newFileBackedRetentionServer(t)
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	insertRetentionClaw(t, s, "claw", reference)
	insertRetentionPR(t, s, "claw", 1)
	oldManifest := insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp-old", clawID: "claw", status: "ready",
		createdAt: reference.Add(-48 * time.Hour), writeManifest: true})
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp-new", clawID: "claw", status: "ready",
		createdAt: reference.Add(-24 * time.Hour), writeManifest: true})

	readOnly, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	readOnly.SetMaxOpenConns(1)
	if _, err := readOnly.Exec(`PRAGMA query_only = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := readOnly.Exec(`UPDATE claw_checkpoints SET error='x' WHERE id='cp-old'`); err == nil {
		t.Fatal("the fixture is not read-only, so it cannot make the UPDATE fail")
	}
	s.db = readOnly

	result, err := s.compactFinalizedCheckpoints(reference.Add(-240*time.Hour), false)
	if err != nil {
		t.Fatalf("compactFinalizedCheckpoints: %v", err)
	}
	if result.itemErrors == 0 {
		t.Fatal("the failed UPDATE was not reported; the fixture did not exercise the failure")
	}
	if result.count != 0 {
		t.Fatalf("compacted=%d after a failed UPDATE, want 0", result.count)
	}
	if _, err := os.Stat(oldManifest); err != nil {
		t.Fatalf("the manifest was unlinked before the row was marked; a failed UPDATE now leaves a 'ready' row pointing at nothing: %v", err)
	}
	status, manifestPath, _ := retentionCheckpointRow(t, s, "cp-old")
	if status != "ready" || manifestPath != oldManifest {
		t.Fatalf("row = %q %q after a failed UPDATE, want ready %q", status, manifestPath, oldManifest)
	}
}

// ---------------------------------------------------------------------------
// Publishing must not orphan a manifest
// ---------------------------------------------------------------------------

// A 'complete' that arrives after the row went terminal is rejected. It used to
// write the manifest first and reject afterwards, leaving a file no row claims
// and nothing ever removes.
//
// The manifest alone cannot tell the pre-check apart from the cleanup after the
// guarded UPDATE: both leave no manifest. What only the pre-check prevents is
// everything written BEFORE the manifest -- the message blob goes into the blob
// store and stays there, and the manifests directory is created -- so those are
// what the test looks at.
func TestRejectedPublishLeavesNoOrphanManifest(t *testing.T) {
	cases := []struct {
		name  string
		apply func(s *Server) error
	}{
		{
			name:  "finalize",
			apply: func(s *Server) error { return s.finalizeCheckpoint("cp", "tenant", "claw", "") },
		},
		{
			name: "metadata-only",
			apply: func(s *Server) error {
				return s.completeMetadataOnlyCheckpoint("cp", "claw", "termination:kill", "bridge unreachable")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRetentionTestServer(t)
			reference := time.Now()
			insertRetentionClaw(t, s, "claw", reference)
			insertRetentionCheckpoint(t, s, retentionCheckpoint{
				id: "cp", clawID: "claw", status: "failed", createdAt: reference})
			if _, err := s.db.Exec(`INSERT INTO messages(id, claw_id, tenant_id, role, content, created_at) VALUES(?,?,?,?,?,?)`,
				"m1", "claw", "tenant", "user", "a message the blob would capture", reference); err != nil {
				t.Fatal(err)
			}
			before := checkpointStoreFiles(t)

			if err := tc.apply(s); err == nil {
				t.Fatal("publishing succeeded against a row that had already gone terminal")
			}
			if _, err := os.Stat(checkpointManifestPath("cp")); !os.IsNotExist(err) {
				t.Fatalf("a rejected publish left a manifest behind: %v", err)
			}
			if after := checkpointStoreFiles(t); after != before {
				t.Fatalf("a rejected publish wrote into the checkpoint store before checking the row; the pre-check is gone.\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

// checkpointStoreFiles lists every file under the checkpoint store, so a test
// can assert a rejected operation wrote nothing at all.
func checkpointStoreFiles(t *testing.T) string {
	t.Helper()
	var files []string
	err := filepath.Walk(checkpointsRoot(), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !info.IsDir() {
			files = append(files, strings.TrimPrefix(path, checkpointsRoot()))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(files, "\n")
}

// ---------------------------------------------------------------------------
// The survivor fallback
// ---------------------------------------------------------------------------

// When nothing is retry-eligible, the fallback still has to honour the same
// predicate retry uses. Ignoring manifest_path let it keep a row retry filters
// out outright, and treating every historically restored id as ineligible is
// not what retry does either -- retry only skips the LATEST attempt's.
func TestNewestRestorableFallbackHonoursTheRetryPredicate(t *testing.T) {
	cand := func(id, root, manifest string) compactionCandidate {
		return compactionCandidate{id: id, reason: "bootstrap", rootTree: root, manifestPath: manifest}
	}
	cases := []struct {
		name       string
		candidates []compactionCandidate
		restored   map[string]struct{}
		want       int
		why        string
	}{
		{
			name:       "a manifest-less row is skipped",
			candidates: []compactionCandidate{cand("a", "tree-a", ""), cand("b", "tree-b", "/m/b.json")},
			want:       1, why: "retryCheckpointIDWithCount requires manifest_path != ''",
		},
		{
			name:       "a metadata-only row is skipped",
			candidates: []compactionCandidate{cand("a", "", "/m/a.json"), cand("b", "tree-b", "/m/b.json")},
			want:       1, why: "an empty root tree restores nothing",
		},
		{
			name:       "the latest attempt's checkpoint is skipped",
			candidates: []compactionCandidate{cand("a", "tree-a", "/m/a.json"), cand("b", "tree-b", "/m/b.json")},
			restored:   map[string]struct{}{"a": {}},
			want:       1, why: "retry refuses to restore the same checkpoint the previous attempt used",
		},
		{
			name:       "the restored rule yields rather than keeping nothing usable",
			candidates: []compactionCandidate{cand("a", "tree-a", "/m/a.json")},
			restored:   map[string]struct{}{"a": {}},
			want:       0, why: "keeping the checkpoint the last attempt used still beats keeping one that restores nothing",
		},
		{
			name:       "nothing satisfies any of it",
			candidates: []compactionCandidate{cand("a", "", ""), cand("b", "", "")},
			want:       0, why: "keep the newest; the row is still the analytics record",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := newestRestorableCheckpoint(tc.candidates, tc.restored); got != tc.want {
				t.Fatalf("newestRestorableCheckpoint = %d, want %d (%s)", got, tc.want, tc.why)
			}
		})
	}
}

// restoredCheckpointIDs must name the LATEST attempt's checkpoint only, exactly
// as retryCheckpointIDWithCount does. Excluding every historically restored id
// can only make compaction discard a checkpoint the next retry would take.
func TestRestoredCheckpointIDsNamesOnlyTheLatestAttempt(t *testing.T) {
	s := newRetentionTestServer(t)
	insertRetentionClaw(t, s, "claw", time.Now())
	if _, err := s.db.Exec(`UPDATE claws SET task_run_id='run' WHERE id='claw'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO task_runs(id, tenant_id, initial_attempt_id, run_kind, owner_type, created_at, updated_at)
		VALUES('run','tenant','a1','code_task','manual',0,0)`); err != nil {
		t.Fatal(err)
	}
	for _, a := range []struct {
		id         string
		number     int
		checkpoint string
	}{{"a1", 1, "cp-old"}, {"a2", 2, "cp-recent"}} {
		if _, err := s.db.Exec(`INSERT INTO task_run_attempts(id, tenant_id, run_id, attempt_id, attempt_number, claw_id, status, restored_checkpoint_id, started_at, created_at, updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			a.id, "tenant", "run", a.id, a.number, "claw", "running", a.checkpoint, 0, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.restoredCheckpointIDs("claw")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["cp-recent"]; !ok {
		t.Fatal("the latest attempt's checkpoint must be excluded from compaction's survivor choice")
	}
	if _, ok := got["cp-old"]; ok {
		t.Fatal("an older attempt's checkpoint must not be excluded; retry will happily reuse it")
	}
}

// ---------------------------------------------------------------------------
// Cycle mechanics
// ---------------------------------------------------------------------------

// The retention indexes are a performance optimisation for the row deletes, and
// building one needs free disk for the whole B-tree. Running that at the top of
// a cycle on a full disk spends the write lock on a doomed build before a single
// byte is reclaimed.
func TestRetentionIndexesAreBuiltAfterTheCycleReclaims(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s.nowFunc = func() time.Time { return reference }
	s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{Enabled: boolPtr(true)}}
	for _, idx := range retentionIndexes {
		if _, err := s.db.Exec(`DROP INDEX ` + idx.name); err != nil {
			t.Fatal(err)
		}
	}
	// A blob the sweep will remove, so there is something to observe the order
	// against: the index must be absent while the reclamation runs.
	orphan := writeRetentionBlob(t, []byte("unreferenced"))
	ageBlob(t, orphan)
	var indexesDuringSweep int
	var hookFired bool
	blobSweepFileHook = func(string) {
		hookFired = true
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name IN (?,?)`,
			retentionIndexes[0].name, retentionIndexes[1].name).Scan(&indexesDuringSweep)
	}
	t.Cleanup(func() { blobSweepFileHook = nil })

	s.retentionSweepOnce()

	if !hookFired {
		t.Fatal("the hook never fired; the fixture did not reach the walk and the assertion below is vacuous")
	}
	if indexesDuringSweep != 0 {
		t.Fatalf("%d retention index(es) existed during the blob sweep; they must be built after reclamation, not before it", indexesDuringSweep)
	}
	for _, idx := range retentionIndexes {
		if !retentionIndexExists(t, s, idx.name) {
			t.Fatalf("%s was not rebuilt by the end of the cycle", idx.name)
		}
	}
}

// bytes_freed counted blobs only, so a cycle that removed nothing but manifests
// and diagnostics logs reported zero -- the same number a cycle that found
// nothing reports.
func TestCycleReportsManifestAndDiagnosticsBytes(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s.nowFunc = func() time.Time { return reference }
	s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{Enabled: boolPtr(true)}}
	expired := reference.Add(-defaultRetentionMaxAge - 24*time.Hour)
	insertRetentionClaw(t, s, "claw", expired)
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "ready", createdAt: expired, writeManifest: true})

	diagDir := filepath.Join(hubDataDir(), "diagnostics")
	if err := os.MkdirAll(diagDir, 0o750); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(diagDir, "claw-gateway.log")
	if err := os.WriteFile(logPath, []byte(strings.Repeat("x", 512)), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(logPath, expired, expired); err != nil {
		t.Fatal(err)
	}

	out := captureRetentionLog(t, s.retentionSweepOnce)
	if strings.Contains(out, "bytes_freed=0 ") {
		t.Fatalf("a cycle that removed a manifest and a diagnostics log reported bytes_freed=0:\n%s", out)
	}
	if !strings.Contains(out, "diagnostics=512") {
		t.Fatalf("the byte breakdown does not attribute the diagnostics log:\n%s", out)
	}
	if !strings.Contains(out, "VACUUM") {
		t.Fatalf("the cycle line must say that row deletes do not shrink the database file:\n%s", out)
	}
}

// A dry run used to log one line per selected item. The documented example
// selects ~43k of them against journald's 10k-per-30s burst, which drops the
// summary line the run exists to produce.
func TestDryRunAggregatesInsteadOfLoggingPerItem(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s.nowFunc = func() time.Time { return reference }
	s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{Enabled: boolPtr(true), DryRun: true}}
	expired := reference.Add(-defaultRetentionMaxAge - 24*time.Hour)
	insertRetentionClaw(t, s, "claw", expired)

	const blobs = 40
	for i := 0; i < blobs; i++ {
		sha := writeRetentionBlob(t, []byte(fmt.Sprintf("orphan %d", i)))
		ageBlob(t, sha)
	}
	for i := 0; i < 20; i++ {
		insertRetentionCheckpoint(t, s, retentionCheckpoint{
			id: fmt.Sprintf("cp-%d", i), clawID: "claw", status: "ready",
			createdAt: expired, writeManifest: true})
	}

	out := captureRetentionLog(t, s.retentionSweepOnce)
	dryRunLines := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.Contains(line, "dry_run:") {
			dryRunLines++
		}
	}
	// One aggregate line per phase that selected anything, never one per item.
	if dryRunLines > 4 {
		t.Fatalf("a dry run emitted %d dry_run lines for 60 selected items; it must aggregate:\n%s", dryRunLines, out)
	}
	if !strings.Contains(out, fmt.Sprintf("%d blob(s)", blobs)) {
		t.Fatalf("the aggregate line does not report the blob total:\n%s", out)
	}
	if !strings.Contains(out, "and 35 more") {
		t.Fatalf("the aggregate line must carry a bounded sample and say how many it elided:\n%s", out)
	}
}

// Go's ParseDuration has no day unit, so `max_age: 30d` is rejected and silently
// replaced by the 90-day default -- under a startup line that said
// adjustments=[none].
func TestRejectedDurationsAppearInTheAdjustments(t *testing.T) {
	s := newRetentionTestServer(t)
	s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{
		Enabled: boolPtr(true), MaxAge: "30d"}}
	cfg := s.retentionSettings()
	if cfg.maxAge != defaultRetentionMaxAge {
		t.Fatalf("max_age = %s, want the default", cfg.maxAge)
	}
	joined := strings.Join(cfg.adjustments, "; ")
	if !strings.Contains(joined, "max_age") || !strings.Contains(joined, "30d") {
		t.Fatalf("adjustments = %q; a rejected value is as much an override as a clamped one", joined)
	}
	out := captureRetentionLog(t, s.logRetentionPolicy)
	if strings.Contains(out, "adjustments=[none]") {
		t.Fatalf("the startup line claims no adjustments after silently rewriting max_age:\n%s", out)
	}
}

// The probe exists to keep a once-a-minute reconciliation off the hub's single
// write lock. It used to run AFTER an unconditional UPDATE, which meant the
// write lock was taken every minute regardless.
func TestFailStuckCreatingCheckpointsProbesBeforeWriting(t *testing.T) {
	s, path := newFileBackedRetentionServer(t)
	insertRetentionClaw(t, s, "claw", now())
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "creating", createdAt: now()})

	// A read-only handle: every write fails, every read succeeds. With nothing
	// stuck and no orphaned references, a correct implementation never writes.
	readOnly, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	// One connection, so the pragma cannot be set on a connection the test then
	// fails to use.
	readOnly.SetMaxOpenConns(1)
	if _, err := readOnly.Exec(`PRAGMA query_only = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := readOnly.Exec(`UPDATE claw_checkpoints SET error='x' WHERE id='cp'`); err == nil {
		t.Fatal("the fixture is not read-only, so it cannot tell a probe from a write")
	}
	if err := readOnly.QueryRow(`SELECT 1 FROM claw_checkpoints WHERE id='cp'`).Scan(new(int)); err != nil {
		t.Fatalf("the fixture cannot read either, so every path fails and the test proves nothing: %v", err)
	}
	s.db = readOnly

	out := captureRetentionLog(t, s.failStuckCreatingCheckpoints)
	if strings.Contains(out, "fail stuck creating checkpoints") || strings.Contains(out, "release orphaned") {
		t.Fatalf("the steady state took the write path:\n%s", out)
	}
}
