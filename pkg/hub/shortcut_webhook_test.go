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

func TestShortcutWorkflowWebhookTerminatesOnLeave(t *testing.T) {
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
	saveShortcutTerminateWorkflowFixture(t, "workspace-a")
	hub.SaveWorkspaceIssueTrackerForTest(t, "workspace-a", "shortcut", "default", "test-shortcut-token", "")

	shortcut.SetStory(127, factorytest.StoryState{
		Name:            "Leaving Story",
		Description:     "Test description",
		WorkflowStateID: shortcut.StateIDForName("Backlog"),
	})
	var tenantID string
	if err := db.QueryRow(`SELECT id FROM tenants LIMIT 1`).Scan(&tenantID); err != nil {
		t.Fatalf("read tenant: %v", err)
	}
	_, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, template, status, shortcut_story_id, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		"claw-shortcut-leave", tenantID, "Shortcut claw", "elasticclaw", "connected", "sc-127")
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	payload, sig := shortcut.BuildWebhookPayload(127, shortcut.StateIDForName("In Progress"), shortcut.StateIDForName("Backlog"), "")

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

	waitForShortcutClawStatus(t, db, "sc-127", "deleted")
}

func TestShortcutWorkflowWebhookTerminateOnLeaveHonorsExcludeLabels(t *testing.T) {
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
	saveShortcutTerminateExcludedWorkflowFixture(t, "workspace-a")
	hub.SaveWorkspaceIssueTrackerForTest(t, "workspace-a", "shortcut", "default", "test-shortcut-token", "")

	shortcut.SetStory(128, factorytest.StoryState{
		Name:            "Bug Leaving Story",
		Description:     "Test description",
		WorkflowStateID: shortcut.StateIDForName("Backlog"),
		Labels:          []string{"Bug"},
	})
	var tenantID string
	if err := db.QueryRow(`SELECT id FROM tenants LIMIT 1`).Scan(&tenantID); err != nil {
		t.Fatalf("read tenant: %v", err)
	}
	_, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, template, status, shortcut_story_id, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		"claw-shortcut-bug-leave", tenantID, "Shortcut bug claw", "elasticclaw", "connected", "sc-128")
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	payload, sig := shortcut.BuildWebhookPayload(128, shortcut.StateIDForName("In Progress"), shortcut.StateIDForName("Backlog"), "")

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

	time.Sleep(100 * time.Millisecond)
	assertShortcutClawStatus(t, db, "sc-128", "connected")
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

func saveShortcutTerminateWorkflowFixture(t *testing.T, workspace string) {
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
		[]*types.WorkflowConfig{{
			SchemaVersion:    "v1",
			Name:             "terminating-workflow",
			Integration:      "shortcut",
			TriggerStatus:    "In Progress",
			TerminateOnLeave: true,
			Stages:           workflowPollTestStages(),
		}},
	)
}

func saveShortcutTerminateExcludedWorkflowFixture(t *testing.T, workspace string) {
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
		[]*types.WorkflowConfig{{
			SchemaVersion:    "v1",
			Name:             "generic-workflow",
			Integration:      "shortcut",
			TriggerStatus:    "In Progress",
			ExcludeLabels:    []string{"Bug"},
			TerminateOnLeave: true,
			Stages:           workflowPollTestStages(),
		}},
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

func waitForShortcutClawStatus(t *testing.T, db *sql.DB, storyID, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var status string
		err := db.QueryRow(`SELECT status FROM claws WHERE shortcut_story_id=? LIMIT 1`, storyID).Scan(&status)
		if err == nil && status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("shortcut claw status for %s = %q, want %q err=%v", storyID, status, want, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertShortcutClawStatus(t *testing.T, db *sql.DB, storyID, want string) {
	t.Helper()
	var status string
	if err := db.QueryRow(`SELECT status FROM claws WHERE shortcut_story_id=? LIMIT 1`, storyID).Scan(&status); err != nil {
		t.Fatalf("read shortcut claw status for %s: %v", storyID, err)
	}
	if status != want {
		t.Fatalf("shortcut claw status for %s = %q, want %q", storyID, status, want)
	}
}
