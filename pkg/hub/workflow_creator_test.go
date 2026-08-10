//go:build !production

package hub

import (
	"encoding/json"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestResolveClawEnvIncludesWorkflowAndIntegrationSecrets(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	server, _ := NewTestServerWithConfig(t, &types.HubConfig{
		ClawToken: "claw-token",
		Secrets: map[string]string{
			"template-ref": "template-value",
		},
	}, "", "", "")

	for _, tc := range []struct {
		name, integration, tokenEnv string
	}{
		{name: "linear", integration: "linear", tokenEnv: "LINEAR_API_KEY"},
		{name: "jira", integration: "jira", tokenEnv: "JIRA_API_KEY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := &types.WorkspaceConfig{Name: "env-workspace-" + tc.name}
			workflow := &types.WorkflowConfig{
				Name:        tc.name + "-workflow",
				Integration: tc.integration,
				SecretRefs:  map[string]string{"WORKFLOW_SECRET": "workflow-ref"},
			}
			SaveWorkspaceForTest(t, workspace, []*types.WorkflowConfig{workflow})
			if err := saveWorkspaceSecret(workspace.Name, "workflow-ref", "workflow-value"); err != nil {
				t.Fatalf("save workspace secret: %v", err)
			}
			SaveWorkspaceIssueTrackerForTest(t, workspace.Name, tc.integration, "default", tc.name+"-token", "")

			env, resolvedSecrets, err := server.resolveClawEnv(workspace.Name, workflow, &types.TemplateConfig{
				SecretRefs: map[string]string{"TEMPLATE_SECRET": "template-ref"},
			}, "claw-id")
			if err != nil {
				t.Fatalf("resolve claw env: %v", err)
			}
			if got := env["WORKFLOW_SECRET"]; got != "workflow-value" {
				t.Fatalf("workflow secret = %q, want workflow-value", got)
			}
			if got := env["TEMPLATE_SECRET"]; got != "template-value" {
				t.Fatalf("template secret = %q, want template-value", got)
			}
			if got := env[tc.tokenEnv]; got != tc.name+"-token" {
				t.Fatalf("%s = %q, want %s-token", tc.tokenEnv, got, tc.name)
			}
			for _, key := range []string{"WORKFLOW_SECRET", "TEMPLATE_SECRET", tc.tokenEnv} {
				if _, ok := resolvedSecrets[key]; !ok {
					t.Fatalf("%s missing from resolved secrets", key)
				}
			}
		})
	}
}

func TestLoadStoredClawProvisionIncludesWorkflowAndIntegrationSecrets(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	server, db := NewTestServerWithConfig(t, &types.HubConfig{
		ClawToken: "claw-token",
		Secrets:   map[string]string{"template-ref": "template-value"},
	}, "", "", "")

	workspace := &types.WorkspaceConfig{Name: "restore-workspace"}
	workflow := &types.WorkflowConfig{
		Name:        "restore-workflow",
		Integration: "linear",
		SecretRefs:  map[string]string{"WORKFLOW_SECRET": "workflow-ref"},
	}
	SaveWorkspaceForTest(t, workspace, []*types.WorkflowConfig{workflow})
	if err := saveWorkspaceSecret(workspace.Name, "workflow-ref", "workflow-value"); err != nil {
		t.Fatalf("save workspace secret: %v", err)
	}
	SaveWorkspaceIssueTrackerForTest(t, workspace.Name, "linear", "default", "linear-token", "")

	templateFiles, err := json.Marshal(map[string]string{
		"elasticclaw-config.yaml": "secret_refs:\n  TEMPLATE_SECRET: template-ref\n",
	})
	if err != nil {
		t.Fatalf("marshal template files: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, template, provider, default_model, template_files, github_repos, linear_workspace, nix, docker, llm_key, tags, status, created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,datetime('now'))`,
		"restore-claw", "test-tenant-id", "restore claw", workspace.Name, "noop", "", string(templateFiles), "[]", "", 0, 0, "", `["workspace:restore-workspace","workflow:restore-workflow"]`, "provisioning"); err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	stored, err := server.loadStoredClawProvision("restore-claw")
	if err != nil {
		t.Fatalf("load stored claw provision: %v", err)
	}
	for key, want := range map[string]string{
		"TEMPLATE_SECRET": "template-value",
		"WORKFLOW_SECRET": "workflow-value",
		"LINEAR_API_KEY":  "linear-token",
	} {
		if got := stored.env[key]; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
		if _, ok := stored.resolvedSecrets[key]; !ok {
			t.Errorf("%s missing from resolved secrets", key)
		}
	}
}

// Malformed template_files must fail the restore rather than silently
// reprovisioning a claw with no template files at all. A stored "null" is not
// malformed: it is what a claw with no template files marshals to.
func TestLoadStoredClawProvisionRejectsMalformedTemplateFiles(t *testing.T) {
	for _, tc := range []struct {
		name          string
		templateFiles string
		wantErr       bool
	}{
		{name: "malformed", templateFiles: `{"broken`, wantErr: true},
		{name: "null is empty not malformed", templateFiles: `null`},
		{name: "empty object", templateFiles: `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
			server, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
			if _, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, template, provider, default_model, template_files, github_repos, linear_workspace, nix, docker, llm_key, tags, status, created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,datetime('now'))`,
				"restore-claw", "test-tenant-id", "restore claw", "restore-workspace", "noop", "", tc.templateFiles, "[]", "", 0, 0, "", `[]`, "provisioning"); err != nil {
				t.Fatalf("insert claw: %v", err)
			}

			stored, err := server.loadStoredClawProvision("restore-claw")
			if tc.wantErr {
				if err == nil {
					t.Fatal("loadStoredClawProvision succeeded, want error for malformed template_files")
				}
				return
			}
			if err != nil {
				t.Fatalf("load stored claw provision: %v", err)
			}
			if stored.templateFiles == nil {
				t.Fatal("templateFiles is nil; writing SECRETS.md into it would panic")
			}
		})
	}
}
