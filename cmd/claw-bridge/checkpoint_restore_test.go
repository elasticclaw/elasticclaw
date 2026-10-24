package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRestoreStagedCheckpoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	staging := filepath.Join(home, ".elasticclaw", "checkpoint-restore")
	write := func(path, content string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, ".openclaw/workspace/repo/script.sh"), "fresh clone", 0644)
	write(filepath.Join(home, ".openclaw/workspace/repo/.git/HEAD"), "git metadata", 0644)
	write(filepath.Join(home, ".openclaw/openclaw.json"), "fresh config", 0600)
	write(filepath.Join(home, ".openclaw/identity/device.json"), "fresh identity", 0600)
	write(filepath.Join(staging, "workspace/repo/script.sh"), "checkpoint script", 0755)
	write(filepath.Join(staging, ".openclaw/agents/main/agent.json"), "checkpoint agent", 0600)
	write(filepath.Join(staging, ".openclaw/openclaw.json"), "old config", 0600)
	write(filepath.Join(staging, ".openclaw/identity/device.json"), "old identity", 0600)
	if err := os.Symlink(filepath.Join(home, ".openclaw/openclaw.json"), filepath.Join(staging, "workspace/link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, ".openclaw/identity"), filepath.Join(staging, "workspace/link-dir")); err != nil {
		t.Fatal(err)
	}
	if err := restoreStagedCheckpoint(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(home, ".openclaw/workspace/repo/script.sh")); string(got) != "fresh clone" {
		t.Fatal("restore without marker changed workspace")
	}
	write(filepath.Join(staging, ".complete"), "ready\n", 0644)
	if err := restoreStagedCheckpoint(); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		"workspace/repo/script.sh": "checkpoint script",
		"workspace/repo/.git/HEAD": "git metadata",
		"agents/main/agent.json":   "checkpoint agent",
		"openclaw.json":            "fresh config",
		"identity/device.json":     "fresh identity",
	} {
		data, err := os.ReadFile(filepath.Join(home, ".openclaw", rel))
		if err != nil || string(data) != want {
			t.Errorf("%s = %q, %v; want %q", rel, data, err, want)
		}
	}
	info, err := os.Stat(filepath.Join(home, ".openclaw/workspace/repo/script.sh"))
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("restored executable mode: %v, %v", info, err)
	}
	for _, rel := range []string{"link", "link-dir"} {
		if _, err := os.Lstat(filepath.Join(home, ".openclaw/workspace", rel)); !os.IsNotExist(err) {
			t.Errorf("source symlink %s was restored: %v", rel, err)
		}
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("staging was not removed: %v", err)
	}
	if err := restoreStagedCheckpoint(); err != nil {
		t.Fatalf("missing staging directory: %v", err)
	}
}

func TestOverlayCheckpointFileRejectsEscapes(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"../outside", filepath.Join(outside, "absolute"), "escape/file", "escape/nested/file"} {
		t.Run(rel, func(t *testing.T) {
			if err := overlayCheckpointFile(root, rel, []byte("bad"), 0644); err == nil {
				t.Fatal("accepted escaping checkpoint path")
			}
		})
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("restore wrote outside root: %v, %v", entries, err)
	}
}
