package hub

import (
	"fmt"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestCheckLLMKeysRecognizesOllama(t *testing.T) {
	s := &Server{}
	checks := s.checkLLMKeys(&types.HubConfig{
		LLMKeys: types.LLMKeysList{
			{Name: "local-ollama", Provider: "ollama", APIKey: "ollama-local", Default: true},
		},
	})

	for _, check := range checks {
		if strings.Contains(check.Title, "Unknown LLM provider") {
			t.Fatalf("ollama was reported as unknown: %#v", check)
		}
	}
	if len(checks) != 1 || !checks[0].OK {
		t.Fatalf("expected one passing LLM key check, got %#v", checks)
	}
}

func TestCheckLLMKeysAllowsBlankOllamaAPIKey(t *testing.T) {
	s := &Server{}
	checks := s.checkLLMKeys(&types.HubConfig{
		LLMKeys: types.LLMKeysList{
			{Name: "local-ollama", Provider: "ollama", Default: true},
		},
	})

	for _, check := range checks {
		if strings.Contains(check.Title, "has no API key") {
			t.Fatalf("blank ollama key was reported as invalid: %#v", check)
		}
	}
	if len(checks) != 1 || !checks[0].OK {
		t.Fatalf("expected one passing LLM key check, got %#v", checks)
	}
}

// A misconfigured notifier previously produced exactly one log line and
// nothing else: no startup error, no doctor row. The doctor must surface the
// three silent failure modes — unsupported type, invalid structure (bad
// lifecycle.via), and a token secret that does not resolve.
func TestCheckNotificationsSurfacesMisconfiguration(t *testing.T) {
	s := &Server{}

	if checks := s.checkNotifications(&types.HubConfig{}); len(checks) != 0 {
		t.Fatalf("absent notifications config produced %d checks, want none", len(checks))
	}

	// Structural failure: lifecycle.via names a notifier that does not exist.
	enabled := true
	badVia := &types.HubConfig{
		Notifications: &types.NotificationsConfig{
			Notifiers: map[string]types.NotifierConfig{
				"eng-agents": {Type: "slack", Settings: map[string]any{"token_secret": "tok", "channel": "C0123ABCD"}},
			},
			Lifecycle: &types.LifecycleNotificationsConfig{Via: "eng-agent", Enabled: &enabled},
		},
	}
	checks := s.checkNotifications(badVia)
	var routeFailure bool
	for _, check := range checks {
		if !check.OK && strings.Contains(check.Title, "Lifecycle route") && strings.Contains(check.Title, "eng-agent") {
			routeFailure = true
		}
	}
	if !routeFailure {
		t.Fatalf("bad lifecycle.via did not produce a route-specific failure: %#v", checks)
	}

	// Provider-level failures: unknown type, then a secret that does not resolve.
	broken := &types.HubConfig{
		Secrets: map[string]string{"slack_tok": "xoxb-1"},
		Notifications: &types.NotificationsConfig{
			Notifiers: map[string]types.NotifierConfig{
				"pigeons": {Type: "carrier-pigeon", Settings: map[string]any{}},
				"rotated": {Type: "slack", Settings: map[string]any{"token_secret": "gone", "channel": "C0123ABCD"}},
				"healthy": {Type: "slack", Settings: map[string]any{"token_secret": "slack_tok", "channel": "C0123ABCD"}},
			},
		},
	}
	byTitle := map[string]DoctorCheck{}
	for _, c := range s.checkNotifications(broken) {
		byTitle[c.Title] = c
	}
	if len(byTitle) != 3 {
		t.Fatalf("got %d checks, want one per notifier: %#v", len(byTitle), byTitle)
	}
	if c, ok := byTitle[`Notifier "pigeons" has unsupported type "carrier-pigeon"`]; !ok || c.OK || c.Severity != "critical" {
		t.Fatalf("unsupported type not surfaced: %#v", byTitle)
	}
	if c, ok := byTitle[`Notifier "rotated" is misconfigured`]; !ok || c.OK || !strings.Contains(c.Error, "not found") {
		t.Fatalf("unresolvable secret not surfaced: %#v", byTitle)
	}
	if c, ok := byTitle[`Notifier "healthy" is configured`]; !ok || !c.OK {
		t.Fatalf("healthy notifier not reported OK: %#v", byTitle)
	}
}

func TestCheckNotificationsChecksEveryLifecycleRoute(t *testing.T) {
	s := &Server{}
	cfg := &types.HubConfig{Secrets: map[string]string{"token": "xoxb-test"}, Notifications: &types.NotificationsConfig{
		Notifiers: map[string]types.NotifierConfig{
			"healthy": {Type: "slack", Settings: map[string]any{"token_secret": "token", "channel": "C0123ABCD"}},
			"empty":   {Type: "slack", Settings: map[string]any{"token_secret": "token"}},
			"broken":  {Type: "carrier-pigeon", Settings: map[string]any{"channel": "C0123ABCD"}},
		},
		Lifecycle: &types.LifecycleNotificationsConfig{Routes: []types.LifecycleRoute{{Via: "healthy"}, {Via: "empty"}, {Via: "broken"}, {Via: "missing"}}},
	}}
	checks := s.checkNotifications(cfg)
	seen := map[string]bool{}
	for _, check := range checks {
		if strings.Contains(check.Title, "Lifecycle route") && !check.OK {
			seen[check.Title] = true
		}
	}
	for _, want := range []string{"(\"empty\") has no channel", "(\"broken\") is not constructible", "(\"missing\") names an unknown notifier"} {
		found := false
		for title := range seen {
			if strings.Contains(title, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing route check %q in %#v", want, checks)
		}
	}
}

// Scheduled destinations get the same per-destination checks lifecycle routes
// get: a schedule pointed at a channel-less (or non-constructible) notifier
// fails every runtime send permanently and silently burns its slot, so doctor
// must not report it green.
func TestCheckNotificationsChecksEveryScheduledDestination(t *testing.T) {
	s := &Server{}
	cfg := &types.HubConfig{Secrets: map[string]string{"token": "xoxb-test"}, Notifications: &types.NotificationsConfig{
		Notifiers: map[string]types.NotifierConfig{
			"healthy": {Type: "slack", Settings: map[string]any{"token_secret": "token", "channel": "C0123ABCD"}},
			"empty":   {Type: "slack", Settings: map[string]any{"token_secret": "token"}},
			"broken":  {Type: "carrier-pigeon", Settings: map[string]any{"channel": "C0123ABCD"}},
		},
		Scheduled: []types.ScheduledNotificationConfig{
			{ID: "digest", Report: "pending_prs", Via: []string{"healthy", "empty", "broken", "missing"}, At: "09:00"},
		},
	}}
	checks := s.checkNotifications(cfg)
	seen := map[string]bool{}
	for _, check := range checks {
		if strings.Contains(check.Title, "Scheduled report") {
			if check.OK {
				t.Fatalf("schedule with broken destinations reported green: %#v", check)
			}
			seen[check.Title] = true
		}
	}
	for _, want := range []string{`destination "empty" has no channel`, `destination "broken" is not constructible`, "sends to an unknown notifier"} {
		found := false
		for title := range seen {
			if strings.Contains(title, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing scheduled destination check %q in %#v", want, checks)
		}
	}
}

// Regression: the per-route checks ran even while lifecycle alerts were off, so
// a deliberately muted config with a dangling via — a state
// ValidateNotificationsConfig explicitly accepts, "an operator who mutes alerts
// and then deletes the notifier must not be left with a hub that refuses to
// load" — reported a permanent critical.
func TestCheckNotificationsSkipsRoutesWhileAlertsDisabled(t *testing.T) {
	s := &Server{}
	disabled := false
	cfg := &types.HubConfig{Notifications: &types.NotificationsConfig{
		Lifecycle: &types.LifecycleNotificationsConfig{Enabled: &disabled, Via: "old-channel"},
	}}
	for _, check := range s.checkNotifications(cfg) {
		if strings.Contains(check.Title, "Lifecycle route") {
			t.Fatalf("muted lifecycle config produced a route check: %#v", check)
		}
	}
}

// Pipeline notify actions must be validated against the hub-level notifiers
// the runner resolves them from, under the runtime's secret scope: a blank or
// unknown "via" and a notifier that fails to construct (unsupported type,
// unresolvable secret) must all surface as critical; construction verdicts
// dedupe per (notifier, secret scope); and a secret that exists only
// workspace-scoped must count as resolved, because runtime resolution for
// workflow claws prefers workspace secrets over hub secrets.
func TestCheckNotifyActionsSurfacesMisconfiguration(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")

	notifyStage := func(id string, notify map[string]interface{}) types.WorkflowStage {
		return types.WorkflowStage{ID: id, OnEnter: map[string]interface{}{"notify": notify}}
	}
	SaveWorkspaceForTest(t, &types.WorkspaceConfig{Name: "doctor-ws"}, []*types.WorkflowConfig{{
		Name: "release",
		Stages: []types.WorkflowStage{
			notifyStage("no-via", map[string]interface{}{"text": "hello"}),
			notifyStage("unknown-via", map[string]interface{}{"via": "ghost", "text": "hello"}),
			notifyStage("bad-type", map[string]interface{}{"via": "pigeons", "text": "hello"}),
			notifyStage("bad-secret", map[string]interface{}{"via": "rotated", "text": "hello"}),
			notifyStage("bad-secret-again", map[string]interface{}{"via": "rotated", "text": "hello"}),
			notifyStage("workspace-secret", map[string]interface{}{"via": "scoped", "text": "hello"}),
		},
	}})
	if err := saveWorkspaceSecret("doctor-ws", "ws-only-token", "xoxb-ws"); err != nil {
		t.Fatalf("save workspace secret: %v", err)
	}

	cfg := &types.HubConfig{
		Secrets: map[string]string{"slack_tok": "xoxb-1"},
		Notifications: &types.NotificationsConfig{
			Notifiers: map[string]types.NotifierConfig{
				"pigeons": {Type: "carrier-pigeon", Settings: map[string]any{}},
				"rotated": {Type: "slack", Settings: map[string]any{"token_secret": "gone", "channel": "C0123ABCD"}},
				"scoped":  {Type: "slack", Settings: map[string]any{"token_secret": "ws-only-token", "channel": "C0123ABCD"}},
			},
		},
	}
	s := &Server{hubCfg: cfg}
	checks := s.checkNotifyActions(cfg)
	byTitle := map[string]DoctorCheck{}
	for _, c := range checks {
		byTitle[c.Title] = c
	}
	// Four failures: "rotated" is used by two stages but produces one row per
	// (notifier, scope), and the workspace-scoped "scoped" resolves cleanly.
	if len(checks) != 4 || len(byTitle) != 4 {
		t.Fatalf("got %d checks (%d distinct titles), want 4: %#v", len(checks), len(byTitle), checks)
	}
	if c, ok := byTitle[`Workflow "release" stage "no-via" notify action has no "via"`]; !ok || c.OK || c.Severity != "critical" {
		t.Fatalf("blank via not surfaced as critical: %#v", byTitle)
	}
	if c, ok := byTitle[`Workflow "release" stage "unknown-via" references missing notifier "ghost"`]; !ok || c.OK || c.Severity != "critical" {
		t.Fatalf("unknown notifier not surfaced as critical: %#v", byTitle)
	}
	if c, ok := byTitle[`Workflow "release" stage "bad-type" notifier "pigeons" is misconfigured`]; !ok || c.OK || c.Severity != "critical" || !strings.Contains(c.Error, "unknown notifier type") {
		t.Fatalf("unsupported type not surfaced as critical: %#v", byTitle)
	}
	if c, ok := byTitle[`Workflow "release" stage "bad-secret" notifier "rotated" is misconfigured`]; !ok || c.OK || c.Severity != "critical" || !strings.Contains(c.Error, "not found") {
		t.Fatalf("unresolvable secret not surfaced as critical: %#v", byTitle)
	}
}

// The check must cover every surface that reaches executeNotifyAction at
// runtime: factory pipelines (pipeline_yaml on the factory config) and
// workflows that author pipeline_yaml directly instead of stages. Factory
// notify actions must resolve hub secrets only — FactoryConfig.Workspace is a
// tracker slug, not an ElasticClaw workspace — so a secret that exists only
// in a same-named workspace still fails the factory's check.
func TestCheckNotifyActionsCoversFactoriesAndRawPipelineYAML(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")

	const pipelineYAML = "stages:\n  - id: announce\n    on_enter:\n      notify:\n        via: %s\n        text: hi\n"
	SaveWorkspaceForTest(t, &types.WorkspaceConfig{Name: "raw-ws"}, []*types.WorkflowConfig{{
		Name:         "rawflow",
		PipelineYAML: fmt.Sprintf(pipelineYAML, "ghost"),
	}})
	if err := saveWorkspaceSecret("raw-ws", "ws-only-token", "xoxb-ws"); err != nil {
		t.Fatalf("save workspace secret: %v", err)
	}
	cfg := &types.HubConfig{
		Factories: []*types.FactoryConfig{{
			Name:         "rel-factory",
			Workspace:    "raw-ws", // tracker slug colliding with the workspace name
			PipelineYAML: fmt.Sprintf(pipelineYAML, "eng"),
		}},
		Notifications: &types.NotificationsConfig{
			Notifiers: map[string]types.NotifierConfig{
				"eng": {Type: "slack", Settings: map[string]any{"token_secret": "ws-only-token", "channel": "C0123ABCD"}},
			},
		},
	}
	s := &Server{hubCfg: cfg}
	checks := s.checkNotifyActions(cfg)
	byTitle := map[string]DoctorCheck{}
	for _, c := range checks {
		byTitle[c.Title] = c
	}
	if len(checks) != 2 {
		t.Fatalf("got %d checks, want 2: %#v", len(checks), checks)
	}
	if c, ok := byTitle[`Workflow "rawflow" stage "announce" references missing notifier "ghost"`]; !ok || c.OK || c.Severity != "critical" {
		t.Fatalf("directly authored pipeline_yaml not covered: %#v", byTitle)
	}
	if c, ok := byTitle[`Factory "rel-factory" stage "announce" notifier "eng" is misconfigured`]; !ok || c.OK || c.Severity != "critical" || !strings.Contains(c.Error, "not found") {
		t.Fatalf("factory pipeline not covered, or resolved with workspace secrets: %#v", byTitle)
	}
}

// A hub whose every pipeline notify action is valid gets a single passing
// summary check.
func TestCheckNotifyActionsAllValid(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	SaveWorkspaceForTest(t, &types.WorkspaceConfig{Name: "doctor-ws"}, []*types.WorkflowConfig{{
		Name: "release",
		Stages: []types.WorkflowStage{{
			ID:      "announce",
			OnEnter: map[string]interface{}{"notify": map[string]interface{}{"via": "eng", "text": "hi"}},
		}},
	}})
	cfg := &types.HubConfig{
		Secrets: map[string]string{"slack_tok": "xoxb-1"},
		Notifications: &types.NotificationsConfig{
			Notifiers: map[string]types.NotifierConfig{
				"eng": {Type: "slack", Settings: map[string]any{"token_secret": "slack_tok", "channel": "C0123ABCD"}},
			},
		},
	}
	s := &Server{hubCfg: cfg}
	checks := s.checkNotifyActions(cfg)
	if len(checks) != 1 || !checks[0].OK || checks[0].Title != "Pipeline notify actions configured" {
		t.Fatalf("got %#v, want one passing summary check", checks)
	}
}
