package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

var hubHTTPClient = &http.Client{Timeout: 30 * time.Second}
var hubTransferHTTPClient = &http.Client{Timeout: 10 * time.Minute}

type checkpointLocalFile struct {
	entry types.CheckpointFile
	abs   string
}

func handleCheckpointCreate(ctx context.Context, payload json.RawMessage) {
	var req types.CheckpointCreatePayload
	if err := json.Unmarshal(payload, &req); err != nil {
		log.Printf("[checkpoint] bad request: %v", err)
		return
	}
	if req.CheckpointID == "" || req.HubURL == "" || req.ClawToken == "" {
		log.Printf("[checkpoint] missing required request fields")
		return
	}
	if err := createCheckpoint(ctx, req); err != nil {
		log.Printf("[checkpoint] failed: %v", err)
		_ = postCheckpointComplete(ctx, req, types.CheckpointComplete{
			CheckpointID: req.CheckpointID,
			Error:        err.Error(),
		})
	}
}

func createCheckpoint(ctx context.Context, req types.CheckpointCreatePayload) error {
	files, err := collectCheckpointFiles()
	if err != nil {
		return err
	}
	entries := make([]types.CheckpointFile, 0, len(files))
	bySHA := map[string]string{}
	for _, f := range files {
		entries = append(entries, f.entry)
		bySHA[f.entry.SHA256] = f.abs
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	treeData, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	rootSHA := shaBytesLocal(treeData)
	planFiles := append([]types.CheckpointFile{}, entries...)
	planFiles = append(planFiles, types.CheckpointFile{
		Path:   ".checkpoint/tree.json",
		SHA256: rootSHA,
		Size:   int64(len(treeData)),
		Mode:   0o640,
	})
	plan := types.CheckpointPlan{
		CheckpointID: req.CheckpointID,
		RootSHA256:   rootSHA,
		Files:        planFiles,
	}
	ack, err := postCheckpointPlan(ctx, req, plan)
	if err != nil {
		return err
	}
	for _, sha := range ack.Upload {
		var body io.Reader
		if sha == rootSHA {
			body = bytes.NewReader(treeData)
		} else {
			path := bySHA[sha]
			if path == "" {
				return fmt.Errorf("hub requested unknown blob %s", sha)
			}
			f, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("open blob %s: %w", path, err)
			}
			body = f
			if err := putCheckpointBlob(ctx, req, sha, body); err != nil {
				_ = f.Close()
				return err
			}
			_ = f.Close()
			continue
		}
		if err := putCheckpointBlob(ctx, req, sha, body); err != nil {
			return err
		}
	}
	return postCheckpointComplete(ctx, req, types.CheckpointComplete{
		CheckpointID: req.CheckpointID,
		RootSHA256:   rootSHA,
	})
}

func collectCheckpointFiles() ([]checkpointLocalFile, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	roots := []struct {
		base  string
		label string
	}{
		{filepath.Join(home, ".openclaw", "workspace"), "workspace"},
		{filepath.Join(home, ".openclaw", "openclaw.json"), ".openclaw/openclaw.json"},
		{filepath.Join(home, ".openclaw", "identity"), ".openclaw/identity"},
		{filepath.Join(home, ".openclaw", "agents"), ".openclaw/agents"},
	}
	var out []checkpointLocalFile
	skipped := 0
	firstSkipped := ""
	noteSkipped := func(path string) {
		skipped++
		if firstSkipped == "" {
			firstSkipped = path
		}
	}
	for _, root := range roots {
		info, err := os.Lstat(root.base)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			f, err := checkpointFile(root.base, root.label, info)
			if errors.Is(err, errNotRegularFile) {
				noteSkipped(root.base)
				continue
			}
			if err != nil {
				return nil, err
			}
			out = append(out, f)
			continue
		}
		err = filepath.WalkDir(root.base, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if checkpointExclude(path, d) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root.base, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(filepath.Join(root.label, rel))
			f, err := checkpointFile(path, rel, info)
			if errors.Is(err, errNotRegularFile) {
				noteSkipped(path)
				return nil
			}
			if err != nil {
				return err
			}
			out = append(out, f)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if skipped > 0 {
		log.Printf("[checkpoint] skipped %d non-regular file(s) (symlink/socket/fifo/device), first: %s", skipped, firstSkipped)
	}
	return out, nil
}

func checkpointExclude(path string, d os.DirEntry) bool {
	name := d.Name()
	if name == ".git" {
		return true
	}
	switch name {
	case "node_modules", ".cache", "tmp", "dist", "build", ".next", "auth-profiles.json", "openclaw-agent.sqlite":
		return true
	}
	// Only exclude the agent sessions directory, not arbitrary "sessions" folders in the workspace.
	if name == "sessions" && strings.Contains(filepath.ToSlash(path), ".openclaw/agents/") {
		return true
	}
	if strings.HasSuffix(name, ".log") && !strings.Contains(path, "workspace") {
		return true
	}
	return false
}

// errNotRegularFile marks entries a checkpoint cannot capture (symlinks, sockets,
// fifos, devices). Callers skip them instead of aborting the whole snapshot.
var errNotRegularFile = errors.New("not a regular file")

func checkpointFile(abs, rel string, info os.FileInfo) (checkpointLocalFile, error) {
	if !info.Mode().IsRegular() {
		return checkpointLocalFile{}, fmt.Errorf("%w: %s", errNotRegularFile, abs)
	}
	f, err := os.Open(abs)
	if err != nil {
		return checkpointLocalFile{}, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return checkpointLocalFile{}, err
	}
	return checkpointLocalFile{
		abs: abs,
		entry: types.CheckpointFile{
			Path:   filepath.ToSlash(rel),
			SHA256: hex.EncodeToString(h.Sum(nil)),
			Size:   info.Size(),
			Mode:   int64(info.Mode().Perm()),
		},
	}, nil
}

func postCheckpointPlan(ctx context.Context, req types.CheckpointCreatePayload, plan types.CheckpointPlan) (*types.CheckpointPlanAck, error) {
	var ack types.CheckpointPlanAck
	if err := checkpointJSON(ctx, req, "POST", fmt.Sprintf("/api/checkpoints/%s/plan", req.CheckpointID), plan, &ack); err != nil {
		return nil, err
	}
	return &ack, nil
}

func postCheckpointComplete(ctx context.Context, req types.CheckpointCreatePayload, complete types.CheckpointComplete) error {
	return checkpointJSON(ctx, req, "POST", fmt.Sprintf("/api/checkpoints/%s/complete", req.CheckpointID), complete, nil)
}

func checkpointJSON(ctx context.Context, req types.CheckpointCreatePayload, method, path string, in interface{}, out interface{}) error {
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(req.HubURL, "/")+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Claw-Token", req.ClawToken)
	resp, err := hubHTTPClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("hub checkpoint %s failed: HTTP %d: %s", path, resp.StatusCode, string(body))
	}
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

func putCheckpointBlob(ctx context.Context, req types.CheckpointCreatePayload, sha string, body io.Reader) error {
	url := fmt.Sprintf("%s/api/checkpoints/blob/%s", strings.TrimRight(req.HubURL, "/"), sha)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, url, body)
	if err != nil {
		return err
	}
	httpReq.Header.Set("X-Claw-Token", req.ClawToken)
	resp, err := hubTransferHTTPClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("upload blob %s: HTTP %d: %s", sha, resp.StatusCode, string(data))
	}
	return nil
}

func shaBytesLocal(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
