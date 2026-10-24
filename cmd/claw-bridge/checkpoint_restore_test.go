package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
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
	// The hub stages files flat as files/<index>, always 0644 (docker CopyIn),
	// and carries the checkpoint modes in the marker manifest.
	staged := []struct {
		path, content string
		mode          int64
	}{
		{"workspace/repo/script.sh", "checkpoint script", 0755},
		{".openclaw/agents/main/agent.json", "checkpoint agent", 0600},
		{".openclaw/openclaw.json", "old config", 0600},
		{".openclaw/identity/device.json", "old identity", 0600},
		{"workspace/link", "", 0644},
		{"workspace/repo/src/main/java/App.java", "nested", 0},
	}
	var manifest stagedCheckpointManifest
	for i, f := range staged {
		manifest.Files = append(manifest.Files, struct {
			Path string `json:"path"`
			Mode int64  `json:"mode"`
		}{f.path, f.mode})
		source := filepath.Join(staging, "files", strconv.Itoa(i))
		if f.path == "workspace/link" {
			if err := os.Symlink(filepath.Join(home, ".openclaw/openclaw.json"), source); err != nil {
				t.Fatal(err)
			}
			continue
		}
		write(source, f.content, 0644)
	}
	if err := restoreStagedCheckpoint(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(home, ".openclaw/workspace/repo/script.sh")); string(got) != "fresh clone" {
		t.Fatal("restore without marker changed workspace")
	}
	// A staging entry the bridge cannot unlink (root-owned in production)
	// must not fail bootstrap once the files are restored.
	locked := filepath.Join(staging, "locked")
	write(filepath.Join(locked, "file"), "x", 0644)
	if err := os.Chmod(locked, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0755) })
	marker, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(staging, ".complete"), string(marker), 0644)
	if err := restoreStagedCheckpoint(); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		"workspace/repo/script.sh":              "checkpoint script",
		"workspace/repo/.git/HEAD":              "git metadata",
		"workspace/repo/src/main/java/App.java": "nested",
		"agents/main/agent.json":                "checkpoint agent",
		"openclaw.json":                         "fresh config",
		"identity/device.json":                  "fresh identity",
	} {
		data, err := os.ReadFile(filepath.Join(home, ".openclaw", rel))
		if err != nil || string(data) != want {
			t.Errorf("%s = %q, %v; want %q", rel, data, err, want)
		}
	}
	for rel, want := range map[string]os.FileMode{
		"workspace/repo/script.sh":              0755,
		"agents/main/agent.json":                0600,
		"workspace/repo/src/main/java/App.java": 0644,
	} {
		info, err := os.Stat(filepath.Join(home, ".openclaw", rel))
		if err != nil || info.Mode().Perm() != want {
			t.Errorf("%s mode: %v, %v; want %v", rel, info, err, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(home, ".openclaw/workspace/link")); !os.IsNotExist(err) {
		t.Errorf("staged symlink was restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(staging, ".complete")); !os.IsNotExist(err) {
		t.Fatalf("marker was not removed: %v", err)
	}
	if err := os.Chmod(locked, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(staging); err != nil {
		t.Fatal(err)
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
