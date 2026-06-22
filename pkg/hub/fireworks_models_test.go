package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestFetchFireworksModelOptionsFiltersAndSortsLatestKimi(t *testing.T) {
	var requests int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("Authorization"); got != "Bearer fw-test" {
			t.Fatalf("authorization = %q", got)
		}
		if got := r.URL.Path; got != "/v1/accounts/fireworks/models" {
			t.Fatalf("path = %q", got)
		}
		if got := r.URL.Query().Get("pageSize"); got != "200" {
			t.Fatalf("pageSize = %q", got)
		}

		type model struct {
			Name               string         `json:"name"`
			DisplayName        string         `json:"displayName,omitempty"`
			Public             bool           `json:"public"`
			ContextLength      int            `json:"contextLength"`
			SupportsServerless bool           `json:"supportsServerless"`
			ConversationConfig map[string]any `json:"conversationConfig,omitempty"`
		}
		resp := map[string]any{}
		switch r.URL.Query().Get("pageToken") {
		case "":
			resp["models"] = []model{
				{Name: "accounts/fireworks/models/kimi-k2p6", DisplayName: "Kimi K2.6", Public: true, ContextLength: 131072, SupportsServerless: true, ConversationConfig: map[string]any{"template": "{{ .Prompt }}"}},
				{Name: "accounts/fireworks/models/glm-5p2", DisplayName: "GLM 5.2", Public: true, ContextLength: 131072, SupportsServerless: true, ConversationConfig: map[string]any{"template": "{{ .Prompt }}"}},
				{Name: "accounts/fireworks/models/private-kimi", DisplayName: "Private Kimi", Public: false, ContextLength: 131072, SupportsServerless: true, ConversationConfig: map[string]any{"template": "{{ .Prompt }}"}},
				{Name: "accounts/fireworks/models/no-chat-template", DisplayName: "No Chat Template", Public: true, ContextLength: 131072, SupportsServerless: true},
			}
			resp["nextPageToken"] = "next"
		case "next":
			resp["models"] = []model{
				{Name: "accounts/fireworks/models/kimi-k2p7", DisplayName: "Kimi K2.7", Public: true, ContextLength: 131072, SupportsServerless: true, ConversationConfig: map[string]any{"template": "{{ .Prompt }}"}},
				{Name: "accounts/fireworks/models/batch-only", DisplayName: "Batch Only", Public: true, ContextLength: 131072, SupportsServerless: false, ConversationConfig: map[string]any{"template": "{{ .Prompt }}"}},
			}
		default:
			t.Fatalf("unexpected pageToken %q", r.URL.Query().Get("pageToken"))
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatal(err)
		}
	}))
	defer api.Close()

	s, _ := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")
	s.fireworksBaseURL = api.URL

	options, err := s.fetchFireworksModelOptions(t.Context(), "fw-test")
	if err != nil {
		t.Fatalf("fetch options: %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if len(options) != 3 {
		t.Fatalf("options = %#v", options)
	}
	if options[0].ID != defaultFireworksModel {
		t.Fatalf("first option = %#v, want latest Kimi", options[0])
	}
	if options[1].ID != "fireworks/accounts/fireworks/models/kimi-k2p6" {
		t.Fatalf("second option = %#v, want previous Kimi", options[1])
	}
	if options[2].ID != "fireworks/accounts/fireworks/models/glm-5p2" {
		t.Fatalf("third option = %#v, want GLM 5.2", options[2])
	}
}

func TestGetSettingsIncludesDynamicFireworksModelOptions(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{
					"name":               "accounts/fireworks/models/kimi-k2p7",
					"displayName":        "Kimi K2.7",
					"public":             true,
					"contextLength":      131072,
					"supportsServerless": true,
					"conversationConfig": map[string]any{"template": "{{ .Prompt }}"},
				},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}))
	defer api.Close()

	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		LLMKeys: types.LLMKeysList{
			{Name: "fireworks-main", Provider: "fireworks", APIKey: "fw-test", Default: true},
		},
	}, "", "", "")
	s.fireworksBaseURL = api.URL

	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	rec := httptest.NewRecorder()
	s.getSettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var view SettingsView
	if err := json.NewDecoder(rec.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	options := view.ModelOptions["fireworks"]
	if len(options) != 2 {
		t.Fatalf("fireworks options = %#v", options)
	}
	if options[0].ID != defaultFireworksModel {
		t.Fatalf("first option = %#v, want dynamic Kimi", options[0])
	}
	if options[1].ID != "__custom" {
		t.Fatalf("last option = %#v, want custom option", options[1])
	}
}

func TestFireworksModelOptionsFallsBackWithoutAPIKey(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")
	options := s.fireworksModelOptions(t.Context(), "")
	if len(options) == 0 || options[0].ID != defaultFireworksModel {
		t.Fatalf("fallback options = %#v", options)
	}
}

func TestHumanizeFireworksModelNameOnlyReplacesVersionSeparator(t *testing.T) {
	tests := map[string]string{
		"fireworks/accounts/fireworks/models/deepseek-v4-pro": "Deepseek V4 Pro",
		"fireworks/accounts/fireworks/models/qwen3p6-plus":    "Qwen3.6 Plus",
		"fireworks/accounts/fireworks/models/gpt-oss-120b":    "Gpt Oss 120b",
		"fireworks/accounts/fireworks/models/kimi-k2p7":       "Kimi K2.7",
	}
	for id, want := range tests {
		if got := humanizeFireworksModelName(id); got != want {
			t.Fatalf("humanizeFireworksModelName(%q) = %q, want %q", id, got, want)
		}
	}
}
