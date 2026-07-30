package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestRoutinePreflightReportsReadyConfiguration(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	s, _ := NewTestServerWithConfig(t, readyRoutineHubConfig(), "", "", "")
	workspace, workflow := readyRoutineFixture()
	SaveWorkspaceForTest(t, workspace, []*types.WorkflowConfig{workflow})

	result := s.preflightRoutine(workspace, workflow)
	if !result.Ready || result.Status != "ready" {
		t.Fatalf("preflight = %#v, want ready", result)
	}
	for _, check := range result.Checks {
		if check.Status == "error" {
			t.Fatalf("unexpected blocker: %#v", check)
		}
	}
}

func TestRoutinePreflightReportsConfigurationBlockers(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")
	workspace := &types.WorkspaceConfig{
		Name:         "engineering",
		Repositories: types.RepositoryAccessList{{Repo: "elasticclaw/elasticclaw", Permissions: "write"}},
		Files: map[string]string{
			"elasticclaw-config.yaml": "provider: daytona\nllm_key: missing\nsecret_refs:\n  DEPLOY_TOKEN: deploy-token\n",
		},
	}
	workflow := routineWorkflowForTest("dependency-report", false)

	result := s.preflightRoutine(workspace, workflow)
	if result.Ready || result.Status != "needs_setup" {
		t.Fatalf("preflight = %#v, want needs_setup", result)
	}
	for _, id := range []string{"sandbox-provider", "model", "secrets", "github"} {
		if !hasRoutinePreflightError(result.Checks, id) {
			t.Fatalf("missing %q blocker in %#v", id, result.Checks)
		}
	}
}

func TestRoutinePreflightEndpointAndEnableGuard(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	workspace := &types.WorkspaceConfig{Name: "engineering"}
	workflow := routineWorkflowForTest("dependency-report", false)
	SaveWorkspaceForTest(t, workspace, []*types.WorkflowConfig{workflow})

	req := httptest.NewRequest(http.MethodGet, "/api/workspaces/engineering/workflows/dependency-report/preflight", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("preflight status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var result RoutinePreflightResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode preflight: %v", err)
	}
	if result.Ready {
		t.Fatalf("preflight unexpectedly ready: %#v", result)
	}

	req = httptest.NewRequest(http.MethodPatch, "/api/workspaces/engineering/workflows/dependency-report", strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("enable status = %d, want 409, body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "routine is not ready") {
		t.Fatalf("enable response does not explain blocker: %s", rr.Body.String())
	}
}

func readyRoutineHubConfig() *types.HubConfig {
	return &types.HubConfig{
		Token:        "test-token",
		DefaultModel: "codex/gpt-5.5",
		Providers: map[string]types.ProviderConfig{
			"docker": {Type: "docker"},
		},
		LLMKeys: types.LLMKeysList{{
			Name:        "codex-chatgpt",
			Provider:    "codex",
			Default:     true,
			AuthProfile: "codex-user",
		}},
		ModelAuthProfiles: []*types.ModelAuthProfileConfig{{
			Name: "codex-user", Provider: "codex", AuthState: `{"auth":"configured"}`,
		}},
		GitHubApps: []*types.GitHubAppConfig{{
			AppID: 42, PrivateKeyPEM: "configured-for-preflight",
		}},
	}
}

func readyRoutineFixture() (*types.WorkspaceConfig, *types.WorkflowConfig) {
	workspace := &types.WorkspaceConfig{
		Name:         "engineering",
		Repositories: types.RepositoryAccessList{{Repo: "elasticclaw/elasticclaw", Permissions: "read"}},
		Files: map[string]string{
			"elasticclaw-config.yaml": "provider: docker\nllm_key: codex-chatgpt\n",
		},
	}
	return workspace, routineWorkflowForTest("dependency-report", false)
}

func routineWorkflowForTest(name string, enabled bool) *types.WorkflowConfig {
	return &types.WorkflowConfig{
		SchemaVersion: "v1",
		Name:          name,
		Enabled:       &enabled,
		Trigger: &types.WorkflowTrigger{Cron: &types.CronWorkflowTrigger{
			Schedule: "*/15 * * * *", Timezone: "UTC", OverlapPolicy: "skip", Timeout: "10m",
		}},
		Stages: []types.WorkflowStage{{
			ID: "working", Entry: true, OnEnter: map[string]interface{}{"inject": "List the latest commits."},
		}},
	}
}

func hasRoutinePreflightError(checks []RoutinePreflightCheck, id string) bool {
	for _, check := range checks {
		if check.ID == id && check.Status == "error" {
			return true
		}
	}
	return false
}
