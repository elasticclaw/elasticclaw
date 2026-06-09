package hub

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/elasticclaw/elasticclaw/pkg/workflowsetup"
	"gopkg.in/yaml.v3"
)

func TestWorkflowSetupConvertPreviewRequiresPostAndAuth(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	path := "/api/workflow-setup/factories/linear-triage/convert-preview"

	unauth := workflowSetupAPIRequest(s, http.MethodPost, path, map[string]string{"workspace": "engineering"}, false)
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauth.Code, http.StatusUnauthorized)
	}

	wrongMethod := workflowSetupAPIRequest(s, http.MethodGet, path, nil, true)
	if wrongMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want %d", wrongMethod.Code, http.StatusMethodNotAllowed)
	}
}

func TestWorkflowSetupConvertPreviewSupportedFactoryReturnsDisabledYAMLAndDoesNotMutate(t *testing.T) {
	workflowSetupConvertPreviewTestWorkspace(t, map[string]string{
		"BOOTSTRAP.md":     "legacy bootstrap\n",
		"scripts/setup.sh": "#!/bin/sh\necho setup\n",
	})
	workflowSetupConvertPreviewTestFactory(t, &types.FactoryConfig{
		Name:          "linear-triage",
		Integration:   "linear",
		Template:      "engineering",
		Workspace:     "product",
		TriggerStatus: "Ready for Agent",
		Provider:      "daytona",
		PipelineYAML:  workflowSetupConvertPreviewPipeline("move_issue: In Progress"),
	}, map[string]string{
		"BOOTSTRAP.md":     "legacy bootstrap\n",
		"scripts/setup.sh": "#!/bin/sh\necho setup\n",
	})

	factoryYAMLPath := filepath.Join(factoriesDir(), "linear-triage", "factory.yaml")
	workspaceConfigPath := filepath.Join(workspacesDir(), "engineering", "elasticclaw-config.yaml")
	workspaceBootstrapPath := filepath.Join(workspacesDir(), "engineering", "BOOTSTRAP.md")
	factoryBefore := workflowSetupConvertPreviewReadFile(t, factoryYAMLPath)
	workspaceConfigBefore := workflowSetupConvertPreviewReadFile(t, workspaceConfigPath)
	workspaceBootstrapBefore := workflowSetupConvertPreviewReadFile(t, workspaceBootstrapPath)

	s, _ := NewTestServerWithConfig(t, workflowSetupConvertPreviewHubConfig(), "", "", "")
	rr := workflowSetupAPIRequest(s, http.MethodPost, "/api/workflow-setup/factories/linear-triage/convert-preview", map[string]string{
		"workspace": "engineering",
	}, true)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body = %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	resp := workflowSetupConvertPreviewDecode(t, rr.Body.Bytes())
	if resp.Status != workflowsetup.FactoryConvertStatusReady {
		t.Fatalf("status = %q, want ready; diagnostics: %#v", resp.Status, resp.Diagnostics)
	}
	if resp.Summary.Critical != 0 {
		t.Fatalf("critical diagnostics = %d, want 0: %#v", resp.Summary.Critical, resp.Diagnostics)
	}
	if resp.WorkflowName != "linear-triage" {
		t.Fatalf("workflowName = %q, want linear-triage", resp.WorkflowName)
	}
	if strings.TrimSpace(resp.Config) == "" {
		t.Fatalf("config is empty")
	}
	if got, want := resp.ConfigHash, workflowsetup.ConfigHash(resp.Config); got != want {
		t.Fatalf("configHash = %q, want %q", got, want)
	}

	var workflow types.WorkflowConfig
	if err := yaml.Unmarshal([]byte(resp.Config), &workflow); err != nil {
		t.Fatalf("converted workflow did not parse: %v\n%s", err, resp.Config)
	}
	if workflow.Enabled == nil || *workflow.Enabled {
		t.Fatalf("enabled = %#v, want explicit false", workflow.Enabled)
	}

	if _, err := os.Stat(workflowSetupSavedWorkflowPath("engineering", "linear-triage")); !os.IsNotExist(err) {
		t.Fatalf("preview created workflow file err = %v, want not exist", err)
	}
	if got := workflowSetupConvertPreviewReadFile(t, factoryYAMLPath); got != factoryBefore {
		t.Fatalf("factory YAML mutated\n got:\n%s\nwant:\n%s", got, factoryBefore)
	}
	if got := workflowSetupConvertPreviewReadFile(t, workspaceConfigPath); got != workspaceConfigBefore {
		t.Fatalf("workspace config mutated\n got:\n%s\nwant:\n%s", got, workspaceConfigBefore)
	}
	if got := workflowSetupConvertPreviewReadFile(t, workspaceBootstrapPath); got != workspaceBootstrapBefore {
		t.Fatalf("workspace file mutated\n got:\n%s\nwant:\n%s", got, workspaceBootstrapBefore)
	}
	if strings.Contains(rr.Body.String(), "workspace-secret-value") {
		t.Fatalf("preview response contains secret value: %s", rr.Body.String())
	}
}

func TestWorkflowSetupConvertPreviewSupportedFactoryRequiresFileParity(t *testing.T) {
	workflowSetupConvertPreviewTestWorkspace(t, nil)
	workflowSetupConvertPreviewTestFactory(t, &types.FactoryConfig{
		Name:          "linear-triage",
		Integration:   "linear",
		Template:      "engineering",
		Workspace:     "product",
		TriggerStatus: "Ready for Agent",
		Provider:      "daytona",
		PipelineYAML:  workflowSetupConvertPreviewPipeline("move_issue: In Progress"),
	}, map[string]string{
		"BOOTSTRAP.md": "legacy bootstrap\n",
	})

	s, _ := NewTestServerWithConfig(t, workflowSetupConvertPreviewHubConfig(), "", "", "")
	rr := workflowSetupAPIRequest(s, http.MethodPost, "/api/workflow-setup/factories/linear-triage/convert-preview", map[string]string{
		"workspace": "engineering",
	}, true)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body = %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	resp := workflowSetupConvertPreviewDecode(t, rr.Body.Bytes())
	if resp.Status != workflowsetup.FactoryConvertStatusBlocked {
		t.Fatalf("status = %q, want blocked; diagnostics: %#v", resp.Status, resp.Diagnostics)
	}
	workflowSetupConvertPreviewAssertCritical(t, resp, "factory-convert-template-file-missing")
	if strings.TrimSpace(resp.Config) != "" {
		t.Fatalf("config = %q, want empty when file parity is missing", resp.Config)
	}
	if _, err := os.Stat(workflowSetupSavedWorkflowPath("engineering", "linear-triage")); !os.IsNotExist(err) {
		t.Fatalf("preview created workflow file err = %v, want not exist", err)
	}
}

func TestWorkflowSetupConvertPreviewUnsupportedFactoryReturnsCritical(t *testing.T) {
	workflowSetupConvertPreviewTestWorkspace(t, nil)
	workflowSetupConvertPreviewTestFactory(t, &types.FactoryConfig{
		Name:        "external-webhook",
		Integration: "external",
		Template:    "engineering",
		Provider:    "daytona",
		ExternalTrigger: &types.ExternalTrigger{
			Source: "generic-webhook",
		},
		PipelineYAML: workflowSetupConvertPreviewPipeline("inject: start"),
	}, nil)

	s, _ := NewTestServerWithConfig(t, workflowSetupConvertPreviewHubConfig(), "", "", "")
	rr := workflowSetupAPIRequest(s, http.MethodPost, "/api/workflow-setup/factories/external-webhook/convert-preview", map[string]string{
		"workspace": "engineering",
	}, true)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body = %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	resp := workflowSetupConvertPreviewDecode(t, rr.Body.Bytes())
	if resp.Status != workflowsetup.FactoryConvertStatusBlocked {
		t.Fatalf("status = %q, want blocked; diagnostics: %#v", resp.Status, resp.Diagnostics)
	}
	workflowSetupConvertPreviewAssertCritical(t, resp, "factory-convert-unsupported-external")
	if strings.TrimSpace(resp.Config) != "" {
		t.Fatalf("config = %q, want empty for unsupported conversion", resp.Config)
	}
	if _, err := os.Stat(workflowSetupSavedWorkflowPath("engineering", "external-webhook")); !os.IsNotExist(err) {
		t.Fatalf("preview created workflow file err = %v, want not exist", err)
	}
}

func workflowSetupConvertPreviewTestWorkspace(t *testing.T, files map[string]string) {
	t.Helper()
	t.Setenv("ELASTICCLAW_HUB_CONFIG", filepath.Join(t.TempDir(), "hub.yaml"))

	workspaceFiles := map[string]string{
		"elasticclaw-config.yaml": workflowSetupConvertPreviewWorkspaceRaw(),
	}
	for name, content := range files {
		workspaceFiles[name] = content
	}
	SaveWorkspaceForTest(t, &types.WorkspaceConfig{
		SchemaVersion: "v1",
		Name:          "engineering",
		Repositories: []types.GitHubRepoAccess{{
			Repo:        "elasticclaw/elasticclaw",
			Permissions: "write",
		}},
		Secrets: []string{"workspace_secret"},
		Env: types.WorkspaceEnv{
			"API_TOKEN": {Secret: "workspace_secret"},
		},
		Files: workspaceFiles,
	}, nil)
	if err := saveWorkspaceSecret("engineering", "workspace_secret", "workspace-secret-value"); err != nil {
		t.Fatalf("save workspace secret: %v", err)
	}
}

func workflowSetupConvertPreviewTestFactory(t *testing.T, factory *types.FactoryConfig, files map[string]string) {
	t.Helper()
	if err := saveExternalFactory(factory); err != nil {
		t.Fatalf("save factory: %v", err)
	}
	factoryDir := filepath.Join(factoriesDir(), factory.Name)
	for name, content := range files {
		workflowSetupConvertPreviewWriteFile(t, filepath.Join(factoryDir, filepath.FromSlash(name)), content)
	}
}

func workflowSetupConvertPreviewWorkspaceRaw() string {
	return strings.Join([]string{
		"schema_version: v1",
		"name: engineering",
		"repositories:",
		"  - repo: elasticclaw/elasticclaw",
		"    permissions: write",
		"provider: daytona",
		"default_model: anthropic/claude-sonnet-4-6",
		"secrets:",
		"  - workspace_secret",
		"env:",
		"  API_TOKEN:",
		"    secret: workspace_secret",
		"",
	}, "\n")
}

func workflowSetupConvertPreviewPipeline(action string) string {
	return "stages:\n" +
		"  - id: working\n" +
		"    label: Working\n" +
		"    entry: true\n" +
		"    on_enter:\n" +
		"      " + action + "\n"
}

func workflowSetupConvertPreviewHubConfig() *types.HubConfig {
	return &types.HubConfig{
		Token:        "test-token",
		ClawToken:    "claw-token-secret",
		DefaultModel: "anthropic/claude-sonnet-4-6",
		Providers: map[string]types.ProviderConfig{
			"daytona": {APIKey: "daytona-api-key-secret"},
		},
	}
}

func workflowSetupConvertPreviewDecode(t *testing.T, data []byte) workflowsetup.FactoryConvertResponse {
	t.Helper()
	var resp workflowsetup.FactoryConvertResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("decode convert preview response: %v\n%s", err, string(data))
	}
	return resp
}

func workflowSetupConvertPreviewAssertCritical(t *testing.T, resp workflowsetup.FactoryConvertResponse, id string) {
	t.Helper()
	for _, diagnostic := range resp.Diagnostics {
		if diagnostic.ID != id {
			continue
		}
		if diagnostic.Severity != workflowsetup.SeverityCritical || !diagnostic.Blocking {
			t.Fatalf("diagnostic %s = %#v, want blocking critical", id, diagnostic)
		}
		return
	}
	t.Fatalf("missing critical diagnostic %q in %#v", id, resp.Diagnostics)
}

func workflowSetupConvertPreviewReadFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func workflowSetupConvertPreviewWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0640); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
