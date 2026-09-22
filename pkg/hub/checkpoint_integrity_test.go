package hub

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// ---------------------------------------------------------------------------
// A digest is the only thing that becomes a blob path
// ---------------------------------------------------------------------------

// checkpointBlobPath is the one function that turns a digest into a path, and
// the digests it is handed come off the wire. It must refuse everything
// normalizeBlobDigest refuses, or a plan entry of "../../x" becomes a path the
// claim primitive stats and touches.
func TestCheckpointBlobPathRefusesEverythingThatIsNotADigest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	valid := strings.Repeat("0123456789abcdef", 4)
	root := filepath.Join(checkpointsRoot(), "blobs", "sha256")
	for _, value := range []string{valid, "sha256:" + valid, " " + valid + "\n"} {
		got := checkpointBlobPath(value)
		want := filepath.Join(root, valid[:2], valid[2:4], valid)
		if got != want {
			t.Errorf("checkpointBlobPath(%q) = %q, want %q", value, got, want)
		}
	}
	for _, value := range []string{
		"", "sha256:", "../../sentinel", "../../../../../../etc/passwd", "sha256:../../x",
		strings.ToUpper(valid), valid[:63], valid + "0", "/" + valid, valid + "/..",
	} {
		if got := checkpointBlobPath(value); got != "" {
			t.Errorf("checkpointBlobPath(%q) = %q, want \"\" (not a digest)", value, got)
		}
	}
}

// writeAgedSentinel puts a file outside the blob root with an mtime a
// traversal would visibly change, and returns its path and that mtime.
func writeAgedSentinel(t *testing.T) (string, time.Time) {
	t.Helper()
	if err := os.MkdirAll(hubDataDir(), 0o750); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(hubDataDir(), "sentinel")
	if err := os.WriteFile(sentinel, []byte("outside the blob root"), 0o640); err != nil {
		t.Fatal(err)
	}
	aged := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(sentinel, aged, aged); err != nil {
		t.Fatal(err)
	}
	return sentinel, aged
}

func assertSentinelUntouched(t *testing.T, sentinel string, aged time.Time) {
	t.Helper()
	info, err := os.Stat(sentinel)
	if err != nil {
		t.Fatalf("the sentinel outside the blob root is gone: %v", err)
	}
	if !info.ModTime().Equal(aged) {
		t.Fatalf("the sentinel outside the blob root was touched: mtime %s, was %s", info.ModTime(), aged)
	}
}

// A plan carrying a traversal digest reached claimBlobPresent, which stats and
// then Chtimes the path the digest resolves to. "../../sentinel" resolved,
// under the unvalidated join, to a file three levels above the blob root --
// any file the hub can write, for any claw-token holder.
func TestPlanWithTraversalDigestIsRejectedAndTouchesNothingOutsideTheBlobRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newCheckpointCompletionTestServer(t)
	insertTestCheckpoint(t, s, "cp", "manual")
	sentinel, aged := writeAgedSentinel(t)

	fileSHA := writeRetentionBlob(t, []byte("a real file"))
	rootSHA := writeRetentionBlob(t, []byte(fmt.Sprintf(`[{"path":"workspace/a.txt","sha256":%q,"size":11}]`, fileSHA)))
	plan := types.CheckpointPlan{RootSHA256: rootSHA, Files: []types.CheckpointFile{
		{Path: "workspace/a.txt", SHA256: fileSHA, Size: 11},
		{Path: "workspace/evil", SHA256: "../../sentinel", Size: 1},
	}}
	body, _ := json.Marshal(plan)
	req := httptest.NewRequest(http.MethodPost, "/api/checkpoints/cp/plan", bytes.NewReader(body))
	req.Header.Set("X-Claw-Token", "claw-token")
	rr := httptest.NewRecorder()
	s.handleCheckpointInternal(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("a plan naming a non-digest was answered %d: %s", rr.Code, rr.Body.String())
	}
	assertSentinelUntouched(t, sentinel, aged)
	if n := checkpointBlobRefCount(t, s, "cp"); n != 0 {
		t.Fatalf("the rejected plan recorded %d edge(s); a rejected plan must have no side effect", n)
	}
	if got := checkpointStatus(t, s, "cp"); got != "creating" {
		t.Fatalf("status = %q after a rejected plan, want creating", got)
	}
}

// The same rule at the upload handler, which claims before it writes.
func TestBlobUploadWithTraversalDigestIsRejectedAndTouchesNothingOutsideTheBlobRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newCheckpointCompletionTestServer(t)
	sentinel, aged := writeAgedSentinel(t)
	for _, digest := range []string{"../../sentinel", strings.ToUpper(strings.Repeat("0123456789abcdef", 4))} {
		req := httptest.NewRequest(http.MethodPut, "/api/checkpoints/blob/"+digest, strings.NewReader("payload"))
		req.Header.Set("X-Claw-Token", "claw-token")
		rr := httptest.NewRecorder()
		s.handleCheckpointBlobUpload(rr, req)
		// "bad sha" is the digest check; "sha mismatch" would mean the handler
		// accepted the name and went on to lay a file out for it.
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "bad sha") {
			t.Fatalf("upload under %q was answered %d %q, want 400 bad sha", digest, rr.Code, rr.Body.String())
		}
	}
	assertSentinelUntouched(t, sentinel, aged)
}

// ---------------------------------------------------------------------------
// Two completes of one checkpoint publish exactly one manifest
// ---------------------------------------------------------------------------

// holdCompletesAtStagedManifest arms the staged-manifest hook as a barrier for
// exactly `parties` completes of the same checkpoint: each waits there until
// all have staged their manifest, so all have passed the pre-check and none
// has committed -- the interleaving in which the loser used to unlink the
// winner's file.
func holdCompletesAtStagedManifest(t *testing.T, parties int) {
	t.Helper()
	var wg sync.WaitGroup
	wg.Add(parties)
	checkpointManifestStagedHook = func(string) {
		wg.Done()
		wg.Wait()
	}
	t.Cleanup(func() { checkpointManifestStagedHook = nil })
}

// assertOnePublishedManifest checks the row is ready, its manifest exists at
// the recorded path with the recorded digest, and no attempt left a staged
// file behind.
func assertOnePublishedManifest(t *testing.T, s *Server, checkpointID string) {
	t.Helper()
	status, manifestPath, manifestSHA := retentionCheckpointRow(t, s, checkpointID)
	if status != "ready" {
		t.Fatalf("status = %q, want ready", status)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("the ready row's manifest is not on disk: %v (the loser removed the winner's file)", err)
	}
	if got := shaBytes(data); got != manifestSHA {
		t.Fatalf("manifest on disk hashes to %s, the row says %s: another attempt's bytes were published under this row", got, manifestSHA)
	}
	entries, err := os.ReadDir(filepath.Dir(manifestPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".attempt-") {
			t.Fatalf("staged manifest %s was left behind", e.Name())
		}
	}
}

func seedPlannedWorkspace(t *testing.T, s *Server, checkpointID string) (rootSHA, fileSHA string) {
	t.Helper()
	fileSHA = writeRetentionBlob(t, []byte("workspace file of "+checkpointID))
	rootSHA = writeRetentionBlob(t, []byte(fmt.Sprintf(`[{"path":"workspace/a.txt","sha256":%q,"size":%d}]`, fileSHA, 18+len(checkpointID))))
	if err := s.recordCheckpointBlobRefs(checkpointID, rootSHA, []types.CheckpointFile{{Path: "workspace/a.txt", SHA256: fileSHA}}); err != nil {
		t.Fatalf("record plan: %v", err)
	}
	return rootSHA, fileSHA
}

// finalizeCheckpoint wrote the manifest to its final, deterministic path
// before the guarded UPDATE, and unlinked that path on every non-published
// exit. Two concurrent completes both passed the pre-check; the loser's
// RowsAffected()==0 exit then removed the file the winner had just committed a
// 'ready' row against.
func TestConcurrentCompletesPublishExactlyOneManifest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newCheckpointCompletionTestServer(t)
	seedCheckpointCompletionState(t, s)
	rootSHA, _ := seedPlannedWorkspace(t, s, "current")
	holdCompletesAtStagedManifest(t, 2)

	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			rr := completeCheckpoint(t, s, fmt.Sprintf(`{"root_sha256":%q}`, rootSHA))
			codes <- rr.Code
		}()
	}
	var ok, rejected int
	for i := 0; i < 2; i++ {
		select {
		case code := <-codes:
			switch code {
			case http.StatusOK:
				ok++
			default:
				rejected++
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the two completes did not both return; the barrier or the write lock deadlocked")
		}
	}
	if ok != 1 || rejected != 1 {
		t.Fatalf("got %d successful and %d rejected completes, want exactly one of each", ok, rejected)
	}
	assertOnePublishedManifest(t, s, "current")
}

// The hub-side capture writes its manifest the same way and had the same
// race.
func TestConcurrentMetadataOnlyCompletesPublishExactlyOneManifest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newCheckpointCompletionTestServer(t)
	insertTestCheckpoint(t, s, "cp", "manual")
	holdCompletesAtStagedManifest(t, 2)

	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { errs <- s.completeMetadataOnlyCheckpoint("cp", "claw", "bridge-unreachable", "detail") }()
	}
	var won, lost int
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			switch {
			case err == nil:
				won++
			case errors.Is(err, errCheckpointNotCreating):
				lost++
			default:
				t.Fatalf("unexpected error: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the two completes did not both return")
		}
	}
	if won != 1 || lost != 1 {
		t.Fatalf("got %d winners and %d losers, want exactly one of each", won, lost)
	}
	assertOnePublishedManifest(t, s, "cp")
}

// ---------------------------------------------------------------------------
// Failing a 'creating' row does not depend on the non-fatal tables
// ---------------------------------------------------------------------------

func dropCheckpointBlobRefTables(t *testing.T, s *Server) {
	t.Helper()
	for _, table := range checkpointBlobRefTables {
		if _, err := s.db.Exec(`DROP TABLE ` + table.name); err != nil {
			t.Fatal(err)
		}
	}
}

// The reference tables are created off the fatal boot path, so a hub that
// could not allocate their pages serves without them -- and on that hub every
// plan is rejected, leaving a 'creating' row. The three paths that fail such a
// row ran their edge DELETE in the same transaction as the status change, so
// "no such table" rolled the status change back too, and the rows were never
// failed. The merge-base failed them unconditionally.
func TestCreatingRowsAreFailedWhenTheReferenceTablesAreMissing(t *testing.T) {
	reference := time.Now()
	cases := []struct {
		name string
		fail func(t *testing.T, s *Server, id string)
	}{
		{"boot reconcile", func(t *testing.T, s *Server, id string) {
			if n, err := failCreatingCheckpointsOnBoot(s.db, reference); err != nil || n != 1 {
				t.Fatalf("failCreatingCheckpointsOnBoot: n=%d err=%v", n, err)
			}
		}},
		{"stuck creating", func(t *testing.T, s *Server, id string) {
			if n, err := failStuckCreatingCheckpointsTx(s.db, reference.Add(-checkpointCreatingMaxAge)); err != nil || n != 1 {
				t.Fatalf("failStuckCreatingCheckpointsTx: n=%d err=%v", n, err)
			}
		}},
		{"explicit failure", func(t *testing.T, s *Server, id string) {
			if err := s.failCheckpoint(id, "plan rejected"); err != nil {
				t.Fatalf("failCheckpoint: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRetentionTestServer(t)
			insertRetentionClaw(t, s, "claw", reference)
			insertRetentionCheckpoint(t, s, retentionCheckpoint{
				id: "cp", clawID: "claw", status: "creating",
				createdAt: reference.Add(-2 * checkpointCreatingMaxAge), noBlobRefs: true})
			dropCheckpointBlobRefTables(t, s)
			tc.fail(t, s, "cp")
			if got := checkpointStatus(t, s, "cp"); got != "failed" {
				t.Fatalf("status = %q without the reference tables, want failed", got)
			}
		})
	}
}

// With the tables present the edge release is still part of the transaction:
// a failed row holds nothing.
func TestFailingARowReleasesItsEdgesWhenTheTablesExist(t *testing.T) {
	reference := time.Now()
	s := newRetentionTestServer(t)
	insertRetentionClaw(t, s, "claw", reference)
	tree := writeRetentionBlob(t, []byte(`[]`))
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "creating", rootTree: tree, createdAt: reference.Add(-2 * checkpointCreatingMaxAge)})
	if n := checkpointBlobRefCount(t, s, "cp"); n == 0 {
		t.Fatal("fixture seeded no edges")
	}
	if n, err := failStuckCreatingCheckpointsTx(s.db, reference.Add(-checkpointCreatingMaxAge)); err != nil || n != 1 {
		t.Fatalf("failStuckCreatingCheckpointsTx: n=%d err=%v", n, err)
	}
	if got := checkpointStatus(t, s, "cp"); got != "failed" {
		t.Fatalf("status = %q, want failed", got)
	}
	if n := checkpointBlobRefCount(t, s, "cp"); n != 0 {
		t.Fatalf("a failed row still holds %d edge(s)", n)
	}
}

// ---------------------------------------------------------------------------
// The gate is per digest column
// ---------------------------------------------------------------------------

// "Does the row have any edge" let a row whose message blob had no edge off
// the work list on the strength of its root edge, and the sweep then read that
// message blob as garbage. Every digest column needs its own edge.
func TestGateSelectsARowWhoseAnyDigestColumnLacksItsEdge(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Now()
	insertRetentionClaw(t, s, "claw", reference)
	tree := writeRetentionBlob(t, []byte(`[]`))
	msg := writeRetentionBlob(t, []byte(`{"role":"user"}`))
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "ready", rootTree: tree, messageTree: msg, createdAt: reference, writeManifest: true})
	count := func() int64 {
		n, _, err := s.unreferencedCheckpoints()
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(); n != 0 {
		t.Fatalf("a fully referenced row is on the work list (%d)", n)
	}
	if _, err := s.db.Exec(`DELETE FROM checkpoint_blob_refs WHERE checkpoint_id='cp' AND sha256=?`, msg); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Fatalf("a row with a root edge but no edge for its message blob is off the work list; the sweep would read that blob as garbage")
	}
	// The backfill gives it the missing edge and takes it off the list.
	if err := s.backfillCheckpointBlobRefs(); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 0 {
		t.Fatalf("the backfill did not clear the row (%d left)", n)
	}
}

// ---------------------------------------------------------------------------
// A complete naming an unplanned root claims the expansion
// ---------------------------------------------------------------------------

func claimedDuringSweep(s *Server, sha string) bool {
	s.blobClaimMu.Lock()
	defer s.blobClaimMu.Unlock()
	_, ok := s.blobClaimWindow[normalizeBlobDigest(sha)]
	return ok
}

// finalizeCheckpoint expands a root the plan never named and commits edges
// for its files -- files that never went through the claim half of the
// interlock. The shipped bridge always completes with the planned root, so
// this is a property the bridge was upholding on the hub's behalf.
func TestCompleteWithUnplannedRootClaimsItsExpansion(t *testing.T) {
	t.Run("unplanned root claims the tree and its files", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		s := newCheckpointCompletionTestServer(t)
		insertTestCheckpoint(t, s, "cp", "manual")
		fileSHA := writeRetentionBlob(t, []byte("never planned"))
		rootSHA := writeRetentionBlob(t, []byte(fmt.Sprintf(`[{"path":"workspace/a.txt","sha256":%q,"size":13}]`, fileSHA)))
		s.beginBlobClaimWindow()
		defer s.endBlobClaimWindow()
		if err := s.finalizeCheckpoint("cp", "tenant", "claw", rootSHA); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		for _, sha := range []string{rootSHA, fileSHA} {
			if !claimedDuringSweep(s, sha) {
				t.Fatalf("%s was referenced by the complete without being claimed through the interlock", sha)
			}
		}
	})
	t.Run("a root the plan did not name is claimed even when the plan named another", func(t *testing.T) {
		// The row already holds an edge (its planned root A), so a probe for
		// "any edge under this checkpoint" would call B planned. B has its
		// own edge or it does not.
		t.Setenv("HOME", t.TempDir())
		s := newCheckpointCompletionTestServer(t)
		insertTestCheckpoint(t, s, "cp", "manual")
		seedPlannedWorkspace(t, s, "cp")
		fileSHA := writeRetentionBlob(t, []byte("completed, never planned"))
		rootSHA := writeRetentionBlob(t, []byte(fmt.Sprintf(`[{"path":"workspace/b.txt","sha256":%q,"size":24}]`, fileSHA)))
		s.beginBlobClaimWindow()
		defer s.endBlobClaimWindow()
		if err := s.finalizeCheckpoint("cp", "tenant", "claw", rootSHA); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		for _, sha := range []string{rootSHA, fileSHA} {
			if !claimedDuringSweep(s, sha) {
				t.Fatalf("%s was referenced by the complete without being claimed: the planned root's edge passed for this one", sha)
			}
		}
	})
	t.Run("a planned root is not re-claimed", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		s := newCheckpointCompletionTestServer(t)
		insertTestCheckpoint(t, s, "cp", "manual")
		rootSHA, fileSHA := seedPlannedWorkspace(t, s, "cp")
		s.beginBlobClaimWindow()
		defer s.endBlobClaimWindow()
		if err := s.finalizeCheckpoint("cp", "tenant", "claw", rootSHA); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if claimedDuringSweep(s, fileSHA) {
			t.Fatal("the planned path re-claimed every file; that is ~12k stats per finalize the plan already paid for")
		}
	})
	t.Run("an unplanned root whose file is missing fails the complete", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		s := newCheckpointCompletionTestServer(t)
		insertTestCheckpoint(t, s, "cp", "manual")
		sum := sha256.Sum256([]byte("never uploaded"))
		missing := hex.EncodeToString(sum[:])
		rootSHA := writeRetentionBlob(t, []byte(fmt.Sprintf(`[{"path":"workspace/a.txt","sha256":%q,"size":14}]`, missing)))
		if err := s.finalizeCheckpoint("cp", "tenant", "claw", rootSHA); err == nil {
			t.Fatal("a complete referencing a blob the store does not hold was recorded as ready")
		}
		if got := checkpointStatus(t, s, "cp"); got != "creating" {
			t.Fatalf("status = %q, want creating", got)
		}
	})
}
