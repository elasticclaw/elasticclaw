package hub

import (
	"bytes"
	"encoding/json"
	"fmt"
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

func TestAgentConfigRejectsCredentialWithoutDefaultModel(t *testing.T) {
	cfg := &types.HubConfig{LLMKeys: types.LLMKeysList{{Name: "custom", Provider: "custom-provider", APIKey: "fixture", Default: true}}}
	_, err := resolveAgentConfig(cfg, types.AgentConfig{LLMKey: "custom"})
	if err == nil || !strings.Contains(err.Error(), "no default model configured for credential") {
		t.Fatalf("error = %v; want missing default model", err)
	}
	cfg.DefaultModel = "custom-provider/hub-model"
	resolved, err := resolveAgentConfig(cfg, types.AgentConfig{LLMKey: "custom"})
	if err != nil || resolved.DefaultModel != "custom-provider/hub-model" {
		t.Fatalf("resolved = %#v, %v", resolved, err)
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

func TestAgentOverridesRequireAdmin(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	cfg := agentTestConfig()
	cfg.Auth = &types.AuthConfig{SessionSecret: "agent-auth-secret", Access: &types.AccessConfig{Admins: []string{"alice"}}}
	s, _ := NewTestServerWithConfig(t, cfg, "", "", "")
	SaveWorkspaceForTest(t, &types.WorkspaceConfig{Name: "engineering", Files: map[string]string{"elasticclaw-config.yaml": "schema_version: v1\nname: engineering\nprovider: noop\n"}}, []*types.WorkflowConfig{{Name: "delivery", EnableManualTrigger: true}})
	session := func(login string) string {
		token, err := signGitHubSession("agent-auth-secret", login, "", "")
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	request := func(method, path, body, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}
	trigger := "/api/workspaces/engineering/workflows/delivery/trigger"
	override := `{"inputs":{},"agents":{"llm_key":"worker"}}`
	if rr := request(http.MethodPost, trigger, override, session("bob")); rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "administrator") {
		t.Fatalf("non-admin override %d: %s", rr.Code, rr.Body.String())
	}
	if rr := request(http.MethodPost, trigger, `{"inputs":{}}`, session("bob")); rr.Code != http.StatusOK {
		t.Fatalf("non-admin trigger %d: %s", rr.Code, rr.Body.String())
	}
	for _, token := range []string{session("alice"), "test-token"} {
		if rr := request(http.MethodPost, trigger, override, token); rr.Code != http.StatusOK {
			t.Fatalf("admin override %d: %s", rr.Code, rr.Body.String())
		}
	}
	// Direct claw creation keeps accepting top-level llm_key/default_model;
	// only its subagents block is an administrator action.
	createClaw := func(name, token, extra string) *httptest.ResponseRecorder {
		return request(http.MethodPost, "/api/claws", `{"name":"`+name+`","provider":"noop",`+extra+`}`, token)
	}
	if rr := createClaw("direct-sub", session("bob"), `"subagents":{"llm_key":"worker"}`); rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "administrator") {
		t.Fatalf("non-admin create with subagents %d: %s", rr.Code, rr.Body.String())
	}
	if rr := createClaw("direct-key", session("bob"), `"llm_key":"worker","default_model":"openai/worker-model"`); rr.Code != http.StatusAccepted {
		t.Fatalf("non-admin create with llm_key %d: %s", rr.Code, rr.Body.String())
	}
	for i, token := range []string{session("alice"), "test-token"} {
		if rr := createClaw(fmt.Sprintf("direct-admin-%d", i), token, `"subagents":{"llm_key":"worker"}`); rr.Code != http.StatusAccepted {
			t.Fatalf("admin create with subagents %d: %s", rr.Code, rr.Body.String())
		}
	}
	if rr := request(http.MethodGet, "/api/agent-options", "", session("bob")); rr.Code != http.StatusForbidden {
		t.Fatalf("non-admin options %d: %s", rr.Code, rr.Body.String())
	}
	if rr := request(http.MethodGet, "/api/agent-options", "", session("alice")); rr.Code != http.StatusOK {
		t.Fatalf("admin options %d: %s", rr.Code, rr.Body.String())
	}
}

func TestWorkflowPushValidatesAgents(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	s, _ := NewTestServerWithConfig(t, agentTestConfig(), "", "", "")
	SaveWorkspaceForTest(t, &types.WorkspaceConfig{Name: "legacy", Files: map[string]string{"elasticclaw-config.yaml": "schema_version: v1\nname: legacy\nprovider: noop\n"}}, nil)
	SaveWorkspaceForTest(t, &types.WorkspaceConfig{Name: "engineering", Files: map[string]string{"elasticclaw-config.yaml": testWorkspaceV2YAML + "\nexecution:\n  provider: noop\n"}}, nil)
	push := func(workspace string, workflow *types.WorkflowConfig) *httptest.ResponseRecorder {
		body, err := json.Marshal(WorkflowPushRequest{Workflows: []*types.WorkflowConfig{workflow}})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+workspace+"/workflows", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-token")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}
	v1 := func(agents types.AgentConfig) *types.WorkflowConfig {
		return &types.WorkflowConfig{Name: "delivery", Integration: "github", EnableManualTrigger: true, DefaultModel: agents.DefaultModel, LLMKey: agents.LLMKey, Subagents: agents.Subagents}
	}
	v2 := func(agentsYAML string) *types.WorkflowConfig {
		return &types.WorkflowConfig{Name: "delivery", SchemaVersion: "2", RawConfig: testWorkflowV2YAML + agentsYAML}
	}
	for _, tc := range []struct {
		name      string
		workspace string
		workflow  *types.WorkflowConfig
		wantCode  int
		wantBody  string
	}{
		{"v1 max_concurrent", "legacy", v1(types.AgentConfig{Subagents: &types.SubagentConfig{MaxConcurrent: 64}}), http.StatusBadRequest, "max_concurrent"},
		{"v1 unknown key", "legacy", v1(types.AgentConfig{LLMKey: "missing"}), http.StatusBadRequest, "unknown LLM credential"},
		{"v1 valid", "legacy", v1(types.AgentConfig{LLMKey: "main", Subagents: &types.SubagentConfig{LLMKey: "worker", MaxConcurrent: 3}}), http.StatusOK, ""},
		{"v2 max_concurrent", "engineering", v2("subagents:\n  max_concurrent: 64\n"), http.StatusBadRequest, "max_concurrent"},
		{"v2 unknown key", "engineering", v2("llm_key: missing\n"), http.StatusBadRequest, "unknown LLM credential"},
		{"v2 valid", "engineering", v2("llm_key: main\nsubagents:\n  llm_key: worker\n  max_concurrent: 3\n"), http.StatusOK, ""},
	} {
		rr := push(tc.workspace, tc.workflow)
		if rr.Code != tc.wantCode || !strings.Contains(rr.Body.String(), tc.wantBody) {
			t.Fatalf("%s: %d: %s", tc.name, rr.Code, rr.Body.String())
		}
	}
	_, workflow, ok, err := s.resolveWorkflowConfig("engineering", "delivery")
	if err != nil || !ok || workflow.Subagents == nil || workflow.Subagents.MaxConcurrent != 3 {
		t.Fatalf("stored v2 agents: %#v %v", workflow, err)
	}
}

func TestWorkflowAgentPatchOmitsEmptyKeys(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	s, _ := NewTestServerWithConfig(t, agentTestConfig(), "", "", "")
	SaveWorkspaceForTest(t, &types.WorkspaceConfig{Name: "engineering", Files: map[string]string{"elasticclaw-config.yaml": testWorkspaceV2YAML + "\nexecution:\n  provider: noop\n"}}, []*types.WorkflowConfig{{Name: "delivery", RawConfig: testWorkflowV2YAML}})
	patch := func(body string) string {
		req := httptest.NewRequest(http.MethodPatch, "/api/workspaces/engineering/workflows/delivery", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-token")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("patch %s %d: %s", body, rr.Code, rr.Body.String())
		}
		_, workflow, ok, err := s.resolveWorkflowConfig("engineering", "delivery")
		if err != nil || !ok {
			t.Fatalf("load: %v", err)
		}
		return workflow.RawConfig
	}
	agentKeys := []string{"default_model", "llm_key", "subagents"}
	raw := patch(`{"agents":{}}`)
	for _, key := range agentKeys {
		if strings.Contains(raw, key+":") {
			t.Fatalf("empty patch wrote %s:\n%s", key, raw)
		}
	}
	raw = patch(`{"agents":{"llm_key":"main","subagents":{"max_concurrent":2}}}`)
	if !strings.Contains(raw, "llm_key: main") || !strings.Contains(raw, "max_concurrent: 2") {
		t.Fatalf("authored settings missing:\n%s", raw)
	}
	raw = patch(`{"agents":{}}`)
	for _, key := range agentKeys {
		if strings.Contains(raw, key+":") {
			t.Fatalf("clearing left %s:\n%s", key, raw)
		}
	}
	if !strings.Contains(raw, "initial_state: implementing") {
		t.Fatalf("v2 body lost:\n%s", raw)
	}
}

func TestWorkflowAgentPatchWithoutCredentials(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token", Providers: map[string]types.ProviderConfig{"noop": {Type: "noop"}}}, "", "", "")
	SaveWorkspaceForTest(t, &types.WorkspaceConfig{Name: "engineering", Files: map[string]string{"elasticclaw-config.yaml": "schema_version: v1\nname: engineering\nprovider: noop\n"}}, []*types.WorkflowConfig{{Name: "delivery"}})
	for body, want := range map[string]int{`{"agents":{}}`: http.StatusOK, `{"agents":{"llm_key":"main"}}`: http.StatusBadRequest} {
		req := httptest.NewRequest(http.MethodPatch, "/api/workspaces/engineering/workflows/delivery", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-token")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != want {
			t.Fatalf("patch %s %d, want %d: %s", body, rr.Code, want, rr.Body.String())
		}
	}
}

func TestAgentConfigSameCredentialInheritsPrincipalModel(t *testing.T) {
	for _, provider := range []string{"anthropic", "codex"} {
		for _, name := range []string{"", "main"} {
			t.Run(provider+"/"+name, func(t *testing.T) {
				cfg := &types.HubConfig{LLMKeys: types.LLMKeysList{{Name: "main", Provider: provider, APIKey: "fixture", Default: true, DefaultModel: "default"}}}
				model := normalizeModelForProvider(provider, "custom")
				got, err := resolveAgentConfig(cfg, types.AgentConfig{DefaultModel: model, LLMKey: "main", Subagents: &types.SubagentConfig{LLMKey: name}})
				if err != nil {
					t.Fatal(err)
				}
				if got.Subagents.Model != model || got.Subagents.LLMKey != "main" {
					t.Fatalf("child did not inherit principal: %+v", got.Subagents)
				}
			})
		}
	}
}
