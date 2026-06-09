package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestCheckLLMKeysRecognizesOllama(t *testing.T) {
	s := &Server{}
	checks := s.checkLLMKeys(&types.HubConfig{
		LLMKeys: types.LLMKeysList{
			{Name: "local-ollama", Provider: "ollama", APIKey: "ollama-local", Default: true},
		},
	})

	for _, check := range checks {
		if strings.Contains(check.Title, "Unknown LLM provider") {
			t.Fatalf("ollama was reported as unknown: %#v", check)
		}
	}
	if len(checks) != 1 || !checks[0].OK {
		t.Fatalf("expected one passing LLM key check, got %#v", checks)
	}
}

func TestCheckLLMKeysAllowsBlankOllamaAPIKey(t *testing.T) {
	s := &Server{}
	checks := s.checkLLMKeys(&types.HubConfig{
		LLMKeys: types.LLMKeysList{
			{Name: "local-ollama", Provider: "ollama", Default: true},
		},
	})

	for _, check := range checks {
		if strings.Contains(check.Title, "has no API key") {
			t.Fatalf("blank ollama key was reported as invalid: %#v", check)
		}
	}
	if len(checks) != 1 || !checks[0].OK {
		t.Fatalf("expected one passing LLM key check, got %#v", checks)
	}
}

func TestDoctorWorkflowReadiness(t *testing.T) {
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
	workflowSetupSaveTestWorkspace(t, []*types.WorkflowConfig{{
		Name:      "issue-triage",
		RawConfig: rawWorkflow,
	}})
	SaveWorkspaceIssueTrackerForTest(t, "engineering", "github-issues", "default", "token-secret-value", "")

	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		Token:     "test-token",
		ClawToken: "claw-token-secret-value",
	}, "", "", "")

	configDir := filepath.Dir(os.Getenv("ELASTICCLAW_HUB_CONFIG"))
	workspacePath := filepath.Join(configDir, "workspaces", "engineering", "elasticclaw-config.yaml")
	workflowPath := workflowSetupSavedWorkflowPath("engineering", "issue-triage")
	trackerPath := workspaceIssueTrackersPath("engineering")
	before := map[string]string{
		workspacePath: readDoctorTestFile(t, workspacePath),
		workflowPath:  readDoctorTestFile(t, workflowPath),
		trackerPath:   readDoctorTestFile(t, trackerPath),
	}

	first := requestDoctorReadinessReport(t, s)
	second := requestDoctorReadinessReport(t, s)

	for path, want := range before {
		if got := readDoctorTestFile(t, path); got != want {
			t.Fatalf("doctor mutated %s\n got:\n%s\nwant:\n%s", path, got, want)
		}
	}

	wantIDs := []string{
		"workflow-readiness:engineering:issue-triage:readiness-provider-missing",
		"workflow-readiness:engineering:issue-triage:readiness-model-missing",
		"workflow-readiness:engineering:issue-triage:readiness-secret-ref-missing",
		"workflow-readiness:engineering:issue-triage:readiness-webhook-secret-missing",
	}
	firstIDs := doctorCheckIDs(first.Checks)
	secondIDs := doctorCheckIDs(second.Checks)
	for _, id := range wantIDs {
		check, ok := firstIDs[id]
		if !ok {
			t.Fatalf("doctor missing workflow readiness check %q; got ids %#v", id, sortedDoctorTestKeys(firstIDs))
		}
		if check.OK {
			t.Fatalf("workflow readiness check %q unexpectedly passed: %#v", id, check)
		}
		if check.Details["diagnosticId"] == "" {
			t.Fatalf("workflow readiness check %q missing original diagnostic id details: %#v", id, check)
		}
		if _, ok := secondIDs[id]; !ok {
			t.Fatalf("workflow readiness check id %q was not stable across runs; second ids %#v", id, sortedDoctorTestKeys(secondIDs))
		}
	}
	if _, ok := firstIDs["workflow-readiness:engineering:issue-triage:readiness-network-checks"]; ok {
		t.Fatalf("doctor should not surface workflow readiness info-only network checks: %#v", firstIDs["workflow-readiness:engineering:issue-triage:readiness-network-checks"])
	}

	data, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal doctor response: %v", err)
	}
	for _, secret := range []string{"token-secret-value", "claw-token-secret-value"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("doctor response leaked secret value %q: %s", secret, data)
		}
	}
}

type doctorReadinessReport struct {
	Checks []doctorReadinessCheck `json:"checks"`
}

type doctorReadinessCheck struct {
	ID       string            `json:"id"`
	Category string            `json:"category"`
	Severity string            `json:"severity"`
	Title    string            `json:"title"`
	OK       bool              `json:"ok"`
	Details  map[string]string `json:"details"`
}

func requestDoctorReadinessReport(t *testing.T, s *Server) doctorReadinessReport {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/doctor?refresh=true", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("doctor status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var report doctorReadinessReport
	if err := json.Unmarshal(rr.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode doctor report: %v", err)
	}
	return report
}

func doctorCheckIDs(checks []doctorReadinessCheck) map[string]doctorReadinessCheck {
	ids := make(map[string]doctorReadinessCheck)
	for _, check := range checks {
		if check.ID != "" {
			ids[check.ID] = check
		}
	}
	return ids
}

func sortedDoctorTestKeys(values map[string]doctorReadinessCheck) []string {
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
