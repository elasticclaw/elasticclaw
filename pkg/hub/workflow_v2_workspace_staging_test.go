package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// TestWorkflowV2TriggerStagesWorkspaceFilesAndRepositories guards the
// workspace assembly contract reported in
// https://github.com/elasticclaw/elasticclaw/issues/691: a v2 workspace pushed
// with scripts/, instruction files, and repositories must create claws whose
// template_files and github_repos carry the full set, exactly like v1.
func TestWorkflowV2TriggerStagesWorkspaceFilesAndRepositories(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token", ClawToken: "claw-token",
		Providers: map[string]types.ProviderConfig{"noop": {Type: "noop"}}}, "", "", "")

	workspaceYAML := `
schema_version: 2
name: replicated-troubleshoot
execution:
  provider: noop
repositories:
  troubleshoot:
    provider: github
    repository: replicated/troubleshoot
    permissions: read
`
	workflowYAML := `
schema_version: 2
name: dependency-update
enabled: true
manual_trigger: true
initial_state: build
states:
  build:
    phase: build
  done:
    phase: done
    terminal: true
`
	// Push payload shaped exactly like the CLI's readWorkspaceDir output for v2:
	// ReadTemplateFiles allow-list (scripts/**, AGENTS.md, flake files) + config.
	pushed := &types.WorkspaceConfig{
		SchemaVersion: "2",
		Name:          "replicated-troubleshoot",
		Files: map[string]string{
			"elasticclaw-config.yaml":        workspaceYAML,
			"AGENTS.md":                      "# agents\n",
			"CONTEXT.md":                     "# context\n",
			"flake.nix":                      "# flake\n",
			"flake.lock":                     "# lock\n",
			"scripts/prepare-deps-branch.sh": "#!/bin/bash\necho hi\n",
		},
	}
	if err := saveExternalWorkspace(pushed); err != nil {
		t.Fatal(err)
	}
	if err := saveExternalWorkflows("replicated-troubleshoot", []*types.WorkflowConfig{{
		Name: "dependency-update", RawConfig: workflowYAML,
	}}); err != nil {
		t.Fatal(err)
	}

	// Hub-side load must see the authored files and the projected repositories.
	loaded, err := loadExternalWorkspace("replicated-troubleshoot")
	if err != nil {
		t.Fatal(err)
	}
	wantFiles := []string{
		"elasticclaw-config.yaml", "AGENTS.md",
		"flake.nix", "flake.lock", "scripts/prepare-deps-branch.sh",
	}
	for _, name := range wantFiles {
		if _, ok := loaded.Files[name]; !ok {
			t.Errorf("loaded workspace missing file %s (have %v)", name, sortedKeys(loaded.Files))
		}
	}
	if len(loaded.Repositories) != 1 || loaded.Repositories[0].Repo != "replicated/troubleshoot" {
		t.Errorf("loaded repositories = %+v", loaded.Repositories)
	}

	req := httptest.NewRequest(http.MethodPost,
		"/api/workspaces/replicated-troubleshoot/workflows/dependency-update/trigger", strings.NewReader(`{"inputs":{}}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var created map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	waitForWorkflowV2TestClaw(t, db, created["claw_id"])

	var filesJSON, reposJSON string
	if err := db.QueryRow(`SELECT template_files, github_repos FROM claws WHERE id=?`,
		created["claw_id"]).Scan(&filesJSON, &reposJSON); err != nil {
		t.Fatal(err)
	}
	var files map[string]string
	if err := json.Unmarshal([]byte(filesJSON), &files); err != nil {
		t.Fatal(err)
	}
	for _, name := range wantFiles {
		if _, ok := files[name]; !ok {
			t.Errorf("claw template_files missing %s (have %v)", name, sortedKeys(files))
		}
	}
	var repos []types.GitHubRepoAccess
	if err := json.Unmarshal([]byte(reposJSON), &repos); err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].Repo != "replicated/troubleshoot" {
		t.Errorf("claw github_repos = %+v", repos)
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
