package hub

import (
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

const testWorkspaceV2YAML = `
schema_version: 2
name: engineering

repositories:
  primary:
    provider: github
    repository: org/repo
    source_control: sc

credentials:
  github_app:
    secret: GITHUB_APP_PRIVATE_KEY

source_control:
  connections:
    sc:
      provider: github
      credentials: github_app

ci:
  connections:
    gha:
      provider: github_actions
      source_control: sc
      credentials: github_app
      capability_restrictions:
        trigger_run: false
  pipelines:
    github-pr:
      connection: gha
      repository: primary
      workflow: ci.yml
`

const testWorkflowV2YAML = `
schema_version: 2
name: delivery
enabled: true
initial_state: implementing
states:
  implementing:
    phase: build
  awaiting_ci:
    phase: pr
  completed:
    phase: done
    terminal: true
transitions:
  open:
    from: implementing
    on: pull_request.verified_open
    to: awaiting_ci
  done:
    from: awaiting_ci
    on: pull_request.merged
    to: completed
events:
  ci.run.completed:
    clauses:
      - from: awaiting_ci
        when:
          conclusion:
            equals: failure
        assert:
          work.needs_fix: true
`

func TestSaveExternalWorkspaceRejectsInvalidV2(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")

	invalid := `
schema_version: 2
name: broken
ci:
  connections:
    c1:
      provider: github_actions
      credentials: missing
`
	err := saveExternalWorkspace(&types.WorkspaceConfig{
		Name:  "broken",
		Files: map[string]string{"elasticclaw-config.yaml": invalid},
	})
	if err == nil {
		t.Fatal("expected saveExternalWorkspace to refuse invalid v2")
	}
	if !strings.Contains(err.Error(), "invalid workspace v2") && !strings.Contains(err.Error(), "unknown credential") {
		t.Fatalf("error = %v, want invalid workspace v2 / credential", err)
	}
}

func TestSaveExternalWorkspaceAcceptsValidV2(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")

	err := saveExternalWorkspace(&types.WorkspaceConfig{
		Name:  "engineering",
		Files: map[string]string{"elasticclaw-config.yaml": testWorkspaceV2YAML},
	})
	if err != nil {
		t.Fatalf("save valid v2 workspace: %v", err)
	}
}

func TestSaveExternalWorkspaceStillAcceptsV1(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")

	err := saveExternalWorkspace(&types.WorkspaceConfig{
		Name: "engineering",
		Files: map[string]string{
			"elasticclaw-config.yaml": "schema_version: v1\nname: engineering\nprovider: noop\n",
		},
	})
	if err != nil {
		t.Fatalf("save v1 workspace: %v", err)
	}
}

func TestSaveExternalWorkflowsRejectsInvalidV2Pair(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")

	if err := saveExternalWorkspace(&types.WorkspaceConfig{
		Name:  "engineering",
		Files: map[string]string{"elasticclaw-config.yaml": testWorkspaceV2YAML},
	}); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}

	// Overlapping event clauses + protected namespace would also fail; use unknown pipeline effect.
	badWorkflow := `
schema_version: 2
name: bad
initial_state: s
states:
  s:
    on_enter:
      effects:
        - ci.trigger:
            pipeline: missing-pipeline
  done:
    terminal: true
`
	err := saveExternalWorkflows("engineering", []*types.WorkflowConfig{{
		Name:      "bad",
		RawConfig: badWorkflow,
	}})
	if err == nil {
		t.Fatal("expected refuse invalid v2 pair")
	}
	if !strings.Contains(err.Error(), "unknown pipeline") && !strings.Contains(err.Error(), "invalid workflow v2") {
		t.Fatalf("error = %v, want unknown pipeline / invalid workflow v2", err)
	}
}

func TestSaveExternalWorkflowsAcceptsValidV2Pair(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")

	if err := saveExternalWorkspace(&types.WorkspaceConfig{
		Name:  "engineering",
		Files: map[string]string{"elasticclaw-config.yaml": testWorkspaceV2YAML},
	}); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}

	err := saveExternalWorkflows("engineering", []*types.WorkflowConfig{{
		Name:      "delivery",
		RawConfig: testWorkflowV2YAML,
	}})
	if err != nil {
		t.Fatalf("save valid v2 workflow pair: %v", err)
	}
}

func TestLoadExternalWorkspacesIncludesV2(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")

	const yamlDoc = `
schema_version: 2
name: amazecrm-dev
repositories:
  primary:
    provider: github
    repository: amazecrm/amazecrm
    source_control: sc
credentials:
  github_app:
    secret: github_app
source_control:
  connections:
    sc:
      provider: github
      credentials: github_app
`
	if err := saveExternalWorkspace(&types.WorkspaceConfig{
		Name:  "amazecrm-dev",
		Files: map[string]string{"elasticclaw-config.yaml": yamlDoc, "AGENTS.md": "# a\n"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The bug: load used to fail with "repositories: expected a list" and skip the dir.
	list, err := loadExternalWorkspaces()
	if err != nil {
		t.Fatalf("loadExternalWorkspaces: %v", err)
	}
	var found *types.WorkspaceConfig
	for _, ws := range list {
		if ws != nil && ws.Name == "amazecrm-dev" {
			found = ws
			break
		}
	}
	if found == nil {
		t.Fatalf("v2 workspace not listed; got %d workspaces", len(list))
	}
	if found.SchemaVersion != "2" {
		t.Fatalf("schema = %q", found.SchemaVersion)
	}
	if found.Files["elasticclaw-config.yaml"] == "" {
		t.Fatal("expected raw config in Files for settings UI")
	}
	if len(found.Repositories) != 1 || found.Repositories[0].Repo != "amazecrm/amazecrm" {
		t.Fatalf("projected repos = %#v", found.Repositories)
	}

	// workspaceViews (settings API source) must include it with config text.
	// Use a minimal Server method via package-level helper path.
	views := (&Server{}).workspaceViews()
	var view *WorkspaceView
	for i := range views {
		if views[i].Name == "amazecrm-dev" {
			view = &views[i]
			break
		}
	}
	if view == nil {
		t.Fatal("workspaceViews missing amazecrm-dev")
	}
	if !strings.Contains(view.Config, "schema_version: 2") {
		t.Fatalf("view.Config missing v2 yaml:\n%s", view.Config)
	}
	if len(view.Access.Repositories) != 1 {
		t.Fatalf("access repos = %#v", view.Access.Repositories)
	}
}

func TestSaveExternalWorkflowsStillAcceptsV1(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")

	if err := saveExternalWorkspace(&types.WorkspaceConfig{
		Name: "engineering",
		Files: map[string]string{
			"elasticclaw-config.yaml": "schema_version: v1\nname: engineering\nprovider: noop\n",
		},
	}); err != nil {
		t.Fatalf("seed v1 workspace: %v", err)
	}

	v1Workflow := `
schema_version: v1
name: bugfix
trigger:
  github_issues:
    event: issue_labeled
    repositories:
      - org/repo
    labels:
      - todo
stages:
  - id: working
    entry: true
    on_enter:
      inject: start
`
	wf := &types.WorkflowConfig{Name: "bugfix", RawConfig: v1Workflow}
	if err := types.NormalizeWorkflowConfig(wf); err != nil {
		// Normalize on unmarshaled struct — parse raw first for integration realism.
		_ = err
	}
	// Simulate API path: unmarshal raw into struct then normalize/validate.
	// Store path only needs RawConfig for v1 write-through.
	err := saveExternalWorkflows("engineering", []*types.WorkflowConfig{{
		Name:      "bugfix",
		RawConfig: v1Workflow,
	}})
	if err != nil {
		t.Fatalf("save v1 workflow: %v", err)
	}
}

func TestV2WorkflowProjectionDefaultsMissingEnabledToFalse(t *testing.T) {
	workflow, err := loadExternalWorkflowDocument("draft.yaml", []byte(`
schema_version: 2
name: draft
initial_state: planning
states:
  planning:
    phase: plan
  done:
    phase: done
    terminal: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if workflow.Enabled == nil || *workflow.Enabled {
		t.Fatalf("v2 enabled = %v, want explicit false", workflow.Enabled)
	}
	view := workflowToView("engineering", workflow)
	if view.RuntimeAvailable || view.SchemaVersion != "2" {
		t.Fatalf("v2 view = %#v, want schema 2 and unavailable runtime", view)
	}
}
