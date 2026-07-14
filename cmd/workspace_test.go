package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCollectWorkspacesForPush_DefaultPath(t *testing.T) {
	// Create a temporary repo structure with .elasticclaw/workspaces/<name>/
	root := t.TempDir()
	workspacesDir := filepath.Join(root, ".elasticclaw", "workspaces")
	if err := os.MkdirAll(workspacesDir, 0755); err != nil {
		t.Fatalf("mkdir workspaces: %v", err)
	}
	writeMinimalWorkspace(t, filepath.Join(workspacesDir, "default-ws"), "default-ws")
	writeMinimalWorkspace(t, filepath.Join(workspacesDir, "other-ws"), "other-ws")

	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(origDir)

	workspaces, err := collectWorkspacesForPush("", "")
	if err != nil {
		t.Fatalf("collect workspaces: %v", err)
	}
	if len(workspaces) != 2 {
		t.Fatalf("want 2 workspaces, got %d", len(workspaces))
	}
}

func TestCollectWorkspacesForPush_DefaultPathWithFilter(t *testing.T) {
	root := t.TempDir()
	workspacesDir := filepath.Join(root, ".elasticclaw", "workspaces")
	if err := os.MkdirAll(workspacesDir, 0755); err != nil {
		t.Fatalf("mkdir workspaces: %v", err)
	}
	writeMinimalWorkspace(t, filepath.Join(workspacesDir, "default-ws"), "default-ws")
	writeMinimalWorkspace(t, filepath.Join(workspacesDir, "other-ws"), "other-ws")

	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(origDir)

	workspaces, err := collectWorkspacesForPush("other-ws", "")
	if err != nil {
		t.Fatalf("collect workspaces: %v", err)
	}
	if len(workspaces) != 1 || workspaces[0].Name != "other-ws" {
		t.Fatalf("want 1 workspace named other-ws, got %+v", workspaces)
	}
}

func TestCollectWorkspacesForPush_CustomPath(t *testing.T) {
	root := t.TempDir()
	customPath := filepath.Join(root, "custom", "my-workspace")
	writeMinimalWorkspace(t, customPath, "my-workspace")

	workspaces, err := collectWorkspacesForPush("", customPath)
	if err != nil {
		t.Fatalf("collect workspaces: %v", err)
	}
	if len(workspaces) != 1 || workspaces[0].Name != "my-workspace" {
		t.Fatalf("want 1 workspace named my-workspace, got %+v", workspaces)
	}
}

func TestCollectWorkspacesForPush_CustomPathFilterMismatch(t *testing.T) {
	root := t.TempDir()
	customPath := filepath.Join(root, "custom", "my-workspace")
	writeMinimalWorkspace(t, customPath, "my-workspace")

	// A filter mismatch on a custom path should behave like the default scan:
	// return no matches (not a hard error), so callers get a consistent
	// "no workspaces matched" message from runWorkspacePush.
	workspaces, err := collectWorkspacesForPush("wrong-name", customPath)
	if err != nil {
		t.Fatalf("expected no error for filter mismatch, got %v", err)
	}
	if len(workspaces) != 0 {
		t.Fatalf("expected 0 workspaces for filter mismatch, got %d", len(workspaces))
	}
}

func TestCollectWorkspacesForPush_CustomPathNotDirectory(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "not-a-dir")
	if err := os.WriteFile(filePath, []byte("x"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	_, err := collectWorkspacesForPush("", filePath)
	if err == nil {
		t.Fatalf("expected error for non-directory path, got nil")
	}
}

func writeMinimalWorkspace(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	config := "schema_version: v1\nname: " + name + "\nprovider: replicated\n"
	if err := os.WriteFile(filepath.Join(dir, "elasticclaw-config.yaml"), []byte(config), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}
