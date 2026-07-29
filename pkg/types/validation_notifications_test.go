package types

import (
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
			name: "valid lifecycle wiring",
			cfg: &NotificationsConfig{
				Notifiers: slack(),
				Lifecycle: &LifecycleNotificationsConfig{Via: "eng-agents"},
			},
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
