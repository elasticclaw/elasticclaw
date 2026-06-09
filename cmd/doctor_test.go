package cmd

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestDoctorWorkflowReadiness(t *testing.T) {
	withTempWorkingDir(t)

	configDir := t.TempDir()
	t.Setenv("ELASTICCLAW_HUB_CONFIG", filepath.Join(configDir, "hub.yaml"))
	rawWorkflow := strings.Join([]string{
		"schema_version: v1",
		"name: issue-triage",
		"secret_refs:",
		"  API_TOKEN: missing_api_token",
		"trigger:",
		"  github_issues:",
		"    event: issue_labeled",
		"    repositories:",
		"      - elasticclaw/elasticclaw",
		"    states:",
		"      - open",
		"    labels:",
		"      - agent-ready",
		"    labelers:",
		"      - \"*\"",
		"stages:",
		"  - id: working",
		"    entry: true",
		"    terminal: true",
		"",
	}, "\n")
	saveDoctorTestWorkspace(t, rawWorkflow)
	hub.SaveWorkspaceIssueTrackerForTest(t, "engineering", "github-issues", "default", "token-secret-value", "")

	factoryPath := writeDoctorTestFile(t, filepath.Join(configDir, "factories", "legacy-factory", "factory.yaml"), strings.Join([]string{
		"name: legacy-factory",
		"template: engineering",
		"integration: github-issues",
		"trigger_status: agent-ready",
		"",
	}, "\n"))
	runtimePath := writeDoctorTestFile(t, filepath.Join(".elasticclaw", "runtime", "sentinel.txt"), "runtime sentinel\n")

	s, _ := hub.NewTestServerWithConfig(t, &types.HubConfig{
		Token:     "test-token",
		ClawToken: "claw-token-secret-value",
	}, "", "", "")
	server := httptest.NewServer(s.Handler())
	t.Cleanup(server.Close)
	t.Setenv("ELASTICCLAW_HUB_URL", server.URL)
	t.Setenv("ELASTICCLAW_TOKEN", "test-token")

	workspacePath := filepath.Join(configDir, "workspaces", "engineering", "elasticclaw-config.yaml")
	workflowPath := filepath.Join(configDir, "workspaces", "engineering", "workflows", "issue-triage.yaml")
	trackerPath := filepath.Join(configDir, "workspaces", "engineering", ".elasticclaw-managed", "issue_trackers.yaml")
	before := map[string]string{
		workspacePath: readDoctorTestFile(t, workspacePath),
		workflowPath:  readDoctorTestFile(t, workflowPath),
		trackerPath:   readDoctorTestFile(t, trackerPath),
		factoryPath:   readDoctorTestFile(t, factoryPath),
		runtimePath:   readDoctorTestFile(t, runtimePath),
	}

	oldJSONOut := jsonOut
	jsonOut = true
	t.Cleanup(func() {
		jsonOut = oldJSONOut
	})

	firstOutput, err := executeDoctorCommand(t)
	if err != nil {
		t.Fatalf("doctor command: %v\n%s", err, firstOutput)
	}
	secondOutput, err := executeDoctorCommand(t)
	if err != nil {
		t.Fatalf("doctor command second run: %v\n%s", err, secondOutput)
	}

	for path, want := range before {
		if got := readDoctorTestFile(t, path); got != want {
			t.Fatalf("doctor mutated %s\n got:\n%s\nwant:\n%s", path, got, want)
		}
	}

	first := decodeDoctorCLIReport(t, firstOutput)
	second := decodeDoctorCLIReport(t, secondOutput)
	firstIDs := doctorCLICheckIDs(first.Checks)
	secondIDs := doctorCLICheckIDs(second.Checks)
	wantIDs := []string{
		"workflow-readiness:engineering:issue-triage:readiness-provider-missing",
		"workflow-readiness:engineering:issue-triage:readiness-model-missing",
		"workflow-readiness:engineering:issue-triage:readiness-secret-ref-missing",
		"workflow-readiness:engineering:issue-triage:readiness-webhook-secret-missing",
	}
	for _, id := range wantIDs {
		check, ok := firstIDs[id]
		if !ok {
			t.Fatalf("doctor CLI missing workflow readiness check %q; got ids %#v", id, sortedDoctorCLIKeys(firstIDs))
		}
		if check.OK {
			t.Fatalf("workflow readiness check %q unexpectedly passed: %#v", id, check)
		}
		if check.Details["diagnosticId"] == "" {
			t.Fatalf("workflow readiness check %q missing original diagnostic id details: %#v", id, check)
		}
		if _, ok := secondIDs[id]; !ok {
			t.Fatalf("workflow readiness check id %q was not stable across runs; second ids %#v", id, sortedDoctorCLIKeys(secondIDs))
		}
	}
	if _, ok := firstIDs["workflow-readiness:engineering:issue-triage:readiness-network-checks"]; ok {
		t.Fatalf("doctor CLI should not surface workflow readiness info-only network checks: %#v", firstIDs["workflow-readiness:engineering:issue-triage:readiness-network-checks"])
	}

	for _, secret := range []string{"token-secret-value", "claw-token-secret-value"} {
		if strings.Contains(firstOutput, secret) || strings.Contains(secondOutput, secret) {
			t.Fatalf("doctor CLI output leaked secret value %q", secret)
		}
	}
}

func executeDoctorCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()

	cmd := DoctorCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func saveDoctorTestWorkspace(t *testing.T, rawWorkflow string) {
	t.Helper()

	workspaceRaw := strings.Join([]string{
		"schema_version: v1",
		"name: engineering",
		"repositories:",
		"  - repo: elasticclaw/elasticclaw",
		"    permissions: write",
		"",
	}, "\n")
	hub.SaveWorkspaceForTest(t, &types.WorkspaceConfig{
		SchemaVersion: "v1",
		Name:          "engineering",
		Repositories: []types.GitHubRepoAccess{{
			Repo:        "elasticclaw/elasticclaw",
			Permissions: "write",
		}},
		Files: map[string]string{"elasticclaw-config.yaml": workspaceRaw},
	}, []*types.WorkflowConfig{{
		Name:      "issue-triage",
		RawConfig: rawWorkflow,
	}})
}

func decodeDoctorCLIReport(t *testing.T, raw string) hub.DoctorResponse {
	t.Helper()

	var report hub.DoctorResponse
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatalf("decode doctor CLI JSON: %v\n%s", err, raw)
	}
	return report
}

func doctorCLICheckIDs(checks []hub.DoctorCheck) map[string]hub.DoctorCheck {
	ids := make(map[string]hub.DoctorCheck)
	for _, check := range checks {
		if check.ID != "" {
			ids[check.ID] = check
		}
	}
	return ids
}

func sortedDoctorCLIKeys(values map[string]hub.DoctorCheck) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func readDoctorTestFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func writeDoctorTestFile(t *testing.T, path, data string) string {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}
