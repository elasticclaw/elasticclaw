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
