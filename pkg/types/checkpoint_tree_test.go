package types

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"testing"
)

// The tree serialization is what every checkpoint on every hub is addressed
// by, and bridges already deployed produce it with the algorithm below: sort
// the entries by path, encoding/json them, sha256 the bytes. The shared
// encoder must reproduce those bytes exactly, or the hub -- which now
// recomputes the digest from the plan -- rejects every plan an older bridge
// sends. This pins the encoder to the historical construction, independently
// re-implemented here rather than called.
func TestEncodeCheckpointTreeMatchesTheShippedBridgeConstruction(t *testing.T) {
	entries := []CheckpointFile{
		{Path: "workspace/z.txt", SHA256: "aa", Size: 3, Mode: 0o644},
		{Path: "workspace/a<b>&.txt", SHA256: "bb", Size: 0, Mode: 0o600},
		{Path: ".openclaw/openclaw.json", SHA256: "cc", Size: 1 << 40, Mode: 0o640},
		{Path: "workspace/café/ .md", SHA256: "dd", Size: 7, Mode: 0o644},
	}
	historical := append([]CheckpointFile{}, entries...)
	sort.Slice(historical, func(i, j int) bool { return historical[i].Path < historical[j].Path })
	wantTree, err := json.Marshal(historical)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(wantTree)
	wantRoot := hex.EncodeToString(sum[:])

	tree, root, err := EncodeCheckpointTree(entries)
	if err != nil {
		t.Fatal(err)
	}
	if string(tree) != string(wantTree) {
		t.Fatalf("tree bytes differ from the shipped bridge construction:\n got %s\nwant %s", tree, wantTree)
	}
	if root != wantRoot {
		t.Fatalf("root = %s, want %s", root, wantRoot)
	}
	// The input is not reordered: the caller's slice is its own.
	if entries[0].Path != "workspace/z.txt" {
		t.Fatal("EncodeCheckpointTree sorted the caller's slice in place")
	}
}

// An empty workspace is a tree too, and the bridge has always sent "[]" for
// it: it builds its entry slice with make, never leaving it nil.
func TestEncodeCheckpointTreeEncodesAnEmptyWorkspaceAsAnEmptyList(t *testing.T) {
	for _, entries := range [][]CheckpointFile{nil, {}} {
		tree, root, err := EncodeCheckpointTree(entries)
		if err != nil {
			t.Fatal(err)
		}
		if string(tree) != "[]" {
			t.Fatalf("empty workspace encodes as %q, want []", tree)
		}
		sum := sha256.Sum256([]byte("[]"))
		if root != hex.EncodeToString(sum[:]) {
			t.Fatalf("root of the empty tree = %s", root)
		}
	}
}

func TestCheckpointPlanFilesAppendsTheTreeEntryLast(t *testing.T) {
	entries := []CheckpointFile{{Path: "workspace/a", SHA256: "aa", Size: 1, Mode: 0o644}}
	tree, root, err := EncodeCheckpointTree(entries)
	if err != nil {
		t.Fatal(err)
	}
	files := CheckpointPlanFiles(entries, tree, root)
	if len(files) != 2 || files[0] != entries[0] {
		t.Fatalf("plan files = %+v", files)
	}
	want := CheckpointFile{Path: CheckpointTreePath, SHA256: root, Size: int64(len(tree)), Mode: 0o640}
	if files[1] != want {
		t.Fatalf("tree entry = %+v, want %+v", files[1], want)
	}
}
