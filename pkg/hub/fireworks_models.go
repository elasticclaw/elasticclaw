package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const defaultFireworksModel = "fireworks/accounts/fireworks/models/kimi-k2p7"

type LLMModelOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

var fallbackFireworksModelOptions = []LLMModelOption{
	{ID: defaultFireworksModel, Name: "Kimi K2.7"},
	{ID: "fireworks/accounts/fireworks/models/glm-5p2", Name: "GLM 5.2"},
	{ID: "fireworks/accounts/fireworks/models/deepseek-v4-pro", Name: "DeepSeek V4 Pro"},
	{ID: "fireworks/accounts/fireworks/models/deepseek-v4-flash", Name: "DeepSeek V4 Flash"},
	{ID: "fireworks/accounts/fireworks/models/minimax-m2p7", Name: "MiniMax M2.7"},
	{ID: "fireworks/accounts/fireworks/models/qwen3p6-plus", Name: "Qwen3.6 Plus"},
	{ID: "fireworks/accounts/fireworks/models/gpt-oss-120b", Name: "OpenAI gpt-oss-120b"},
	{ID: "fireworks/accounts/fireworks/models/gpt-oss-20b", Name: "OpenAI gpt-oss-20b"},
	{ID: "fireworks/accounts/fireworks/models/llama-v3p3-70b-instruct", Name: "Llama 3.3 70B Instruct"},
	{ID: "__custom", Name: "Custom Fireworks model"},
}

var (
	kimiVersionRE          = regexp.MustCompile(`k(?:imi[-_ ]*)?2(?:p|\.)(\d+)`)
	decimalVersionRE       = regexp.MustCompile(`(?:p|\.)(\d+)`)
	slugVersionSeparatorRE = regexp.MustCompile(`(\d)p(\d)`)
)

func (s *Server) fireworksModelOptions(ctx context.Context, apiKey string) []LLMModelOption {
	fallback := cloneModelOptions(fallbackFireworksModelOptions)
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return fallback
	}

	cacheKey := apiKey
	now := time.Now()
	s.fireworksModelsMu.Lock()
	if s.fireworksModelsCacheKey == cacheKey && now.Before(s.fireworksModelsCacheUntil) && len(s.fireworksModelsCache) > 0 {
		cached := cloneModelOptions(s.fireworksModelsCache)
		s.fireworksModelsMu.Unlock()
		return cached
	}
	s.fireworksModelsMu.Unlock()

	options, err := s.fetchFireworksModelOptions(ctx, apiKey)
	if err != nil || len(options) == 0 {
		return fallback
	}
	options = appendCustomModelOption(options, "Custom Fireworks model")

	s.fireworksModelsMu.Lock()
	s.fireworksModelsCacheKey = cacheKey
	s.fireworksModelsCache = cloneModelOptions(options)
	s.fireworksModelsCacheUntil = now.Add(1 * time.Hour)
	s.fireworksModelsMu.Unlock()

	return options
}

func (s *Server) fetchFireworksModelOptions(ctx context.Context, apiKey string) ([]LLMModelOption, error) {
	baseURL := strings.TrimRight(s.fireworksBaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.fireworks.ai"
	}

	var options []LLMModelOption
	pageToken := ""
	for {
		u, err := url.Parse(baseURL + "/v1/accounts/fireworks/models")
		if err != nil {
			return nil, err
		}
		q := u.Query()
		q.Set("pageSize", "200")
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		u.RawQuery = q.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)

		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}

		var body struct {
			Models []struct {
				Name               string `json:"name"`
				DisplayName        string `json:"displayName"`
				Public             bool   `json:"public"`
				ContextLength      int    `json:"contextLength"`
				SupportsServerless bool   `json:"supportsServerless"`
				ConversationConfig struct {
					Style    string `json:"style"`
					System   string `json:"system"`
					Template string `json:"template"`
				} `json:"conversationConfig"`
			} `json:"models"`
			NextPageToken string `json:"nextPageToken"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("fireworks list models returned %d", resp.StatusCode)
		}
		if decodeErr != nil {
			return nil, decodeErr
		}

		for _, model := range body.Models {
			if !fireworksModelSupported(model.Public, model.SupportsServerless, model.ContextLength, model.ConversationConfig.Style, model.ConversationConfig.System, model.ConversationConfig.Template) {
				continue
			}
			id := fireworksModelID(model.Name)
			if id == "" {
				continue
			}
			name := strings.TrimSpace(model.DisplayName)
			if name == "" {
				name = humanizeFireworksModelName(id)
			}
			options = append(options, LLMModelOption{ID: id, Name: name})
		}

		pageToken = strings.TrimSpace(body.NextPageToken)
		if pageToken == "" {
			break
		}
	}

	return sortFireworksModelOptions(dedupeModelOptions(options)), nil
}

func fireworksModelSupported(public, supportsServerless bool, contextLength int, conversationStyle, conversationSystem, conversationTemplate string) bool {
	if !public || !supportsServerless || contextLength <= 0 {
		return false
	}
	return strings.TrimSpace(conversationStyle) != "" ||
		strings.TrimSpace(conversationSystem) != "" ||
		strings.TrimSpace(conversationTemplate) != ""
}

func fireworksModelID(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if strings.HasPrefix(name, "fireworks/") {
		return name
	}
	if strings.HasPrefix(name, "accounts/") {
		return "fireworks/" + name
	}
	return "fireworks/accounts/fireworks/models/" + strings.TrimPrefix(name, "/")
}

func dedupeModelOptions(options []LLMModelOption) []LLMModelOption {
	seen := make(map[string]bool, len(options))
	out := make([]LLMModelOption, 0, len(options))
	for _, option := range options {
		if option.ID == "" || seen[option.ID] {
			continue
		}
		seen[option.ID] = true
		out = append(out, option)
	}
	return out
}

func appendCustomModelOption(options []LLMModelOption, name string) []LLMModelOption {
	options = dedupeModelOptions(options)
	for _, option := range options {
		if option.ID == "__custom" {
			return options
		}
	}
	return append(options, LLMModelOption{ID: "__custom", Name: name})
}

func cloneModelOptions(options []LLMModelOption) []LLMModelOption {
	if len(options) == 0 {
		return nil
	}
	return append([]LLMModelOption(nil), options...)
}

func sortFireworksModelOptions(options []LLMModelOption) []LLMModelOption {
	sort.SliceStable(options, func(i, j int) bool {
		ai, aj := fireworksModelRank(options[i]), fireworksModelRank(options[j])
		if ai != aj {
			return ai < aj
		}
		return strings.ToLower(options[i].Name) < strings.ToLower(options[j].Name)
	})
	return options
}

func fireworksModelRank(option LLMModelOption) int {
	normalized := strings.ToLower(option.ID + " " + option.Name)
	if strings.Contains(normalized, "kimi") {
		return 0 - parseKimiVersion(normalized)
	}
	if strings.Contains(normalized, "glm") {
		return 100 - parseDecimalVersion(normalized)
	}
	return 1000
}

func parseKimiVersion(value string) int {
	if match := kimiVersionRE.FindStringSubmatch(value); len(match) == 2 {
		n, _ := strconv.Atoi(match[1])
		return n
	}
	return 0
}

func parseDecimalVersion(value string) int {
	if match := decimalVersionRE.FindStringSubmatch(value); len(match) == 2 {
		n, _ := strconv.Atoi(match[1])
		return n
	}
	return 0
}

func humanizeFireworksModelName(id string) string {
	parts := strings.Split(strings.TrimSuffix(id, "/"), "/")
	if len(parts) == 0 {
		return id
	}
	slug := parts[len(parts)-1]
	slug = slugVersionSeparatorRE.ReplaceAllString(slug, "$1.$2")
	words := strings.FieldsFunc(slug, func(r rune) bool {
		return r == '-' || r == '_'
	})
	for i, word := range words {
		if word == "" {
			continue
		}
		words[i] = strings.ToUpper(word[:1]) + word[1:]
	}
	return strings.Join(words, " ")
}
