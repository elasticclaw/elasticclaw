package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCollectCheckpointFilesExcludesSessionsAndAuthProfiles(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	files := map[string]string{
		".openclaw/workspace/src/main.go":          "package main",
		".openclaw/openclaw.json":                  "{}",
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
