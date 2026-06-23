package hub_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/elasticclaw/elasticclaw/pkg/hub/factorytest"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestLinearWorkflowPollCreatesOnceForMissedWebhook(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	linear := factorytest.NewMockLinear(t)

	cfg := &types.HubConfig{
		ClawToken: "test-claw-token",
		Providers: map[string]types.ProviderConfig{
			"noop": {Type: "noop"},
		},
	}
	s, db := hub.NewTestServerWithConfig(t, cfg, "", linear.URL, "")
	saveWorkflowPollWorkspaceFixture(t, "workspace-a", []*types.WorkflowConfig{{
		SchemaVersion: "v1",
		Name:          "linear-workflow",
		Trigger: &types.WorkflowTrigger{
			Linear: &types.LinearWorkflowTrigger{
				Event:  "status_changed",
				Team:   "ELA",
				States: []string{"In Progress"},
			},
		},
		Stages: workflowPollTestStages(),
	}})
	hub.SaveWorkspaceIssueTrackerForTest(t, "workspace-a", "linear", "default", "test-linear-token", "")
	linear.SetIssueStateName("ELA-123", "In Progress")

	s.PollIntegrationsForTest()
	s.PollIntegrationsForTest()

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claws WHERE linear_issue_id='ELA-123'`).Scan(&count); err != nil {
		t.Fatalf("count claws: %v", err)
	}
	if count != 1 {
		t.Fatalf("poll created %d claws for the same Linear issue, want 1", count)
	}
}

func TestShortcutWorkflowPollCreatesOnceForMissedWebhook(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	shortcut := factorytest.NewMockShortcut(t)

	cfg := &types.HubConfig{
		ClawToken: "test-claw-token",
		Providers: map[string]types.ProviderConfig{
			"noop": {Type: "noop"},
		},
	}
	s, db := hub.NewTestServerWithConfig(t, cfg, "", "", shortcut.URL)
	saveWorkflowPollWorkspaceFixture(t, "workspace-a", []*types.WorkflowConfig{{
		SchemaVersion: "v1",
		Name:          "shortcut-workflow",
		Trigger: &types.WorkflowTrigger{
			Shortcut: &types.ShortcutWorkflowTrigger{
				Event:  "status_changed",
				States: []string{"In Progress"},
			},
		},
		Stages: workflowPollTestStages(),
	}})
	hub.SaveWorkspaceIssueTrackerForTest(t, "workspace-a", "shortcut", "default", "test-shortcut-token", "")
	shortcut.SetStory(123, factorytest.StoryState{
		Name:            "Test Story",
		Description:     "Test description",
		WorkflowStateID: shortcut.StateIDForName("In Progress"),
	})

	s.PollIntegrationsForTest()
	s.PollIntegrationsForTest()

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claws WHERE shortcut_story_id='sc-123'`).Scan(&count); err != nil {
		t.Fatalf("count claws: %v", err)
	}
	if count != 1 {
		t.Fatalf("poll created %d claws for the same Shortcut story, want 1", count)
	}
}

func TestLinearWorkflowPollExcludeLabelsRoutesBugWorkflow(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	linear := factorytest.NewMockLinear(t)

	cfg := &types.HubConfig{
		ClawToken: "test-claw-token",
		Providers: map[string]types.ProviderConfig{
			"noop": {Type: "noop"},
		},
	}
	s, db := hub.NewTestServerWithConfig(t, cfg, "", linear.URL, "")
	saveWorkflowPollWorkspaceFixture(t, "workspace-a", []*types.WorkflowConfig{
		{
			SchemaVersion: "v1",
			Name:          "generic-workflow",
			Trigger: &types.WorkflowTrigger{
				Linear: &types.LinearWorkflowTrigger{
					Event:         "status_changed",
					Team:          "ELA",
					States:        []string{"Ready For Agent"},
					ExcludeLabels: []string{"Bug"},
				},
			},
			Stages: workflowPollTestStages(),
		},
		{
			SchemaVersion: "v1",
			Name:          "bug-workflow",
			Trigger: &types.WorkflowTrigger{
				Linear: &types.LinearWorkflowTrigger{
					Event:  "status_changed",
					Team:   "ELA",
					States: []string{"Ready For Agent"},
					Labels: []string{"Bug"},
				},
			},
			Stages: workflowPollTestStages(),
		},
	})
	hub.SaveWorkspaceIssueTrackerForTest(t, "workspace-a", "linear", "default", "test-linear-token", "")
	linear.SetIssueStateName("ELA-123", "Ready For Agent")
	linear.PollingIssues["ELA-123"]["labels"] = map[string]interface{}{"nodes": []map[string]interface{}{{"name": "Bug"}}}

	s.PollIntegrationsForTest()

	assertPollClawTagsContain(t, db, "linear_issue_id", "ELA-123", "workflow:bug-workflow")
}

func TestShortcutWorkflowPollExcludeLabelsRoutesBugWorkflow(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	shortcut := factorytest.NewMockShortcut(t)

	cfg := &types.HubConfig{
		ClawToken: "test-claw-token",
		Providers: map[string]types.ProviderConfig{
			"noop": {Type: "noop"},
		},
	}
	s, db := hub.NewTestServerWithConfig(t, cfg, "", "", shortcut.URL)
	saveWorkflowPollWorkspaceFixture(t, "workspace-a", []*types.WorkflowConfig{
		{
			SchemaVersion: "v1",
			Name:          "generic-workflow",
			Trigger: &types.WorkflowTrigger{
				Shortcut: &types.ShortcutWorkflowTrigger{
					Event:         "status_changed",
					States:        []string{"In Progress"},
					ExcludeLabels: []string{"Bug"},
				},
			},
			Stages: workflowPollTestStages(),
		},
		{
			SchemaVersion: "v1",
			Name:          "bug-workflow",
			Trigger: &types.WorkflowTrigger{
				Shortcut: &types.ShortcutWorkflowTrigger{
					Event:  "status_changed",
					States: []string{"In Progress"},
					Labels: []string{"Bug"},
				},
			},
			Stages: workflowPollTestStages(),
		},
	})
	hub.SaveWorkspaceIssueTrackerForTest(t, "workspace-a", "shortcut", "default", "test-shortcut-token", "")
	shortcut.SetStory(124, factorytest.StoryState{
		Name:            "Bug Story",
		Description:     "Test description",
		WorkflowStateID: shortcut.StateIDForName("In Progress"),
		Labels:          []string{"Bug"},
	})

	s.PollIntegrationsForTest()

	assertPollClawTagsContain(t, db, "shortcut_story_id", "sc-124", "workflow:bug-workflow")
}

func saveWorkflowPollWorkspaceFixture(t *testing.T, workspace string, workflows []*types.WorkflowConfig) {
	t.Helper()
	hub.SaveWorkspaceForTest(t,
		&types.WorkspaceConfig{
			SchemaVersion: "v1",
			Name:          workspace,
			Files: map[string]string{
				"elasticclaw-config.yaml": "schema_version: v1\nname: " + workspace + "\nprovider: noop\n",
				"CONTEXT.md":              "Test context\n",
			},
		},
		workflows,
	)
}

func assertPollClawTagsContain(t *testing.T, db *sql.DB, idColumn, idValue, want string) {
	t.Helper()
	var tags string
	if err := db.QueryRow(`SELECT tags FROM claws WHERE `+idColumn+`=? LIMIT 1`, idValue).Scan(&tags); err != nil {
		t.Fatalf("query claw tags: %v", err)
	}
	if !strings.Contains(tags, want) {
		t.Fatalf("tags for %s = %s, want %q", idValue, tags, want)
	}
}

func workflowPollTestStages() []types.WorkflowStage {
	return []types.WorkflowStage{{
		ID:    "working",
		Label: "Working",
		Entry: true,
		OnEnter: map[string]interface{}{
			"inject": "Read your CONTEXT.md and start working on the issue.\n",
		},
	}}
}
