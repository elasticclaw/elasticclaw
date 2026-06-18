package hub_test

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/elasticclaw/elasticclaw/pkg/hub/factorytest"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestShortcutWorkflowWebhookExcludeLabelsRoutesBugWorkflow(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	shortcut := factorytest.NewMockShortcut(t)

	cfg := &types.HubConfig{
		ClawToken: "test-claw-token",
		Integrations: &types.IntegrationsConfig{
			Shortcut: []*types.ShortcutIntegrationConfig{{Workspace: "default", Token: "test-shortcut-token"}},
		},
		Providers: map[string]types.ProviderConfig{
			"noop": {Type: "noop"},
		},
	}
	s, db := hub.NewTestServerWithConfig(t, cfg, "", "", shortcut.URL)
	saveShortcutRoutingWorkflowFixture(t, "workspace-a")
	hub.SaveWorkspaceIssueTrackerForTest(t, "workspace-a", "shortcut", "default", "test-shortcut-token", "")

	shortcut.SetStory(126, factorytest.StoryState{
		Name:            "Bug Story",
		Description:     "Test description",
		WorkflowStateID: shortcut.StateIDForName("In Progress"),
		Labels:          []string{"Bug"},
	})
	payload, sig := shortcut.BuildWebhookPayload(126, shortcut.StateIDForName("Backlog"), shortcut.StateIDForName("In Progress"), "")

	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/workspaces/workspace-a/webhooks/shortcut", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if sig != "" {
		req.Header.Set("Payload-Signature", sig)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	waitForShortcutClawTagsContain(t, db, "sc-126", "workflow:bug-workflow")
}

func saveShortcutRoutingWorkflowFixture(t *testing.T, workspace string) {
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
		[]*types.WorkflowConfig{
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
		},
	)
}

func waitForShortcutClawTagsContain(t *testing.T, db *sql.DB, storyID, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var tags string
		err := db.QueryRow(`SELECT tags FROM claws WHERE shortcut_story_id=? LIMIT 1`, storyID).Scan(&tags)
		if err == nil && strings.Contains(tags, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("shortcut claw tags for %s missing %q, last tags=%q err=%v", storyID, want, tags, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
