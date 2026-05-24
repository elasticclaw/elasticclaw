package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestWorkspacesEndpointReturnsPersistedWorkspacesOnly(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		Token: "test-token",
		Factories: []*types.FactoryConfig{
			{Name: "legacy", Integration: "github", Template: "bugbot", EnableManualTrigger: true},
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
	if len(workspaces) != 0 {
		t.Fatalf("expected no workspaces, got %#v", workspaces)
	}
}

func TestWorkflowPushPersistsWorkspaceWorkflows(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("ELASTICCLAW_HUB_CONFIG", configDir+"/hub.yaml")
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	body := `{"workspaces":[{"name":"engineering","repositories":["elasticclaw/elasticclaw"],"secrets":["openai_api_key"]}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	body = `{"workflows":[{"name":"bugfix","integration":"github","enable_manual_trigger":true}]}`
	req = httptest.NewRequest(http.MethodPost, "/api/workspaces/engineering/workflows", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("workflow push status = %d, body = %s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/workspaces/engineering/workflows/bugfix", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var workflow WorkflowView
	if err := json.Unmarshal(rr.Body.Bytes(), &workflow); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if workflow.Name != "bugfix" || workflow.WorkspaceName != "engineering" || workflow.Source != "workflow" {
		t.Fatalf("unexpected workflow: %#v", workflow)
	}

	body = `{"workflows":[{"name":"second","integration":"github","enable_manual_trigger":true}]}`
	req = httptest.NewRequest(http.MethodPost, "/api/workspaces/engineering/workflows", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("second workflow push status = %d, body = %s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/workspaces/engineering/workflows/bugfix", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("existing workflow status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/workspaces/engineering/workflows/second", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("second workflow detail status = %d, body = %s", rr.Code, rr.Body.String())
	}

	body = `{"workflows":[{"name":"Bugfix","integration":"github","enable_manual_trigger":true}]}`
	req = httptest.NewRequest(http.MethodPost, "/api/workspaces/engineering/workflows", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("case-variant workflow push status = %d, body = %s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/workspaces/engineering/workflows", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("workflow list status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var workflows []WorkflowView
	if err := json.Unmarshal(rr.Body.Bytes(), &workflows); err != nil {
		t.Fatalf("decode workflows: %v", err)
	}
	if len(workflows) != 2 {
		t.Fatalf("workflow count = %d, want 2: %#v", len(workflows), workflows)
	}

	staleWorkflowPath := filepath.Join(configDir, "workspaces", "engineering", "workflows", "bugfix.yaml")
	if err := os.WriteFile(staleWorkflowPath, []byte("schema_version: v1\nname: bugfix\njobs:\n  - id: old\n"), 0640); err != nil {
		t.Fatalf("write stale workflow: %v", err)
	}

	body = `{"workflows":[{"name":"bugfix","integration":"github","enable_manual_trigger":true}]}`
	req = httptest.NewRequest(http.MethodPost, "/api/workspaces/engineering/workflows", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("repair workflow push status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

func TestWorkspaceWorkflowTriggerUsesWorkflowRules(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	body := `{"workspaces":[{"name":"engineering"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("push status = %d, body = %s", rr.Code, rr.Body.String())
	}

	body = `{"workflows":[{"name":"disabled","enable_manual_trigger":false}]}`
	req = httptest.NewRequest(http.MethodPost, "/api/workspaces/engineering/workflows", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("workflow push status = %d, body = %s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/workspaces/engineering/workflows/disabled/trigger", strings.NewReader(`{"inputs":{}}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d, body = %s", rr.Code, http.StatusForbidden, rr.Body.String())
	}
}
