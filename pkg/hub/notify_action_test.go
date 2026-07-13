package hub

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestNotifyActionSendsRenderedSlackMessage(t *testing.T) {
	var payload map[string]any
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode Slack payload: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer slack.Close()

	s, db, clawID := newNotifyActionTestServer(t, map[string]types.NotifierConfig{
		"team-slack": {
			Type: "slack",
			Settings: map[string]any{
				"token_secret": "slack-token",
				"channel":      "#agents",
				"api_base":     slack.URL,
			},
		},
	})
	s.persistPipelineOutput(clawID, "build", "result", &pipelineRunResult{
		ExitCode: 0,
		Stdout:   `{"status":"passed","channel":"#releases"}`,
	})

	stage := pipeline.Stage{ID: "announce", OnEnter: pipeline.OnEnter{Notify: pipeline.NotifyAction{
		Enabled: true,
		Via:     "team-slack",
		Text:    "Build {{.Outputs.result.status}}",
		Target:  "{{.Outputs.result.channel}}",
	}}}
	if proceeded := s.runOnEnter(clawID, stage, pipelineContext{}); !proceeded {
		t.Fatal("notify action blocked the stage transition")
	}
	if payload["text"] != "Build passed" {
		t.Fatalf("Slack text = %#v, want %q", payload["text"], "Build passed")
	}
	if payload["channel"] != "#releases" {
		t.Fatalf("Slack channel = %#v, want %q", payload["channel"], "#releases")
	}

	var warningCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND content LIKE '%Notify action failed%'`, clawID).Scan(&warningCount); err != nil {
		t.Fatalf("count warnings: %v", err)
	}
	if warningCount != 0 {
		t.Fatalf("warning count = %d, want 0", warningCount)
	}
}

func TestNotifyActionResolvesWorkspaceSecretFirst(t *testing.T) {
	var authorization string
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer slack.Close()

	s, db, clawID := newNotifyActionTestServer(t, map[string]types.NotifierConfig{
		"team-slack": {
			Type: "slack",
			Settings: map[string]any{
				"token_secret": "slack-token",
				"channel":      "#agents",
				"api_base":     slack.URL,
			},
		},
	})
	// The same secret name exists at hub level ("xoxb-test"); the
	// workspace-scoped value must win, matching workflow_creator resolution.
	if err := saveWorkspaceSecret("notify-workspace", "slack-token", "xoxb-workspace"); err != nil {
		t.Fatalf("save workspace secret: %v", err)
	}

	stage := pipeline.Stage{ID: "announce", OnEnter: pipeline.OnEnter{Notify: pipeline.NotifyAction{
		Enabled: true, Via: "team-slack", Text: "hello",
	}}}
	if proceeded := s.runOnEnter(clawID, stage, pipelineContext{}); !proceeded {
		t.Fatal("notify action blocked the stage transition")
	}
	if authorization != "Bearer xoxb-workspace" {
		t.Fatalf("Authorization = %q, want workspace-scoped token", authorization)
	}
	if contents := notifyActionMessageContents(t, db, clawID); strings.Contains(contents, "Notify action failed") {
		t.Fatalf("unexpected notify warning: %q", contents)
	}
}

func TestNotifyActionResolvesWorkspaceOnlySecret(t *testing.T) {
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer slack.Close()

	s, db, clawID := newNotifyActionTestServer(t, map[string]types.NotifierConfig{
		"team-slack": {
			Type: "slack",
			Settings: map[string]any{
				"token_secret": "ws-only-token",
				"channel":      "#agents",
				"api_base":     slack.URL,
			},
		},
	})
	if err := saveWorkspaceSecret("notify-workspace", "ws-only-token", "xoxb-ws-only"); err != nil {
		t.Fatalf("save workspace secret: %v", err)
	}

	stage := pipeline.Stage{ID: "announce", OnEnter: pipeline.OnEnter{Notify: pipeline.NotifyAction{
		Enabled: true, Via: "team-slack", Text: "hello",
	}}}
	if proceeded := s.runOnEnter(clawID, stage, pipelineContext{}); !proceeded {
		t.Fatal("notify action blocked the stage transition")
	}
	if contents := notifyActionMessageContents(t, db, clawID); strings.Contains(contents, "Notify action failed") {
		t.Fatalf("workspace-scoped secret was not resolved: %q", contents)
	}
}

func TestNotifyActionUsesPipelineContextWorkspace(t *testing.T) {
	var payload map[string]any
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&payload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer slack.Close()

	s, db, _ := newNotifyActionTestServer(t, map[string]types.NotifierConfig{
		"team-slack": {
			Type: "slack",
			Settings: map[string]any{
				"token_secret": "slack-token",
				"channel":      "#agents",
				"api_base":     slack.URL,
			},
		},
	})
	// A claw with no factory/workflow tags: the workspace must come from the
	// pipelineContext handed to runOnEnter, not from re-resolution.
	const clawID = "claw-notify-ctx"
	if _, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, tags, created_at) VALUES(?,?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "ctx-claw", "base", "connected", "", `[]`,
	); err != nil {
		t.Fatalf("insert claw: %v", err)
	}
	workspace, err := loadExternalWorkspace("notify-workspace")
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}

	stage := pipeline.Stage{ID: "announce", OnEnter: pipeline.OnEnter{Notify: pipeline.NotifyAction{
		Enabled: true, Via: "team-slack", Text: "hello",
	}}}
	if proceeded := s.runOnEnter(clawID, stage, pipelineContext{Workspace: workspace}); !proceeded {
		t.Fatal("notify action blocked the stage transition")
	}
	if payload["text"] != "hello" {
		t.Fatalf("Slack text = %#v, want %q", payload["text"], "hello")
	}
	if contents := notifyActionMessageContents(t, db, clawID); strings.Contains(contents, "Notify action failed") {
		t.Fatalf("pipeline context workspace was not used: %q", contents)
	}
}

func TestNotifyActionUnknownViaIsNonBlocking(t *testing.T) {
	s, db, clawID := newNotifyActionTestServer(t, nil)
	stage := pipeline.Stage{ID: "announce", OnEnter: pipeline.OnEnter{
		Inject: "transition continued",
		Notify: pipeline.NotifyAction{Enabled: true, Via: "missing", Text: "hello"},
	}}

	if proceeded := s.runOnEnter(clawID, stage, pipelineContext{}); !proceeded {
		t.Fatal("unknown notifier blocked the stage transition")
	}
	contents := notifyActionMessageContents(t, db, clawID)
	if !strings.Contains(contents, "transition continued") {
		t.Fatalf("messages do not contain subsequent action: %q", contents)
	}
	if !strings.Contains(contents, "[hub] Warning: Notify action failed") || !strings.Contains(contents, `notifier "missing"`) {
		t.Fatalf("messages do not contain clear notify warning: %q", contents)
	}
}

func TestNotifyActionSendFailureIsNonBlocking(t *testing.T) {
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
	}))
	defer slack.Close()

	s, db, clawID := newNotifyActionTestServer(t, map[string]types.NotifierConfig{
		"team-slack": {
			Type: "slack",
			Settings: map[string]any{
				"token_secret": "slack-token",
				"channel":      "#agents",
				"api_base":     slack.URL,
			},
		},
	})
	stage := pipeline.Stage{ID: "announce", OnEnter: pipeline.OnEnter{Notify: pipeline.NotifyAction{
		Enabled: true, Via: "team-slack", Text: "hello",
	}}}

	if proceeded := s.runOnEnter(clawID, stage, pipelineContext{}); !proceeded {
		t.Fatal("Slack send failure blocked the stage transition")
	}
	contents := notifyActionMessageContents(t, db, clawID)
	if !strings.Contains(contents, "[hub] Warning: Notify action failed") || !strings.Contains(contents, "channel_not_found") {
		t.Fatalf("messages do not contain Slack failure warning: %q", contents)
	}
}

func newNotifyActionTestServer(t *testing.T, notifiers map[string]types.NotifierConfig) (*Server, *sql.DB, string) {
	t.Helper()
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	const clawID = "claw-notify-action"
	factory := &types.FactoryConfig{Name: "notify-factory", Workspace: "notify-workspace"}
	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		Token:     "test-token",
		Secrets:   map[string]string{"slack-token": "xoxb-test"},
		Factories: []*types.FactoryConfig{factory},
	}, "", "", "")
	SaveWorkspaceForTest(t, &types.WorkspaceConfig{Name: "notify-workspace", Notifiers: notifiers}, nil)
	if _, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, tags, created_at) VALUES(?,?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test-claw", "base", "connected", "", `["factory:notify-factory"]`,
	); err != nil {
		t.Fatalf("insert claw: %v", err)
	}
	return s, db, clawID
}

func notifyActionMessageContents(t *testing.T, db *sql.DB, clawID string) string {
	t.Helper()
	rows, err := db.Query(`SELECT content FROM messages WHERE claw_id=? ORDER BY created_at`, clawID)
	if err != nil {
		t.Fatalf("query messages: %v", err)
	}
	defer rows.Close()
	var contents []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			t.Fatalf("scan message: %v", err)
		}
		contents = append(contents, content)
	}
	return strings.Join(contents, "\n")
}
