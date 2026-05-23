package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestProjectDefaultWorkspaceProjectsFactoriesAsWorkflows(t *testing.T) {
	enabled := true
	workspace := projectDefaultWorkspace(&types.HubConfig{
		Secrets: map[string]string{
			"openai_api_key": "secret",
			"github_token":   "secret",
		},
		Integrations: &types.IntegrationsConfig{
			Linear: []*types.LinearIntegrationConfig{
				{Workspace: "engineering", WebhookSecret: "linear-secret"},
			},
		},
	}, []*types.FactoryConfig{
		{
			Name:                "bugfix",
			Enabled:             &enabled,
			Integration:         "github",
			Workspace:           "repo-events",
			Template:            "bugbot-resolution",
			TriggerRepos:        []string{"elasticclaw/elasticclaw"},
			WebhookSecretRef:    "github_webhook",
			EnableManualTrigger: true,
			SecretRefs:          map[string]string{"OPENAI_API_KEY": "openai_api_key"},
		},
	})

	if workspace.Name != "default" {
		t.Fatalf("workspace.Name = %q, want default", workspace.Name)
	}
	if got, want := workspace.Access.Repositories, []string{"elasticclaw/elasticclaw"}; !stringSlicesEqual(got, want) {
		t.Fatalf("repositories = %#v, want %#v", got, want)
	}
	if got, want := workspace.Access.Secrets, []string{"github_token", "openai_api_key"}; !stringSlicesEqual(got, want) {
		t.Fatalf("secrets = %#v, want %#v", got, want)
	}
	if got, want := workspace.Access.WebhookSecrets, []string{"github_webhook", "linear:engineering"}; !stringSlicesEqual(got, want) {
		t.Fatalf("webhook secrets = %#v, want %#v", got, want)
	}
	if len(workspace.Workflows) != 1 {
		t.Fatalf("len(workflows) = %d, want 1", len(workspace.Workflows))
	}
	workflow := workspace.Workflows[0]
	if workflow.Name != "bugfix" || workflow.LegacyFactoryName != "bugfix" || workflow.Source != "factory" {
		t.Fatalf("unexpected workflow identity: %#v", workflow)
	}
	if workflow.IntegrationWorkspace != "repo-events" {
		t.Fatalf("IntegrationWorkspace = %q, want repo-events", workflow.IntegrationWorkspace)
	}
	if !workflow.EnableManualTrigger {
		t.Fatal("workflow should preserve manual trigger setting")
	}
}

func TestWorkspacesEndpointReturnsCompatibilityWorkspace(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		Token:   "test-token",
		Secrets: map[string]string{"openai_api_key": "secret"},
		Factories: []*types.FactoryConfig{
			{Name: "manual", Integration: "github", Template: "bugbot", EnableManualTrigger: true},
		},
	}, "", "", "")

	req := httptest.NewRequest(http.MethodGet, "/api/workspaces", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var workspaces []WorkspaceView
	if err := json.Unmarshal(rr.Body.Bytes(), &workspaces); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(workspaces) != 1 {
		t.Fatalf("len(workspaces) = %d, want 1", len(workspaces))
	}
	if len(workspaces[0].Workflows) != 1 || workspaces[0].Workflows[0].Name != "manual" {
		t.Fatalf("unexpected workspaces response: %#v", workspaces)
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
