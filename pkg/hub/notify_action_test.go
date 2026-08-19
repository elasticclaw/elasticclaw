package hub

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/notify"
	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

const (
	notifyTestChannel = "C0AGENTS01"
	notifyTestVia     = "team-slack"
)

func notifyTestNotifiers(apiBase string) map[string]types.NotifierConfig {
	return map[string]types.NotifierConfig{
		notifyTestVia: {
			Type: "slack",
			Settings: map[string]any{
				"token_secret":      "slack-token",
				"channel":           notifyTestChannel,
				"api_base":          apiBase,
				"min_send_interval": "1ns",
			},
		},
	}
}

// newNotifyActionTestServer builds a hub with hub-level named notifiers, an
// ElasticClaw workspace on disk, a factory, and a claw tagged to that factory.
// Workflow-shaped sends pass an explicit pipelineContext.Workspace; the
// factory tag exercises the factory (hub-secrets-only) resolution path.
func newNotifyActionTestServer(t *testing.T, notifiers map[string]types.NotifierConfig) (*Server, *sql.DB, string) {
	t.Helper()
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	const clawID = "claw-notify-action"
	// The factory's "workspace" field is deliberately the same string as the
	// ElasticClaw workspace name below: it is a tracker slug
	// (integrations.<type>[].workspace) and must never be treated as a
	// workspace-secrets namespace even when the names collide.
	factory := &types.FactoryConfig{Name: "notify-factory", Workspace: "notify-workspace"}
	cfg := &types.HubConfig{
		Token:     "test-token",
		Secrets:   map[string]string{"slack-token": "xoxb-test"},
		Factories: []*types.FactoryConfig{factory},
	}
	if notifiers != nil {
		cfg.Notifications = &types.NotificationsConfig{Notifiers: notifiers}
	}
	s, db := NewTestServerWithConfig(t, cfg, "", "", "")
	SaveWorkspaceForTest(t, &types.WorkspaceConfig{Name: "notify-workspace"}, nil)
	if _, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, tags, created_at) VALUES(?,?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test-claw", "base", "connected", "", `["factory:notify-factory"]`,
	); err != nil {
		t.Fatalf("insert claw: %v", err)
	}
	return s, db, clawID
}

// notifyWorkspaceContext is the pipeline context of a workflow claw in the
// test workspace — the only claw shape whose notify actions resolve
// workspace-scoped secrets.
func notifyWorkspaceContext() pipelineContext {
	return pipelineContext{Workspace: &types.WorkspaceConfig{Name: "notify-workspace"}}
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

// waitForNotify polls cond until it holds; notify action sends run in a
// goroutine off the stage-transition path, so tests must wait for delivery.
func waitForNotify(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

func waitForSlackRequests(t *testing.T, fake *fakeSlackServer, want int) {
	t.Helper()
	waitForNotify(t, fmt.Sprintf("%d Slack request(s)", want), func() bool { return fake.count() >= want })
	if got := fake.count(); got != want {
		t.Fatalf("Slack received %d requests, want %d", got, want)
	}
}

func TestNotifyActionSendsRenderedSlackMessage(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db, clawID := newNotifyActionTestServer(t, notifyTestNotifiers(fake.server.URL))
	s.persistPipelineOutput(clawID, "build", "result", &pipelineRunResult{
		ExitCode: 0,
		Stdout:   `{"status":"passed","channel":"C0RELEASE1"}`,
	})

	stage := pipeline.Stage{ID: "announce", OnEnter: pipeline.OnEnter{Notify: pipeline.NotifyAction{
		Enabled: true,
		Via:     notifyTestVia,
		Text:    "Build {{.Outputs.result.status}}",
		Target:  "{{.Outputs.result.channel}}",
	}}}
	if _, err := s.runOnEnter(clawID, stage, pipelineContext{}); err != nil {
		t.Fatalf("notify action blocked the stage transition: %v", err)
	}
	waitForSlackRequests(t, fake, 1)
	req := fake.request(0)
	if req.Text != "" {
		t.Fatalf("semantic Slack message has top-level text %q, want empty", req.Text)
	}
	if req.Fallback != "Build passed" {
		t.Fatalf("Slack fallback = %q, want %q", req.Fallback, "Build passed")
	}
	if req.Color != notify.SlackColorInfo || len(req.Blocks) == 0 {
		t.Fatalf("plain notify action did not use semantic Slack rendering: color=%q blocks=%d", req.Color, len(req.Blocks))
	}
	if blocks := fmt.Sprintf("%v", req.Blocks); !strings.Contains(blocks, "Build passed") {
		t.Fatalf("blocks %q do not preserve rendered operator text", blocks)
	}
	if req.Channel != "C0RELEASE1" {
		t.Fatalf("Slack channel = %q, want templated target %q", req.Channel, "C0RELEASE1")
	}
	if contents := notifyActionMessageContents(t, db, clawID); strings.Contains(contents, "Notify action failed") {
		t.Fatalf("unexpected notify warning: %q", contents)
	}
}

// Text, subject and target all render the same template variables as inject:
// {{.Issue.*}} from the pipeline context and {{.Inputs.*}} from a manual
// trigger. A message with a subject opts into Slack's rich (attachment)
// rendering, so the subject is asserted inside the blocks.
// severity: picks the colour stripe; a value the save-time check never lets
// through still sends, as info, with a warning.
func TestNotifyActionMapsSeverity(t *testing.T) {
	for severity, want := range map[string]string{
		"warning": notify.SlackColorWarning,
		"error":   notify.SlackColorError,
		"success": notify.SlackColorSuccess,
		"urgent":  notify.SlackColorInfo,
	} {
		t.Run(severity, func(t *testing.T) {
			fake := newFakeSlackServer(t)
			s, _, clawID := newNotifyActionTestServer(t, notifyTestNotifiers(fake.server.URL))
			stage := pipeline.Stage{ID: "announce", OnEnter: pipeline.OnEnter{Notify: pipeline.NotifyAction{
				Enabled:  true,
				Via:      notifyTestVia,
				Text:     "Build failed",
				Severity: severity,
			}}}
			if _, err := s.runOnEnter(clawID, stage, pipelineContext{}); err != nil {
				t.Fatalf("notify action blocked the stage transition: %v", err)
			}
			waitForSlackRequests(t, fake, 1)
			if got := fake.request(0).Color; got != want {
				t.Fatalf("severity %q colour = %q, want %q", severity, got, want)
			}
		})
	}
}

func TestNotifyActionRendersIssueAndInputTemplates(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db, _ := newNotifyActionTestServer(t, notifyTestNotifiers(fake.server.URL))

	const clawID = "claw-notify-templates"
	if _, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, tags, template_files, created_at) VALUES(?,?,?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "template-claw", "base", "connected", "",
		`["manual-trigger"]`, `{"TRIGGER_INPUTS.json":"{\"requester\":\"ana\"}"}`,
	); err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	stage := pipeline.Stage{ID: "announce", OnEnter: pipeline.OnEnter{Notify: pipeline.NotifyAction{
		Enabled: true,
		Via:     notifyTestVia,
		Text:    "Merged {{.Issue.URL}}",
		Subject: "{{.Issue.Identifier}} requested by {{.Inputs.requester}}",
	}}}
	// No GitHub token is configured, so the fallback issue details render
	// without any network fetch.
	if _, err := s.runOnEnter(clawID, stage, pipelineContext{IssueID: "octo/app/7"}); err != nil {
		t.Fatalf("notify action blocked the stage transition: %v", err)
	}
	waitForSlackRequests(t, fake, 1)
	req := fake.request(0)
	// The fallback leads with the rendered text and appends the issue
	// identifier as summary context, matching the lifecycle cards' slot
	// discipline.
	if want := "Merged https://github.com/octo/app/issues/7 — octo/app/7"; req.Fallback != want {
		t.Fatalf("fallback = %q, want %q", req.Fallback, want)
	}
	blocks := fmt.Sprintf("%v", req.Blocks)
	if !strings.Contains(blocks, "#7 requested by ana") {
		t.Fatalf("blocks %q do not contain the rendered subject", blocks)
	}
	if contents := notifyActionMessageContents(t, db, clawID); strings.Contains(contents, "Notify action failed") {
		t.Fatalf("unexpected notify warning: %q", contents)
	}
}

// Regression guard for mrkdwn injection: untrusted data (issue titles,
// command stdout) templated into the notify text must reach Slack with the
// mrkdwn control characters escaped, so a crafted value cannot broadcast
// <!channel> or spoof a labelled link in the destination channel.
func TestNotifyActionEscapesUntrustedTemplateData(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, _, clawID := newNotifyActionTestServer(t, notifyTestNotifiers(fake.server.URL))
	s.persistPipelineOutput(clawID, "build", "result", &pipelineRunResult{
		ExitCode: 0,
		Stdout:   `{"status":"<!channel> <https://evil.example|Approve> & done"}`,
	})

	stage := pipeline.Stage{ID: "announce", OnEnter: pipeline.OnEnter{Notify: pipeline.NotifyAction{
		Enabled: true,
		Via:     notifyTestVia,
		Text:    "Build {{.Outputs.result.status}}",
	}}}
	if _, err := s.runOnEnter(clawID, stage, pipelineContext{}); err != nil {
		t.Fatalf("notify action blocked the stage transition: %v", err)
	}
	waitForSlackRequests(t, fake, 1)
	// The notify action now renders through the semantic tier, so the
	// escaped text lands in the fallback and blocks, not the top-level
	// "text" field (which the semantic tier always omits).
	want := "Build &lt;!channel&gt; &lt;https://evil.example|Approve&gt; &amp; done"
	req := fake.request(0)
	if req.Fallback != want {
		t.Fatalf("Slack fallback = %q, want escaped %q", req.Fallback, want)
	}
	if blocks := fmt.Sprintf("%v", req.Blocks); !strings.Contains(blocks, want) {
		t.Fatalf("blocks %q do not contain escaped text %q", blocks, want)
	}
}

func TestNotifyActionResolvesWorkspaceSecretFirst(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db, clawID := newNotifyActionTestServer(t, notifyTestNotifiers(fake.server.URL))
	// The same secret name exists at hub level ("xoxb-test"); the
	// workspace-scoped value must win for a workflow claw, matching
	// workflow_creator resolution.
	if err := saveWorkspaceSecret("notify-workspace", "slack-token", "xoxb-workspace"); err != nil {
		t.Fatalf("save workspace secret: %v", err)
	}

	stage := pipeline.Stage{ID: "announce", OnEnter: pipeline.OnEnter{Notify: pipeline.NotifyAction{
		Enabled: true, Via: notifyTestVia, Text: "hello",
	}}}
	if _, err := s.runOnEnter(clawID, stage, notifyWorkspaceContext()); err != nil {
		t.Fatalf("notify action blocked the stage transition: %v", err)
	}
	waitForSlackRequests(t, fake, 1)
	if auth := fake.request(0).Auth; auth != "Bearer xoxb-workspace" {
		t.Fatalf("Authorization = %q, want workspace-scoped token", auth)
	}
	if contents := notifyActionMessageContents(t, db, clawID); strings.Contains(contents, "Notify action failed") {
		t.Fatalf("unexpected notify warning: %q", contents)
	}
}

// Regression guard: FactoryConfig.Workspace is the integration/tracker
// workspace slug (integrations.<type>[].workspace), NOT an ElasticClaw
// workspace name. A factory claw's notify action must resolve hub secrets
// only — even when an ElasticClaw workspace with the exact same name exists
// and overrides the notifier's secret, that unrelated workspace's token must
// never be picked up.
func TestNotifyActionFactoryTrackerWorkspaceDoesNotScopeSecrets(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db, clawID := newNotifyActionTestServer(t, notifyTestNotifiers(fake.server.URL))
	// "notify-workspace" is both the factory's tracker slug and an existing
	// ElasticClaw workspace (see newNotifyActionTestServer); give the
	// workspace an overriding secret that must NOT be used.
	if err := saveWorkspaceSecret("notify-workspace", "slack-token", "xoxb-workspace"); err != nil {
		t.Fatalf("save workspace secret: %v", err)
	}

	stage := pipeline.Stage{ID: "announce", OnEnter: pipeline.OnEnter{Notify: pipeline.NotifyAction{
		Enabled: true, Via: notifyTestVia, Text: "hello",
	}}}
	// Empty context: the claw's factory tag resolves the factory context.
	if _, err := s.runOnEnter(clawID, stage, pipelineContext{}); err != nil {
		t.Fatalf("notify action blocked the stage transition: %v", err)
	}
	waitForSlackRequests(t, fake, 1)
	if auth := fake.request(0).Auth; auth != "Bearer xoxb-test" {
		t.Fatalf("Authorization = %q, want the hub token — the factory's tracker slug leaked into workspace secret resolution", auth)
	}
	if contents := notifyActionMessageContents(t, db, clawID); strings.Contains(contents, "Notify action failed") {
		t.Fatalf("unexpected notify warning: %q", contents)
	}
}

// Regression guard for the notifierFor cache: a workspace that overrides one
// of the notifier's secrets gets its own cache entry, so the hub-scoped
// instance used by the lifecycle notifier keeps sending with the hub token.
func TestNotifyActionWorkspaceSecretCannotPoisonHubNotifier(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, _, clawID := newNotifyActionTestServer(t, notifyTestNotifiers(fake.server.URL))
	if err := saveWorkspaceSecret("notify-workspace", "slack-token", "xoxb-workspace"); err != nil {
		t.Fatalf("save workspace secret: %v", err)
	}

	stage := pipeline.Stage{ID: "announce", OnEnter: pipeline.OnEnter{Notify: pipeline.NotifyAction{
		Enabled: true, Via: notifyTestVia, Text: "workspace send",
	}}}
	if _, err := s.runOnEnter(clawID, stage, notifyWorkspaceContext()); err != nil {
		t.Fatalf("notify action blocked the stage transition: %v", err)
	}
	waitForSlackRequests(t, fake, 1)

	// The lifecycle path resolves the same notifier name with hub secrets.
	nc := s.notificationsConfig().Notifiers[notifyTestVia]
	lifecycleNotifier, err := s.notifierFor(notifyTestVia, nc, s.hubSecretResolver())
	if err != nil {
		t.Fatalf("notifierFor: %v", err)
	}
	if _, err := lifecycleNotifier.Send(t.Context(), notify.Message{Text: "lifecycle send"}); err != nil {
		t.Fatalf("lifecycle send: %v", err)
	}

	waitForSlackRequests(t, fake, 2)
	if auth := fake.request(0).Auth; auth != "Bearer xoxb-workspace" {
		t.Fatalf("pipeline Authorization = %q, want workspace token", auth)
	}
	if auth := fake.request(1).Auth; auth != "Bearer xoxb-test" {
		t.Fatalf("lifecycle Authorization = %q, want hub token — workspace secret poisoned the hub-scoped notifier", auth)
	}
}

// When the workspace overrides none of the notifier's secrets, the pipeline
// action must reuse the exact instance cached for the lifecycle notifier
// (and, through it, the shared pacing).
func TestNotifyActionSharesLifecycleNotifierInstance(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, _, clawID := newNotifyActionTestServer(t, notifyTestNotifiers(fake.server.URL))

	nc := s.notificationsConfig().Notifiers[notifyTestVia]
	lifecycleNotifier, err := s.notifierFor(notifyTestVia, nc, s.hubSecretResolver())
	if err != nil {
		t.Fatalf("notifierFor: %v", err)
	}
	pipelineNotifier, err := s.notifierForPipeline(clawID, notifyTestVia, nc, notifyWorkspaceContext())
	if err != nil {
		t.Fatalf("notifierForPipeline: %v", err)
	}
	if pipelineNotifier != lifecycleNotifier {
		t.Fatal("pipeline notify action built its own notifier instead of sharing the cached lifecycle instance")
	}
}

func TestNotifyActionUnknownViaIsNonBlocking(t *testing.T) {
	s, db, clawID := newNotifyActionTestServer(t, nil)
	stage := pipeline.Stage{ID: "announce", OnEnter: pipeline.OnEnter{
		Inject: "transition continued",
		Notify: pipeline.NotifyAction{Enabled: true, Via: "missing", Text: "hello"},
	}}

	if _, err := s.runOnEnter(clawID, stage, pipelineContext{}); err != nil {
		t.Fatalf("unknown notifier blocked the stage transition: %v", err)
	}
	contents := notifyActionMessageContents(t, db, clawID)
	if !strings.Contains(contents, "transition continued") {
		t.Fatalf("messages do not contain the inject action output: %q", contents)
	}
	if !strings.Contains(contents, "[hub] Warning: Notify action failed") || !strings.Contains(contents, `notifier "missing"`) {
		t.Fatalf("messages do not contain a clear notify warning: %q", contents)
	}
}

// A notify block with a blank "via" is deliberately not a pipeline parse
// error (one notification typo must never disable the pipeline); the runtime
// must warn loudly in the claw conversation and continue the transition.
func TestNotifyActionBlankViaWarnsAndContinues(t *testing.T) {
	s, db, clawID := newNotifyActionTestServer(t, nil)
	stage := pipeline.Stage{ID: "announce", OnEnter: pipeline.OnEnter{
		Inject: "transition continued",
		Notify: pipeline.NotifyAction{Enabled: true, Via: "   ", Text: "hello"},
	}}

	if _, err := s.runOnEnter(clawID, stage, pipelineContext{}); err != nil {
		t.Fatalf("blank via blocked the stage transition: %v", err)
	}
	contents := notifyActionMessageContents(t, db, clawID)
	if !strings.Contains(contents, "transition continued") {
		t.Fatalf("messages do not contain the inject action output: %q", contents)
	}
	if !strings.Contains(contents, "[hub] Warning: Notify action failed") || !strings.Contains(contents, "no via") {
		t.Fatalf("messages do not contain a clear blank-via warning: %q", contents)
	}
}

func TestNotifyActionSendFailureIsNonBlocking(t *testing.T) {
	fake := newFakeSlackServer(t)
	fake.setRespond(func(_ int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
	})
	s, db, clawID := newNotifyActionTestServer(t, notifyTestNotifiers(fake.server.URL))
	stage := pipeline.Stage{ID: "announce", OnEnter: pipeline.OnEnter{Notify: pipeline.NotifyAction{
		Enabled: true, Via: notifyTestVia, Text: "hello",
	}}}

	if _, err := s.runOnEnter(clawID, stage, pipelineContext{}); err != nil {
		t.Fatalf("Slack send failure blocked the stage transition: %v", err)
	}
	waitForNotify(t, "the Slack failure warning", func() bool {
		contents := notifyActionMessageContents(t, db, clawID)
		return strings.Contains(contents, "[hub] Warning: Notify action failed") && strings.Contains(contents, "channel_not_found")
	})
}

// Regression guard: the network send must not run on the stage-transition
// path. Transitions fire inline from the PR watcher's sequential poll loop
// and the process-wide Slack limiter paces sends per channel, so a
// synchronous send would stall the whole tick behind one slow channel.
// runOnEnter must return while the Slack call is still in flight.
func TestNotifyActionSendDoesNotBlockStageTransition(t *testing.T) {
	fake := newFakeSlackServer(t)
	release := make(chan struct{})
	fake.setRespond(func(_ int, w http.ResponseWriter) {
		<-release
		fmt.Fprint(w, `{"ok":true,"ts":"1000.000001"}`)
	})
	s, db, clawID := newNotifyActionTestServer(t, notifyTestNotifiers(fake.server.URL))
	stage := pipeline.Stage{ID: "announce", OnEnter: pipeline.OnEnter{Notify: pipeline.NotifyAction{
		Enabled: true, Via: notifyTestVia, Text: "hello",
	}}}

	start := time.Now()
	if _, err := s.runOnEnter(clawID, stage, pipelineContext{}); err != nil {
		t.Fatalf("notify action blocked the stage transition: %v", err)
	}
	// A synchronous send would sit here for the full notifyActionTimeout.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("runOnEnter took %s waiting on the Slack send; the send must be asynchronous", elapsed)
	}
	close(release)
	waitForSlackRequests(t, fake, 1)
	if contents := notifyActionMessageContents(t, db, clawID); strings.Contains(contents, "Notify action failed") {
		t.Fatalf("unexpected notify warning: %q", contents)
	}
}
