package types

import (
	"strings"
	"testing"
)

func TestValidateSlackNotificationsConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *SlackNotificationsConfig
		wantErr bool
		errMsg  string
	}{
		{
			name: "nil config is valid",
			cfg:  nil,
		},
		{
			name: "valid enabled config",
			cfg: &SlackNotificationsConfig{
				Enabled:     true,
				BotTokenRef: "slack_bot_token",
				Channel:     "C0123ABCD",
			},
		},
		{
			name: "disabled config skips required fields",
			cfg:  &SlackNotificationsConfig{Enabled: false},
		},
		{
			name: "missing bot token ref",
			cfg: &SlackNotificationsConfig{
				Enabled: true,
				Channel: "C0123ABCD",
			},
			wantErr: true,
			errMsg:  "bot_token_ref is required",
		},
		{
			name: "missing channel",
			cfg: &SlackNotificationsConfig{
				Enabled:     true,
				BotTokenRef: "slack_bot_token",
			},
			wantErr: true,
			errMsg:  "channel is required",
		},
		{
			name: "channel name with hash rejected",
			cfg: &SlackNotificationsConfig{
				Enabled:     true,
				BotTokenRef: "slack_bot_token",
				Channel:     "#deploys",
			},
			wantErr: true,
			errMsg:  "use the channel ID",
		},
		{
			name: "channel that is not a Slack ID rejected",
			cfg: &SlackNotificationsConfig{
				Enabled:     true,
				BotTokenRef: "slack_bot_token",
				Channel:     "deploys",
			},
			wantErr: true,
			errMsg:  "does not look like a Slack channel ID",
		},
		{
			name: "private group and DM IDs accepted",
			cfg: &SlackNotificationsConfig{
				Enabled:     true,
				BotTokenRef: "slack_bot_token",
				Channel:     "G0123ABCD",
			},
		},
		{
			name: "unparseable poll interval",
			cfg: &SlackNotificationsConfig{
				Enabled:      true,
				BotTokenRef:  "slack_bot_token",
				Channel:      "C0123ABCD",
				PollInterval: "sometimes",
			},
			wantErr: true,
			errMsg:  "invalid poll_interval",
		},
		{
			name: "poll interval below 1s",
			cfg: &SlackNotificationsConfig{
				Enabled:      true,
				BotTokenRef:  "slack_bot_token",
				Channel:      "C0123ABCD",
				PollInterval: "200ms",
			},
			wantErr: true,
			errMsg:  "at least 1s",
		},
		{
			name: "poll interval validated even when disabled",
			cfg: &SlackNotificationsConfig{
				PollInterval: "nope",
			},
			wantErr: true,
			errMsg:  "invalid poll_interval",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSlackNotificationsConfig(tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ValidateSlackNotificationsConfig() succeeded, want error")
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.errMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateSlackNotificationsConfig() error = %v", err)
			}
		})
	}
}

func TestHubConfigValidateWiresSlackNotifications(t *testing.T) {
	cfg := &HubConfig{
		Notifications: &NotificationsConfig{
			Slack: &SlackNotificationsConfig{
				Enabled: true,
				Channel: "#name",
			},
		},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "use the channel ID") {
		t.Fatalf("HubConfig.Validate() = %v, want slack channel error", err)
	}
}
