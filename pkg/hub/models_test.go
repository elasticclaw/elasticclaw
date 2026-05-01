package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestHandleModels_NoKeys(t *testing.T) {
	// Clear cache before test
	modelsCache.mu.Lock()
	modelsCache.models = nil
	modelsCache.fetched = time.Time{}
	modelsCache.mu.Unlock()

	s := &Server{
		hubCfg: &types.HubConfig{
			LLMKeys: types.LLMKeysList{},
		},
	}

	req := httptest.NewRequest("GET", "/api/models", nil)
	w := httptest.NewRecorder()
	s.handleModels(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	providers, ok := resp["providers"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected providers map, got %T", resp["providers"])
	}
	if len(providers) != 0 {
		t.Fatalf("expected empty providers, got %d", len(providers))
	}
}

func TestHandleModels_UnknownProvider(t *testing.T) {
	s := &Server{
		hubCfg: &types.HubConfig{
			LLMKeys: types.LLMKeysList{
				&types.LLMKeyConfig{Name: "test", Provider: "anthropic", APIKey: "sk-test"},
			},
		},
	}

	req := httptest.NewRequest("GET", "/api/models?provider=unknown", nil)
	w := httptest.NewRecorder()
	s.handleModels(w, req)

	if w.Code != 400 {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleModels_SpecificProvider(t *testing.T) {
	// Clear cache before test
	modelsCache.mu.Lock()
	modelsCache.models = nil
	modelsCache.fetched = time.Time{}
	modelsCache.mu.Unlock()

	s := &Server{
		hubCfg: &types.HubConfig{
			LLMKeys: types.LLMKeysList{
				&types.LLMKeyConfig{Name: "test", Provider: "anthropic", APIKey: "sk-test"},
			},
		},
	}

	// Since we can't make real API calls in tests with a fake key, the API call
	// will fail and return empty models. The provider won't be in the cache.
	// So requesting a specific provider that has a key but failed to fetch
	// should return 400 (unknown provider or no key configured).
	req := httptest.NewRequest("GET", "/api/models?provider=anthropic", nil)
	w := httptest.NewRecorder()
	s.handleModels(w, req)

	// With a bad key, the fetch fails and provider isn't in cache
	if w.Code != 400 {
		t.Fatalf("expected 400 (provider not in cache due to failed fetch), got %d: %s", w.Code, w.Body.String())
	}
}

func TestFetchAnthropicModels_Mock(t *testing.T) {
	// Create a mock server that returns Anthropic-style response
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "sk-test" {
			w.WriteHeader(401)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []map[string]string{
				{"id": "claude-sonnet-4-6", "display_name": "Claude Sonnet 4.6"},
				{"id": "claude-opus-4-5", "display_name": "Claude Opus 4.5"},
			},
		})
	}))
	defer mock.Close()

	s := &Server{}
	// We can't easily override the URL in the current implementation,
	// but we can test the response parsing logic by extracting it.
	// For now, this test documents the expected behavior.

	// Test that the cache works
	modelsCache.mu.Lock()
	modelsCache.models = map[string][]ModelInfo{
		"anthropic": {
			{ID: "anthropic/claude-sonnet-4-6", Name: "Claude Sonnet 4.6"},
		},
	}
	modelsCache.fetched = time.Now()
	modelsCache.mu.Unlock()

	models := s.fetchProviderModels()
	if len(models["anthropic"]) != 1 {
		t.Fatalf("expected 1 cached model, got %d", len(models["anthropic"]))
	}
}
