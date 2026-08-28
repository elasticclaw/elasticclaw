package types

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func boolPtr(v bool) *bool { return &v }

// Channel and token_secret shape are deliberately NOT validated here: they are
// provider settings, validated by the provider's constructor in pkg/hub/notify
// (see the slack provider tests). This layer validates the wiring only —
// notifier names, types, and the lifecycle block's reference into them.
func TestValidateNotificationsConfig(t *testing.T) {
	slack := func() map[string]NotifierConfig {
		return map[string]NotifierConfig{
			"eng-agents": {Type: "slack", Settings: map[string]any{
				"token_secret": "slack_bot_token",
				"channel":      "C0123ABCD",
			}},
		}
	}

	tests := []struct {
		name    string
		cfg     *NotificationsConfig
		wantErr bool
		errMsg  string
	}{
		{
			name: "nil config is valid",
			cfg:  nil,
		},
		{
			name: "notifiers without a lifecycle block are valid",
			cfg:  &NotificationsConfig{Notifiers: slack()},
		},
		{
			name: "valid scheduled notification",
			cfg: &NotificationsConfig{Notifiers: slack(), Scheduled: []ScheduledNotificationConfig{{
				ID: "morning-prs", Report: "pending_prs", Via: []string{"eng-agents"}, At: "09:30", Timezone: "America/Sao_Paulo", Weekdays: []string{"mon", "tue"},
			}}},
		},
		{
			name:    "scheduled notification requires unique id",
			cfg:     &NotificationsConfig{Notifiers: slack(), Scheduled: []ScheduledNotificationConfig{{ID: "daily", Report: "x", Via: []string{"eng-agents"}, At: "09:00"}, {ID: "daily", Report: "x", Via: []string{"eng-agents"}, At: "10:00"}}},
			wantErr: true,
			errMsg:  "id \"daily\" is duplicated",
		},
		{
			name:    "scheduled notification validates fields",
			cfg:     &NotificationsConfig{Notifiers: slack(), Scheduled: []ScheduledNotificationConfig{{ID: "daily", Via: []string{"eng-agents"}, At: "09:00"}}},
			wantErr: true,
			errMsg:  "report is required",
		},
		{
			name:    "scheduled notification validates notifier and clock",
			cfg:     &NotificationsConfig{Notifiers: slack(), Scheduled: []ScheduledNotificationConfig{{ID: "daily", Report: "x", Via: []string{"missing"}, At: "9:00"}}},
			wantErr: true,
			errMsg:  "does not name a configured notifier",
		},
		{
			// The scheduler sends once per pending via entry, so a repeated
			// name would post the same report twice in the same tick.
			name:    "scheduled notification rejects duplicate via",
			cfg:     &NotificationsConfig{Notifiers: slack(), Scheduled: []ScheduledNotificationConfig{{ID: "daily", Report: "x", Via: []string{"eng-agents", "eng-agents"}, At: "09:00"}}},
			wantErr: true,
			errMsg:  `via "eng-agents" is duplicated`,
		},
		{
			// Symmetric with the disabled lifecycle block: an operator who
			// pauses a schedule and then deletes its notifier must not be left
			// with a hub that refuses to load.
			name: "paused scheduled notification accepts a dangling via",
			cfg:  &NotificationsConfig{Notifiers: slack(), Scheduled: []ScheduledNotificationConfig{{ID: "daily", Report: "x", Via: []string{"gone"}, At: "09:00", Enabled: boolPtr(false)}}},
		},
		{
			name:    "paused scheduled notification still rejects duplicate via",
			cfg:     &NotificationsConfig{Notifiers: slack(), Scheduled: []ScheduledNotificationConfig{{ID: "daily", Report: "x", Via: []string{"gone", "gone"}, At: "09:00", Enabled: boolPtr(false)}}},
			wantErr: true,
			errMsg:  `via "gone" is duplicated`,
		},
		{
			name:    "scheduled notification validates timezone and weekdays",
			cfg:     &NotificationsConfig{Notifiers: slack(), Scheduled: []ScheduledNotificationConfig{{ID: "daily", Report: "x", Via: []string{"eng-agents"}, At: "09:00", Timezone: "not/a-zone"}}},
			wantErr: true,
			errMsg:  "invalid timezone",
		},
		{
			name: "valid lifecycle wiring",
			cfg: &NotificationsConfig{
				Notifiers: slack(),
				Lifecycle: &LifecycleNotificationsConfig{Via: "eng-agents"},
			},
		},
		{
			name: "valid lifecycle routes",
			cfg: &NotificationsConfig{
				Notifiers: map[string]NotifierConfig{
					"eng-agents": {Type: "slack"},
					"failures":   {Type: "slack"},
				},
				Lifecycle: &LifecycleNotificationsConfig{Routes: []LifecycleRoute{
					{Via: "eng-agents", Events: []string{"agent_started", "pr_opened"}},
					{Via: "failures"},
				}},
			},
		},
		{
			name: "via and routes cannot both be set",
			cfg: &NotificationsConfig{
				Notifiers: slack(),
				Lifecycle: &LifecycleNotificationsConfig{Via: "eng-agents", Routes: []LifecycleRoute{{Via: "eng-agents"}}},
			},
			wantErr: true,
			errMsg:  "via and routes cannot both be set",
		},
		{
			name:    "route without via",
			cfg:     &NotificationsConfig{Notifiers: slack(), Lifecycle: &LifecycleNotificationsConfig{Routes: []LifecycleRoute{{}}}},
			wantErr: true,
			errMsg:  "routes[0]: via is required",
		},
		{
			name:    "route via naming an undefined notifier",
			cfg:     &NotificationsConfig{Notifiers: slack(), Lifecycle: &LifecycleNotificationsConfig{Routes: []LifecycleRoute{{Via: "typo"}}}},
			wantErr: true,
			errMsg:  "does not name a configured notifier",
		},
		{
			name:    "route with unsupported event",
			cfg:     &NotificationsConfig{Notifiers: slack(), Lifecycle: &LifecycleNotificationsConfig{Routes: []LifecycleRoute{{Via: "eng-agents", Events: []string{"not_an_event"}}}}},
			wantErr: true,
			errMsg:  "not a supported lifecycle event type",
		},
		{
			name:    "route with duplicate event",
			cfg:     &NotificationsConfig{Notifiers: slack(), Lifecycle: &LifecycleNotificationsConfig{Routes: []LifecycleRoute{{Via: "eng-agents", Events: []string{"agent_started", "agent_started"}}}}},
			wantErr: true,
			errMsg:  "event \"agent_started\" is duplicated",
		},
		{
			name:    "duplicate route via",
			cfg:     &NotificationsConfig{Notifiers: slack(), Lifecycle: &LifecycleNotificationsConfig{Routes: []LifecycleRoute{{Via: "eng-agents"}, {Via: "eng-agents"}}}},
			wantErr: true,
			errMsg:  "via \"eng-agents\" is duplicated",
		},
		{
			name: "notifier without a type",
			cfg: &NotificationsConfig{
				Notifiers: map[string]NotifierConfig{"eng-agents": {}},
			},
			wantErr: true,
			errMsg:  "type is required",
		},
		{
			// The hub keys per-route delivery state by notifier name and
			// reserves "\x00legacy" for the legacy un-routed rows: a name
			// carrying a NUL forges that sentinel and fences an event for every
			// other route. A name carrying a newline forges hub log lines.
			name: "notifier name with a NUL",
			cfg: &NotificationsConfig{
				Notifiers: map[string]NotifierConfig{"\x00legacy": {Type: "slack"}},
			},
			wantErr: true,
			errMsg:  "cannot contain control characters",
		},
		{
			name: "notifier name with a newline",
			cfg: &NotificationsConfig{
				Notifiers: map[string]NotifierConfig{"eng\nagents": {Type: "slack"}},
			},
			wantErr: true,
			errMsg:  "cannot contain control characters",
		},
		{
			name: "lifecycle enabled without via",
			cfg: &NotificationsConfig{
				Notifiers: slack(),
				Lifecycle: &LifecycleNotificationsConfig{},
			},
			wantErr: true,
			errMsg:  "via is required",
		},
		{
			name: "via naming an undefined notifier",
			cfg: &NotificationsConfig{
				Notifiers: slack(),
				Lifecycle: &LifecycleNotificationsConfig{Via: "typo"},
			},
			wantErr: true,
			errMsg:  "does not name a configured notifier",
		},
		{
			name: "disabled lifecycle skips via checks",
			cfg: &NotificationsConfig{
				Notifiers: slack(),
				Lifecycle: &LifecycleNotificationsConfig{Enabled: boolPtr(false)},
			},
		},
		{
			name: "unparseable poll interval",
			cfg: &NotificationsConfig{
				Notifiers: slack(),
				Lifecycle: &LifecycleNotificationsConfig{Via: "eng-agents", PollInterval: "sometimes"},
			},
			wantErr: true,
			errMsg:  "invalid poll_interval",
		},
		{
			name: "poll interval below 1s",
			cfg: &NotificationsConfig{
				Notifiers: slack(),
				Lifecycle: &LifecycleNotificationsConfig{Via: "eng-agents", PollInterval: "200ms"},
			},
			wantErr: true,
			errMsg:  "at least 1s",
		},
		{
			// A typo must be caught before the operator flips the feature on.
			name: "poll interval validated even when disabled",
			cfg: &NotificationsConfig{
				Notifiers: slack(),
				Lifecycle: &LifecycleNotificationsConfig{Enabled: boolPtr(false), PollInterval: "nope"},
			},
			wantErr: true,
			errMsg:  "invalid poll_interval",
		},
		{
			name: "valid idle_after",
			cfg: &NotificationsConfig{
				Notifiers: slack(),
				Lifecycle: &LifecycleNotificationsConfig{Via: "eng-agents", IdleAfter: "10m"},
			},
		},
		{
			name: "unparseable idle_after",
			cfg: &NotificationsConfig{
				Notifiers: slack(),
				Lifecycle: &LifecycleNotificationsConfig{Via: "eng-agents", IdleAfter: "whenever"},
			},
			wantErr: true,
			errMsg:  "invalid idle_after",
		},
		{
			name: "idle_after below 1m",
			cfg: &NotificationsConfig{
				Notifiers: slack(),
				Lifecycle: &LifecycleNotificationsConfig{Via: "eng-agents", IdleAfter: "30s"},
			},
			wantErr: true,
			errMsg:  "at least 1m",
		},
		{
			name: "idle_after validated even when disabled",
			cfg: &NotificationsConfig{
				Notifiers: slack(),
				Lifecycle: &LifecycleNotificationsConfig{Enabled: boolPtr(false), IdleAfter: "nah"},
			},
			wantErr: true,
			errMsg:  "invalid idle_after",
		},
		{
			// Routes are validated symmetrically with the legacy `via`: both
			// only while lifecycle is enabled. A disabled block whose `via`
			// names a deleted notifier loads fine, and the settings screen
			// derives its route list from that same `via` — so validating
			// routes above the short-circuit made every save from that screen
			// 400 on an entry the screen never renders and cannot drop.
			name: "dangling route is accepted while lifecycle is disabled",
			cfg: &NotificationsConfig{
				Notifiers: slack(),
				Lifecycle: &LifecycleNotificationsConfig{
					Enabled: boolPtr(false),
					Routes:  []LifecycleRoute{{Via: "gone"}},
				},
			},
		},
		{
			name: "dangling route is rejected while lifecycle is enabled",
			cfg: &NotificationsConfig{
				Notifiers: slack(),
				Lifecycle: &LifecycleNotificationsConfig{Routes: []LifecycleRoute{{Via: "gone"}}},
			},
			wantErr: true,
			errMsg:  `via "gone" does not name a configured notifier`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateNotificationsConfig(tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ValidateNotificationsConfig() succeeded, want error")
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.errMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateNotificationsConfig() error = %v", err)
			}
		})
	}
}

func TestLifecycleNotificationsRoutesYAMLRoundTrip(t *testing.T) {
	const source = "routes:\n  - via: eng-agents\n    events:\n      - agent_started\n      - pr_opened\n  - via: failures\n"
	var lifecycle LifecycleNotificationsConfig
	if err := yaml.Unmarshal([]byte(source), &lifecycle); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []LifecycleRoute{
		{Via: "eng-agents", Events: []string{"agent_started", "pr_opened"}},
		{Via: "failures"},
	}
	if !reflect.DeepEqual(lifecycle.Routes, want) {
		t.Fatalf("Routes = %#v, want %#v", lifecycle.Routes, want)
	}
	data, err := yaml.Marshal(&lifecycle)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var roundTripped LifecycleNotificationsConfig
	if err := yaml.Unmarshal(data, &roundTripped); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if !reflect.DeepEqual(roundTripped.Routes, want) {
		t.Fatalf("round-trip Routes = %#v, want %#v", roundTripped.Routes, want)
	}
}

func TestLifecycleNotificationsConfigEffectiveRoutes(t *testing.T) {
	tests := []struct {
		name string
		cfg  *LifecycleNotificationsConfig
		want []LifecycleRoute
	}{
		{name: "nil", cfg: nil},
		{name: "empty", cfg: &LifecycleNotificationsConfig{}},
		{name: "legacy via", cfg: &LifecycleNotificationsConfig{Via: "eng-agents"}, want: []LifecycleRoute{{Via: "eng-agents"}}},
		{name: "routes", cfg: &LifecycleNotificationsConfig{Routes: []LifecycleRoute{{Via: "eng-agents", Events: []string{"agent_started"}}}}, want: []LifecycleRoute{{Via: "eng-agents", Events: []string{"agent_started"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.EffectiveRoutes(); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("EffectiveRoutes() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestLifecycleEventTypes(t *testing.T) {
	// Regression: the vocabulary also listed the concrete failure kinds, which
	// exist only as task_run_events.failure_type. Route matching compares
	// against event_type, so a checkbox for one of them built a route that
	// could never fire while the test-send endpoint reported success.
	want := []string{"agent_started", "pr_opened", "agent_stopped", "agent_idle", "stage_stalled", "done_without_pr"}
	if !reflect.DeepEqual(LifecycleEventTypes, want) {
		t.Fatalf("LifecycleEventTypes = %v, want %v", LifecycleEventTypes, want)
	}
	for _, event := range LifecycleEventTypes {
		if !IsLifecycleEventType(event) {
			t.Errorf("IsLifecycleEventType(%q) = false, want true", event)
		}
	}
	for _, event := range []string{"not_an_event", "provision_failed", "timeout", "unknown_failure"} {
		if IsLifecycleEventType(event) {
			t.Errorf("IsLifecycleEventType(%q) = true, want false", event)
		}
	}
}

func TestHubConfigValidateWiresNotifications(t *testing.T) {
	cfg := &HubConfig{
		Notifications: &NotificationsConfig{
			Lifecycle: &LifecycleNotificationsConfig{Via: "missing"},
		},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "does not name a configured notifier") {
		t.Fatalf("HubConfig.Validate() = %v, want notifier wiring error", err)
	}
}

// NotifierConfig flattens provider settings next to "type" in YAML and JSON;
// a round trip must not lose them or leak "type" into Settings.
func TestNotifierConfigRoundTrip(t *testing.T) {
	const yamlSrc = "type: slack\ntoken_secret: slack_bot_token\nchannel: C0123ABCD\n"
	var nc NotifierConfig
	if err := yaml.Unmarshal([]byte(yamlSrc), &nc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if nc.Type != "slack" {
		t.Fatalf("Type = %q, want slack", nc.Type)
	}
	if got := nc.Settings["channel"]; got != "C0123ABCD" {
		t.Fatalf("Settings[channel] = %v, want C0123ABCD", got)
	}
	if _, leaked := nc.Settings["type"]; leaked {
		t.Fatal("Settings must not contain the type key")
	}
}
