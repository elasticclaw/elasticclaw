package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestFetchAnthropicModelOptionsPaginatesInOrder(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "sk-ant-test" {
			t.Fatalf("x-api-key = %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got == "" {
			t.Fatal("missing anthropic-version")
		}
		resp := map[string]any{}
		switch r.URL.Query().Get("after_id") {
		case "":
			resp["data"] = []map[string]string{
				{"id": "claude-sonnet-5-5", "display_name": "Claude Sonnet 5.5"},
				{"id": "claude-opus-5"},
			}
			resp["has_more"] = true
			resp["last_id"] = "claude-opus-5"
		case "claude-opus-5":
			resp["data"] = []map[string]string{{"id": "claude-sonnet-4-6", "display_name": "Claude Sonnet 4.6"}}
		default:
			t.Fatalf("unexpected after_id %q", r.URL.Query().Get("after_id"))
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer api.Close()

	s, _ := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")
	s.anthropicBaseURL = api.URL

	options, err := s.fetchAnthropicModelOptions(t.Context(), "sk-ant-test")
	if err != nil {
		t.Fatalf("fetch options: %v", err)
	}
	want := []LLMModelOption{
		{ID: "anthropic/claude-sonnet-5-5", Name: "Claude Sonnet 5.5"},
		{ID: "anthropic/claude-opus-5", Name: "claude-opus-5"},
		{ID: "anthropic/claude-sonnet-4-6", Name: "Claude Sonnet 4.6"},
	}
	if !reflect.DeepEqual(options, want) {
		t.Fatalf("options = %#v, want %#v", options, want)
	}
}

func TestFetchAnthropicModelOptionsUsesBearerForOAuthTokens(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-ant-oat01-test" {
			t.Fatalf("authorization = %q", got)
		}
		if got := r.Header.Get("x-api-key"); got != "" {
			t.Fatalf("x-api-key = %q, want unset", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "claude-sonnet-5-5"}}})
	}))
	defer api.Close()

	s, _ := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")
	s.anthropicBaseURL = api.URL

	if _, err := s.fetchAnthropicModelOptions(t.Context(), "sk-ant-oat01-test"); err != nil {
		t.Fatalf("fetch options: %v", err)
	}
}

func TestFetchOpenAIModelOptionsKeepsChatModelsNewestFirst(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Fatalf("authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "gpt-5.5", "created": 100},
			{"id": "gpt-5.6-sol", "created": 200},
			{"id": "o4-mini", "created": 50},
			{"id": "gpt-5.5-2026-04-01", "created": 90},
			{"id": "gpt-4-0613", "created": 10},
			{"id": "gpt-4o-realtime-preview", "created": 80},
			{"id": "gpt-image-1", "created": 80},
			{"id": "text-embedding-3-large", "created": 80},
			{"id": "whisper-1", "created": 80},
			{"id": "dall-e-3", "created": 80},
		}})
	}))
	defer api.Close()

	s, _ := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")
	s.openAIBaseURL = api.URL

	options, err := s.fetchOpenAIModelOptions(t.Context(), "sk-test")
	if err != nil {
		t.Fatalf("fetch options: %v", err)
	}
	want := []LLMModelOption{
		{ID: "openai/gpt-5.6-sol", Name: "GPT-5.6 Sol"},
		{ID: "openai/gpt-5.5", Name: "GPT-5.5"},
		{ID: "openai/o4-mini", Name: "o4-mini"},
	}
	if !reflect.DeepEqual(options, want) {
		t.Fatalf("options = %#v, want %#v", options, want)
	}
}

func TestGetSettingsIncludesDynamicAnthropicAndOpenAIModelOptions(t *testing.T) {
	var anthropicCalls int
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropicCalls++
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "claude-sonnet-5-5", "display_name": "Claude Sonnet 5.5"}}})
	}))
	defer anthropic.Close()
	openai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer openai.Close()

	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		LLMKeys: types.LLMKeysList{
			{Name: "anthropic-main", Provider: "anthropic", APIKey: "sk-ant-test", Default: true},
			{Name: "openai-main", Provider: "openai", APIKey: "sk-bad"},
		},
	}, "", "", "")
	s.anthropicBaseURL = anthropic.URL
	s.openAIBaseURL = openai.URL

	for range 2 {
		rec := httptest.NewRecorder()
		s.getSettings(rec, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var view SettingsView
		if err := json.NewDecoder(rec.Body).Decode(&view); err != nil {
			t.Fatal(err)
		}
		want := []LLMModelOption{
			{ID: "anthropic/claude-sonnet-5-5", Name: "Claude Sonnet 5.5"},
			{ID: "__custom", Name: "Custom Anthropic model"},
		}
		if got := view.ModelOptions["anthropic"]; !reflect.DeepEqual(got, want) {
			t.Fatalf("anthropic options = %#v, want %#v", got, want)
		}
		if got, ok := view.ModelOptions["openai"]; ok {
			t.Fatalf("openai options = %#v, want omitted so the dashboard falls back", got)
		}
	}
	if anthropicCalls != 1 {
		t.Fatalf("anthropic calls = %d, want 1 (cached)", anthropicCalls)
	}
}

func TestOpenAIChatModel(t *testing.T) {
	for id, want := range map[string]bool{
		"gpt-5.6-sol":            true,
		"gpt-5.3-codex":          true,
		"o3":                     true,
		"gpt-5.5-2026-04-01":     false,
		"gpt-4o-mini-tts":        false,
		"gpt-4o-search-preview":  false,
		"omni-moderation-latest": false,
		"babbage-002":            false,
	} {
		if got := openAIChatModel(id); got != want {
			t.Errorf("openAIChatModel(%q) = %v, want %v", id, got, want)
		}
	}
}
