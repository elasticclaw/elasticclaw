package hub

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// ---------------------------------------------------------------------------
// A plan's file list must hash to the root it claims
// ---------------------------------------------------------------------------

// bridgePlan builds the plan the shipped bridge sends for a workspace, with
// the bridge's own construction: the tree is encoded by the shared encoder
// and the tree entry is appended after hashing. A test that wants a
// legitimate plan must go through here, so that a hub that rejects one is a
// hub that rejects the bridge.
func bridgePlan(t *testing.T, entries []types.CheckpointFile) (types.CheckpointPlan, []byte) {
	t.Helper()
	tree, root, err := types.EncodeCheckpointTree(entries)
	if err != nil {
		t.Fatal(err)
	}
	return types.CheckpointPlan{RootSHA256: root, Files: types.CheckpointPlanFiles(entries, tree, root)}, tree
}

// workspaceEntries writes n distinct file blobs and returns their entries,
// unsorted the way a directory walk might hand them out.
func workspaceEntries(t *testing.T, n int, seed string) []types.CheckpointFile {
	t.Helper()
	entries := make([]types.CheckpointFile, 0, n)
	for i := n - 1; i >= 0; i-- {
		content := []byte(fmt.Sprintf("file %s%d", seed, i))
		sha := writeRetentionBlob(t, content)
		entries = append(entries, types.CheckpointFile{Path: fmt.Sprintf("workspace/%d.txt", i), SHA256: sha, Size: int64(len(content)), Mode: 0o644})
	}
	return entries
}

// postPlan sends the plan over the wire exactly as the bridge's checkpointJSON
// does: json.Marshal of the struct, so a path the bridge could not encode
// verbatim reaches the hub the way it really would.
func postPlan(t *testing.T, s *Server, checkpointID string, plan types.CheckpointPlan) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/checkpoints/"+checkpointID+"/plan", bytes.NewReader(body))
	req.Header.Set("X-Claw-Token", "claw-token")
	rr := httptest.NewRecorder()
	s.handleCheckpointInternal(rr, req)
	return rr
}

func assertPlanRejectedWithoutSideEffects(t *testing.T, s *Server, checkpointID, root string, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("plan answered %d %q, want 400", rr.Code, rr.Body.String())
	}
	if n := checkpointBlobRefCount(t, s, checkpointID); n != 0 {
		t.Fatalf("the rejected plan recorded %d edge(s) under the checkpoint", n)
	}
	if n := treeBlobRefCount(t, s, root); n != 0 {
		t.Fatalf("the rejected plan recorded %d expansion row(s) for the tree", n)
	}
	if got := checkpointStatus(t, s, checkpointID); got != "creating" {
		t.Fatalf("status = %q after a rejected plan, want creating", got)
	}
}

// The hub recorded whatever file list a plan carried as the expansion of the
// root it named, and every later plan on a known tree trusts the existing
// expansion wholesale. A truncated list under a real digest therefore made
// the omitted files unreferenced for every tenant sharing the tree, and the
// sweep unlinked them. The plan's list must hash to the root; every way of
// tampering with it is rejected before anything is written.
func TestPlanFilesMustHashToTheRootTree(t *testing.T) {
	tamper := []struct {
		name  string
		apply func(files []types.CheckpointFile) []types.CheckpointFile
	}{
		{"a file removed", func(files []types.CheckpointFile) []types.CheckpointFile {
			return append(files[:1:1], files[2:]...)
		}},
		{"a file added", func(files []types.CheckpointFile) []types.CheckpointFile {
			extra := types.CheckpointFile{Path: "workspace/extra.txt", SHA256: strings.Repeat("ab", 32), Size: 1, Mode: 0o644}
			return append([]types.CheckpointFile{extra}, files...)
		}},
		{"a digest altered", func(files []types.CheckpointFile) []types.CheckpointFile {
			out := append([]types.CheckpointFile{}, files...)
			out[0].SHA256 = strings.Repeat("cd", 32)
			return out
		}},
		{"a size altered", func(files []types.CheckpointFile) []types.CheckpointFile {
			out := append([]types.CheckpointFile{}, files...)
			out[0].Size++
			return out
		}},
		{"a path altered", func(files []types.CheckpointFile) []types.CheckpointFile {
			out := append([]types.CheckpointFile{}, files...)
			out[0].Path += ".moved"
			return out
		}},
		{"every file removed", func(files []types.CheckpointFile) []types.CheckpointFile {
			return files[len(files)-1:]
		}},
	}
	for _, tc := range tamper {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			s := newCheckpointCompletionTestServer(t)
			insertTestCheckpoint(t, s, "cp", "manual")
			plan, _ := bridgePlan(t, workspaceEntries(t, 3, ""))
			plan.Files = tc.apply(plan.Files)
			rr := postPlan(t, s, "cp", plan)
			assertPlanRejectedWithoutSideEffects(t, s, "cp", plan.RootSHA256, rr)
		})
	}
}

// The other half, and the one that would take production down if wrong: a
// plan the bridge builds is never rejected. Each case is constructed with the
// bridge's own encoder and sent the way the bridge sends it.
func TestPlanBuiltByTheBridgeIsAccepted(t *testing.T) {
	cases := []struct {
		name    string
		entries func(t *testing.T) []types.CheckpointFile
		reorder func(files []types.CheckpointFile) []types.CheckpointFile
	}{
		{"a workspace", func(t *testing.T) []types.CheckpointFile { return workspaceEntries(t, 4, "") }, nil},
		{"an empty workspace", func(t *testing.T) []types.CheckpointFile { return nil }, nil},
		{"a path with characters the encoder escapes", func(t *testing.T) []types.CheckpointFile {
			e := workspaceEntries(t, 2, "esc")
			e[0].Path = "workspace/a<b>&c   \"q\" \\ \n.txt"
			e[1].Path = "workspace/café/�.txt"
			return e
		}, nil},
		{"a path that was not valid UTF-8 on the claw", func(t *testing.T) []types.CheckpointFile {
			e := workspaceEntries(t, 2, "utf")
			e[0].Path = "workspace/caf\xe9.txt"
			e[1].Path = "workspace/\xff\xfe/latin1-\xe4.md"
			return e
		}, nil},
		{"the plan's files sent out of order", func(t *testing.T) []types.CheckpointFile { return workspaceEntries(t, 3, "ord") },
			func(files []types.CheckpointFile) []types.CheckpointFile {
				out := append([]types.CheckpointFile{}, files...)
				for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
					out[i], out[j] = out[j], out[i]
				}
				return out
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			s := newCheckpointCompletionTestServer(t)
			insertTestCheckpoint(t, s, "cp", "manual")
			entries := tc.entries(t)
			plan, _ := bridgePlan(t, entries)
			if tc.reorder != nil {
				plan.Files = tc.reorder(plan.Files)
			}
			rr := postPlan(t, s, "cp", plan)
			if rr.Code != http.StatusOK {
				t.Fatalf("the bridge's own plan was answered %d %q; every checkpoint of this workspace would fail", rr.Code, rr.Body.String())
			}
			if n := checkpointBlobRefCount(t, s, "cp"); n != 1 {
				t.Fatalf("checkpoint holds %d edge(s) after its plan, want 1 (the root)", n)
			}
			if n := treeBlobRefCount(t, s, plan.RootSHA256); n != len(entries) {
				t.Fatalf("expansion has %d row(s), want one per file (%d)", n, len(entries))
			}
			var ack types.CheckpointPlanAck
			if err := json.Unmarshal(rr.Body.Bytes(), &ack); err != nil {
				t.Fatal(err)
			}
			// The tree blob is not on disk yet, so the hub asks for it.
			if len(ack.Upload) != 1 || ack.Upload[0] != plan.RootSHA256 {
				t.Fatalf("upload list = %v, want just the tree %s", ack.Upload, plan.RootSHA256)
			}
		})
	}
}

// A truncated plan for tree R must not establish R's expansion, or the honest
// plan that follows -- from any tenant -- finds the tree "known" and records
// nothing, leaving the omitted files unreferenced.
func TestTruncatedPlanCannotPoisonATreeForTheHonestPlanThatFollows(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newCheckpointCompletionTestServer(t)
	insertTestCheckpoint(t, s, "poisoner", "manual")
	insertTestCheckpoint(t, s, "honest", "manual")
	entries := workspaceEntries(t, 4, "")
	honest, _ := bridgePlan(t, entries)

	truncated := honest
	truncated.Files = append([]types.CheckpointFile{}, honest.Files[:1]...)
	truncated.Files = append(truncated.Files, honest.Files[len(honest.Files)-1]) // the tree entry
	rr := postPlan(t, s, "poisoner", truncated)
	assertPlanRejectedWithoutSideEffects(t, s, "poisoner", honest.RootSHA256, rr)

	if rr := postPlan(t, s, "honest", honest); rr.Code != http.StatusOK {
		t.Fatalf("honest plan answered %d %q", rr.Code, rr.Body.String())
	}
	if n := treeBlobRefCount(t, s, honest.RootSHA256); n != len(entries) {
		t.Fatalf("the tree's expansion has %d row(s) after the honest plan, want %d: the truncated plan's list was believed", n, len(entries))
	}
	var missing int
	for _, e := range entries {
		var present bool
		if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM tree_blob_refs WHERE tree_sha256=? AND sha256=?)`, honest.RootSHA256, e.SHA256).Scan(&present); err != nil {
			t.Fatal(err)
		}
		if !present {
			missing++
		}
	}
	if missing != 0 {
		t.Fatalf("%d file(s) of the honest plan are missing from the tree's expansion", missing)
	}
}

// A plan without a root is not a tree. It keeps the rootless fallback: one
// edge per file under the checkpoint, nothing expanded, nothing rejected.
func TestRootlessPlanStillTakesThePerFileFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newCheckpointCompletionTestServer(t)
	insertTestCheckpoint(t, s, "cp", "manual")
	entries := workspaceEntries(t, 3, "")
	rr := postPlan(t, s, "cp", types.CheckpointPlan{Files: entries})
	if rr.Code != http.StatusOK {
		t.Fatalf("rootless plan answered %d %q", rr.Code, rr.Body.String())
	}
	if n := checkpointBlobRefCount(t, s, "cp"); n != len(entries) {
		t.Fatalf("checkpoint holds %d edge(s), want one per file (%d)", n, len(entries))
	}
	var trees int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tree_blob_refs`).Scan(&trees); err != nil {
		t.Fatal(err)
	}
	if trees != 0 {
		t.Fatalf("a rootless plan expanded %d row(s)", trees)
	}
}

// ---------------------------------------------------------------------------
// Staged manifests a dead process left behind are collected at boot
// ---------------------------------------------------------------------------

// A crash between staging a manifest and renaming it leaves
// <id>.json.attempt-<uuid> under manifests/. The blob sweep never looks there,
// so they accumulated for the life of the installation.
func TestBootCollectsStagedManifestsLeftByADeadProcess(t *testing.T) {
	s := newRetentionTestServer(t)
	dir := filepath.Join(checkpointsRoot(), "manifests")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	write := func(name string, age time.Duration) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("{}"), 0o640); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		return path
	}
	dead := write("cp-dead.json"+stagedManifestMarker+"1111", 2*time.Hour)
	deadYoung := write("cp-young.json"+stagedManifestMarker+"2222", time.Second)
	manifest := write("cp-ready.json", 2*time.Hour)
	inFlight := write("cp-live.json"+stagedManifestMarker+"3333", -time.Minute)

	s.reconcileCheckpointsOnBoot()

	for _, path := range []string{dead, deadYoung} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("staged manifest %s survived boot (err %v); it belonged to a process that no longer exists", filepath.Base(path), err)
		}
	}
	for _, path := range []string{manifest, inFlight} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s was removed at boot: %v", filepath.Base(path), err)
		}
	}
}

// ---------------------------------------------------------------------------
// The publish step: rename then commit, and undo on either failure
// ---------------------------------------------------------------------------

// seedFinalizeState gives a checkpoint a planned workspace and a message so
// finalizeCheckpoint has everything it needs to publish.
func seedFinalizeState(t *testing.T, s *Server, checkpointID string) (rootSHA string) {
	t.Helper()
	insertTestCheckpoint(t, s, checkpointID, "manual")
	rootSHA, _ = seedPlannedWorkspace(t, s, checkpointID)
	return rootSHA
}

func stagedManifests(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(checkpointsRoot(), "manifests"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var staged []string
	for _, e := range entries {
		if strings.Contains(e.Name(), stagedManifestMarker) {
			staged = append(staged, e.Name())
		}
	}
	return staged
}

// A commit that fails AFTER the rename leaves a 'creating' row and a file at
// the final path. The file must go, or a later failure of the row strands it
// forever; and the row must still be winnable by the next complete. The
// commit is made to fail by ending the transaction underneath it: COMMIT on a
// connection with no open transaction is an error from the driver, at
// exactly the call publishCheckpointManifest makes.
func TestCommitFailureAfterTheRenameUnpublishesTheManifest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newCheckpointCompletionTestServer(t)
	rootSHA := seedFinalizeState(t, s, "cp")
	final := checkpointManifestPath("cp")

	renamed := false
	checkpointManifestPublishHook = func(tx *sql.Tx, path string) {
		if _, err := tx.Exec(`ROLLBACK`); err != nil {
			t.Errorf("end the transaction under the commit: %v", err)
		}
		// Observed from inside the hook, the final path must not exist yet:
		// the rename is what this failure has to undo.
		_, err := os.Stat(path)
		renamed = err == nil
	}
	t.Cleanup(func() { checkpointManifestPublishHook = nil })

	err := s.finalizeCheckpoint("cp", "tenant", "claw", rootSHA)
	if err == nil {
		t.Fatal("finalize reported success while its commit failed")
	}
	if !strings.Contains(err.Error(), "cannot commit") {
		t.Fatalf("finalize failed with %q, want the driver's commit failure: something before the commit failed instead", err)
	}
	if renamed {
		t.Fatal("the manifest was at its final path before the transaction committed or failed")
	}
	if _, statErr := os.Stat(final); !os.IsNotExist(statErr) {
		t.Fatalf("the manifest is still at its final path after the commit failed (stat err %v); a later failure of the row would strand it", statErr)
	}
	if staged := stagedManifests(t); len(staged) != 0 {
		t.Fatalf("staged manifests left behind: %v", staged)
	}
	if got := checkpointStatus(t, s, "cp"); got != "creating" {
		t.Fatalf("status = %q after the failed commit, want creating", got)
	}

	// The row is still winnable.
	checkpointManifestPublishHook = nil
	if err := s.finalizeCheckpoint("cp", "tenant", "claw", rootSHA); err != nil {
		t.Fatalf("the complete after the failed commit could not win the row: %v", err)
	}
	assertOnePublishedManifest(t, s, "cp")
}

// The rename comes BEFORE the commit. A rename that fails must leave the row
// 'creating', which is only true in that order: commit first, and a rename
// failure publishes a 'ready' row whose manifest does not exist.
func TestRenameFailureNeverPublishesTheRow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newCheckpointCompletionTestServer(t)
	rootSHA := seedFinalizeState(t, s, "cp")
	final := checkpointManifestPath("cp")

	checkpointManifestPublishHook = func(_ *sql.Tx, path string) {
		// A non-empty directory at the final path makes the rename fail.
		if err := os.MkdirAll(filepath.Join(path, "occupied"), 0o750); err != nil {
			t.Errorf("occupy the final path: %v", err)
		}
	}
	t.Cleanup(func() { checkpointManifestPublishHook = nil })

	if err := s.finalizeCheckpoint("cp", "tenant", "claw", rootSHA); err == nil {
		t.Fatal("finalize reported success while its rename failed")
	}
	if got := checkpointStatus(t, s, "cp"); got != "creating" {
		t.Fatalf("status = %q after the failed rename, want creating: the row was committed before its manifest was in place", got)
	}
	if staged := stagedManifests(t); len(staged) != 0 {
		t.Fatalf("staged manifests left behind: %v", staged)
	}
	if info, err := os.Stat(final); err != nil || !info.IsDir() {
		t.Fatalf("the occupied final path was disturbed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// A list the hub cannot verify is deferred, not rejected
// ---------------------------------------------------------------------------

// unverifiableEntries is a workspace the hub cannot verify from the plan
// alone: a name with a byte that is not UTF-8 (0x80) and a name with "é"
// (0xc3 0xa9). The bridge sorts the raw bytes, so 0x80 comes first; the wire
// turns 0x80 into U+FFFD (0xef 0xbf 0xbd), which sorts AFTER "é". No
// re-encoding on the hub restores the order the bridge hashed, so neither
// the raw nor the re-escaped digest matches the root.
func unverifiableEntries(t *testing.T, seed string) []types.CheckpointFile {
	t.Helper()
	e := workspaceEntries(t, 2, seed)
	e[0].Path = "workspace/\x80.txt"
	e[1].Path = "workspace/é.txt"
	return e
}

func assertTreeExpandedTo(t *testing.T, s *Server, root string, entries []types.CheckpointFile, when string) {
	t.Helper()
	if n := treeBlobRefCount(t, s, root); n != len(entries) {
		t.Fatalf("expansion has %d row(s) %s, want one per file (%d)", n, when, len(entries))
	}
	for _, e := range entries {
		var present bool
		if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM tree_blob_refs WHERE tree_sha256=? AND sha256=?)`, root, e.SHA256).Scan(&present); err != nil {
			t.Fatal(err)
		}
		if !present {
			t.Fatalf("file %s is missing from the tree's expansion %s", shortID(e.SHA256), when)
		}
	}
}

// One file whose name is not valid UTF-8 must not cost a claw every checkpoint
// it will ever take. The plan is accepted; what the hub declines to do is
// believe its list -- the expansion is written at finalize, from the tree
// blob the bridge uploads, which is content-addressed under the root.
func TestUnverifiablePlanIsAcceptedAndExpandedFromItsTreeBlob(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newCheckpointCompletionTestServer(t)
	insertTestCheckpoint(t, s, "cp", "manual")
	entries := unverifiableEntries(t, "")
	plan, tree := bridgePlan(t, entries)

	rr := postPlan(t, s, "cp", plan)
	if rr.Code != http.StatusOK {
		t.Fatalf("the bridge's plan for a workspace with an invalid-UTF-8 name was answered %d %q; every checkpoint of that claw would be refused", rr.Code, rr.Body.String())
	}
	if n := checkpointBlobRefCount(t, s, "cp"); n != 1 {
		t.Fatalf("checkpoint holds %d edge(s) after its plan, want 1 (the root)", n)
	}
	if n := treeBlobRefCount(t, s, plan.RootSHA256); n != 0 {
		t.Fatalf("the plan's list was recorded as the tree's expansion (%d row(s)) although the hub could not verify it", n)
	}
	var ack types.CheckpointPlanAck
	if err := json.Unmarshal(rr.Body.Bytes(), &ack); err != nil {
		t.Fatal(err)
	}
	if len(ack.Upload) != 1 || ack.Upload[0] != plan.RootSHA256 {
		t.Fatalf("upload list = %v, want just the tree %s", ack.Upload, plan.RootSHA256)
	}

	// The bridge uploads the tree it hashed and completes.
	if sha := writeRetentionBlob(t, tree); sha != plan.RootSHA256 {
		t.Fatalf("fixture: tree blob hashes to %s, plan root is %s", sha, plan.RootSHA256)
	}
	if err := s.finalizeCheckpoint("cp", "tenant", "claw", plan.RootSHA256); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if got := checkpointStatus(t, s, "cp"); got != "ready" {
		t.Fatalf("status = %q after finalize, want ready", got)
	}
	assertTreeExpandedTo(t, s, plan.RootSHA256, entries, "after the tree blob was uploaded and the checkpoint finalized")
}

// The security property survives the deferral: a truncated list the hub
// cannot verify must not become the tree's expansion any more than a
// truncated list it can. The honest plan that follows still finds the tree
// unknown and records it whole.
func TestUnverifiableTruncatedPlanCannotPoisonTheTree(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newCheckpointCompletionTestServer(t)
	insertTestCheckpoint(t, s, "poisoner", "manual")
	insertTestCheckpoint(t, s, "honest", "manual")
	entries := unverifiableEntries(t, "")
	entries = append(entries, workspaceEntries(t, 2, "more")...)
	honest, _ := bridgePlan(t, entries)

	// Drop a file with a plain name; the U+FFFD name stays, so the hub cannot
	// tell this list from one the wire mangled.
	truncated := honest
	truncated.Files = nil
	for _, f := range honest.Files {
		if f.Path == "workspace/1.txt" {
			continue
		}
		truncated.Files = append(truncated.Files, f)
	}
	rr := postPlan(t, s, "poisoner", truncated)
	if rr.Code != http.StatusOK {
		t.Fatalf("the unverifiable plan was answered %d %q, want 200: the hub cannot prove it wrong", rr.Code, rr.Body.String())
	}
	if n := treeBlobRefCount(t, s, honest.RootSHA256); n != 0 {
		t.Fatalf("the truncated list was recorded as the tree's expansion (%d row(s))", n)
	}

	if rr := postPlan(t, s, "honest", honest); rr.Code != http.StatusOK {
		t.Fatalf("honest plan answered %d %q", rr.Code, rr.Body.String())
	}
	if n := treeBlobRefCount(t, s, honest.RootSHA256); n != 0 {
		t.Fatalf("an honest but equally unverifiable plan recorded %d expansion row(s) at plan time", n)
	}
}

// Between an unverifiable plan and its finalize the files have no edge, and
// the hub does not bound how long that takes. The finalize must therefore
// prove every file the tree lists is still on disk before publishing, and
// refuse to publish over one the sweep took -- rather than record a 'ready'
// checkpoint that cannot be restored.
func TestFinalizeOfAnUnverifiablePlanFailsOnAFileTheSweepTook(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newCheckpointCompletionTestServer(t)
	insertTestCheckpoint(t, s, "cp", "manual")
	entries := unverifiableEntries(t, "")
	plan, tree := bridgePlan(t, entries)
	if rr := postPlan(t, s, "cp", plan); rr.Code != http.StatusOK {
		t.Fatalf("plan answered %d %q", rr.Code, rr.Body.String())
	}
	writeRetentionBlob(t, tree)
	if err := os.Remove(checkpointBlobPath(entries[1].SHA256)); err != nil {
		t.Fatal(err)
	}

	err := s.finalizeCheckpoint("cp", "tenant", "claw", plan.RootSHA256)
	if err == nil {
		t.Fatal("finalize published a checkpoint over a file blob that is gone")
	}
	if !strings.Contains(err.Error(), entries[1].SHA256) {
		t.Fatalf("finalize failed with %q, want the missing blob named", err)
	}
	if got := checkpointStatus(t, s, "cp"); got != "creating" {
		t.Fatalf("status = %q, want creating", got)
	}
	if n := treeBlobRefCount(t, s, plan.RootSHA256); n != 0 {
		t.Fatalf("the failed finalize recorded %d expansion row(s)", n)
	}
}

// A list with every path intact that does not hash to its root is a mismatch
// the producer earned: the deferral above is only for lists the hub cannot
// reproduce, never a way past the check. (TestPlanFilesMustHashToTheRootTree
// covers each way of tampering; this holds the boundary between the two
// verdicts on the same workspace.)
func TestTamperedPlanWithFaithfulPathsIsStillRejected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := newCheckpointCompletionTestServer(t)
	insertTestCheckpoint(t, s, "cp", "manual")
	entries := workspaceEntries(t, 3, "")
	entries[0].Path = "workspace/café.txt" // valid UTF-8, escaped by nothing
	plan, _ := bridgePlan(t, entries)
	plan.Files = plan.Files[1:] // drop one file, keep the tree entry
	rr := postPlan(t, s, "cp", plan)
	assertPlanRejectedWithoutSideEffects(t, s, "cp", plan.RootSHA256, rr)
}
