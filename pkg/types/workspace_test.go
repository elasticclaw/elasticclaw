package types

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWorkspaceConfigNotifiersYAMLRoundTrip(t *testing.T) {
	want := WorkspaceConfig{
		Name: "example",
		Notifiers: map[string]NotifierConfig{
			"team-slack": {
				Type: "slack",
				Settings: map[string]any{
					"token_secret": "slack-bot-token",
					"channel":      "#agents",
				},
			},
		},
	}

	data, err := yaml.Marshal(&want)
	if err != nil {
		t.Fatalf("marshal workspace: %v", err)
	}
	text := string(data)
	if strings.Contains(text, "settings:") {
		t.Fatalf("marshaled notifier contains nested settings: %s", text)
	}
	for _, field := range []string{"type: slack", "token_secret: slack-bot-token", "channel: '#agents'"} {
		if !strings.Contains(text, field) {
			t.Fatalf("marshaled workspace does not contain %q: %s", field, text)
		}
	}

	var got WorkspaceConfig
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal workspace: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip workspace = %#v, want %#v", got, want)
	}
}

func TestWorkspaceConfigNotifierValidation(t *testing.T) {
	tests := []struct {
		name      string
		notifiers map[string]NotifierConfig
		want      string
	}{
		{
			name:      "missing type",
			notifiers: map[string]NotifierConfig{"team-slack": {}},
			want:      "notifier type is required",
		},
		{
			name:      "empty name",
			notifiers: map[string]NotifierConfig{"": {Type: "slack"}},
			want:      "notifier name is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := &WorkspaceConfig{Name: "example", Notifiers: tt.notifiers}
			err := workspace.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestWorkspaceConfigNotifiersJSONRoundTrip(t *testing.T) {
	want := WorkspaceConfig{
		Name: "example",
		Notifiers: map[string]NotifierConfig{
			"team-slack": {
				Type: "slack",
				Settings: map[string]any{
					"token_secret": "slack-bot-token",
					"channel":      "#agents",
				},
			},
		},
	}

	data, err := json.Marshal(&want)
	if err != nil {
		t.Fatalf("marshal workspace: %v", err)
	}
	var shape map[string]any
	if err := json.Unmarshal(data, &shape); err != nil {
		t.Fatalf("unmarshal JSON shape: %v", err)
	}
	notifier := shape["notifiers"].(map[string]any)["team-slack"].(map[string]any)
	if _, ok := notifier["settings"]; ok {
		t.Fatalf("marshaled notifier contains nested settings: %s", data)
	}
	if notifier["type"] != "slack" || notifier["token_secret"] != "slack-bot-token" || notifier["channel"] != "#agents" {
		t.Fatalf("marshaled notifier has unexpected shape: %#v", notifier)
	}

	var got WorkspaceConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal workspace: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip workspace = %#v, want %#v", got, want)
	}
}
