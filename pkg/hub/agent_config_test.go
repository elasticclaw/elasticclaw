package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func agentTestConfig() *types.HubConfig {
	return &types.HubConfig{Token: "test-token", ClawToken: "test-claw-token", Providers: map[string]types.ProviderConfig{"noop": {Type: "noop"}}, LLMKeys: types.LLMKeysList{
		{Name: "main", Provider: "anthropic", APIKey: "secret-main", Default: true, DefaultModel: "anthropic/main-model"},
		{Name: "worker", Provider: "openai", APIKey: "secret-worker", DefaultModel: "openai/worker-model"},
		{Name: "other-main", Provider: "anthropic", APIKey: "secret-other"},
		{Name: "oauth", Provider: "codex", AuthProfile: "missing"},
	}}
}

func TestAgentConfigResolution(t *testing.T) {
	cfg := agentTestConfig()
	for _, tc := range []struct {
		name    string
		config  types.AgentConfig
		wantErr string
	}{
		{"cross provider", types.AgentConfig{Subagents: &types.SubagentConfig{LLMKey: "worker", MaxConcurrent: 3}}, ""},
		{"inherit", types.AgentConfig{Subagents: &types.SubagentConfig{}}, ""},
		{"missing main key", types.AgentConfig{LLMKey: "missing"}, "unknown LLM"},
		{"missing child key", types.AgentConfig{Subagents: &types.SubagentConfig{LLMKey: "missing"}}, "unknown LLM"},
		{"mismatch", types.AgentConfig{Subagents: &types.SubagentConfig{Model: "openai/model"}}, "does not match"},
		{"colliding credentials", types.AgentConfig{Subagents: &types.SubagentConfig{LLMKey: "other-main"}}, "sharing"},
		{"missing oauth", types.AgentConfig{Subagents: &types.SubagentConfig{LLMKey: "oauth"}}, "unavailable"},
		{"negative concurrency", types.AgentConfig{Subagents: &types.SubagentConfig{MaxConcurrent: -1}}, "max_concurrent"},
		{"excess concurrency", types.AgentConfig{Subagents: &types.SubagentConfig{MaxConcurrent: 33}}, "max_concurrent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolved, err := resolveAgentConfig(cfg, tc.config)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v; want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if resolved.LLMKey != "main" || resolved.DefaultModel != "anthropic/main-model" {
				t.Fatalf("principal = %#v", resolved)
			}
			if tc.name == "cross provider" && (resolved.Subagents.Model != "openai/worker-model" || resolved.Subagents.LLMKey != "worker") {
				t.Fatalf("child = %#v", resolved.Subagents)
			}
			if tc.name == "inherit" && (resolved.Subagents.Model != resolved.DefaultModel || resolved.Subagents.LLMKey != resolved.LLMKey) {
				t.Fatalf("inheritance = %#v", resolved)
			}
		})
	}
	if _, err := resolveAgentConfig(&types.HubConfig{}, types.AgentConfig{DefaultModel: "openai/model"}); err == nil {
		t.Fatal("accepted model without credentials")
	}
}

func TestAgentOptionsDoesNotExposeSecrets(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, agentTestConfig(), "", "", "")
	req := httptest.NewRequest(http.MethodGet, "/api/agent-options", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code == http.StatusOK {
		t.Fatal("unauthenticated request accepted")
	}
	req.Header.Set("Authorization", "Bearer test-token")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("%d: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "secret-") || strings.Contains(rr.Body.String(), "auth_profile") {
		t.Fatal("credential secret exposed")
	}
	if !strings.Contains(rr.Body.String(), `"available":false`) {
		t.Fatal("missing OAuth profile marked available")
	}
}

func TestWorkflowAgentPatchAndSnapshot(t *testing.T) {
	for _, version := range []string{"v1", "v2"} {
		t.Run(version, func(t *testing.T) {
			t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
			t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
			s, db := NewTestServerWithConfig(t, agentTestConfig(), "", "", "")
			workspaceRaw := "schema_version: v1\nname: engineering\nprovider: noop\n"
			workflowRaw := "name: delivery\nenable_manual_trigger: true\n# Preserve this comment.\ncolor: blue\n"
			if version == "v2" {
				workspaceRaw = testWorkspaceV2YAML + "\nexecution:\n  provider: noop\n"
				workflowRaw = testWorkflowV2YAML
			}
			SaveWorkspaceForTest(t, &types.WorkspaceConfig{Name: "engineering", Files: map[string]string{"elasticclaw-config.yaml": workspaceRaw}}, []*types.WorkflowConfig{{Name: "delivery", RawConfig: workflowRaw}})
			request := func(method, path, body string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(method, path, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer test-token")
				rr := httptest.NewRecorder()
				s.Handler().ServeHTTP(rr, req)
				return rr
			}
			path := "/api/workspaces/engineering/workflows/delivery"
			rr := request(http.MethodPatch, path, `{"agents":{"llm_key":"main","subagents":{"llm_key":"worker","max_concurrent":3}}}`)
			if rr.Code != http.StatusOK {
				t.Fatalf("patch %d: %s", rr.Code, rr.Body.String())
			}
			workspace, workflow, ok, err := s.resolveWorkflowConfig("engineering", "delivery")
			if err != nil || !ok {
				t.Fatalf("load: %v", err)
			}
			if workflow.Subagents == nil || workflow.Subagents.LLMKey != "worker" {
				t.Fatalf("settings lost: %#v", workflow)
			}
			if version == "v1" && !strings.Contains(workflow.RawConfig, "Preserve this comment") {
				t.Fatal("unrelated YAML comment lost")
			}
			clawID, _, err := s.createClawFromWorkflow(workspace, workflow, nil, "snapshot test")
			if err != nil {
				t.Fatal(err)
			}
			rr = request(http.MethodPatch, path, `{"agents":{"subagents":{}}}`)
			if rr.Code != http.StatusOK {
				t.Fatalf("reset %d: %s", rr.Code, rr.Body.String())
			}
			snapshot, err := s.loadClawSubagentConfig(clawID)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.LLMKey != "worker" || snapshot.Model != "openai/worker-model" || snapshot.MaxConcurrent != 3 {
				t.Fatalf("snapshot changed: %#v", snapshot)
			}
			var mainKey string
			if err := db.QueryRow(`SELECT llm_key FROM claws WHERE id=?`, clawID).Scan(&mainKey); err != nil || mainKey != "main" {
				t.Fatalf("main snapshot %q: %v", mainKey, err)
			}
			rr = request(http.MethodPost, path+"/trigger", `{"inputs":{},"agents":{"subagents":{"llm_key":"worker","max_concurrent":2}}}`)
			if rr.Code != http.StatusOK {
				t.Fatalf("trigger %d: %s", rr.Code, rr.Body.String())
			}
			var created map[string]string
			if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			child, err := s.loadClawSubagentConfig(created["claw_id"])
			if err != nil || child == nil || child.MaxConcurrent != 2 {
				t.Fatalf("trigger override: %#v %v", child, err)
			}
		})
	}
}
