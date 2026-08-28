package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub/notify"
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

	body := []byte(`{"notifications":{"notifiers":{"ops":{"type":"slack","channel":"C123","token_secret":"slack_token","api_base":"https://slack.com/api","min_send_interval":"2s"}},"lifecycle":{"enabled":true,"via":"old","routes":[{"via":"ops","events":["agent_started"]}],"idleAfter":"5m","events":{"agentStarted":true}}}}`)
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

// Regression: the provider-level "can this be built" check ran over every
// notifier in the PATCH body, and the Notifier screen always submits the whole
// map. A hub.yaml holding a notifier this build refuses to construct — load
// validation accepts one, the hub just logs "notifier unavailable" each tick —
// therefore 400'd every save from the screen, including saves that touch an
// entirely different channel, and the screen has no control for the keys the
// check rejects.
func TestSettingsPatchAllowsUnchangedBrokenNotifier(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.yaml")
	t.Setenv("ELASTICCLAW_HUB_CONFIG", path)
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Notifications: &types.NotificationsConfig{
		Notifiers: map[string]types.NotifierConfig{"ops": {Type: "slack", Settings: map[string]any{
			"channel": "C0123ABCD", "token_secret": "slack_token", "min_send_interval": "5 minutes",
		}}},
		Lifecycle: &types.LifecycleNotificationsConfig{Via: "ops"},
	}}, "", "", "")
	if err := config.SaveHubConfig(s.hubCfg); err != nil {
		t.Fatal(err)
	}

	// Adding an unrelated channel, exactly as the screen sends it: the whole
	// notifiers map, with `ops` re-sent from the GET projection.
	body := []byte(`{"notifications":{"notifiers":` +
		`{"ops":{"type":"slack","channel":"C0123ABCD","token_secret":"slack_token"},` +
		`"alerts":{"type":"slack","channel":"C0ALERTS","token_secret":"slack_token"}},` +
		`"lifecycle":{"enabled":true,"routes":[{"via":"ops"},{"via":"alerts"}]}}}`)
	rr := httptest.NewRecorder()
	s.patchSettings(rr, httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	diskCfg, err := config.LoadHubConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := diskCfg.Notifications.Notifiers["alerts"]; !ok {
		t.Fatal("the new channel was not persisted")
	}
	if got := diskCfg.Notifications.Notifiers["ops"].Settings["min_send_interval"]; got != "5 minutes" {
		t.Fatalf("untouched notifier setting = %v, want it left alone", got)
	}

	// Touching the offender is still checked: the operator is editing it, so
	// the patch may not persist a notifier the hub cannot build.
	body = []byte(`{"notifications":{"notifiers":` +
		`{"ops":{"type":"slack","channel":"C0999NEW","token_secret":"slack_token"},` +
		`"alerts":{"type":"slack","channel":"C0ALERTS","token_secret":"slack_token"}},` +
		`"lifecycle":{"enabled":true,"routes":[{"via":"ops"},{"via":"alerts"}]}}}`)
	rr = httptest.NewRecorder()
	s.patchSettings(rr, httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewReader(body)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("editing the broken notifier: status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); !strings.Contains(got, "notifications.notifiers.ops") {
		t.Fatalf("error does not name the edited notifier: %s", got)
	}
}

// Regression: the only key the provider check rejects that the screen does not
// model was min_send_interval, so a notifier holding an unparseable one could
// be neither edited (every save 400'd on it) nor removed (a pipeline notifies
// through it). The Notifier dialog now edits the key, so both repairs — fixing
// the value and clearing it back to the default — must land.
func TestSettingsPatchRepairsBrokenMinSendInterval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.yaml")
	t.Setenv("ELASTICCLAW_HUB_CONFIG", path)
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Notifications: &types.NotificationsConfig{
		Notifiers: map[string]types.NotifierConfig{"ops": {Type: "slack", Settings: map[string]any{
			"channel": "C0123ABCD", "token_secret": "slack_token", "min_send_interval": "5 minutes",
		}}},
		Lifecycle: &types.LifecycleNotificationsConfig{Via: "ops"},
	}}, "", "", "")
	if err := config.SaveHubConfig(s.hubCfg); err != nil {
		t.Fatal(err)
	}

	patch := func(t *testing.T, interval string) {
		t.Helper()
		body := []byte(`{"notifications":{"notifiers":` +
			`{"ops":{"type":"slack","channel":"C0123ABCD","token_secret":"slack_token","min_send_interval":"` + interval + `"}},` +
			`"lifecycle":{"enabled":true,"routes":[{"via":"ops"}]}}}`)
		rr := httptest.NewRecorder()
		s.patchSettings(rr, httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewReader(body)))
		if rr.Code != http.StatusOK {
			t.Fatalf("PATCH min_send_interval %q: status = %d, want 200; body = %s", interval, rr.Code, rr.Body.String())
		}
	}
	stored := func(t *testing.T) any {
		t.Helper()
		diskCfg, err := config.LoadHubConfig()
		if err != nil {
			t.Fatal(err)
		}
		return diskCfg.Notifications.Notifiers["ops"].Settings["min_send_interval"]
	}

	patch(t, "5m")
	if got := stored(t); got != "5m" {
		t.Fatalf("min_send_interval = %v, want the repaired value", got)
	}
	// Emptied, not omitted: the patch is folded over the settings on disk, so
	// only an explicit empty value clears the key.
	patch(t, "")
	if got := stored(t); got != "" {
		t.Fatalf("min_send_interval = %v, want it cleared", got)
	}
}

// Regression: lifecycle poll_interval and idle_after are validated on every
// patch, floors included, and the Notifier screen renders neither — so a
// hub.yaml holding a sub-floor value made EVERY save from that screen 400 on a
// field the operator could not see, let alone repair.
func TestSettingsPatchClearsUnusableLifecycleDurations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.yaml")
	t.Setenv("ELASTICCLAW_HUB_CONFIG", path)
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Notifications: &types.NotificationsConfig{
		Notifiers: map[string]types.NotifierConfig{"eng": {Type: "slack", Settings: map[string]any{
			"channel": "C0123ABCD", "token_secret": "slack_token",
		}}},
		Lifecycle: &types.LifecycleNotificationsConfig{Via: "eng", PollInterval: "500ms", IdleAfter: "30s"},
	}}, "", "", "")
	if err := config.SaveHubConfig(s.hubCfg); err != nil {
		t.Fatal(err)
	}

	// The master switch, as the screen sends it: the whole block rebuilt from
	// the GET projection, both durations re-sent verbatim.
	body := []byte(`{"notifications":{"notifiers":` +
		`{"eng":{"type":"slack","channel":"C0123ABCD","token_secret":"slack_token"}},` +
		`"lifecycle":{"enabled":false,"routes":[{"via":"eng"}],"pollInterval":"500ms","idleAfter":"30s"}}}`)
	rr := httptest.NewRecorder()
	s.patchSettings(rr, httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	diskCfg, err := config.LoadHubConfig()
	if err != nil {
		t.Fatal(err)
	}
	lc := diskCfg.Notifications.Lifecycle
	if lc.PollInterval != "" || lc.IdleAfter != "" {
		t.Fatalf("lifecycle durations = %q/%q, want both cleared", lc.PollInterval, lc.IdleAfter)
	}

	// A value the patch itself introduces is still a typo worth reporting.
	body = []byte(`{"notifications":{"notifiers":` +
		`{"eng":{"type":"slack","channel":"C0123ABCD","token_secret":"slack_token"}},` +
		`"lifecycle":{"enabled":false,"routes":[{"via":"eng"}],"idleAfter":"10s"}}}`)
	rr = httptest.NewRecorder()
	s.patchSettings(rr, httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewReader(body)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("introducing a sub-floor idle_after: status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
}

// Regression: the drop above must fire only on a duration validation actually
// rejects. The probe it validates carries no `via` and no routes, so an ENABLED
// probe is rejected for THAT — reporting every value as unusable and erasing a
// perfectly valid poll_interval/idle_after from hub.yaml on every save made
// from the Notifier screen, which re-sends both verbatim.
func TestSettingsPatchKeepsValidLifecycleDurations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.yaml")
	t.Setenv("ELASTICCLAW_HUB_CONFIG", path)
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Notifications: &types.NotificationsConfig{
		Notifiers: map[string]types.NotifierConfig{"eng": {Type: "slack", Settings: map[string]any{
			"channel": "C0123ABCD", "token_secret": "slack_token",
		}}},
		Lifecycle: &types.LifecycleNotificationsConfig{Via: "eng", PollInterval: "60s", IdleAfter: "30m"},
	}}, "", "", "")
	if err := config.SaveHubConfig(s.hubCfg); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"notifications":{"notifiers":` +
		`{"eng":{"type":"slack","channel":"C0123ABCD","token_secret":"slack_token"}},` +
		`"lifecycle":{"enabled":true,"routes":[{"via":"eng"}],"pollInterval":"60s","idleAfter":"30m"}}}`)
	rr := httptest.NewRecorder()
	s.patchSettings(rr, httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	diskCfg, err := config.LoadHubConfig()
	if err != nil {
		t.Fatal(err)
	}
	lc := diskCfg.Notifications.Lifecycle
	if lc.PollInterval != "60s" || lc.IdleAfter != "30m" {
		t.Fatalf("lifecycle durations = %q/%q, want them preserved", lc.PollInterval, lc.IdleAfter)
	}
}

// Regression: api_base decides where the notifier's bot token is sent, and the
// patch preserves token_secret, so repointing it through the settings API was
// token exfiltration (and SSRF) in one PATCH.
func TestSettingsPatchRejectsRepointedNotifierAPIBase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.yaml")
	t.Setenv("ELASTICCLAW_HUB_CONFIG", path)
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Notifications: &types.NotificationsConfig{
		Notifiers: map[string]types.NotifierConfig{"eng": {Type: "slack", Settings: map[string]any{
			"channel": "C0123ABCD", "token_secret": "slack_token", "api_base": "https://slack-proxy.internal",
		}}},
		Lifecycle: &types.LifecycleNotificationsConfig{Via: "eng"},
	}}, "", "", "")
	if err := config.SaveHubConfig(s.hubCfg); err != nil {
		t.Fatal(err)
	}

	patch := func(t *testing.T, apiBase string) *httptest.ResponseRecorder {
		t.Helper()
		body := []byte(`{"notifications":{"notifiers":` +
			`{"eng":{"type":"slack","channel":"C0999NEW","token_secret":"slack_token","api_base":"` + apiBase + `"}},` +
			`"lifecycle":{"enabled":true,"routes":[{"via":"eng"}]}}}`)
		rr := httptest.NewRecorder()
		s.patchSettings(rr, httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewReader(body)))
		return rr
	}

	rr := patch(t, "https://attacker.example")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("repointed api_base: status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); !strings.Contains(got, "api_base") {
		t.Fatalf("error does not name the offending setting: %s", got)
	}
	diskCfg, err := config.LoadHubConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := diskCfg.Notifications.Notifiers["eng"].Settings["api_base"]; got != "https://slack-proxy.internal" {
		t.Fatalf("stored api_base = %v, want the rejected patch to have written nothing", got)
	}

	// The operator's own hub.yaml value is exempt: re-sending it unchanged —
	// what the settings screen does on every save — still edits the channel.
	if rr = patch(t, "https://slack-proxy.internal"); rr.Code != http.StatusOK {
		t.Fatalf("unchanged api_base: status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
}

// Regression: a stored api_base this build refuses to construct (written by
// hand, or accepted by an older build) is exempt from validateNotifierAPIBase,
// but the provider check that follows judged the merged settings — stored
// api_base included — so every save that touched the channel 400d on a field
// the settings screen neither renders nor can clear, leaving a hub.yaml edit as
// the only repair.
func TestSettingsPatchEditsChannelWithUnbuildableStoredAPIBase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.yaml")
	t.Setenv("ELASTICCLAW_HUB_CONFIG", path)
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Notifications: &types.NotificationsConfig{
		Notifiers: map[string]types.NotifierConfig{"eng": {Type: "slack", Settings: map[string]any{
			// Scheme-less: accepted at load, refused by newSlack.
			"channel": "C0123ABCD", "token_secret": "slack_token", "api_base": "slack.example.com/api",
		}}},
		Lifecycle: &types.LifecycleNotificationsConfig{Via: "eng"},
	}}, "", "", "")
	if err := config.SaveHubConfig(s.hubCfg); err != nil {
		t.Fatal(err)
	}

	patch := func(t *testing.T, apiBase string) *httptest.ResponseRecorder {
		t.Helper()
		body := []byte(`{"notifications":{"notifiers":` +
			`{"eng":{"type":"slack","channel":"C0999NEW","token_secret":"slack_token","api_base":"` + apiBase + `"}},` +
			`"lifecycle":{"enabled":true,"routes":[{"via":"eng"}]}}}`)
		rr := httptest.NewRecorder()
		s.patchSettings(rr, httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewReader(body)))
		return rr
	}

	// The repair the screen offers — a corrected channel ID, api_base
	// round-tripped unchanged — must go through.
	rr := patch(t, "slack.example.com/api")
	if rr.Code != http.StatusOK {
		t.Fatalf("editing a channel with an unbuildable stored api_base: status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	diskCfg, err := config.LoadHubConfig()
	if err != nil {
		t.Fatal(err)
	}
	stored := diskCfg.Notifications.Notifiers["eng"].Settings
	if stored["channel"] != "C0999NEW" {
		t.Fatalf("stored channel = %v, want the edit to have landed", stored["channel"])
	}
	if stored["api_base"] != "slack.example.com/api" {
		t.Fatalf("stored api_base = %v, want it preserved", stored["api_base"])
	}

	// A value the patch itself introduces is still judged, exemption or not.
	if rr = patch(t, "slack.other.example/api"); rr.Code != http.StatusBadRequest {
		t.Fatalf("changed api_base: status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
}

// ── Scheduled reports ─────────────────────────────────────────────────────────

// The settings screen rebuilds the whole notifications block from the view it
// GETs, so every scheduled-report field has to survive a GET → PATCH → disk
// round trip. A field the view drops is a field the first save from the screen
// silently deletes from hub.yaml.
func TestSettingsScheduledNotificationsRoundTripThroughPatch(t *testing.T) {
	registerScheduledReport("settings-round-trip-report", func(context.Context, *Server) (*notify.Message, bool, error) {
		return nil, false, nil
	})
	path := filepath.Join(t.TempDir(), "hub.yaml")
	t.Setenv("ELASTICCLAW_HUB_CONFIG", path)
	disabled := false
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Notifications: &types.NotificationsConfig{
		Notifiers: map[string]types.NotifierConfig{
			"eng": {Type: "slack", Settings: map[string]any{"channel": "C0123ABCD", "token_secret": "slack_token"}},
			"ops": {Type: "slack", Settings: map[string]any{"channel": "C0456EFGH", "token_secret": "slack_token"}},
		},
		Scheduled: []types.ScheduledNotificationConfig{
			{
				ID: "morning-prs", Report: "settings-round-trip-report",
				Via: []string{"eng", "ops"}, At: "09:30",
				Timezone: "America/Sao_Paulo", Weekdays: []string{"mon", "fri"},
			},
			// Every-day, UTC, paused: the shapes whose zero values are easiest
			// to lose on the way through the view.
			{ID: "nightly", Report: "settings-round-trip-report", Via: []string{"ops"}, At: "23:00", Enabled: &disabled},
		},
	}}, "", "", "")
	if err := config.SaveHubConfig(s.hubCfg); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	s.getSettings(rr, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET status = %d: %s", rr.Code, rr.Body.String())
	}
	var view SettingsView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	want := []ScheduledNotificationView{
		{
			ID: "morning-prs", Report: "settings-round-trip-report", Via: []string{"eng", "ops"},
			At: "09:30", Timezone: "America/Sao_Paulo", Weekdays: []string{"mon", "fri"}, Enabled: true,
		},
		{ID: "nightly", Report: "settings-round-trip-report", Via: []string{"ops"}, At: "23:00", Weekdays: []string{}, Enabled: false},
	}
	if !reflect.DeepEqual(view.Notifications.Scheduled, want) {
		t.Fatalf("scheduled view = %#v, want %#v", view.Notifications.Scheduled, want)
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
	if len(diskCfg.Notifications.Scheduled) != 2 {
		t.Fatalf("disk scheduled = %#v, want both schedules", diskCfg.Notifications.Scheduled)
	}
	first := diskCfg.Notifications.Scheduled[0]
	if first.ID != "morning-prs" || first.At != "09:30" || first.Timezone != "America/Sao_Paulo" ||
		!reflect.DeepEqual(first.Via, []string{"eng", "ops"}) || !reflect.DeepEqual(first.Weekdays, []string{"mon", "fri"}) {
		t.Fatalf("disk schedule = %#v, want every field preserved", first)
	}
	if second := diskCfg.Notifications.Scheduled[1]; second.Enabled == nil || *second.Enabled {
		t.Fatalf("disk schedule %#v lost its paused state", second)
	}
}

// A schedule the patch ADDS must name a report this build carries: persisting
// one it does not would leave a schedule that logs "not registered" every
// minute and delivers nothing, with the settings screen reporting success.
func TestSettingsPatchRejectsUnknownScheduledReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.yaml")
	t.Setenv("ELASTICCLAW_HUB_CONFIG", path)
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Notifications: &types.NotificationsConfig{
		Notifiers: map[string]types.NotifierConfig{
			"eng": {Type: "slack", Settings: map[string]any{"channel": "C0123ABCD", "token_secret": "slack_token"}},
		},
	}}, "", "", "")
	if err := config.SaveHubConfig(s.hubCfg); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"notifications":{"notifiers":{"eng":{"type":"slack","channel":"C0123ABCD","token_secret":"slack_token"}},` +
		`"scheduled":[{"id":"x","report":"no-such-report","via":["eng"],"at":"09:00","weekdays":[],"enabled":true}]}}`)
	rr := httptest.NewRecorder()
	s.patchSettings(rr, httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewReader(body)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "unknown report") {
		t.Fatalf("error does not name the problem: %s", rr.Body.String())
	}
	if len(s.hubCfg.Notifications.Scheduled) != 0 {
		t.Fatalf("rejected patch still wrote %#v", s.hubCfg.Notifications.Scheduled)
	}
}

// A stored schedule naming a report THIS build does not carry (an older or
// newer binary's, a hand-written hub.yaml) must not 400 every save from the
// screen — including the save that deletes it. Only entries the patch adds or
// changes are judged, exactly as unbuildable notifiers are.
func TestSettingsPatchAllowsUnchangedUnknownScheduledReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.yaml")
	t.Setenv("ELASTICCLAW_HUB_CONFIG", path)
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Notifications: &types.NotificationsConfig{
		Notifiers: map[string]types.NotifierConfig{
			"eng": {Type: "slack", Settings: map[string]any{"channel": "C0123ABCD", "token_secret": "slack_token"}},
		},
		Scheduled: []types.ScheduledNotificationConfig{
			{ID: "legacy", Report: "report-from-another-build", Via: []string{"eng"}, At: "09:00"},
		},
	}}, "", "", "")
	if err := config.SaveHubConfig(s.hubCfg); err != nil {
		t.Fatal(err)
	}

	notifiers := `{"eng":{"type":"slack","channel":"C0123ABCD","token_secret":"slack_token"}}`
	unchanged := []byte(`{"notifications":{"notifiers":` + notifiers +
		`,"scheduled":[{"id":"legacy","report":"report-from-another-build","via":["eng"],"at":"09:00","weekdays":[],"enabled":true}]}}`)
	rr := httptest.NewRecorder()
	s.patchSettings(rr, httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewReader(unchanged)))
	if rr.Code != http.StatusOK {
		t.Fatalf("re-saving the stored schedule = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}

	// Pausing it goes through: only the report name is pinned to the stored
	// value, every other edit to the entry — the pause the doctor's "pause
	// before upgrading" guidance depends on, a re-route, a new slot — is the
	// operator's to make.
	paused := []byte(`{"notifications":{"notifiers":` + notifiers +
		`,"scheduled":[{"id":"legacy","report":"report-from-another-build","via":["eng"],"at":"09:00","weekdays":[],"enabled":false}]}}`)
	rr = httptest.NewRecorder()
	s.patchSettings(rr, httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewReader(paused)))
	if rr.Code != http.StatusOK {
		t.Fatalf("pausing the stored schedule = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	if sched := s.hubCfg.Notifications.Scheduled; len(sched) != 1 || sched[0].Enabled == nil || *sched[0].Enabled {
		t.Fatalf("pause did not persist: %#v", sched)
	}

	// And the repair — deleting it — goes through too.
	removed := []byte(`{"notifications":{"notifiers":` + notifiers + `,"scheduled":[]}}`)
	rr = httptest.NewRecorder()
	s.patchSettings(rr, httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewReader(removed)))
	if rr.Code != http.StatusOK {
		t.Fatalf("deleting the stored schedule = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	diskCfg, err := config.LoadHubConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(diskCfg.Notifications.Scheduled) != 0 {
		t.Fatalf("disk scheduled = %#v, want it deleted", diskCfg.Notifications.Scheduled)
	}
}

// A PATCH that omits the `scheduled` key entirely comes from a client that
// never saw the field (an older screen, a hand-written notifiers-only PATCH),
// not from one asking to delete every schedule: the stored schedules must be
// carried forward, exactly as the empty-routes guard preserves the lifecycle
// channel binding. Deleting is expressed as a present, empty list.
func TestSettingsPatchWithoutScheduledKeyKeepsStoredSchedules(t *testing.T) {
	registerScheduledReport("carry-forward-report", func(context.Context, *Server) (*notify.Message, bool, error) {
		return nil, false, nil
	})
	path := filepath.Join(t.TempDir(), "hub.yaml")
	t.Setenv("ELASTICCLAW_HUB_CONFIG", path)
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Notifications: &types.NotificationsConfig{
		Notifiers: map[string]types.NotifierConfig{
			"eng": {Type: "slack", Settings: map[string]any{"channel": "C0123ABCD", "token_secret": "slack_token"}},
		},
		Scheduled: []types.ScheduledNotificationConfig{
			{ID: "morning", Report: "carry-forward-report", Via: []string{"eng"}, At: "09:00"},
		},
	}}, "", "", "")
	if err := config.SaveHubConfig(s.hubCfg); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"notifications":{"notifiers":{"eng":{"type":"slack","channel":"C0999NEW","token_secret":"slack_token"}}}}`)
	rr := httptest.NewRecorder()
	s.patchSettings(rr, httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	diskCfg, err := config.LoadHubConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(diskCfg.Notifications.Scheduled) != 1 || diskCfg.Notifications.Scheduled[0].ID != "morning" {
		t.Fatalf("a scheduled-less patch wiped the stored schedules: %#v", diskCfg.Notifications.Scheduled)
	}
	if got, _ := diskCfg.Notifications.Notifiers["eng"].Settings["channel"].(string); got != "C0999NEW" {
		t.Fatalf("the notifier edit itself did not land: channel = %q", got)
	}
}
