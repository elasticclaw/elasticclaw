package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	providerModelsTTL        = time.Hour
	providerModelsFailureTTL = 5 * time.Minute
	providerModelsTimeout    = 10 * time.Second
)

type providerModelsCacheEntry struct {
	options []LLMModelOption
	until   time.Time
}

// modelOptionsByProvider lists the selectable models of every provider whose
// API exposes them. A provider missing from the result (no key, or the listing
// failed) falls back to the dashboard's built-in list.
func (s *Server) modelOptionsByProvider(ctx context.Context, apiKeys map[string]string) map[string][]LLMModelOption {
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out = map[string][]LLMModelOption{}
	)
	set := func(provider string, options []LLMModelOption) {
		if len(options) == 0 {
			return
		}
		mu.Lock()
		out[provider] = options
		mu.Unlock()
	}
	wg.Add(3)
	go func() {
		defer wg.Done()
		set("fireworks", s.fireworksModelOptions(ctx, apiKeys["fireworks"]))
	}()
	go func() {
		defer wg.Done()
		set("anthropic", s.cachedProviderModelOptions(ctx, "anthropic", apiKeys["anthropic"], "Custom Anthropic model", s.fetchAnthropicModelOptions))
	}()
	go func() {
		defer wg.Done()
		set("openai", s.cachedProviderModelOptions(ctx, "openai", apiKeys["openai"], "Custom OpenAI model", s.fetchOpenAIModelOptions))
	}()
	wg.Wait()
	return out
}

// cachedProviderModelOptions returns the provider's live model list, cached
// per key for an hour. Failures are cached briefly too, so a bad key does not
// stall every settings page load on the provider's timeout.
func (s *Server) cachedProviderModelOptions(ctx context.Context, provider, apiKey, customName string, fetch func(context.Context, string) ([]LLMModelOption, error)) []LLMModelOption {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil
	}
	cacheKey := provider + "\x00" + apiKey
	now := time.Now()

	s.providerModelsMu.Lock()
	if entry, ok := s.providerModelsCache[cacheKey]; ok && now.Before(entry.until) {
		s.providerModelsMu.Unlock()
		return cloneModelOptions(entry.options)
	}
	s.providerModelsMu.Unlock()

	options, err := fetch(ctx, apiKey)
	ttl := providerModelsTTL
	if err != nil || len(options) == 0 {
		if err != nil {
			log.Printf("list %s models: %v", provider, err)
		}
		options, ttl = nil, providerModelsFailureTTL
	} else {
		options = appendCustomModelOption(options, customName)
	}

	s.providerModelsMu.Lock()
	if s.providerModelsCache == nil {
		s.providerModelsCache = map[string]providerModelsCacheEntry{}
	}
	s.providerModelsCache[cacheKey] = providerModelsCacheEntry{options: cloneModelOptions(options), until: now.Add(ttl)}
	s.providerModelsMu.Unlock()

	return options
}

// fetchAnthropicModelOptions lists models from GET /v1/models, which already
// returns them newest first with a display name.
func (s *Server) fetchAnthropicModelOptions(ctx context.Context, apiKey string) ([]LLMModelOption, error) {
	baseURL := strings.TrimRight(s.anthropicBaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	var options []LLMModelOption
	afterID := ""
	for {
		q := url.Values{"limit": {"1000"}}
		if afterID != "" {
			q.Set("after_id", afterID)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/models?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("anthropic-version", "2023-06-01")
		if strings.HasPrefix(apiKey, "sk-ant-oat") {
			req.Header.Set("Authorization", "Bearer "+apiKey)
			req.Header.Set("anthropic-beta", "oauth-2025-04-20")
		} else {
			req.Header.Set("x-api-key", apiKey)
		}

		var body struct {
			Data []struct {
				ID          string `json:"id"`
				DisplayName string `json:"display_name"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err := getProviderJSON(req, "anthropic", &body); err != nil {
			return nil, err
		}
		for _, model := range body.Data {
			id := strings.TrimSpace(model.ID)
			if id == "" {
				continue
			}
			name := strings.TrimSpace(model.DisplayName)
			if name == "" {
				name = id
			}
			options = append(options, LLMModelOption{ID: "anthropic/" + id, Name: name})
		}
		if !body.HasMore || body.LastID == "" || body.LastID == afterID {
			break
		}
		afterID = body.LastID
	}
	return dedupeModelOptions(options), nil
}

// fetchOpenAIModelOptions lists models from GET /v1/models, keeping only the
// chat models an agent can run, newest first.
func (s *Server) fetchOpenAIModelOptions(ctx context.Context, apiKey string) ([]LLMModelOption, error) {
	baseURL := strings.TrimRight(s.openAIBaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	var body struct {
		Data []struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
		} `json:"data"`
	}
	if err := getProviderJSON(req, "openai", &body); err != nil {
		return nil, err
	}

	models := body.Data[:0]
	for _, model := range body.Data {
		if openAIChatModel(model.ID) {
			models = append(models, model)
		}
	}
	sort.SliceStable(models, func(i, j int) bool {
		if models[i].Created != models[j].Created {
			return models[i].Created > models[j].Created
		}
		return models[i].ID < models[j].ID
	})

	options := make([]LLMModelOption, 0, len(models))
	for _, model := range models {
		options = append(options, LLMModelOption{ID: "openai/" + model.ID, Name: humanizeOpenAIModelName(model.ID)})
	}
	return dedupeModelOptions(options), nil
}

var (
	openAIChatModelRE    = regexp.MustCompile(`^(gpt-\d|o\d)`)
	openAIDatedSnapshot  = regexp.MustCompile(`-(\d{4}-\d{2}-\d{2}|\d{4})$`)
	openAINonChatMarkers = []string{"audio", "realtime", "tts", "transcribe", "image", "search", "embedding", "instruct", "moderation", "diarize"}
)

// openAIChatModel drops embeddings, audio, image and other non-chat models,
// plus dated snapshots of a model whose alias is already listed.
func openAIChatModel(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	if !openAIChatModelRE.MatchString(id) || openAIDatedSnapshot.MatchString(id) {
		return false
	}
	for _, marker := range openAINonChatMarkers {
		if strings.Contains(id, marker) {
			return false
		}
	}
	return true
}

// humanizeOpenAIModelName turns "gpt-5.6-sol" into "GPT-5.6 Sol". Reasoning
// models such as "o4-mini" keep their id, which is how OpenAI names them.
func humanizeOpenAIModelName(id string) string {
	rest, ok := strings.CutPrefix(id, "gpt-")
	if !ok {
		return id
	}
	words := strings.Split(rest, "-")
	for i := 1; i < len(words); i++ {
		if words[i] != "" {
			words[i] = strings.ToUpper(words[i][:1]) + words[i][1:]
		}
	}
	return "GPT-" + strings.Join(words, " ")
}

func getProviderJSON(req *http.Request, provider string, out any) error {
	resp, err := (&http.Client{Timeout: providerModelsTimeout}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s list models returned %d", provider, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
