package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCollectWorkspacesForPush_DefaultPath(t *testing.T) {
	// Create a temporary repo structure with factory-workspaces/<name>/
	root := t.TempDir()
	workspacesDir := filepath.Join(root, "factory-workspaces")
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

	workspaces, err := collectAndValidateWorkspaces("", "")
	if err != nil {
		t.Fatalf("collect workspaces: %v", err)
	}
	if len(workspaces) != 2 {
		t.Fatalf("want 2 workspaces, got %d", len(workspaces))
	}
}

func TestCollectWorkspacesForPush_LegacyPath(t *testing.T) {
	// .elasticclaw/workspaces remains supported when factory-workspaces is absent.
	root := t.TempDir()
	workspacesDir := filepath.Join(root, ".elasticclaw", "workspaces")
	if err := os.MkdirAll(workspacesDir, 0755); err != nil {
		t.Fatalf("mkdir workspaces: %v", err)
	}
	writeMinimalWorkspace(t, filepath.Join(workspacesDir, "legacy-ws"), "legacy-ws")

	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(origDir)

	workspaces, err := collectAndValidateWorkspaces("", "")
	if err != nil {
		t.Fatalf("collect workspaces: %v", err)
	}
	if len(workspaces) != 1 || workspaces[0].Name != "legacy-ws" {
		t.Fatalf("want 1 workspace named legacy-ws, got %+v", workspaces)
	}
}

func TestCollectWorkspacesForPush_PrefersFactoryWorkspaces(t *testing.T) {
	root := t.TempDir()
	writeMinimalWorkspace(t, filepath.Join(root, "factory-workspaces", "shared"), "shared")
	writeMinimalWorkspace(t, filepath.Join(root, ".elasticclaw", "workspaces", "shared"), "shared")

	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(origDir)

	workspaces, err := collectAndValidateWorkspaces("", "")
	if err != nil {
		t.Fatalf("collect workspaces: %v", err)
	}
	if len(workspaces) != 1 || workspaces[0].Name != "shared" {
		t.Fatalf("want single preferred workspace shared, got %+v", workspaces)
	}
}

func TestCollectWorkspacesForPush_DefaultPathWithFilter(t *testing.T) {
	root := t.TempDir()
	workspacesDir := filepath.Join(root, "factory-workspaces")
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

	workspaces, err := collectAndValidateWorkspaces("other-ws", "")
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

	workspaces, err := collectAndValidateWorkspaces("", customPath)
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
	workspaces, err := collectAndValidateWorkspaces("wrong-name", customPath)
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

	_, err := collectAndValidateWorkspaces("", filePath)
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

func TestReadWorkspaceDirV2MapRepositories(t *testing.T) {
	dir := t.TempDir()
	// Shape matches amazecrm-dev: named map repositories (not v1 list).
	config := `
schema_version: 2
name: amazecrm-dev

repositories:
  primary:
    provider: github
    repository: amazecrm/amazecrm
    source_control: github-default

credentials:
  github_app:
    secret: github_app

source_control:
  connections:
    github-default:
      provider: github
      credentials: github_app
`
	if err := os.WriteFile(filepath.Join(dir, "elasticclaw-config.yaml"), []byte(config), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# agents\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ws, err := readWorkspaceDir(dir)
	if err != nil {
		t.Fatalf("readWorkspaceDir: %v", err)
	}
	if ws.Name != "amazecrm-dev" {
		t.Fatalf("name = %q", ws.Name)
	}
	if ws.SchemaVersion != "2" {
		t.Fatalf("schema = %q", ws.SchemaVersion)
	}
	raw := ws.Files["elasticclaw-config.yaml"]
	if !strings.Contains(raw, "schema_version: 2") || !strings.Contains(raw, "primary:") {
		t.Fatalf("Files missing authored v2 YAML:\n%s", raw)
	}
	if ws.Files["AGENTS.md"] == "" {
		t.Fatal("expected AGENTS.md in Files")
	}
	if err := ws.Validate(); err != nil {
		t.Fatalf("shell Validate: %v", err)
	}

	// collectAndValidateWorkspaces must accept --path to a v2 workspace dir.
	got, err := collectAndValidateWorkspaces("", dir)
	if err != nil {
		t.Fatalf("collectAndValidateWorkspaces: %v", err)
	}
	if len(got) != 1 || got[0].Name != "amazecrm-dev" {
		t.Fatalf("got %#v", got)
	}
}

func TestReadWorkspaceDirV2InvalidRejected(t *testing.T) {
	dir := t.TempDir()
	config := `
schema_version: 2
name: broken
ci:
  connections:
    c1:
      provider: github_actions
      credentials: missing
`
	if err := os.WriteFile(filepath.Join(dir, "elasticclaw-config.yaml"), []byte(config), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := readWorkspaceDir(dir)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "unknown credential") && !strings.Contains(err.Error(), "validate workspace v2") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunWorkspaceValidate_DefaultPath(t *testing.T) {
	root := t.TempDir()
	workspacesDir := filepath.Join(root, "factory-workspaces")
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

	out, err := captureStdout(func() error {
		return runWorkspaceValidate("", "")
	})
	if err != nil {
		t.Fatalf("validate workspaces: %v", err)
	}
	if !strings.Contains(out, "Validated 2 workspace(s)") {
		t.Fatalf("output missing validation count:\n%s", out)
	}
	if !strings.Contains(out, "default-ws") || !strings.Contains(out, "other-ws") {
		t.Fatalf("output missing workspace names:\n%s", out)
	}
}

func TestRunWorkspaceValidate_FilterName(t *testing.T) {
	root := t.TempDir()
	workspacesDir := filepath.Join(root, "factory-workspaces")
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

	out, err := captureStdout(func() error {
		return runWorkspaceValidate("other-ws", "")
	})
	if err != nil {
		t.Fatalf("validate filtered workspace: %v", err)
	}
	if !strings.Contains(out, "Validated 1 workspace(s)") || !strings.Contains(out, "other-ws") {
		t.Fatalf("output missing filtered workspace:\n%s", out)
	}
	if strings.Contains(out, "default-ws") {
		t.Fatalf("output should not include default-ws:\n%s", out)
	}
}

func TestRunWorkspaceValidate_InvalidWorkspace(t *testing.T) {
	root := t.TempDir()
	workspacesDir := filepath.Join(root, "factory-workspaces")
	if err := os.MkdirAll(workspacesDir, 0755); err != nil {
		t.Fatalf("mkdir workspaces: %v", err)
	}
	invalidDir := filepath.Join(workspacesDir, "invalid-ws")
	if err := os.MkdirAll(invalidDir, 0755); err != nil {
		t.Fatalf("mkdir invalid workspace: %v", err)
	}
	// Invalid repository permissions make this workspace invalid.
	config := "schema_version: v1\nname: invalid-ws\nprovider: replicated\nrepositories:\n  - repo: owner/repo\n    permissions: admin\n"
	if err := os.WriteFile(filepath.Join(invalidDir, "elasticclaw-config.yaml"), []byte(config), 0644); err != nil {
		t.Fatalf("write invalid config: %v", err)
	}

	origDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(origDir)

	_, err = captureStdout(func() error {
		return runWorkspaceValidate("", "")
	})
	if err == nil {
		t.Fatalf("expected validation error for invalid workspace, got nil")
	}
	if !strings.Contains(err.Error(), "invalid permissions") {
		t.Fatalf("expected invalid permissions error, got %v", err)
	}
}

func TestWorkspaceValidateCmdRejectsTooManyArgs(t *testing.T) {
	cmd := workspaceValidateCmd()
	err := cmd.Args(cmd, []string{"first", "second"})
	if err == nil {
		t.Fatalf("expected error for too many args, got nil")
	}
}

func TestRunWorkspaceValidate_CustomPath(t *testing.T) {
	root := t.TempDir()
	customPath := filepath.Join(root, "custom", "my-workspace")
	writeMinimalWorkspace(t, customPath, "my-workspace")

	out, err := captureStdout(func() error {
		return runWorkspaceValidate("", customPath)
	})
	if err != nil {
		t.Fatalf("validate custom path workspace: %v", err)
	}
	if !strings.Contains(out, "Validated 1 workspace(s)") || !strings.Contains(out, "my-workspace") {
		t.Fatalf("output missing custom path workspace:\n%s", out)
	}
}
