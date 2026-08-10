//go:build !production

package hub

import (
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestResolveClawEnvIncludesWorkflowAndIntegrationSecrets(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	server, _ := NewTestServerWithConfig(t, &types.HubConfig{
		ClawToken: "claw-token",
		Secrets: map[string]string{
			"shared": "hub-value",
		},
	}, "", "", "")

	workspace := &types.WorkspaceConfig{Name: "env-workspace"}
	workflow := &types.WorkflowConfig{
		Name:        "linear-workflow",
		Integration: "linear",
		SecretRefs:  map[string]string{"WORKFLOW_SECRET": "shared"},
	}
	SaveWorkspaceForTest(t, workspace, []*types.WorkflowConfig{workflow})
	if err := saveWorkspaceSecret(workspace.Name, "shared", "workspace-value"); err != nil {
		t.Fatalf("save workspace secret: %v", err)
	}
	SaveWorkspaceIssueTrackerForTest(t, workspace.Name, "linear", "default", "linear-token", "")

	env, resolvedSecrets, err := server.resolveClawEnv(workspace.Name, workflow, &types.TemplateConfig{
		SecretRefs: map[string]string{"TEMPLATE_SECRET": "shared"},
	}, "claw-id")
	if err != nil {
		t.Fatalf("resolve claw env: %v", err)
	}
	if got := env["WORKFLOW_SECRET"]; got != "workspace-value" {
		t.Fatalf("workflow secret = %q, want workspace-value", got)
	}
	if got := env["TEMPLATE_SECRET"]; got != "workspace-value" {
		t.Fatalf("template secret = %q, want workspace-value", got)
	}
	if got := env["LINEAR_API_KEY"]; got != "linear-token" {
		t.Fatalf("LINEAR_API_KEY = %q, want linear-token", got)
	}
	if _, ok := resolvedSecrets["WORKFLOW_SECRET"]; !ok {
		t.Fatal("workflow secret missing from resolved secrets")
	}
	if _, ok := resolvedSecrets["LINEAR_API_KEY"]; !ok {
		t.Fatal("Linear integration token missing from resolved secrets")
	}
}
