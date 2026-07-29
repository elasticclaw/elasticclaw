package hub

import (
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
	if len(checks) != 1 || checks[0].OK || checks[0].Severity != "critical" {
		t.Fatalf("bad lifecycle.via: got %#v, want one critical failing check", checks)
	}
	if !strings.Contains(checks[0].Error, "eng-agent") {
		t.Fatalf("check error %q does not name the bad via", checks[0].Error)
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
