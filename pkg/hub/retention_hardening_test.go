package hub

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// captureRetentionLog runs fn with the standard logger redirected, and returns
// what it wrote.
func captureRetentionLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	previousOut, previousFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(previousOut)
		log.SetFlags(previousFlags)
	}()
	fn()
	return buf.String()
}

func containsString(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

// ---------------------------------------------------------------------------
// The claim/unlink interlock (finding 1)
// ---------------------------------------------------------------------------

// ageBlob pushes a blob's mtime well outside the sweep's grace window, which is
// the state every deduplicated blob is in: it was written by whichever claw
// captured those bytes first, possibly months ago.
func ageBlob(t *testing.T, sha string) {
	t.Helper()
	aged := time.Now().Add(-2 * blobSweepGrace)
	if err := os.Chtimes(checkpointBlobPath(sha), aged, aged); err != nil {
		t.Fatalf("age blob %s: %v", sha, err)
	}
}

// writeBlobSortingBefore writes a blob whose digest sorts before target, so
// filepath.Walk is guaranteed to visit it first. The blob layout is
// blobs/sha256/<d[0:2]>/<d[2:4]>/<d>, so lexical path order is lexical digest
// order.
func writeBlobSortingBefore(t *testing.T, target string) string {
	t.Helper()
	for i := 0; i < 5000; i++ {
		sha := writeRetentionBlob(t, []byte(fmt.Sprintf("walk-order decoy %d", i)))
		if sha < target {
			return sha
		}
		if err := os.Remove(checkpointBlobPath(sha)); err != nil {
			t.Fatalf("discard decoy: %v", err)
		}
	}
	t.Fatal("could not generate a blob sorting before the target")
	return ""
}

// The durable claim closes "a 'creating' row carries no digests". It does NOT
// close "a claim committed AFTER the keep set was snapshotted": the keep set is
// read once and filepath.Walk then runs for minutes. A plan arriving inside that
// window commits its claim, sees the blob via os.Stat, and answers "already
// have it, skip the upload" -- while the walker, holding the stale snapshot,
// unlinks it. The checkpoint is then published 'ready' referencing a file that
// is gone.
//
// Every existing test seeded the claim BEFORE the sweep, so none of them could
// reach this interleaving. This one drives the real plan path from inside the
// walk.
func TestBlobSweepRespectsAClaimThatArrivesDuringTheWalk(t *testing.T) {
	cases := []struct {
		name string
		// defeatGraceWindow re-ages the blob immediately after the plan touched
		// it, so the mtime grace cannot be what spares it and the interlock is
		// the only remaining explanation.
		defeatGraceWindow bool
		explanation       string
	}{
		{
			name:        "the plan answers 'already have it' and the blob survives",
			explanation: "claim recorded before the stat, so the walker must see it",
		},
		{
			name:              "the interlock alone spares it when the grace window cannot",
			defeatGraceWindow: true,
			explanation:       "a deduplicated blob's mtime is arbitrarily old; only the claim protects it",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRetentionTestServer(t)
			reference := time.Now()
			insertRetentionClaw(t, s, "claw", reference)

			target := writeRetentionBlob(t, []byte("a blob an earlier claw already uploaded"))
			ageBlob(t, target)
			decoy := writeBlobSortingBefore(t, target)
			ageBlob(t, decoy)

			// The checkpoint exists and is 'creating', but its plan has not
			// arrived yet -- so nothing claims the target when the keep set is
			// built, exactly as in the real race.
			insertRetentionCheckpoint(t, s, retentionCheckpoint{
				id: "cp", clawID: "claw", status: "creating", createdAt: reference})

			var planReportedMissing bool
			var planRan bool
			blobSweepFileHook = func(name string) {
				if name != decoy || planRan {
					return
				}
				planRan = true
				// The plan handler's exact sequence: claim durably, then answer.
				if err := s.recordCheckpointBlobRefs("cp", testTreeSHA, []types.CheckpointFile{
					{Path: "workspace/a.txt", SHA256: target, Size: 39},
				}); err != nil {
					t.Errorf("recordCheckpointBlobRefs: %v", err)
					return
				}
				planReportedMissing = s.planBlobMissing(target)
				if tc.defeatGraceWindow {
					ageBlob(t, target)
				}
			}
			t.Cleanup(func() { blobSweepFileHook = nil })

			removed, _, itemErrors, err := s.sweepCheckpointBlobs(false, nil)
			if err != nil {
				t.Fatalf("sweepCheckpointBlobs: %v", err)
			}
			if itemErrors != 0 {
				t.Fatalf("item errors = %d, want 0", itemErrors)
			}
			if !planRan {
				t.Fatal("the hook never fired; the fixture did not reach the walk")
			}
			if planReportedMissing {
				t.Fatal("the plan asked for an upload of a blob that was on disk")
			}
			if _, err := os.Stat(checkpointBlobPath(target)); err != nil {
				t.Fatalf("a blob claimed during the walk was swept (%s): %v", tc.explanation, err)
			}
			if removed != 1 {
				t.Fatalf("removed %d blobs, want 1 (the decoy only)", removed)
			}
		})
	}
}

// The other half of the interlock: when the walker gets there first, the plan
// must find the blob gone and ask for it again. Losing the upload is the only
// acceptable outcome of the race; losing the blob is not.
func TestPlanAsksForAnUploadWhenTheSweepAlreadyRemovedTheBlob(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Now()
	insertRetentionClaw(t, s, "claw", reference)
	orphan := writeRetentionBlob(t, []byte("nothing references this"))
	ageBlob(t, orphan)

	if removed, _, _, err := s.sweepCheckpointBlobs(false, nil); err != nil || removed != 1 {
		t.Fatalf("sweep removed %d (err %v), want 1", removed, err)
	}
	if !s.planBlobMissing(orphan) {
		t.Fatal("the plan claimed the hub still had a blob the sweep removed")
	}
}

// The mtime grace is documented as a second line of defence. It was inert on
// the one path where deduplication actually happens -- the plan answering "do
// not upload" -- because that path only stat'ed the file. A previous fix added
// the touch to the upload handler and the message-blob writer instead, which
// are not that path.
func TestPlanRefreshesTheMtimeOfABlobItDeduplicates(t *testing.T) {
	s := newRetentionTestServer(t)
	sha := writeRetentionBlob(t, []byte("shared across claws for months"))
	ageBlob(t, sha)

	before, err := os.Stat(checkpointBlobPath(sha))
	if err != nil {
		t.Fatal(err)
	}
	if s.planBlobMissing(sha) {
		t.Fatal("planBlobMissing reported a blob that is on disk as missing")
	}
	after, err := os.Stat(checkpointBlobPath(sha))
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().After(before.ModTime()) {
		t.Fatalf("mtime %s was not refreshed (was %s); the grace window stays inert on the dedup path",
			after.ModTime(), before.ModTime())
	}
}

// ---------------------------------------------------------------------------
// Terminal transitions (findings 2 and 3)
// ---------------------------------------------------------------------------

// newFileBackedRetentionServer gives the test a database it can close and
// reopen, which is the only way to observe what a FAILED write left behind.
func newFileBackedRetentionServer(t *testing.T) (*Server, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, "hub.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO tenants(id,name,token,claw_token,created_at) VALUES(?,?,?,?,?)`,
		"tenant", "tenant", "token", "claw-token", now()); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return &Server{db: db, hubCfg: &types.HubConfig{}, claws: map[string]*clawConn{}}, path
}

// failCheckpoint used to DELETE the references unconditionally, whatever the
// UPDATE did. Under ENOSPC or SQLITE_BUSY that leaves the row 'creating' -- still
// expecting its blobs -- with nothing keeping them out of the next sweep: the
// worst of both states. They now move in one transaction.
//
// The fault is injected on ONE statement at a time, on a live handle. A closed
// handle fails every statement, which any implementation survives -- including
// two bare Execs in either order. What has to hold is that the two writes are
// one unit: when the UPDATE fails the DELETE must not have happened, and when
// the DELETE fails the UPDATE must not have stuck.
func TestFailCheckpointKeepsItsClaimsWhenTheUpdateFails(t *testing.T) {
	cases := []struct {
		name    string
		trigger string
		why     string
	}{
		{
			name:    "the UPDATE fails",
			trigger: `CREATE TRIGGER fail_update BEFORE UPDATE ON claw_checkpoints BEGIN SELECT RAISE(ABORT, 'simulated write failure'); END`,
			why:     "the row stays 'creating' and must keep the claims that protect its upload",
		},
		{
			name:    "the DELETE fails",
			trigger: `CREATE TRIGGER fail_delete BEFORE DELETE ON checkpoint_blob_refs BEGIN SELECT RAISE(ABORT, 'simulated write failure'); END`,
			why:     "a row recorded as 'failed' while still holding its claims would pin the blobs forever",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRetentionTestServer(t)
			reference := time.Now()
			insertRetentionClaw(t, s, "claw", reference)
			insertRetentionCheckpoint(t, s, retentionCheckpoint{
				id: "cp", clawID: "claw", status: "creating", createdAt: reference})
			sha := writeRetentionBlob(t, []byte("planned but never uploaded"))
			if err := s.recordCheckpointBlobRefs("cp", testTreeSHA, []types.CheckpointFile{
				{Path: "workspace/a.txt", SHA256: sha, Size: 25},
			}); err != nil {
				t.Fatalf("recordCheckpointBlobRefs: %v", err)
			}
			if _, err := s.db.Exec(tc.trigger); err != nil {
				t.Fatal(err)
			}

			if err := s.failCheckpoint("cp", "disk full"); err == nil {
				t.Fatalf("failCheckpoint reported success while %s", tc.name)
			}
			if got := checkpointBlobRefCount(t, s, "cp"); got != 1 {
				t.Fatalf("claims after %s = %d, want 1 (%s)", tc.name, got, tc.why)
			}
			var status string
			if err := s.db.QueryRow(`SELECT status FROM claw_checkpoints WHERE id='cp'`).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status != "creating" {
				t.Fatalf("status = %q after %s, want creating (%s)", status, tc.name, tc.why)
			}
		})
	}
}

// A delayed plan or a late 'complete' must not mutate a row that already went
// terminal: a released row's edge set is settled, so an edge added under it
// would pin blobs nothing will ever release, and a resurrected 'ready' row would
// re-declare blobs its own release had already made collectable.
func TestCheckpointTransitionsRequireTheRowToStillBeCreating(t *testing.T) {
	cases := []struct {
		name        string
		status      string
		apply       func(s *Server) error
		wantErr     error
		wantStatus  string
		explanation string
	}{
		{
			name: "plan retried after the checkpoint failed", status: "failed",
			apply: func(s *Server) error {
				return s.recordCheckpointBlobRefs("cp", testTreeSHA, []types.CheckpointFile{
					{Path: "workspace/a.txt", SHA256: validTestSHA, Size: 1}})
			},
			wantErr: errCheckpointNotCreating, wantStatus: "failed",
			explanation: "a claim under a terminal row is invisible to the keep set",
		},
		{
			name: "plan for a checkpoint that no longer exists", status: "",
			apply: func(s *Server) error {
				return s.recordCheckpointBlobRefs("cp", testTreeSHA, []types.CheckpointFile{
					{Path: "workspace/a.txt", SHA256: validTestSHA, Size: 1}})
			},
			wantErr: errCheckpointNotCreating, wantStatus: "",
			explanation: "expiry can delete the row out from under an in-flight plan",
		},
		{
			name: "complete arriving after the row went terminal", status: "failed",
			apply: func(s *Server) error {
				return s.finalizeCheckpoint("cp", "tenant", "claw", "")
			},
			wantErr: errCheckpointNotCreating, wantStatus: "failed",
			explanation: "a late complete must not flip a failed row back to ready",
		},
		{
			name: "skip arriving after the row went terminal", status: "ready",
			apply: func(s *Server) error {
				return s.markCheckpointSkipped("cp", "")
			},
			wantErr: errCheckpointNotCreating, wantStatus: "ready",
			explanation: "the same argument as complete, on the duplicate-idle path",
		},
		{
			name: "fail arriving after the row went ready", status: "ready",
			apply: func(s *Server) error {
				return s.failCheckpoint("cp", "late error")
			},
			wantErr: nil, wantStatus: "ready",
			explanation: "ignorable at the call site, but it must not rewrite the row",
		},
		{
			name: "fail on a row still creating", status: "creating",
			apply: func(s *Server) error {
				return s.failCheckpoint("cp", "bridge went away")
			},
			wantErr: nil, wantStatus: "failed",
			explanation: "the normal path still works",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRetentionTestServer(t)
			reference := time.Now()
			insertRetentionClaw(t, s, "claw", reference)
			// A 'ready' row holds a real workspace: root tree, message blob and
			// manifest edges plus the tree's expansion, exactly as finalize left
			// them. Seeding empty digests instead would make "the edge count is
			// unchanged" and "the edges were deleted" the same number.
			rootSHA, _, _ := planTreeFixture(t, 2)
			msgSHA := writeRetentionBlob(t, []byte(`{"id":"m1"}`))
			if tc.status != "" {
				cp := retentionCheckpoint{id: "cp", clawID: "claw", status: tc.status, createdAt: reference}
				if tc.status == "ready" {
					cp.rootTree, cp.messageTree, cp.writeManifest = rootSHA, msgSHA, true
				}
				insertRetentionCheckpoint(t, s, cp)
			}
			edgesBefore := checkpointBlobRefCount(t, s, "cp")
			expansionBefore := treeBlobRefCount(t, s, rootSHA)
			if tc.status == "ready" && (edgesBefore != 3 || expansionBefore != 2) {
				t.Fatalf("fixture seeded %d edges and %d expansion rows, want 3 and 2", edgesBefore, expansionBefore)
			}

			err := tc.apply(s)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("got error %v, want none (%s)", err, tc.explanation)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("got error %v, want %v (%s)", err, tc.wantErr, tc.explanation)
			}

			var status string
			queryErr := s.db.QueryRow(`SELECT status FROM claw_checkpoints WHERE id='cp'`).Scan(&status)
			if tc.wantStatus == "" {
				if queryErr == nil {
					t.Fatalf("a checkpoint row was created for a checkpoint that did not exist")
				}
				return
			}
			if queryErr != nil {
				t.Fatal(queryErr)
			}
			if status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (%s)", status, tc.wantStatus, tc.explanation)
			}
			// A rejected transition must not have touched the references either:
			// a plan answering for a row that already went terminal would tell
			// the claw to skip uploads for a checkpoint whose edges nothing will
			// ever release, and a late fail against a 'ready' row must not
			// release what a published checkpoint holds.
			wantEdges := edgesBefore
			if tc.wantStatus == "failed" {
				wantEdges = 0
			}
			if got := checkpointBlobRefCount(t, s, "cp"); got != wantEdges {
				t.Fatalf("row holds %d references, want %d (%s)", got, wantEdges, tc.explanation)
			}
			if got := treeBlobRefCount(t, s, rootSHA); got != expansionBefore {
				t.Fatalf("tree expansion has %d rows, want %d (%s)", got, expansionBefore, tc.explanation)
			}
		})
	}
}

const validTestSHA = "1111111111111111111111111111111111111111111111111111111111111111"

// testTreeSHA stands in for the root tree digest a bridge sends with its plan.
// The tree blob itself is not on disk at plan time -- it is uploaded with the
// rest -- so a plan only ever needs the digest, and a fixture only needs one
// that is well-formed.
const testTreeSHA = "2222222222222222222222222222222222222222222222222222222222222222"

// ---------------------------------------------------------------------------
// Retention indexes off the boot path (findings 4 and 12)
// ---------------------------------------------------------------------------

func retentionIndexExists(t *testing.T, s *Server, name string) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&n); err != nil {
		t.Fatalf("probe index %s: %v", name, err)
	}
	return n == 1
}

// The two retention indexes must exist after a boot, be rebuildable on a later
// boot when an earlier one could not create them (the disk-full case this whole
// feature exists for), and not be required by the deletes they accelerate.
func TestRetentionIndexesAreBuiltOutsideTheBootCriticalPath(t *testing.T) {
	for _, idx := range retentionIndexes {
		t.Run(idx.name, func(t *testing.T) {
			s := newRetentionTestServer(t)
			if !retentionIndexExists(t, s, idx.name) {
				t.Fatalf("%s missing after migrate", idx.name)
			}
			// Stand in for the boot where SQLITE_FULL killed the CREATE: the
			// index is absent and everything else is intact.
			if _, err := s.db.Exec(`DROP INDEX ` + idx.name); err != nil {
				t.Fatal(err)
			}
			if retentionIndexExists(t, s, idx.name) {
				t.Fatal("fixture did not drop the index")
			}
			// The table the same schema Exec used to create AFTER the failing
			// statement must still be there -- that collateral damage is half
			// the finding.
			if _, err := s.db.Exec(`SELECT COUNT(*) FROM checkpoint_blob_refs`); err != nil {
				t.Fatalf("checkpoint_blob_refs is missing: %v", err)
			}
			// The delete still works without it, just more slowly.
			if _, err := s.pruneRowsBatched("messages", "created_at", time.Now(), false, newRetentionPacer("messages", time.Time{})); err != nil {
				t.Fatalf("pruneRowsBatched needs the index to work: %v", err)
			}
			ensureRetentionIndexes(s.db)
			if !retentionIndexExists(t, s, idx.name) {
				t.Fatalf("%s was not rebuilt on the retry path", idx.name)
			}
			// Idempotent: every boot and every sweep calls it.
			ensureRetentionIndexes(s.db)
			if !retentionIndexExists(t, s, idx.name) {
				t.Fatalf("%s vanished on a second ensure", idx.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Dry run parity (finding 5)
// ---------------------------------------------------------------------------

// seedDryRunParityFixture builds a claw whose old checkpoints will be compacted
// and expired, each holding blobs nothing else references.
func seedDryRunParityFixture(t *testing.T, s *Server, reference time.Time) {
	t.Helper()
	insertRetentionClaw(t, s, "claw", reference.Add(-400*24*time.Hour))

	// Two superseded ready checkpoints plus the survivor. Each names a tree
	// blob whose file list points at a per-file blob: the closure compaction
	// releases.
	for i, id := range []string{"old-1", "old-2", "newest"} {
		fileSHA := writeRetentionBlob(t, []byte(fmt.Sprintf("file contents %d", i)))
		treeSHA := writeRetentionBlob(t, []byte(fmt.Sprintf(`[{"path":"workspace/a%d.txt","sha256":%q,"size":16}]`, i, fileSHA)))
		ageBlob(t, fileSHA)
		ageBlob(t, treeSHA)
		insertRetentionCheckpoint(t, s, retentionCheckpoint{
			id: id, clawID: "claw", status: "ready", rootTree: treeSHA,
			createdAt: reference.Add(-time.Duration(30-10*i) * 24 * time.Hour),
			// Only the survivor keeps a manifest in the end, but all three
			// start with one, as they do in production.
			writeManifest: true,
		})
	}
	// An idle duplicate pinning a tree of its own. Compaction releases its
	// digests, so the dry run has to account for that release too.
	skippedFile := writeRetentionBlob(t, []byte("workspace the skipped row pins"))
	skippedTree := writeRetentionBlob(t, []byte(fmt.Sprintf(`[{"path":"workspace/s.txt","sha256":%q,"size":30}]`, skippedFile)))
	ageBlob(t, skippedFile)
	ageBlob(t, skippedTree)
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "skipped", clawID: "claw", status: "skipped", rootTree: skippedTree,
		createdAt: reference.Add(-25 * 24 * time.Hour),
	})

	// A checkpoint past the retention window: expiry deletes the row outright.
	expiredFile := writeRetentionBlob(t, []byte("expired workspace file"))
	expiredTree := writeRetentionBlob(t, []byte(fmt.Sprintf(`[{"path":"workspace/e.txt","sha256":%q,"size":22}]`, expiredFile)))
	ageBlob(t, expiredFile)
	ageBlob(t, expiredTree)
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "expired", clawID: "claw", status: "ready", rootTree: expiredTree,
		createdAt: reference.Add(-400 * 24 * time.Hour), writeManifest: true,
	})
}

// RetentionConfig.DryRun promises it "logs exactly what would be removed". For
// the blob sweep -- the phase with the largest blast radius -- it did not:
// compaction and expiry mutate nothing in a dry run, so the keep set was built
// from rows and manifests a real cycle would already have removed, and the dry
// run reported a fraction of the blobs the real run deletes.
//
// The test drives the whole cycle (retentionSweepOnce) in both modes and
// compares the "cycle done" lines, because the wiring is the thing under test:
// the cycle assembles the released ids from what compaction and expiry report
// and hands them to the sweep, and the runbook's first step -- a dry run -- is
// exactly as accurate as that hand-off. An earlier version of this test
// re-assembled the list by hand and called the sweep directly, and passed with
// the cycle passing nil.
func TestDryRunReportsTheSameBlobsTheRealCycleRemoves(t *testing.T) {
	reference := time.Now()

	run := func(t *testing.T, dryRun bool) (blobs int, bytes int64) {
		t.Helper()
		s := newRetentionTestServer(t)
		s.nowFunc = func() time.Time { return reference }
		s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{Enabled: boolPtr(true), DryRun: dryRun}}
		seedDryRunParityFixture(t, s, reference)
		out := captureRetentionLog(t, s.retentionSweepOnce)
		return parseCycleDoneBlobs(t, out)
	}

	wantRemoved, wantFreed := run(t, false)
	if wantRemoved == 0 {
		t.Fatal("the fixture released no blobs; it cannot distinguish the two runs")
	}
	gotRemoved, gotFreed := run(t, true)
	if gotRemoved != wantRemoved || gotFreed != wantFreed {
		t.Fatalf("dry run reports %d blobs / %d bytes, the real cycle removes %d / %d; dry_run must not understate the most destructive phase",
			gotRemoved, gotFreed, wantRemoved, wantFreed)
	}
}

// parseCycleDoneBlobs reads the blob count and blob bytes out of a cycle's
// "cycle done" line. A declined sweep is a fixture error, not a parity result.
func parseCycleDoneBlobs(t *testing.T, out string) (int, int64) {
	t.Helper()
	line := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "[retention] cycle done") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no cycle done line; log was:\n%s", out)
	}
	if strings.Contains(line, "DECLINED") {
		t.Fatalf("the sweep declined; the fixture is not fully referenced:\n%s", out)
	}
	var blobs int
	var blobBytes int64
	if _, err := fmt.Sscanf(line[strings.Index(line, " blobs="):], " blobs=%d", &blobs); err != nil {
		t.Fatalf("parse blobs from %q: %v", line, err)
	}
	if _, err := fmt.Sscanf(line[strings.Index(line, "(blobs="):], "(blobs=%d", &blobBytes); err != nil {
		t.Fatalf("parse blob bytes from %q: %v", line, err)
	}
	return blobs, blobBytes
}

// ---------------------------------------------------------------------------
// The survivor and the retry policy (finding 6)
// ---------------------------------------------------------------------------

// retryEligibleCheckpoint mirrors retryCheckpointIDWithCount's filter. Keeping
// a checkpoint that filter would refuse means keeping a claw that looks
// recoverable and, on the next retry, is not.
func TestRetryEligibleCheckpointMirrorsTheRetryPolicy(t *testing.T) {
	cand := func(id, reason, root, manifest string) compactionCandidate {
		return compactionCandidate{id: id, reason: reason, rootTree: root, manifestPath: manifest}
	}
	cases := []struct {
		name       string
		candidates []compactionCandidate
		progressed bool
		restored   map[string]struct{}
		want       int
		why        string
	}{
		{
			name: "newest usable checkpoint wins",
			candidates: []compactionCandidate{
				cand("a", "idle-timer", "tree-a", "/m/a.json"),
				cand("b", "idle-timer", "tree-b", "/m/b.json"),
			},
			want: 0, why: "nothing disqualifies the newest",
		},
		{
			name: "a bootstrap capture is skipped once the claw progressed",
			candidates: []compactionCandidate{
				cand("boot", "bootstrap", "tree-boot", "/m/boot.json"),
				cand("work", "idle-timer", "tree-work", "/m/work.json"),
			},
			progressed: true,
			want:       1, why: "retry walks past a capture of state zero, so compaction must not keep it instead",
		},
		{
			name: "a bootstrap capture is kept while it is all there is",
			candidates: []compactionCandidate{
				cand("boot", "bootstrap", "tree-boot", "/m/boot.json"),
			},
			want: 0, why: "no progress yet, so retry would accept it",
		},
		{
			name: "the previously restored checkpoint is skipped",
			candidates: []compactionCandidate{
				cand("used", "idle-timer", "tree-used", "/m/used.json"),
				cand("older", "idle-timer", "tree-older", "/m/older.json"),
			},
			restored: map[string]struct{}{"used": {}},
			want:     1, why: "retry refuses to restore the same checkpoint twice",
		},
		{
			name: "a metadata-only capture is skipped",
			candidates: []compactionCandidate{
				cand("meta", "termination:kill", "", "/m/meta.json"),
				cand("real", "idle-timer", "tree-real", "/m/real.json"),
			},
			want: 1, why: "an empty root tree restores nothing",
		},
		{
			name: "a compacted row with no manifest is skipped",
			candidates: []compactionCandidate{
				cand("nomanifest", "idle-timer", "tree-x", ""),
				cand("real", "idle-timer", "tree-real", "/m/real.json"),
			},
			want: 1, why: "retryCheckpointIDWithCount requires manifest_path != ''",
		},
		{
			name: "nothing is eligible",
			candidates: []compactionCandidate{
				cand("boot", "bootstrap", "tree-boot", "/m/boot.json"),
			},
			progressed: true,
			want:       -1, why: "the caller must fall back to the newest restorable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := retryEligibleCheckpoint(tc.candidates, tc.progressed, tc.restored)
			if got != tc.want {
				t.Fatalf("retryEligibleCheckpoint = %d, want %d (%s)", got, tc.want, tc.why)
			}
		})
	}
}

// End to end: compaction must not keep a bootstrap capture and delete the only
// checkpoint a retry would actually accept.
func TestCompactionKeepsTheCheckpointARetryWouldAccept(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Now()
	insertRetentionClaw(t, s, "claw", reference.Add(-400*24*time.Hour))

	workTree := writeRetentionBlob(t, []byte(`[]`))
	bootTree := writeRetentionBlob(t, []byte(`[]`))
	// The bootstrap capture sorts newest, which is the shape that made the
	// previous survivor rule pick exactly the wrong row.
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "work", clawID: "claw", status: "ready", rootTree: workTree,
		createdAt: reference.Add(-20 * 24 * time.Hour), writeManifest: true})
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "boot", clawID: "claw", status: "ready", rootTree: bootTree,
		createdAt: reference.Add(-19 * 24 * time.Hour), writeManifest: true})
	if _, err := s.db.Exec(`UPDATE claw_checkpoints SET reason='bootstrap' WHERE id='boot'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE claw_checkpoints SET reason='idle-timer' WHERE id='work'`); err != nil {
		t.Fatal(err)
	}

	if _, err := s.compactFinalizedCheckpoints(reference.Add(-10*24*time.Hour), false); err != nil {
		t.Fatalf("compactFinalizedCheckpoints: %v", err)
	}
	kept, _, _ := retentionCheckpointRow(t, s, "work")
	if kept != "ready" {
		t.Fatalf("the checkpoint retry would restore is %q; compaction kept the bootstrap capture instead", kept)
	}
	compactedStatus, _, _ := retentionCheckpointRow(t, s, "boot")
	if compactedStatus != "compacted" {
		t.Fatalf("bootstrap capture status = %q, want compacted", compactedStatus)
	}
}

// ---------------------------------------------------------------------------
// Item-level failures in the summary (finding 7)
// ---------------------------------------------------------------------------

// A cycle that failed to unlink every blob it selected reported errors=0
// blobs=0 -- byte-identical to a cycle where nothing was eligible, and the two
// call for opposite responses.
func TestSweepCountsBlobsItCouldNotRemove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unwritable directory does not stop os.Remove")
	}
	s := newRetentionTestServer(t)
	insertRetentionClaw(t, s, "claw", time.Now())
	orphan := writeRetentionBlob(t, []byte("an orphan in a directory we cannot write"))
	ageBlob(t, orphan)

	dir := filepath.Dir(checkpointBlobPath(orphan))
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })

	removed, _, itemErrors, err := s.sweepCheckpointBlobs(false, nil)
	if err != nil {
		t.Fatalf("sweepCheckpointBlobs: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	if itemErrors != 1 {
		t.Fatalf("item errors = %d, want 1; a failed unlink must not read as 'nothing was eligible'", itemErrors)
	}
}

// The same for the diagnostics phase, which also logs and continues.
func TestDiagnosticsPruneCountsLogsItCouldNotRemove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unwritable directory does not stop os.Remove")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(hubDataDir(), "diagnostics")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "claw.log")
	if err := os.WriteFile(path, []byte("captured gateway log"), 0o640); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-200 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })

	result, err := pruneDiagnosticsLogs(hubDataDir(), time.Now(), false)
	if err != nil {
		t.Fatalf("pruneDiagnosticsLogs: %v", err)
	}
	if result.removed != 0 || result.itemErrors != 1 {
		t.Fatalf("removed=%d itemErrors=%d, want 0 and 1", result.removed, result.itemErrors)
	}
}

// ---------------------------------------------------------------------------
// 'skipped' rows pinning released trees (finding 8)
// ---------------------------------------------------------------------------

// markCheckpointSkipped records a skipped row holding the tree of the ready
// checkpoint it duplicated. Compaction only touches 'ready' rows, so without a
// release of its own a sibling 'skipped' row went on naming the same tree.
//
// The keep set reads ONLY the edge tables, so the assertion that matters is on
// the edges and on the blobs: a release that cleared the digest columns and
// left the edge behind would pass a column check and still pin the workspace.
func TestCompactionReleasesTheTreesSkippedRowsWerePinning(t *testing.T) {
	cases := []struct {
		name          string
		readyCount    int
		wantBlobsGone bool
		why           string
	}{
		{
			name: "a claw with superseded checkpoints", readyCount: 2, wantBlobsGone: false,
			why: "the normal compaction path; the survivor still names the tree",
		},
		{
			name: "a claw already down to one ready checkpoint", readyCount: 1, wantBlobsGone: false,
			why: "an earlier cycle compacted the row the skipped one duplicated; the early return must not skip the release, and the survivor still names the tree",
		},
		{
			name: "a claw whose only holder is the skipped row", readyCount: 0, wantBlobsGone: true,
			why: "the skipped row was the last thing naming the tree; releasing it is what lets the sweep reclaim the workspace",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRetentionTestServer(t)
			reference := time.Now()
			insertRetentionClaw(t, s, "claw", reference.Add(-400*24*time.Hour))

			fileSHA := writeRetentionBlob(t, []byte("the workspace the skipped row pins"))
			treeSHA := writeRetentionBlob(t, []byte(fmt.Sprintf(`[{"path":"workspace/a.txt","sha256":%q,"size":34}]`, fileSHA)))
			ageBlob(t, fileSHA)
			ageBlob(t, treeSHA)

			for i := 0; i < tc.readyCount; i++ {
				insertRetentionCheckpoint(t, s, retentionCheckpoint{
					id: fmt.Sprintf("ready-%d", i), clawID: "claw", status: "ready", rootTree: treeSHA,
					createdAt: reference.Add(-time.Duration(30-i) * 24 * time.Hour), writeManifest: true})
			}
			// The idle duplicate: never restorable, but it names the same tree.
			// Older than compact_after, like every other checkpoint of a claw
			// the staleness arm finalizes -- a checkpoint inside the window is
			// itself proof the claw is still changing.
			insertRetentionCheckpoint(t, s, retentionCheckpoint{
				id: "skipped", clawID: "claw", status: "skipped", rootTree: treeSHA,
				createdAt: reference.Add(-15 * 24 * time.Hour)})
			if got := checkpointBlobRefCount(t, s, "skipped"); got == 0 {
				t.Fatal("the fixture seeded no edge for the skipped row; the test would prove nothing")
			}

			if _, err := s.compactFinalizedCheckpoints(reference.Add(-10*24*time.Hour), false); err != nil {
				t.Fatalf("compactFinalizedCheckpoints: %v", err)
			}
			if got := checkpointBlobRefCount(t, s, "skipped"); got != 0 {
				t.Fatalf("skipped row still holds %d edge(s) after compaction, want 0 (%s)", got, tc.why)
			}
			var root, workspace string
			if err := s.db.QueryRow(
				`SELECT root_tree_sha256, workspace_tree_sha256 FROM claw_checkpoints WHERE id='skipped'`).
				Scan(&root, &workspace); err != nil {
				t.Fatal(err)
			}
			if root != "" || workspace != "" {
				t.Fatalf("skipped row still names %q/%q; compaction must clear the columns with the edges", root, workspace)
			}
			// The row itself survives: it is the record that the claw was idle.
			var n int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM claw_checkpoints WHERE id='skipped'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Fatal("the skipped row was deleted; only its digests may go")
			}

			// The proof: the sweep, which reads only the edge tables.
			if _, _, _, err := s.sweepCheckpointBlobs(false, nil); err != nil {
				t.Fatalf("sweepCheckpointBlobs: %v", err)
			}
			for _, sha := range []string{fileSHA, treeSHA} {
				_, err := os.Stat(checkpointBlobPath(sha))
				if gone := os.IsNotExist(err); gone != tc.wantBlobsGone {
					t.Fatalf("blob %s gone after the sweep = %v, want %v (%s)", shortID(sha), gone, tc.wantBlobsGone, tc.why)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Timing out a 'creating' checkpoint (finding 9)
// ---------------------------------------------------------------------------

// A claw that dies mid-upload leaves its row 'creating' forever, pinning its
// whole planned digest set. Nothing released it at runtime, and the boot path
// that did ran only when liveness was enabled.
func TestFailStuckCreatingCheckpointsReleasesTheirClaims(t *testing.T) {
	cases := []struct {
		name        string
		age         time.Duration
		wantStatus  string
		wantClaims  int
		explanation string
	}{
		{
			name: "a checkpoint still inside the bound", age: checkpointCreatingMaxAge / 2,
			wantStatus: "creating", wantClaims: 1,
			explanation: "a large upload is allowed to take a long time",
		},
		{
			name: "a checkpoint abandoned past the bound", age: checkpointCreatingMaxAge + time.Hour,
			wantStatus: "failed", wantClaims: 0,
			explanation: "the claw is gone; nothing else will ever release these blobs",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRetentionTestServer(t)
			// now(), not time.Now(): failStuckCreatingCheckpoints derives its
			// cutoff from the hub's own UTC clock, and the driver compares
			// stored timestamps as text, so a fixture written in local time
			// compares against a UTC cutoff by its digits rather than its
			// instant.
			reference := now()
			insertRetentionClaw(t, s, "claw", reference)
			insertRetentionCheckpoint(t, s, retentionCheckpoint{
				id: "cp", clawID: "claw", status: "creating", createdAt: reference.Add(-tc.age)})
			sha := writeRetentionBlob(t, []byte("planned by a claw that died"))
			if err := s.recordCheckpointBlobRefs("cp", testTreeSHA, []types.CheckpointFile{
				{Path: "workspace/a.txt", SHA256: sha, Size: 27}}); err != nil {
				t.Fatalf("recordCheckpointBlobRefs: %v", err)
			}

			s.failStuckCreatingCheckpoints()

			status, _, _ := retentionCheckpointRow(t, s, "cp")
			if status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (%s)", status, tc.wantStatus, tc.explanation)
			}
			if got := checkpointBlobRefCount(t, s, "cp"); got != tc.wantClaims {
				t.Fatalf("claims = %d, want %d (%s)", got, tc.wantClaims, tc.explanation)
			}
		})
	}
}

// reconcileCheckpointsOnBoot itself never consults liveness: with the reaper
// disabled it still fails the rows the previous process left creating and
// releases their claims. Whether the BOOT calls it without consulting liveness
// is a different property, held by
// TestNewServerReconcilesCheckpointsWithoutConsultingLiveness below: this
// test calls the function directly, so re-gating the call site would not
// fail it.
func TestReconcileCheckpointsOnBootDoesNotConsultLiveness(t *testing.T) {
	s := newRetentionTestServer(t)
	disabled := false
	s.hubCfg = &types.HubConfig{Liveness: &types.LivenessConfig{Enabled: &disabled}}
	if s.livenessEnabled() {
		t.Fatal("fixture did not disable liveness")
	}
	reference := time.Now()
	insertRetentionClaw(t, s, "claw", reference)
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "creating", createdAt: reference})
	sha := writeRetentionBlob(t, []byte("interrupted upload"))
	if err := s.recordCheckpointBlobRefs("cp", testTreeSHA, []types.CheckpointFile{
		{Path: "workspace/a.txt", SHA256: sha, Size: 18}}); err != nil {
		t.Fatalf("recordCheckpointBlobRefs: %v", err)
	}

	s.reconcileCheckpointsOnBoot()

	status, _, _ := retentionCheckpointRow(t, s, "cp")
	if status != "failed" {
		t.Fatalf("status = %q, want failed", status)
	}
	if got := checkpointBlobRefCount(t, s, "cp"); got != 0 {
		t.Fatalf("claims = %d, want 0", got)
	}
}

// The boot reconciliation must not depend on the reaper: a claim left behind by
// a crash pins blobs against a sweeper that runs whether liveness does or not.
// NewServer starts every background loop and touches the network, so the call
// site is held at the source: reconcileCheckpointsOnBoot must be a statement
// of NewServer's own body, not nested under the `if srv.livenessEnabled()`
// that guards reconcileOnBoot.
//
// Revert verified against: the call moved inside `if srv.livenessEnabled()`.
func TestNewServerReconcilesCheckpointsWithoutConsultingLiveness(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var newServer *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "NewServer" && fn.Recv == nil {
			newServer = fn
		}
	}
	if newServer == nil {
		t.Fatal("NewServer not found in server.go")
	}
	// The statements of the function body itself, not of any block nested in it.
	topLevel := map[string]bool{}
	for _, stmt := range newServer.Body.List {
		if name, ok := methodCallOnSrv(stmt); ok {
			topLevel[name] = true
		}
	}
	// Every call anywhere in the body, so a call that moved into an `if` is
	// distinguished from one that was deleted.
	anywhere := map[string]bool{}
	ast.Inspect(newServer.Body, func(n ast.Node) bool {
		if stmt, ok := n.(ast.Stmt); ok {
			if name, ok := methodCallOnSrv(stmt); ok {
				anywhere[name] = true
			}
		}
		return true
	})
	if !anywhere["reconcileCheckpointsOnBoot"] {
		t.Fatal("NewServer no longer calls reconcileCheckpointsOnBoot at all")
	}
	if !topLevel["reconcileCheckpointsOnBoot"] {
		t.Fatal("NewServer calls reconcileCheckpointsOnBoot inside a nested block; it must run whether or not liveness is enabled")
	}
	// The discriminator is real: reconcileOnBoot IS gated, and reads as such.
	if !anywhere["reconcileOnBoot"] || topLevel["reconcileOnBoot"] {
		t.Fatal("reconcileOnBoot is expected under `if srv.livenessEnabled()`; the test's notion of nesting no longer matches server.go")
	}
}

// methodCallOnSrv reports the method name when stmt is `srv.<method>(...)`.
func methodCallOnSrv(stmt ast.Stmt) (string, bool) {
	expr, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return "", false
	}
	call, ok := expr.X.(*ast.CallExpr)
	if !ok {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	if recv, ok := sel.X.(*ast.Ident); !ok || recv.Name != "srv" {
		return "", false
	}
	return sel.Sel.Name, true
}

// ---------------------------------------------------------------------------
// The batch pause and the cycle budget (finding 10)
// ---------------------------------------------------------------------------

// The pause has to land on a step of SQLite's busy-handler ladder
// (1,2,5,10,15,20,25,25,25,50,50,100ms). At 2ms the deleter re-acquired the
// write lock before any waiter woke, so the lock was released on paper and held
// for ~90% of wall time in practice.
func TestRetentionBatchPauseClearsABusyHandlerStep(t *testing.T) {
	if retentionBatchPause < 50*time.Millisecond {
		t.Fatalf("retentionBatchPause = %s; it must be at least one busy-handler step (50ms)", retentionBatchPause)
	}
}

// A huge first sweep must be spread across cycles rather than monopolising one.
func TestPruneRowsBatchedStopsAtTheCycleBudget(t *testing.T) {
	cases := []struct {
		name     string
		deadline func() time.Time
		wantAll  bool
		why      string
	}{
		{
			name:     "budget already exhausted",
			deadline: func() time.Time { return time.Now().Add(-time.Second) },
			wantAll:  false,
			why:      "one batch, then stop and leave the rest for the next cycle",
		},
		{
			name:     "no budget set",
			deadline: func() time.Time { return time.Time{} },
			wantAll:  true,
			why:      "the zero deadline means unbounded, as the direct callers in tests use",
		},
	}
	total := int64(2*retentionDeleteBatch + 7)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRetentionTestServer(t)
			insertRetentionClaw(t, s, "claw", time.Now())
			old := time.Now().Add(-200 * 24 * time.Hour)
			tx, err := s.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			for i := int64(0); i < total; i++ {
				if _, err := tx.Exec(
					`INSERT INTO messages(id, claw_id, tenant_id, role, content, created_at) VALUES(?,?,?,?,?,?)`,
					fmt.Sprintf("msg-%d", i), "claw", "tenant", "user", "old", old); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}

			deleted, err := s.pruneRowsBatched("messages", "created_at", time.Now(), false, newRetentionPacer("messages", tc.deadline()))
			if err != nil {
				t.Fatalf("pruneRowsBatched: %v", err)
			}
			if tc.wantAll && deleted != total {
				t.Fatalf("deleted %d of %d rows, want all (%s)", deleted, total, tc.why)
			}
			if !tc.wantAll {
				if deleted != retentionDeleteBatch {
					t.Fatalf("deleted %d rows, want exactly one batch of %d (%s)", deleted, retentionDeleteBatch, tc.why)
				}
				var left int64
				if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&left); err != nil {
					t.Fatal(err)
				}
				if left != total-retentionDeleteBatch {
					t.Fatalf("%d rows left, want %d; the next cycle picks them up", left, total-retentionDeleteBatch)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Live-claw statuses (finding 11)
// ---------------------------------------------------------------------------

// 'offline' is a dropped WebSocket, which routinely self-recovers; 'idle' is
// resumable by design. Treating either as finished let one merged PR collapse
// the recovery points of a claw that was about to carry on working.
func TestFinalizedPredicateTreatsOfflineAndIdleAsLive(t *testing.T) {
	cases := []struct {
		name          string
		status        string
		wantFinalized bool
		why           string
	}{
		{"connected", "connected", false, "obviously running"},
		{"starting", "starting", false, "obviously running"},
		{"provisioning", "provisioning", false, "obviously running"},
		{"offline", "offline", false, "a dropped WebSocket that usually comes back"},
		{"idle", "idle", false, "resumable by design; the idle-resume path exists to wake it"},
		{"completed", "completed", true, "genuinely done"},
		{"error", "error", true, "terminal"},
		{"deleted", "deleted", true, "terminal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRetentionTestServer(t)
			// Fresh last_seen so the staleness arm stays inert and the merged-PR
			// arm is the only thing that could finalize the claw.
			insertRetentionClaw(t, s, "claw", time.Now())
			if _, err := s.db.Exec(`UPDATE claws SET status=? WHERE id='claw'`, tc.status); err != nil {
				t.Fatal(err)
			}
			insertRetentionPR(t, s, "claw", 1)

			finalized, err := s.clawFinalized("claw", time.Now().Add(-240*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if finalized != tc.wantFinalized {
				t.Fatalf("finalized = %v, want %v (%s)", finalized, tc.wantFinalized, tc.why)
			}
		})
	}
}

// The staleness arm still reaches offline and idle claws: they are excluded
// from the merged-PR shortcut, not from compaction altogether.
func TestStalenessArmStillFinalizesOfflineAndIdleClaws(t *testing.T) {
	for _, status := range []string{"offline", "idle"} {
		t.Run(status, func(t *testing.T) {
			s := newRetentionTestServer(t)
			insertRetentionClaw(t, s, "claw", time.Now().Add(-400*24*time.Hour))
			if _, err := s.db.Exec(`UPDATE claws SET status=? WHERE id='claw'`, status); err != nil {
				t.Fatal(err)
			}
			finalized, err := s.clawFinalized("claw", time.Now().Add(-240*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if !finalized {
				t.Fatalf("a %s claw untouched for longer than compact_after must still finalize", status)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The policy line (finding 13)
// ---------------------------------------------------------------------------

// Nothing logged the effective policy. A disabled sweeper is silent forever, so
// a misconfiguration produced no signal at all, and an enabled one first spoke
// an hour later without ever saying which values it was using -- the floors and
// the compact_after ordering rule can both replace a configured value silently.
func TestLogRetentionPolicyStatesTheEffectivePolicy(t *testing.T) {
	enabled, disabled := true, false
	cases := []struct {
		name     string
		cfg      *types.RetentionConfig
		wantAny  []string
		wantNone []string
	}{
		{
			name: "disabled says so explicitly",
			cfg:  &types.RetentionConfig{Enabled: &disabled},
			wantAny: []string{
				"[retention] disabled",
				"retention.enabled",
			},
		},
		{
			name: "absent config is disabled",
			cfg:  nil,
			wantAny: []string{
				"[retention] disabled",
			},
		},
		{
			name: "enabled reports every effective knob",
			cfg:  &types.RetentionConfig{Enabled: &enabled, DryRun: true, Interval: "2h", MaxAge: "720h", CompactAfter: "48h"},
			wantAny: []string{
				"[retention] enabled", "dry_run=true", "interval=2h0m0s", "max_age=720h0m0s",
				"compact_after=48h0m0s", "adjustments=[none]", "first_cycle_at=",
			},
		},
		{
			name: "a clamped knob is named",
			cfg:  &types.RetentionConfig{Enabled: &enabled, Interval: "1s", MaxAge: "1h"},
			wantAny: []string{
				"interval raised to the 10m0s minimum",
				"max_age raised to the 168h0m0s minimum",
			},
			wantNone: []string{"adjustments=[none]"},
		},
		{
			name: "an ordering fix is named",
			cfg:  &types.RetentionConfig{Enabled: &enabled, MaxAge: "192h", CompactAfter: "200h"},
			wantAny: []string{
				"compact_after lowered to 96h0m0s so it stays below max_age",
				"compact_after=96h0m0s",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRetentionTestServer(t)
			s.hubCfg = &types.HubConfig{Retention: tc.cfg}
			out := captureRetentionLog(t, s.logRetentionPolicy)
			for _, want := range tc.wantAny {
				if !containsString(out, want) {
					t.Fatalf("policy line %q does not mention %q", out, want)
				}
			}
			for _, unwanted := range tc.wantNone {
				if containsString(out, unwanted) {
					t.Fatalf("policy line %q should not mention %q", out, unwanted)
				}
			}
		})
	}
}
