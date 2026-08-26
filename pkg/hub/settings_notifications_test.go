package hub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestSettingsGetNotificationsIsRedacted(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", filepath.Join(t.TempDir(), "hub.yaml"))
	trueValue := true
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		Secrets: map[string]string{"slack_token": "the-secret-value"},
		Notifications: &types.NotificationsConfig{
			Notifiers: map[string]types.NotifierConfig{
				"ops": {Type: "slack", Settings: map[string]any{"channel": "C123", "token_secret": "slack_token", "unexpected_secret": "must-not-leak"}},
			},
			Lifecycle: &types.LifecycleNotificationsConfig{Via: "ops", Events: &types.LifecycleEventToggles{AgentStarted: &trueValue}},
		},
	}, "", "", "")

	rr := httptest.NewRecorder()
	s.getSettings(rr, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET status = %d: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "the-secret-value") || strings.Contains(rr.Body.String(), "must-not-leak") {
		t.Fatalf("GET leaked a secret value: %s", rr.Body.String())
	}
	var view SettingsView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if got := view.Notifications.Notifiers["ops"]; got.TokenSecret != "slack_token" || got.Channel != "C123" {
		t.Fatalf("notifier view = %#v", got)
	}
	if view.Notifications.Lifecycle.Via != "ops" || !view.Notifications.Lifecycle.Enabled {
		t.Fatalf("lifecycle view = %#v", view.Notifications.Lifecycle)
	}
	if !reflect.DeepEqual(view.LifecycleEventTypes, types.LifecycleEventTypes) {
		t.Fatalf("lifecycle event types = %v, want %v", view.LifecycleEventTypes, types.LifecycleEventTypes)
	}
}

func TestSettingsPatchNotificationsPersistsRoutesAndClearsLegacyVia(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.yaml")
	t.Setenv("ELASTICCLAW_HUB_CONFIG", path)
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Notifications: &types.NotificationsConfig{
		Notifiers: map[string]types.NotifierConfig{"old": {Type: "slack"}},
		Lifecycle: &types.LifecycleNotificationsConfig{Via: "old"},
	}}, "", "", "")
	if err := config.SaveHubConfig(s.hubCfg); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"notifications":{"notifiers":{"ops":{"type":"slack","channel":"C123","token_secret":"slack_token","api_base":"https://slack.example","min_send_interval":"2s"}},"lifecycle":{"enabled":true,"via":"old","routes":[{"via":"ops","events":["agent_started"]}],"idleAfter":"5m","events":{"agentStarted":true}}}}`)
	rr := httptest.NewRecorder()
	s.patchSettings(rr, httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d: %s", rr.Code, rr.Body.String())
	}
	if got := s.hubCfg.Notifications.Lifecycle.Via; got != "" {
		t.Fatalf("legacy via = %q, want cleared", got)
	}
	diskCfg, err := config.LoadHubConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := diskCfg.Notifications.Lifecycle.Routes; !reflect.DeepEqual(got, []types.LifecycleRoute{{Via: "ops", Events: []string{"agent_started"}}}) {
		t.Fatalf("disk routes = %#v", got)
	}
	if diskCfg.Notifications.Lifecycle.Via != "" {
		t.Fatalf("disk legacy via = %q, want cleared", diskCfg.Notifications.Lifecycle.Via)
	}
}

// Regression: LifecycleNotificationsView always emits `routes` (as `[]` for a
// legacy via-only config), and the via→routes migration was gated on the
// decoded slice being non-nil. A client that GETs the view and PATCHes it back
// verbatim therefore destroyed `via` and left the hub with no channel binding
// at all — or, with alerts enabled, could not save ANY notifications change.
func TestSettingsPatchLegacyViaOnlyViewRoundTripKeepsVia(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "enabled"
		if !enabled {
			name = "paused"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hub.yaml")
			t.Setenv("ELASTICCLAW_HUB_CONFIG", path)
			s, _ := NewTestServerWithConfig(t, &types.HubConfig{Notifications: &types.NotificationsConfig{
				Notifiers: map[string]types.NotifierConfig{"eng": {Type: "slack", Settings: map[string]any{"channel": "C123", "token_secret": "slack_token"}}},
				Lifecycle: &types.LifecycleNotificationsConfig{Enabled: &enabled, Via: "eng"},
			}}, "", "", "")
			if err := config.SaveHubConfig(s.hubCfg); err != nil {
				t.Fatal(err)
			}

			rr := httptest.NewRecorder()
			s.getSettings(rr, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
			var view SettingsView
			if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			// Byte-for-byte what the view returned, PATCHed straight back.
			body, err := json.Marshal(map[string]any{"notifications": view.Notifications})
			if err != nil {
				t.Fatal(err)
			}
			rr = httptest.NewRecorder()
			s.patchSettings(rr, httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewReader(body)))
			if rr.Code != http.StatusOK {
				t.Fatalf("round-tripping the view rejected the save: %d: %s", rr.Code, rr.Body.String())
			}
			diskCfg, err := config.LoadHubConfig()
			if err != nil {
				t.Fatal(err)
			}
			if got := diskCfg.Notifications.Lifecycle.Via; got != "eng" {
				t.Fatalf("disk via = %q, want it preserved (routes = %#v)", got, diskCfg.Notifications.Lifecycle.Routes)
			}
		})
	}
}

// Regression: the settings view models only five notifier settings keys, so a
// patch rebuilt from it used to erase everything else configured under
// notifications.notifiers.<name> the first time the screen was saved.
func TestSettingsPatchNotificationsKeepsUnmodelledNotifierSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.yaml")
	t.Setenv("ELASTICCLAW_HUB_CONFIG", path)
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Notifications: &types.NotificationsConfig{
		Notifiers: map[string]types.NotifierConfig{"ops": {Type: "slack", Settings: map[string]any{
			"channel": "C123", "token_secret": "slack_token", "unexpected_secret": "keep-me",
		}}},
		Lifecycle: &types.LifecycleNotificationsConfig{Via: "ops"},
	}}, "", "", "")
	if err := config.SaveHubConfig(s.hubCfg); err != nil {
		t.Fatal(err)
	}

	// Exactly what the Notifier screen sends: the projected keys only.
	body := []byte(`{"notifications":{"notifiers":{"ops":{"type":"slack","channel":"C999","token_secret":"slack_token"}},"lifecycle":{"enabled":true,"routes":[{"via":"ops","events":[]}]}}}`)
	rr := httptest.NewRecorder()
	s.patchSettings(rr, httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d: %s", rr.Code, rr.Body.String())
	}
	diskCfg, err := config.LoadHubConfig()
	if err != nil {
		t.Fatal(err)
	}
	settings := diskCfg.Notifications.Notifiers["ops"].Settings
	if got := settings["unexpected_secret"]; got != "keep-me" {
		t.Fatalf("unmodelled setting = %v, want it preserved", got)
	}
	if got := settings["channel"]; got != "C999" {
		t.Fatalf("patched setting = %v, want C999", got)
	}
}

func TestSettingsPatchNotificationsRejectsInvalidConfigWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.yaml")
	t.Setenv("ELASTICCLAW_HUB_CONFIG", path)
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Notifications: &types.NotificationsConfig{
		Notifiers: map[string]types.NotifierConfig{"ops": {Type: "slack"}},
		Lifecycle: &types.LifecycleNotificationsConfig{Via: "ops"},
	}}, "", "", "")
	if err := config.SaveHubConfig(s.hubCfg); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, body := range [][]byte{
		[]byte(`{"notifications":{"notifiers":{"ops":{"type":"slack"}},"lifecycle":{"routes":[{"via":"ops","events":["not_an_event"]}]}}}`),
		[]byte(`{"notifications":{"notifiers":{"ops":{"type":"slack"}},"lifecycle":{"routes":[{"via":"missing"}]}}}`),
	} {
		rr := httptest.NewRecorder()
		s.patchSettings(rr, httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewReader(body)))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("PATCH status = %d: %s", rr.Code, rr.Body.String())
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, before) {
			t.Fatalf("invalid patch changed disk config:\n%s", after)
		}
	}
}

// Regression: the Notifier screen lists every hub notifier, including ones no
// lifecycle route uses and only a pipeline notify action references, and its
// Remove button PATCHes the whole notifiers map without the deleted key.
// Nothing server-side looked at pipelines, so the save returned 200, the
// notifier left hub.yaml, and every stage notification through it was then
// dropped at runtime with only a warning in the claw conversation.
func TestSettingsPatchRejectsRemovingNotifierUsedByPipeline(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", filepath.Join(t.TempDir(), "hub.yaml"))
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		Factories: []*types.FactoryConfig{{
			Name:         "triage",
			PipelineYAML: "stages:\n  - id: announce\n    on_enter:\n      notify:\n        via: releases\n        text: hi\n",
		}},
		Notifications: &types.NotificationsConfig{
			Notifiers: map[string]types.NotifierConfig{
				"releases": {Type: "slack", Settings: map[string]any{"channel": "C0123ABCD", "token_secret": "slack_tok"}},
				"ops":      {Type: "slack", Settings: map[string]any{"channel": "C0456EFGH", "token_secret": "slack_tok"}},
			},
			Lifecycle: &types.LifecycleNotificationsConfig{Via: "ops"},
		},
	}, "", "", "")

	body := []byte(`{"notifications":{"notifiers":{"ops":{"type":"slack","channel":"C0456EFGH","token_secret":"slack_tok"}},"lifecycle":{"enabled":true,"routes":[{"via":"ops"}]}}}`)
	rr := httptest.NewRecorder()
	s.patchSettings(rr, httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewReader(body)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("PATCH status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); !strings.Contains(got, `"releases"`) || !strings.Contains(got, `stage "announce"`) {
		t.Fatalf("error does not name the notifier and the stage that depends on it: %s", got)
	}
	if _, ok := s.hubCfg.Notifications.Notifiers["releases"]; !ok {
		t.Fatal("a rejected patch must not have removed the notifier from the running config")
	}
}

// Regression: the GET view emitted the two lifecycle durations as
// poll_interval/idle_after while the PATCH body is decoded into
// types.LifecycleNotificationsConfig, which reads pollInterval/idleAfter. A
// client that GETs the settings, edits the routes and PATCHes the object back
// therefore reset a deliberately raised idle_after to the 5m default, and
// SaveHubConfig persisted the loss.
func TestSettingsLifecycleViewRoundTripsThroughPatch(t *testing.T) {
	encoded, err := json.Marshal(LifecycleNotificationsView{
		Enabled: true, Routes: []types.LifecycleRoute{{Via: "ops"}},
		PollInterval: "30s", IdleAfter: "30m",
	})
	if err != nil {
		t.Fatal(err)
	}
	var patched types.LifecycleNotificationsConfig
	if err := json.Unmarshal(encoded, &patched); err != nil {
		t.Fatal(err)
	}
	if patched.PollInterval != "30s" || patched.IdleAfter != "30m" {
		t.Fatalf("GET→PATCH round trip dropped the durations: %#v (from %s)", patched, encoded)
	}
}

// Regression: PATCH validated only that the notifier TYPE was supported, so a
// Slack channel saved without token_secret — or with a #name where a channel ID
// belongs — persisted with a 200. Every later lifecycle tick then failed to
// build that notifier and delivered nothing, saying so in the hub log alone.
func TestSettingsPatchRejectsNotifierMissingRequiredSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.yaml")
	t.Setenv("ELASTICCLAW_HUB_CONFIG", path)
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Notifications: &types.NotificationsConfig{
		Notifiers: map[string]types.NotifierConfig{"ops": {Type: "slack", Settings: map[string]any{
			"channel": "C0123ABCD", "token_secret": "slack_token",
		}}},
		Lifecycle: &types.LifecycleNotificationsConfig{Via: "ops"},
	}}, "", "", "")
	if err := config.SaveHubConfig(s.hubCfg); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// A brand-new channel, exactly as the Notifier screen adds one — the
	// settings merge cannot backfill anything for a name the config has never
	// seen.
	for name, notifier := range map[string]string{
		"no token secret": `{"type":"slack","channel":"C0NEWCHAN"}`,
		"channel name":    `{"type":"slack","channel":"#alerts","token_secret":"slack_token"}`,
		"bad interval":    `{"type":"slack","channel":"C0NEWCHAN","token_secret":"slack_token","min_send_interval":"soon"}`,
	} {
		t.Run(name, func(t *testing.T) {
			body := []byte(`{"notifications":{"notifiers":{"ops":{"type":"slack","channel":"C0123ABCD","token_secret":"slack_token"},"added":` +
				notifier + `},"lifecycle":{"enabled":true,"routes":[{"via":"ops"},{"via":"added"}]}}}`)
			rr := httptest.NewRecorder()
			s.patchSettings(rr, httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewReader(body)))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("PATCH status = %d, want 400; body = %s", rr.Code, rr.Body.String())
			}
			if got := rr.Body.String(); !strings.Contains(got, "notifications.notifiers.added") {
				t.Fatalf("error does not name the offending notifier: %s", got)
			}
			if _, ok := s.hubCfg.Notifications.Notifiers["added"]; ok {
				t.Fatal("a rejected patch must not have added the notifier to the running config")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Fatalf("rejected patch changed disk config:\n%s", after)
			}
		})
	}
}
