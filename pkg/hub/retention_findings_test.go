package hub

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// Tests for the findings of the third review loop. Each one was written so
// that reverting the change it guards fails it; the revert performed for each
// is named in its comment.

// ---------------------------------------------------------------------------
// The gate selects exactly what the writer accepts
// ---------------------------------------------------------------------------

// Revert verified against: unreferencedCheckpointsWhere testing the digest
// columns with `COALESCE(col,”) <> ”` instead of blobDigestSQL.
//
// The backfill records only values normalizeBlobDigest accepts. A row the gate
// selects that yields no edge stays on the work list forever, and the sweep
// declines forever, so the gate must select a row if and only if the writer
// would record at least one of its digests. Every value here is inserted as
// the row's only digest column and the two verdicts are held to each other.
func TestGatePredicateMatchesTheWriter(t *testing.T) {
	valid := strings.Repeat("0123456789abcdef", 4)
	values := []string{
		valid,
		"sha256:" + valid,
		"  " + valid + "  ",
		"\t" + valid + "\n",
		" sha256:" + valid + " ",
		strings.ToUpper(valid),
		"sha256:" + strings.ToUpper(valid),
		valid[:63],
		valid + "0",
		strings.Repeat("g", 64),
		valid[:63] + "G",
		"not-a-digest",
		"sha256:",
		"",
	}
	s := newRetentionTestServer(t)
	insertRetentionClaw(t, s, "claw", time.Now())
	for i, value := range values {
		for _, column := range []string{"manifest_sha256", "root_tree_sha256", "message_tree_sha256", "workspace_tree_sha256"} {
			id := fmt.Sprintf("cp-%d-%s", i, column)
			if _, err := s.db.Exec(`INSERT INTO claw_checkpoints(id, tenant_id, claw_id, status, created_at) VALUES(?,?,?,?,?)`,
				id, "tenant", "claw", "ready", time.Now()); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(fmt.Sprintf(`UPDATE claw_checkpoints SET %s=? WHERE id=?`, column), value, id); err != nil {
				t.Fatal(err)
			}
			var selected int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM claw_checkpoints WHERE id=? AND `+unreferencedCheckpointsWhere, id).Scan(&selected); err != nil {
				t.Fatalf("gate on %q: %v", value, err)
			}
			writerAccepts := normalizeBlobDigest(value) != ""
			if (selected == 1) != writerAccepts {
				t.Errorf("%s=%q: gate selects=%v, writer accepts=%v; the two must agree or the sweep declines on this row forever",
					column, value, selected == 1, writerAccepts)
			}
		}
	}
}

// The same, through a cycle. A 'skipped' row whose only digest is one the
// writer rejects must neither hold the sweep DECLINED nor be reported as
// "given references".
func TestBackfillDoesNotHoldTheSweepOnARowItCannotRecord(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s.nowFunc = func() time.Time { return reference }
	s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{Enabled: boolPtr(true)}}
	insertRetentionClaw(t, s, "claw", reference)
	if _, err := s.db.Exec(`INSERT INTO claw_checkpoints(id, tenant_id, claw_id, status, root_tree_sha256, created_at) VALUES(?,?,?,?,?,?)`,
		"skipped", "tenant", "claw", "skipped", strings.ToUpper(testTreeSHA[:60])+"WXYZ", reference); err != nil {
		t.Fatal(err)
	}
	orphan := writeRetentionBlob(t, []byte("nothing references this"))
	ageBlob(t, orphan)

	out := captureRetentionLog(t, s.retentionSweepOnce)
	if strings.Contains(out, "DECLINED") {
		t.Fatalf("the sweep declined on a row that names no recordable digest:\n%s", out)
	}
	if strings.Contains(out, "given references") {
		t.Fatalf("the backfill claimed progress on a row it could not record:\n%s", out)
	}
	if _, err := os.Stat(checkpointBlobPath(orphan)); !os.IsNotExist(err) {
		t.Fatalf("the orphan blob survived: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The tree gc does not collect what the backfill just wrote
// ---------------------------------------------------------------------------

// Revert verified against: the tree reference gc running whenever the cycle
// is not a dry run, instead of only when the sweep ran.
//
// The backfill commits a tree's expansion before the edge of the row that
// names it, and the budget can fire on the yield in between. The gc selects
// trees no edge names, so it would delete the expansion in the same cycle and
// the next cycle would write it again.
func TestTreeGCDoesNotCollectAnExpansionTheBackfillJustWrote(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s.nowFunc = func() time.Time { return reference }
	s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{Enabled: boolPtr(true)}}
	countRetentionPauses(t)
	previous := retentionRowBudget
	retentionRowBudget = -time.Second
	t.Cleanup(func() { retentionRowBudget = previous })
	insertRetentionClaw(t, s, "claw", reference)
	rootSHA, fileSHAs, _ := planTreeFixture(t, 3)
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "ready", createdAt: reference,
		rootTree: rootSHA, writeManifest: true, noBlobRefs: true})

	out := captureRetentionLog(t, s.retentionSweepOnce)
	if !strings.Contains(out, "blob reference backfill: stopping after") || !strings.Contains(out, "DECLINED") {
		t.Fatalf("the fixture did not reach the interleaving (budget after the expansion, sweep declined):\n%s", out)
	}
	if got := checkpointBlobRefCount(t, s, "cp"); got != 0 {
		t.Fatalf("the row has %d edges; the fixture needs it left edge-less this cycle", got)
	}
	if got := treeBlobRefCount(t, s, rootSHA); got != len(fileSHAs) {
		t.Fatalf("the expansion has %d rows after the cycle, want %d: the tree gc collected what the backfill had just written", got, len(fileSHAs))
	}
	if strings.Contains(out, "tree reference gc") {
		t.Fatalf("the tree gc ran in a cycle whose sweep declined:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// The index build is gated, not unconditional
// ---------------------------------------------------------------------------

// Revert verified against: the cycle end calling ensureRetentionIndexes
// unconditionally.
//
// The build is the one write-lock hold in the cycle no pacer bounds. On the
// hub whose boot could not build the indexes, it scanned the two largest
// tables and failed at the last page at the end of every cycle. A cycle that
// freed nothing has nothing to offer it.
func TestRetentionIndexBuildWaitsForACycleThatReclaims(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s.nowFunc = func() time.Time { return reference }
	s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{Enabled: boolPtr(true)}}
	for _, idx := range retentionIndexes {
		if _, err := s.db.Exec(`DROP INDEX ` + idx.name); err != nil {
			t.Fatal(err)
		}
	}
	out := captureRetentionLog(t, s.retentionSweepOnce)
	for _, idx := range retentionIndexes {
		if retentionIndexExists(t, s, idx.name) {
			t.Fatalf("%s was built by a cycle that reclaimed nothing:\n%s", idx.name, out)
		}
	}
	if !strings.Contains(out, "the build waits for a cycle that reclaims disk space") {
		t.Fatalf("a missing index went unreported:\n%s", out)
	}
	// Said once, not every cycle.
	if again := captureRetentionLog(t, s.retentionSweepOnce); strings.Contains(again, "the build waits") {
		t.Fatalf("the waiting line repeats every cycle:\n%s", again)
	}

	orphan := writeRetentionBlob(t, []byte("unreferenced"))
	ageBlob(t, orphan)
	out = captureRetentionLog(t, s.retentionSweepOnce)
	for _, idx := range retentionIndexes {
		if !retentionIndexExists(t, s, idx.name) {
			t.Fatalf("%s was not built after a cycle that reclaimed bytes:\n%s", idx.name, out)
		}
	}
}

// Revert verified against: the same, and separately against a build that
// retries on every reclaiming cycle with no backoff.
//
// A failed build sits out an increasing number of reclaiming cycles. The
// failure is real: the database is forbidden to grow, so CREATE INDEX cannot
// allocate its root page, while the blobs the cycle reclaims are files.
func TestRetentionIndexBuildBacksOffAfterAFailure(t *testing.T) {
	s, _ := newFileBackedRetentionServer(t)
	s.db.SetMaxOpenConns(1)
	t.Cleanup(func() { s.db.Close() })
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s.nowFunc = func() time.Time { return reference }
	s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{Enabled: boolPtr(true)}}
	for _, idx := range retentionIndexes {
		if _, err := s.db.Exec(`DROP INDEX ` + idx.name); err != nil {
			t.Fatal(err)
		}
	}
	// The dropped indexes left free pages; without the VACUUM the cap would
	// be satisfied from the freelist and prove nothing.
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		t.Fatal(err)
	}
	forbidDatabaseGrowth(t, s.db)

	reclaimingCycle := func(i int) string {
		t.Helper()
		orphan := writeRetentionBlob(t, []byte(fmt.Sprintf("unreferenced %d", i)))
		ageBlob(t, orphan)
		out := captureRetentionLog(t, s.retentionSweepOnce)
		if !strings.Contains(out, "blobs=1") {
			t.Fatalf("cycle %d reclaimed nothing; the fixture is wrong:\n%s", i, out)
		}
		return out
	}
	attempted := func(out string) bool { return strings.Contains(out, "[migrate] retention index") }

	// Cycle 1 reclaims, attempts, fails.
	if out := reclaimingCycle(1); !attempted(out) {
		t.Fatalf("the first reclaiming cycle did not attempt the build:\n%s", out)
	}
	// Cycle 2 reclaims but sits out: one failure, one cycle of backoff.
	if out := reclaimingCycle(2); attempted(out) || !strings.Contains(out, "sitting out this cycle") {
		t.Fatalf("a failed build was retried on the very next reclaiming cycle:\n%s", out)
	}
	// Cycle 3 attempts again and fails; cycles 4-6 sit out.
	if out := reclaimingCycle(3); !attempted(out) {
		t.Fatalf("the backoff never ends:\n%s", out)
	}
	for i := 4; i <= 6; i++ {
		if out := reclaimingCycle(i); attempted(out) {
			t.Fatalf("cycle %d attempted the build inside the backoff:\n%s", i, out)
		}
	}
	if out := reclaimingCycle(7); !attempted(out) {
		t.Fatalf("cycle 7 should have attempted again:\n%s", out)
	}

	// With room to grow, the next reclaiming cycle in the schedule builds
	// them and the backoff state is forgotten.
	allowDatabaseGrowth(t, s.db)
	for i := 8; i <= 20; i++ {
		out := reclaimingCycle(i)
		if built := retentionIndexExists(t, s, retentionIndexes[0].name); built {
			if s.retentionIndexes != (retentionIndexBuild{}) {
				t.Fatalf("a successful build did not reset the backoff: %+v", s.retentionIndexes)
			}
			return
		}
		if attempted(out) {
			t.Fatalf("cycle %d attempted the build with room to grow and failed:\n%s", i, out)
		}
	}
	t.Fatal("the indexes were never built once the disk had room")
}

// ---------------------------------------------------------------------------
// One budget spans the whole compaction phase
// ---------------------------------------------------------------------------

// Revert verified against: newRetentionPacer("compaction", ...) moved from
// compactFinalizedCheckpoints into compactClawCheckpoints.
//
// A pacer built per claw restarts the deadline with every claw, so a phase
// over many small claws -- none of which alone outlives the budget -- runs
// unbounded. That is invisible to a spent-budget fixture (the phase loop
// breaks on the first claw either way) and to the real clock (a test finishes
// in milliseconds), so the clock is driven: it advances one minute per read
// and the budget is ninety seconds. Three claws with one hold each: a shared
// budget stops after the second hold, a per-claw budget never stops.
func TestCompactionBudgetSpansClaws(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	countRetentionPauses(t)
	previousBudget, previousClock := retentionRowBudget, retentionClock
	retentionRowBudget = 90 * time.Second
	clock := reference
	retentionClock = func() time.Time {
		clock = clock.Add(time.Minute)
		return clock
	}
	t.Cleanup(func() { retentionRowBudget, retentionClock = previousBudget, previousClock })

	tree := strings.Repeat("ab", 32)
	for _, claw := range []string{"claw-a", "claw-b", "claw-c"} {
		insertRetentionClaw(t, s, claw, reference.Add(-400*24*time.Hour))
		for i := 0; i < 2; i++ {
			insertRetentionCheckpoint(t, s, retentionCheckpoint{
				id: fmt.Sprintf("%s-cp-%d", claw, i), clawID: claw, status: "ready",
				createdAt: reference.Add(-30 * 24 * time.Hour), rootTree: tree, writeManifest: true})
		}
	}
	out := captureRetentionLog(t, func() {
		if _, err := s.compactFinalizedCheckpoints(reference.Add(-defaultRetentionCompactAfter), false); err != nil {
			t.Fatal(err)
		}
	})
	if got := countRetentionRows(t, s, `SELECT COUNT(*) FROM claw_checkpoints WHERE status='compacted'`); got != 2 {
		t.Fatalf("%d checkpoints compacted across three claws under a 90s budget with the clock advancing a minute per read, want 2: the budget restarted with a claw\n%s", got, out)
	}
	if !strings.Contains(out, "[retention] compaction: stopping after 2 item(s)") {
		t.Fatalf("no budget line:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// The claim touches the blob under the interlock
// ---------------------------------------------------------------------------

// Revert verified against: claimBlobPresent releasing blobClaimMu before
// calling touchCheckpointBlob.
//
// For the message blob the fresh mtime is the only protection between the
// claim and the finalize commit, and a sweep arming its window between the
// Unlock and the Chtimes records no claim and can lstat the old mtime. The
// hook runs inside the touch; if it can take the lock, the touch is outside
// the hold.
func TestClaimTouchesTheBlobUnderTheInterlock(t *testing.T) {
	s := newRetentionTestServer(t)
	sha := writeRetentionBlob(t, []byte("a reused blob"))
	ageBlob(t, sha)
	var fired, outsideLock bool
	blobTouchHook = func(string) {
		fired = true
		if s.blobClaimMu.TryLock() {
			outsideLock = true
			s.blobClaimMu.Unlock()
		}
	}
	t.Cleanup(func() { blobTouchHook = nil })
	if !s.claimBlobPresent(sha) {
		t.Fatal("the blob is on disk")
	}
	if !fired {
		t.Fatal("the touch never ran")
	}
	if outsideLock {
		t.Fatal("the touch ran outside blobClaimMu; a sweep can arm its window between the claim and the touch")
	}
}

// Revert verified against: touchCheckpointBlob discarding the Chtimes error.
//
// A store the hub cannot Chtimes has no grace window for reused blobs. That
// must be visible -- once, because the plan handler touches thousands of blobs
// per checkpoint. The failure is produced by removing the file between the
// stat and the touch.
func TestBlobTouchFailureIsReportedOnce(t *testing.T) {
	s := newRetentionTestServer(t)
	blobTouchHook = func(path string) { _ = os.Remove(path) }
	t.Cleanup(func() { blobTouchHook = nil })
	var out string
	for i := 0; i < 3; i++ {
		sha := writeRetentionBlob(t, []byte(fmt.Sprintf("blob %d", i)))
		out += captureRetentionLog(t, func() { s.claimBlobPresent(sha) })
	}
	if n := strings.Count(out, "cannot refresh the mtime of reused blob"); n != 1 {
		t.Fatalf("%d touch-failure lines for 3 failures, want exactly 1:\n%s", n, out)
	}
}

// ---------------------------------------------------------------------------
// The policy is one snapshot of the configuration
// ---------------------------------------------------------------------------

// Revert verified against: retentionPolicy reading `enabled` and the knobs
// under two separate lock holds.
//
// A settings apply swaps s.hubCfg wholesale. Two reads can pair `enabled`
// from one configuration with the knobs of another and produce a policy no
// configuration ever described. The swapper here does what the apply path
// does, and every policy read must be one of the two configurations.
func TestRetentionPolicyIsOneSnapshotOfTheConfiguration(t *testing.T) {
	s := newRetentionTestServer(t)
	enabled := true
	dryRun := &types.HubConfig{Retention: &types.RetentionConfig{Enabled: &enabled, DryRun: true, Interval: "20m", MaxAge: "240h", CompactAfter: "48h"}}
	unset := &types.HubConfig{}
	s.hubCfg = dryRun
	want := []retentionPolicy{
		{enabled: true, retentionSettings: retentionSettingsFrom(dryRun.Retention)},
		{enabled: false, retentionSettings: retentionSettingsFrom(nil)},
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			s.mu.Lock()
			if i%2 == 0 {
				s.hubCfg = unset
			} else {
				s.hubCfg = dryRun
			}
			s.mu.Unlock()
		}
	}()
	defer func() { close(stop); wg.Wait() }()

	for i := 0; i < 20000; i++ {
		got := s.retentionPolicy()
		if !got.same(want[0]) && !got.same(want[1]) {
			t.Fatalf("read %d: policy [%s] matches neither configuration: `enabled` and the knobs came from different reads", i, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Expiry and compaction spare the checkpoint a restore is reading
// ---------------------------------------------------------------------------

// Revert verified against: pruneExpiredCheckpoints selecting by created_at
// alone, and finalizedClawPredicateSQL without the restore_checkpoint_id arm.
//
// restoreCheckpointFilesTo reads the source's blobs by path for minutes with
// nothing pinning the row. Expiry deletes rows past max_age with their edges,
// and the same cycle's sweep then unlinks the files under the restore. The UI
// offers a restore from any 'ready' row, however old.
func TestExpirySparesTheCheckpointARestoreIsReading(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s.nowFunc = func() time.Time { return reference }
	s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{Enabled: boolPtr(true)}}
	insertRetentionClaw(t, s, "claw", reference)
	rootSHA, fileSHAs, _ := planTreeFixture(t, 2)
	manifest := insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "ready", createdAt: reference.Add(-400 * 24 * time.Hour),
		rootTree: rootSHA, writeManifest: true})
	// The restore is in flight: restoreClawFromCheckpoint has set the id and
	// markRestoreApplied has not cleared it yet.
	if _, err := s.db.Exec(`UPDATE claws SET status='provisioning', restore_checkpoint_id='cp' WHERE id='claw'`); err != nil {
		t.Fatal(err)
	}

	out := captureRetentionLog(t, s.retentionSweepOnce)
	if n := countRetentionRows(t, s, `SELECT COUNT(*) FROM claw_checkpoints WHERE id='cp' AND status='ready'`); n != 1 {
		t.Fatalf("expiry deleted the checkpoint a restore is reading:\n%s", out)
	}
	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("the manifest of the checkpoint being restored is gone: %v", err)
	}
	for _, sha := range append(fileSHAs, rootSHA) {
		if _, err := os.Stat(checkpointBlobPath(sha)); err != nil {
			t.Fatalf("blob %s of the checkpoint being restored was swept: %v", shortID(sha), err)
		}
	}

	// Once the restore has applied, the row expires like any other.
	s.markRestoreApplied("claw", "cp")
	captureRetentionLog(t, s.retentionSweepOnce)
	if n := countRetentionRows(t, s, `SELECT COUNT(*) FROM claw_checkpoints WHERE id='cp'`); n != 0 {
		t.Fatal("the row did not expire once the restore had applied")
	}
}

func TestCompactionSparesTheClawARestoreIsReading(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s.nowFunc = func() time.Time { return reference }
	s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{Enabled: boolPtr(true)}}
	// Stale enough for the staleness arm to finalize it: a claw that went
	// quiet long ago and whose old checkpoint an operator now restores.
	insertRetentionClaw(t, s, "claw", reference.Add(-400*24*time.Hour))
	oldRoot, oldFiles, _ := planTreeFixtureSeeded(t, 2, "old ")
	newRoot, _, _ := planTreeFixtureSeeded(t, 1, "new ")
	oldManifest := insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "old", clawID: "claw", status: "ready", createdAt: reference.Add(-60 * 24 * time.Hour),
		rootTree: oldRoot, writeManifest: true})
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "newest", clawID: "claw", status: "ready", createdAt: reference.Add(-30 * 24 * time.Hour),
		rootTree: newRoot, writeManifest: true})
	if _, err := s.db.Exec(`UPDATE claws SET status='provisioning', restore_checkpoint_id='old' WHERE id='claw'`); err != nil {
		t.Fatal(err)
	}

	out := captureRetentionLog(t, s.retentionSweepOnce)
	if status, _, _ := retentionCheckpointRow(t, s, "old"); status != "ready" {
		t.Fatalf("the checkpoint being restored was compacted to %q:\n%s", status, out)
	}
	if _, err := os.Stat(oldManifest); err != nil {
		t.Fatalf("its manifest is gone: %v", err)
	}
	for _, sha := range oldFiles {
		if _, err := os.Stat(checkpointBlobPath(sha)); err != nil {
			t.Fatalf("its file %s was swept: %v", shortID(sha), err)
		}
	}

	s.markRestoreApplied("claw", "old")
	if _, err := s.db.Exec(`UPDATE claws SET status='completed' WHERE id='claw'`); err != nil {
		t.Fatal(err)
	}
	captureRetentionLog(t, s.retentionSweepOnce)
	if status, _, _ := retentionCheckpointRow(t, s, "old"); status != "compacted" {
		t.Fatalf("the superseded checkpoint was not compacted once the restore had applied: %q", status)
	}
}

// ---------------------------------------------------------------------------
// The tree gc re-checks the edge under the write lock
// ---------------------------------------------------------------------------

// Revert verified against: the DELETE's `NOT EXISTS (...)` replaced by `1=1`.
//
// The gc lists unreferenced trees outside any transaction. A plan committing
// between that list and the DELETE probes "any row means all rows"
// (addTreeBlobRefsTx), finds the expansion, and writes nothing; a DELETE that
// trusted the list would then leave a referenced tree with no expansion, and
// the next sweep would unlink that workspace. The hook plans the checkpoint at
// exactly that point.
func TestTreeGCReChecksTheEdgeUnderTheWriteLock(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Now()
	insertRetentionClaw(t, s, "claw", reference)
	rootSHA, fileSHAs, plan := planTreeFixture(t, 3)
	// The expansion exists (a previous holder wrote it) and no edge names it:
	// the gc's candidate.
	if _, err := s.insertTreeBlobRefs(rootSHA, plan); err != nil {
		t.Fatal(err)
	}
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "creating", createdAt: reference, noBlobRefs: true})

	var planned bool
	treeGCHook = func(tree string) {
		if tree != rootSHA || planned {
			return
		}
		planned = true
		if err := s.recordCheckpointBlobRefs("cp", rootSHA, plan); err != nil {
			t.Errorf("plan during the gc: %v", err)
		}
	}
	t.Cleanup(func() { treeGCHook = nil })

	if _, err := s.pruneUnreferencedTreeBlobRefs(newRetentionPacer("tree reference gc", time.Time{})); err != nil {
		t.Fatal(err)
	}
	if !planned {
		t.Fatal("the hook never fired; the fixture did not reach the gc's DELETE")
	}
	if got := treeBlobRefCount(t, s, rootSHA); got != len(fileSHAs) {
		t.Fatalf("the expansion has %d rows after a plan committed mid-gc, want %d: a referenced tree was left with no expansion", got, len(fileSHAs))
	}
	referenced, err := s.referencedBlobDigests(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, sha := range fileSHAs {
		if _, ok := referenced[sha]; !ok {
			t.Errorf("file %s of the planned tree is not in the keep set", shortID(sha))
		}
	}
}

// ---------------------------------------------------------------------------
// The backfill runs during a dry run
// ---------------------------------------------------------------------------

// Revert verified against: retentionSweepCycle skipping the backfill when
// cfg.dryRun is set.
//
// Without the backfill a dry run on an un-backfilled hub declines the sweep
// forever and tells the operator nothing about the blast radius they are
// trying to measure.
func TestDryRunBackfillsBeforeItReports(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s.nowFunc = func() time.Time { return reference }
	s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{Enabled: boolPtr(true), DryRun: true}}
	insertRetentionClaw(t, s, "claw", reference)
	rootSHA, fileSHAs, _ := planTreeFixture(t, 2)
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "ready", createdAt: reference,
		rootTree: rootSHA, writeManifest: true, noBlobRefs: true})
	orphan := writeRetentionBlob(t, []byte("nothing references this"))
	ageBlob(t, orphan)

	out := captureRetentionLog(t, s.retentionSweepOnce)
	if got := checkpointBlobRefCount(t, s, "cp"); got == 0 {
		t.Fatalf("the dry run left the row without edges:\n%s", out)
	}
	if strings.Contains(out, "DECLINED") {
		t.Fatalf("the dry run declined the sweep it exists to measure:\n%s", out)
	}
	if !strings.Contains(out, "would sweep 1 blob(s)") {
		t.Fatalf("the dry run did not report the one unreferenced blob:\n%s", out)
	}
	for _, sha := range append(fileSHAs, rootSHA, orphan) {
		if _, err := os.Stat(checkpointBlobPath(sha)); err != nil {
			t.Fatalf("a dry run removed %s: %v", shortID(sha), err)
		}
	}
}

// ---------------------------------------------------------------------------
// Finalize drops the plan's edges it no longer needs
// ---------------------------------------------------------------------------

// Revert verified against: finalizeCheckpoint and markCheckpointSkipped
// without the pruneCheckpointBlobRefsTx call.
//
// A 'complete' naming a root the plan did not left the plan's root edge in
// place, so the published row pinned two workspaces until it was compacted or
// expired. A leak, not a loss -- closed in the same transaction.
func TestFinalizeDropsThePlannedRootTheCompleteReplaced(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Now()
	insertRetentionClaw(t, s, "claw", reference)
	plannedRoot, plannedFiles, plan := planTreeFixtureSeeded(t, 2, "planned ")
	completedRoot, completedFiles, _ := planTreeFixtureSeeded(t, 3, "completed ")
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "creating", createdAt: reference, noBlobRefs: true})
	if err := s.recordCheckpointBlobRefs("cp", plannedRoot, plan); err != nil {
		t.Fatal(err)
	}
	if err := s.finalizeCheckpoint("cp", "tenant", "claw", completedRoot); err != nil {
		t.Fatal(err)
	}
	if got := checkpointBlobRefCount(t, s, "cp"); got != 3 {
		t.Fatalf("published checkpoint holds %d edges, want 3 (the completed root, the message blob, the manifest)", got)
	}
	referenced, err := s.referencedBlobDigests(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := referenced[plannedRoot]; ok {
		t.Fatal("the planned root is still referenced by a checkpoint that holds another")
	}
	for _, sha := range plannedFiles {
		if _, ok := referenced[sha]; ok {
			t.Errorf("file %s of the planned tree is still in the keep set", shortID(sha))
		}
	}
	for _, sha := range completedFiles {
		if _, ok := referenced[sha]; !ok {
			t.Errorf("file %s of the completed tree is not in the keep set", shortID(sha))
		}
	}
}

func TestSkipDropsThePlannedRootTheCompleteReplaced(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Now()
	insertRetentionClaw(t, s, "claw", reference)
	plannedRoot, _, plan := planTreeFixture(t, 2)
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "creating", createdAt: reference, noBlobRefs: true})
	if err := s.recordCheckpointBlobRefs("cp", plannedRoot, plan); err != nil {
		t.Fatal(err)
	}
	if err := s.markCheckpointSkipped("cp", testTreeSHA); err != nil {
		t.Fatal(err)
	}
	var edges []string
	rows, err := s.db.Query(`SELECT sha256 FROM checkpoint_blob_refs WHERE checkpoint_id='cp'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			t.Fatal(err)
		}
		edges = append(edges, sha)
	}
	rows.Close()
	if len(edges) != 1 || edges[0] != testTreeSHA {
		t.Fatalf("skipped row holds %v, want only the tree it duplicated", edges)
	}
}

// The prune must not run when the published root is not a usable digest: a
// rootless plan records one edge per file under the checkpoint, and those are
// the only reference to the files the checkpoint holds.
func TestFinalizeKeepsPerFileEdgesWhenThereIsNoRoot(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Now()
	insertRetentionClaw(t, s, "claw", reference)
	var files []types.CheckpointFile
	var fileSHAs []string
	for i := 0; i < 2; i++ {
		sha := writeRetentionBlob(t, []byte(fmt.Sprintf("rootless file %d", i)))
		fileSHAs = append(fileSHAs, sha)
		files = append(files, types.CheckpointFile{Path: fmt.Sprintf("workspace/%d", i), SHA256: sha, Size: 15})
	}
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "creating", createdAt: reference, noBlobRefs: true})
	if err := s.recordCheckpointBlobRefs("cp", "", files); err != nil {
		t.Fatal(err)
	}
	if err := s.finalizeCheckpoint("cp", "tenant", "claw", ""); err != nil {
		t.Fatal(err)
	}
	referenced, err := s.referencedBlobDigests(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, sha := range fileSHAs {
		if _, ok := referenced[sha]; !ok {
			t.Errorf("file %s of a rootless checkpoint lost its only edge", shortID(sha))
		}
	}
}
