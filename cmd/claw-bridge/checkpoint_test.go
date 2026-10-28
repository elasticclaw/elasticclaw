package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestCollectCheckpointFilesSkipsOversizedAndBuildCaches(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	ws := filepath.Join(tmpDir, ".openclaw", "workspace")
	for _, rel := range []string{"a.txt", ".turbo/x.log", ".pnpm-store/v3/f"} {
		abs := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	big := filepath.Join(ws, "big.bin")
	if err := os.WriteFile(big, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(big, types.MaxCheckpointBlobBytes+1); err != nil {
		t.Fatal(err)
	}
	got, err := collectCheckpointFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].entry.Path != "workspace/a.txt" {
		t.Fatalf("expected only workspace/a.txt, got %+v", got)
	}
}

func TestCollectCheckpointFilesExcludesSessionsAndAuthProfiles(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	files := map[string]string{
		".openclaw/workspace/src/main.go":                   "package main",
		".openclaw/workspace/sessions/data.json":            "[]",
		".openclaw/openclaw.json":                           "{}",
		".openclaw/agents/main/agent/openclaw-agent.sqlite": "sqlite",
		".openclaw/agents/main/agent/auth-profiles.json":    "auth",
		".openclaw/agents/main/sessions/session.jsonl":      "{}",
	}
	for rel, content := range files {
		abs := filepath.Join(tmpDir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(abs), err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", abs, err)
		}
	}

	got, err := collectCheckpointFiles()
	if err != nil {
		t.Fatalf("collectCheckpointFiles: %v", err)
	}

	byPath := map[string]bool{}
	for _, f := range got {
		byPath[f.entry.Path] = true
	}

	if !byPath["workspace/src/main.go"] {
		t.Errorf("expected workspace file to be checkpointed, got paths: %v", byPath)
	}
	if !byPath["workspace/sessions/data.json"] {
		t.Errorf("expected workspace sessions folder to be checkpointed, got paths: %v", byPath)
	}
	if !byPath[".openclaw/openclaw.json"] {
		t.Errorf("expected openclaw.json to be checkpointed, got paths: %v", byPath)
	}
	if byPath[".openclaw/agents/main/agent/openclaw-agent.sqlite"] {
		t.Errorf("openclaw-agent.sqlite should be excluded from checkpoints")
	}
	if byPath[".openclaw/agents/main/agent/auth-profiles.json"] {
		t.Errorf("auth-profiles.json should be excluded from checkpoints")
	}
	if byPath[".openclaw/agents/main/sessions/session.jsonl"] {
		t.Errorf("session files should be excluded from checkpoints")
	}
}

func TestCollectCheckpointFilesSkipsSymlinks(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	workspace := filepath.Join(tmpDir, ".openclaw", "workspace", "next_mobile")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", workspace, err)
	}
	target := filepath.Join(workspace, "AGENTS.md")
	if err := os.WriteFile(target, []byte("# agents"), 0o600); err != nil {
		t.Fatalf("write %s: %v", target, err)
	}
	if err := os.Symlink(target, filepath.Join(workspace, "CLAUDE.md")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got, err := collectCheckpointFiles()
	if err != nil {
		t.Fatalf("collectCheckpointFiles: %v", err)
	}

	byPath := map[string]bool{}
	for _, f := range got {
		byPath[f.entry.Path] = true
	}

	if !byPath["workspace/next_mobile/AGENTS.md"] {
		t.Errorf("expected regular file to be checkpointed, got paths: %v", byPath)
	}
	if byPath["workspace/next_mobile/CLAUDE.md"] {
		t.Errorf("symlink should be skipped, got paths: %v", byPath)
	}
}

func TestCollectCheckpointFilesSkipsDanglingSymlink(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	workspace := filepath.Join(tmpDir, ".openclaw", "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", workspace, err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	if err := os.Symlink(filepath.Join(tmpDir, "gone.md"), filepath.Join(workspace, "broken.md")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got, err := collectCheckpointFiles()
	if err != nil {
		t.Fatalf("collectCheckpointFiles: %v", err)
	}

	byPath := map[string]bool{}
	for _, f := range got {
		byPath[f.entry.Path] = true
	}

	if !byPath["workspace/main.go"] {
		t.Errorf("expected regular file to be checkpointed, got paths: %v", byPath)
	}
	if byPath["workspace/broken.md"] {
		t.Errorf("dangling symlink should be skipped, got paths: %v", byPath)
	}
}

// fakeCheckpointHub asks for every planned blob and answers the first
// mismatchUploads content-blob PUTs with the hub's "sha mismatch" 400.
func fakeCheckpointHub(t *testing.T, mismatchUploads int32) (srv *httptest.Server, plans, completes *atomic.Int32) {
	t.Helper()
	plans, completes = &atomic.Int32{}, &atomic.Int32{}
	var mismatches atomic.Int32
	var rootSHA atomic.Value
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/plan"):
			plans.Add(1)
			var plan types.CheckpointPlan
			if err := json.NewDecoder(r.Body).Decode(&plan); err != nil {
				t.Errorf("decode plan: %v", err)
			}
			rootSHA.Store(plan.RootSHA256)
			var ack types.CheckpointPlanAck
			for _, f := range plan.Files {
				ack.Upload = append(ack.Upload, f.SHA256)
			}
			_ = json.NewEncoder(w).Encode(ack)
		case strings.HasPrefix(r.URL.Path, "/api/checkpoints/blob/"):
			sha := strings.TrimPrefix(r.URL.Path, "/api/checkpoints/blob/")
			if sha != rootSHA.Load() && mismatches.Add(1) <= mismatchUploads {
				http.Error(w, "sha mismatch", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/complete"):
			completes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, plans, completes
}

func writeCheckpointWorkspaceFile(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	ws := filepath.Join(home, ".openclaw", "workspace")
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCreateCheckpointRetriesOnceWhenHubReportsShaMismatch(t *testing.T) {
	writeCheckpointWorkspaceFile(t)
	srv, plans, completes := fakeCheckpointHub(t, 1)

	err := createCheckpoint(context.Background(), types.CheckpointCreatePayload{CheckpointID: "cp-1", HubURL: srv.URL})
	if err != nil {
		t.Fatalf("createCheckpoint: %v", err)
	}
	if plans.Load() != 2 || completes.Load() != 1 {
		t.Fatalf("plans=%d completes=%d, want 2 and 1", plans.Load(), completes.Load())
	}
}

func TestCreateCheckpointGivesUpAfterSecondShaMismatch(t *testing.T) {
	writeCheckpointWorkspaceFile(t)
	srv, plans, completes := fakeCheckpointHub(t, 2)

	err := createCheckpoint(context.Background(), types.CheckpointCreatePayload{CheckpointID: "cp-2", HubURL: srv.URL})
	if err == nil || !strings.Contains(err.Error(), "sha mismatch") {
		t.Fatalf("createCheckpoint err=%v, want sha mismatch", err)
	}
	if plans.Load() != 2 || completes.Load() != 0 {
		t.Fatalf("plans=%d completes=%d, want 2 and 0", plans.Load(), completes.Load())
	}
}
