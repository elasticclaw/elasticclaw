package types

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// CheckpointTreePath is the plan entry under which the bridge lists the tree
// blob itself, so the hub asks for its upload like any other blob.
const CheckpointTreePath = ".checkpoint/tree.json"

// EncodeCheckpointTree serializes a workspace's file entries into the tree
// blob a checkpoint is addressed by, and returns the blob with its digest.
//
// This is the ONE definition of the tree serialization. The bridge calls it to
// build the tree it uploads, and the hub calls it to recompute the digest of
// the file list a plan carries before it believes that list is the tree's
// expansion. Two copies of this logic -- one per side -- would be a
// production outage waiting for a refactor: a hub that serializes even one
// byte differently rejects every legitimate plan, and a hub that verifies
// nothing lets a claw record a truncated expansion under a real tree digest
// (see verifyCheckpointPlanTree in pkg/hub).
//
// The entries are ordered by path (stably, so a caller that already ordered
// them gets its order back) and the encoding is encoding/json's compact form,
// which is what the bridge has always shipped; a nil or empty list encodes as
// "[]", never "null".
func EncodeCheckpointTree(entries []CheckpointFile) (tree []byte, root string, err error) {
	sorted := make([]CheckpointFile, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	tree, err = json.Marshal(sorted)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(tree)
	return tree, hex.EncodeToString(sum[:]), nil
}

// CheckpointPlanFiles is the file list a plan carries for a tree: the
// workspace entries, then the tree blob under CheckpointTreePath.
func CheckpointPlanFiles(entries []CheckpointFile, tree []byte, root string) []CheckpointFile {
	files := make([]CheckpointFile, 0, len(entries)+1)
	files = append(files, entries...)
	return append(files, CheckpointFile{
		Path:   CheckpointTreePath,
		SHA256: root,
		Size:   int64(len(tree)),
		Mode:   0o640,
	})
}
