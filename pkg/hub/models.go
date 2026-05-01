package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ModelInfo represents a single model available for a provider.
type ModelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// providerModelsCache holds cached model lists per provider.
type providerModelsCache struct {
	mu      sync.RWMutex
	models  map[string][]ModelInfo
	fetched time.Time
}

var modelsCache = &providerModelsCache{}

const modelsCacheTTL = 5 * time.Minute

// fetchProviderModels fetches available models from each provider API using the configured keys.
func (s *Server) fetchProviderModels() map[string][]ModelInfo {
	modelsCache.mu.RLock()
	if modelsCache.models != nil && time.Since(modelsCache.fetched) < modelsCacheTTL {
		defer modelsCache.mu.RUnlock()
		return modelsCache.models
	}
	modelsCache.mu.RUnlock()

	modelsCache.mu.Lock()
	defer modelsCache.mu.Unlock()

	// Double-check after acquiring write lock
	if modelsCache.models != nil && time.Since(modelsCache.fetched) < modelsCacheTTL {
		return modelsCache.models
	}

	result := make(map[string][]ModelInfo)

	s.mu.RLock()
	keys := s.hubCfg.LLMKeys
	s.mu.RUnlock()

	for _, k := range keys {
		if k.APIKey == "" {
			continue
		}
		models := s.fetchModelsForProvider(k.Provider, k.APIKey)
		if len(models) > 0 {
			result[k.Provider] = models
		}
	}

	modelsCache.models = result
	modelsCache.fetched = time.Now()
	return result
}

// fetchModelsForProvider calls the provider's model list API.
func (s *Server) fetchModelsForProvider(provider, apiKey string) []ModelInfo {
	switch provider {
	case "anthropic":
		return s.fetchAnthropicModels(apiKey)
	case "fireworks":
		return s.fetchFireworksModels(apiKey)
	case "openai":
		return s.fetchOpenAIModels(apiKey)
	case "groq":
		return s.fetchGroqModels(apiKey)
	case "deepseek":
		return s.fetchDeepSeekModels(apiKey)
	default:
		return nil
	}
}

// anthropicModelsResponse matches Anthropic's /v1/models response.
type anthropicModelsResponse struct {
	Data []struct {
		ID   string `json:"id"`
		Name string `json:"display_name"` // may be empty
	} `json:"data"`
}

func (s *Server) fetchAnthropicModels(apiKey string) []ModelInfo {
	req, err := http.NewRequest("GET", "https://api.anthropic.com/v1/models", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil
	}

	var data anthropicModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil
	}

	models := make([]ModelInfo, 0, len(data.Data))
	for _, m := range data.Data {
		name := m.Name
		if name == "" {
			name = m.ID
		}
		models = append(models, ModelInfo{ID: "anthropic/" + m.ID, Name: name})
	}
	return models
}

// fireworksModelsResponse matches Fireworks' /v1/accounts/{account_id}/models response.
type fireworksModelsResponse struct {
	Models []struct {
		Name        string `json:"name"`         // e.g. "accounts/my-account/models/my-model"
		DisplayName string `json:"displayName"`  // human-readable
	} `json:"models"`
}

func (s *Server) fetchFireworksModels(apiKey string) []ModelInfo {
	// Fireworks uses account-specific endpoints. We use "fireworks" as the account
	// for their public models, or we can list from the user's account.
	// The public endpoint is: https://api.fireworks.ai/inference/v1/models
	// But the docs show /v1/accounts/{account_id}/models. Let's try the inference endpoint.
	req, err := http.NewRequest("GET", "https://api.fireworks.ai/inference/v1/models", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil
	}

	var data fireworksModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil
	}

	models := make([]ModelInfo, 0, len(data.Models))
	for _, m := range data.Models {
		name := m.DisplayName
		if name == "" {
			// Extract model name from resource path
			parts := strings.Split(m.Name, "/")
			if len(parts) > 0 {
				name = parts[len(parts)-1]
			} else {
				name = m.Name
			}
		}
		models = append(models, ModelInfo{ID: "fireworks/" + m.Name, Name: name})
	}
	return models
}

// openAIModelsResponse matches OpenAI's /v1/models response.
type openAIModelsResponse struct {
	Data []struct {
		ID      string `json:"id"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
}

func (s *Server) fetchOpenAIModels(apiKey string) []ModelInfo {
	req, err := http.NewRequest("GET", "https://api.openai.com/v1/models", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil
	}

	var data openAIModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil
	}

	models := make([]ModelInfo, 0, len(data.Data))
	for _, m := range data.Data {
		// Skip non-OpenAI models and non-chat models to keep the list reasonable
		if !strings.HasPrefix(m.ID, "gpt-") && !strings.HasPrefix(m.ID, "o1") && !strings.HasPrefix(m.ID, "o3") {
			continue
		}
		name := m.ID
		if m.OwnedBy != "" && m.OwnedBy != "openai" {
			name = fmt.Sprintf("%s (%s)", m.ID, m.OwnedBy)
		}
		models = append(models, ModelInfo{ID: "openai/" + m.ID, Name: name})
	}
	return models
}

// groqModelsResponse matches Groq's /openai/v1/models response.
type groqModelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

func (s *Server) fetchGroqModels(apiKey string) []ModelInfo {
	req, err := http.NewRequest("GET", "https://api.groq.com/openai/v1/models", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil
	}

	var data groqModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil
	}

	models := make([]ModelInfo, 0, len(data.Data))
	for _, m := range data.Data {
		models = append(models, ModelInfo{ID: "groq/" + m.ID, Name: m.ID})
	}
	return models
}

// deepseekModelsResponse matches DeepSeek's /v1/models response.
type deepseekModelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

func (s *Server) fetchDeepSeekModels(apiKey string) []ModelInfo {
	req, err := http.NewRequest("GET", "https://api.deepseek.com/v1/models", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil
	}

	var data deepseekModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil
	}

	models := make([]ModelInfo, 0, len(data.Data))
	for _, m := range data.Data {
		models = append(models, ModelInfo{ID: "deepseek/" + m.ID, Name: m.ID})
	}
	return models
}

// handleModels returns the available models for all configured providers or a specific provider.
// GET /api/models — returns all providers and their models
// GET /api/models?provider=anthropic — returns models for a specific provider
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	provider := r.URL.Query().Get("provider")
	allModels := s.fetchProviderModels()

	if provider != "" {
		models, ok := allModels[provider]
		if !ok {
			http.Error(w, "unknown provider or no key configured", http.StatusBadRequest)
			return
		}
		jsonOK(w, map[string]interface{}{
			"provider": provider,
			"models":   models,
		})
		return
	}

	jsonOK(w, map[string]interface{}{
		"providers": allModels,
	})
}
