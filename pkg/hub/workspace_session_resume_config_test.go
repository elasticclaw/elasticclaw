//go:build !production

package hub

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestWorkflowCreatorWorkspaceSessionResumeConfig(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", filepath.Join(t.TempDir(), "hub.yaml"))
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	server, db := NewTestServerWithConfig(t, &types.HubConfig{
		ClawToken:    "claw-token",
		DefaultModel: "hub-model",
		Providers:    map[string]types.ProviderConfig{"noop": {Type: "noop"}},
		Secrets:      map[string]string{"workspace-key": "secret-value"},
	}, "", "", "")
	workspace := &types.WorkspaceConfig{
		Name: "resume-workspace",
		SessionResume: &types.SessionResumeConfig{
			ReadFiles: []string{"NOTES.md"}, StateCheck: "Read current task status",
		},
		Env: types.WorkspaceEnv{
			"ENVIRONMENT":   {Value: "development"},
			"WORKSPACE_KEY": {Secret: "workspace-key"},
		},
		Files: map[string]string{"elasticclaw-config.yaml": "provider: noop\ndefault_model: template-model\nsecret_refs:\n  TEMPLATE_KEY: workspace-key\n"},
	}
	data, err := marshalWorkspaceElasticClawConfig(workspace, workspace.Files["elasticclaw-config.yaml"])
	if err != nil {
		t.Fatal(err)
	}
	workspace.Files["elasticclaw-config.yaml"] = string(data)
	workflow := &types.WorkflowConfig{Name: "resume-workflow"}
	SaveWorkspaceForTest(t, workspace, []*types.WorkflowConfig{workflow})
	storedWorkspace, err := loadExternalWorkspace(workspace.Name)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ParseTemplateConfig([]byte(storedWorkspace.Files["elasticclaw-config.yaml"]))
	if err != nil {
		t.Fatalf("parse saved workspace config: %v", err)
	}
	if !reflect.DeepEqual(cfg.SessionResume, workspace.SessionResume) {
		t.Fatalf("session resume = %#v, want %#v", cfg.SessionResume, workspace.SessionResume)
	}
	// The filesystem reader also uses strict decoding of the same file.
	loadedCfg, err := config.LoadTemplateConfig(filepath.Join(workspacesDir(), workspace.Name))
	if err != nil {
		t.Fatalf("load saved workspace config: %v", err)
	}
	if !reflect.DeepEqual(loadedCfg, cfg) {
		t.Fatalf("filesystem and byte readers disagree: %#v / %#v", loadedCfg, cfg)
	}
	clawID, _, err := server.createClawFromWorkflow(storedWorkspace, workflow, nil, "test recovery config")
	if err != nil {
		t.Fatalf("create claw from saved workspace: %v", err)
	}
	waitForWorkflowV2TestClaw(t, db, clawID)
	var provider, model string
	if err := db.QueryRow(`SELECT provider, default_model FROM claws WHERE id=?`, clawID).Scan(&provider, &model); err != nil {
		t.Fatal(err)
	}
	if provider != "noop" || model != "template-model" {
		t.Fatalf("provider/model = %q/%q, want noop/template-model", provider, model)
	}
	provision, err := server.loadStoredClawProvision(clawID)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"ENVIRONMENT": "development", "WORKSPACE_KEY": "secret-value", "TEMPLATE_KEY": "secret-value"} {
		if got := provision.env[key]; got != want {
			t.Errorf("environment %s = %q, want %q", key, got, want)
		}
	}
}
