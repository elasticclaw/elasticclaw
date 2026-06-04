package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

const (
	failureSummaryInputLimit = 6000
	failureCommentLimit      = 1800
)

var (
	htmlScriptRE      = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	htmlStyleRE       = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	htmlTagRE         = regexp.MustCompile(`(?is)<[^>]{1,200}>`)
	envAssignmentRE   = regexp.MustCompile(`(?m)\b[A-Za-z_][A-Za-z0-9_]*(KEY|TOKEN|SECRET|PASSWORD|PASS|CREDENTIAL|AUTH|COOKIE)\b\s*=\s*("[^"]*"|'[^']*'|[^\s]+)`)
	bearerRE          = regexp.MustCompile(`(?i)\b(bearer|token|api[-_ ]?key|password|secret)\s+([A-Za-z0-9._~+/=-]{12,})`)
	longOpaqueRE      = regexp.MustCompile(`\b[A-Za-z0-9+/=_-]{80,}\b`)
	pemBlockRE        = regexp.MustCompile(`(?s)-----BEGIN [^-]+-----.*?-----END [^-]+-----`)
	secretLikeValueRE = regexp.MustCompile(`(?i)(sk-[A-Za-z0-9_-]{12,}|gh[pousr]_[A-Za-z0-9_]{12,}|xox[baprs]-[A-Za-z0-9-]{12,})`)
)

func (s *Server) buildAgentStopComment(clawID, reason string) string {
	sanitized := sanitizeFailureDetails(reason)

	s.mu.RLock()
	llmKeys := cloneLLMKeys(s.hubCfg.LLMKeys)
	defaultModel := s.hubCfg.DefaultModel
	s.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if summary, err := summarizeFailureWithLLM(ctx, sanitized, llmKeys, defaultModel); err == nil {
		summary = sanitizeFailureDetails(summary)
		if summary != "" {
			return clampFailureComment("Agent stopped unexpectedly.\n\n" + summary)
		}
	}

	return fallbackFailureSummary(clawID, sanitized)
}

func sanitizeFailureDetails(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = pemBlockRE.ReplaceAllString(s, "[redacted private key/certificate]")
	s = htmlScriptRE.ReplaceAllString(s, " ")
	s = htmlStyleRE.ReplaceAllString(s, " ")
	s = htmlTagRE.ReplaceAllString(s, " ")
	s = envAssignmentRE.ReplaceAllStringFunc(s, func(m string) string {
		if idx := strings.Index(m, "="); idx > 0 {
			return m[:idx+1] + "[redacted]"
		}
		return "[redacted secret]"
	})
	s = bearerRE.ReplaceAllString(s, "$1 [redacted]")
	s = secretLikeValueRE.ReplaceAllString(s, "[redacted secret]")
	s = longOpaqueRE.ReplaceAllString(s, "[redacted opaque data]")

	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.Join(strings.Fields(line), " ")
		if line == "[redacted opaque data]" {
			continue
		}
		out = append(out, line)
		if len(strings.Join(out, "\n")) >= failureSummaryInputLimit {
			break
		}
	}
	s = strings.TrimSpace(strings.Join(out, "\n"))
	if runeLen(s) > failureSummaryInputLimit {
		s = truncateRunes(s, failureSummaryInputLimit) + "\n[truncated]"
	}
	return s
}

func fallbackFailureSummary(clawID, sanitizedReason string) string {
	var b strings.Builder
	b.WriteString("Agent stopped unexpectedly.\n\n")
	b.WriteString("Summary: ElasticClaw could not complete the agent run. ElasticClaw Server could not produce an LLM summary, so this is a sanitized fallback.\n\n")
	if sanitizedReason != "" {
		b.WriteString("Most relevant sanitized detail:\n")
		b.WriteString("```text\n")
		b.WriteString(firstUsefulFailureLines(sanitizedReason, 8))
		b.WriteString("\n```\n\n")
	}
	if len(clawID) >= 8 {
		b.WriteString(fmt.Sprintf("Agent: `%s`\n", clawID[:8]))
	}
	return clampFailureComment(b.String())
}

func firstUsefulFailureLines(s string, maxLines int) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "[redacted opaque data]") {
			continue
		}
		out = append(out, line)
		if len(out) >= maxLines {
			break
		}
	}
	if len(out) == 0 {
		return "No safe diagnostic detail was available."
	}
	return strings.Join(out, "\n")
}

func clampFailureComment(s string) string {
	s = strings.TrimSpace(s)
	if runeLen(s) <= failureCommentLimit {
		return s
	}
	return strings.TrimSpace(truncateRunes(s, failureCommentLimit)) + "\n\n[truncated]"
}

func runeLen(s string) int {
	return len([]rune(s))
}

func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}

func cloneLLMKeys(keys types.LLMKeysList) types.LLMKeysList {
	if len(keys) == 0 {
		return nil
	}
	cloned := make(types.LLMKeysList, 0, len(keys))
	for _, key := range keys {
		if key == nil {
			continue
		}
		keyCopy := *key
		cloned = append(cloned, &keyCopy)
	}
	return cloned
}

func summarizeFailureWithLLM(ctx context.Context, sanitizedReason string, llmKeys types.LLMKeysList, defaultModel string) (string, error) {
	if strings.TrimSpace(sanitizedReason) == "" {
		return "", fmt.Errorf("empty failure details")
	}
	key, model, err := selectFailureSummaryModel(llmKeys, defaultModel)
	if err != nil {
		return "", err
	}
	systemPrompt := "You summarize ElasticClaw agent/sandbox failures for issue tracker comments. Be concise, factual, and safe. Do not include secrets, environment dumps, base64 blobs, HTML, stack traces, or raw logs. Explain the likely failure, what was attempted, and one or two next diagnostic steps. Keep under 180 words."
	msgs := []aiChatMessage{{
		Role:    "user",
		Content: "Summarize this sanitized failure detail for a Linear/GitHub/Shortcut issue comment:\n\n" + sanitizedReason,
	}}
	switch key.Provider {
	case "anthropic":
		return callAnthropicModel(ctx, key.APIKey, stripProviderPrefix(model), systemPrompt, msgs, 700)
	case "openai", "codex", "fireworks", "groq", "deepseek", "ollama":
		return callOpenAICompatible(ctx, openAICompatibleConfig(key.Provider), key.APIKey, stripProviderPrefix(model), systemPrompt, msgs)
	default:
		return "", fmt.Errorf("unsupported LLM provider %q", key.Provider)
	}
}

func selectFailureSummaryModel(llmKeys types.LLMKeysList, defaultModel string) (*types.LLMKeyConfig, string, error) {
	if len(llmKeys) == 0 {
		return nil, "", fmt.Errorf("no LLM keys configured")
	}
	defaultProvider := strings.TrimSpace(strings.SplitN(defaultModel, "/", 2)[0])
	if defaultProvider != "" {
		for _, key := range llmKeys {
			if key != nil && llmKeyHasRequiredAPIKey(key) && key.Provider == defaultProvider && isFailureSummaryProvider(key.Provider) {
				return key, modelForFailureSummary(key, defaultModel), nil
			}
		}
	}
	for _, key := range llmKeys {
		if key != nil && llmKeyHasRequiredAPIKey(key) && key.Default && isFailureSummaryProvider(key.Provider) {
			return key, modelForFailureSummary(key, defaultModel), nil
		}
	}
	for _, key := range llmKeys {
		if key != nil && llmKeyHasRequiredAPIKey(key) && isFailureSummaryProvider(key.Provider) {
			return key, modelForFailureSummary(key, defaultModel), nil
		}
	}
	return nil, "", fmt.Errorf("no supported LLM key configured")
}

func isFailureSummaryProvider(provider string) bool {
	switch provider {
	case "anthropic", "openai", "codex", "fireworks", "groq", "deepseek", "ollama":
		return true
	default:
		return false
	}
}

func modelForFailureSummary(key *types.LLMKeyConfig, defaultModel string) string {
	if key.DefaultModel != "" {
		if strings.HasPrefix(key.DefaultModel, key.Provider+"/") {
			return key.DefaultModel
		}
		return key.Provider + "/" + key.DefaultModel
	}
	if defaultModel != "" && strings.HasPrefix(defaultModel, key.Provider+"/") {
		return defaultModel
	}
	switch key.Provider {
	case "anthropic":
		return "anthropic/claude-sonnet-4-6"
	case "openai":
		return "openai/gpt-5.4-mini"
	case "codex":
		return "codex/o4-mini"
	case "fireworks":
		return "fireworks/accounts/fireworks/models/kimi-k2p6"
	case "groq":
		return "groq/llama-3.3-70b-versatile"
	case "deepseek":
		return "deepseek/deepseek-chat"
	case "ollama":
		return "ollama/qwen2.5-coder:1.5b"
	default:
		return defaultModel
	}
}

func stripProviderPrefix(model string) string {
	parts := strings.SplitN(model, "/", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return model
}

type openAICompatibleProvider struct {
	Name    string
	BaseURL string
}

func openAICompatibleConfig(provider string) openAICompatibleProvider {
	switch provider {
	case "fireworks":
		return openAICompatibleProvider{Name: "Fireworks", BaseURL: "https://api.fireworks.ai/inference/v1"}
	case "groq":
		return openAICompatibleProvider{Name: "Groq", BaseURL: "https://api.groq.com/openai/v1"}
	case "deepseek":
		return openAICompatibleProvider{Name: "DeepSeek", BaseURL: "https://api.deepseek.com/v1"}
	case "ollama":
		return openAICompatibleProvider{Name: "Ollama", BaseURL: "http://ollama:11434/v1"}
	default:
		return openAICompatibleProvider{Name: "OpenAI", BaseURL: "https://api.openai.com/v1"}
	}
}

func callAnthropicModel(ctx context.Context, apiKey, model, systemPrompt string, msgs []aiChatMessage, maxTokens int) (string, error) {
	type anthropicMsg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type anthropicReq struct {
		Model     string         `json:"model"`
		MaxTokens int            `json:"max_tokens"`
		System    string         `json:"system"`
		Messages  []anthropicMsg `json:"messages"`
	}
	type anthropicContent struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	var anthropicMsgs []anthropicMsg
	for _, m := range msgs {
		anthropicMsgs = append(anthropicMsgs, anthropicMsg{Role: m.Role, Content: m.Content})
	}
	body, _ := json.Marshal(anthropicReq{Model: model, MaxTokens: maxTokens, System: systemPrompt, Messages: anthropicMsgs})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("Anthropic error %d", resp.StatusCode)
	}
	var parsed struct {
		Content []anthropicContent `json:"content"`
		Error   *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", fmt.Errorf("invalid Anthropic response: %w", err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("Anthropic error: %s", parsed.Error.Message)
	}
	for _, c := range parsed.Content {
		if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
			return c.Text, nil
		}
	}
	return "", fmt.Errorf("no text content in Anthropic response")
}

func callOpenAICompatible(ctx context.Context, provider openAICompatibleProvider, apiKey, model, systemPrompt string, msgs []aiChatMessage) (string, error) {
	type chatMsg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type chatReq struct {
		Model       string    `json:"model"`
		Messages    []chatMsg `json:"messages"`
		Temperature float64   `json:"temperature"`
	}
	var chatMsgs []chatMsg
	chatMsgs = append(chatMsgs, chatMsg{Role: "system", Content: systemPrompt})
	for _, m := range msgs {
		chatMsgs = append(chatMsgs, chatMsg{Role: m.Role, Content: m.Content})
	}
	body, _ := json.Marshal(chatReq{Model: model, Messages: chatMsgs, Temperature: 0.1})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(provider.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("%s error %d", provider.Name, resp.StatusCode)
	}
	var parsed struct {
		Choices []struct {
			Message chatMsg `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", fmt.Errorf("invalid %s response: %w", provider.Name, err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("%s error: %s", provider.Name, parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 || strings.TrimSpace(parsed.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("no choices in %s response", provider.Name)
	}
	return parsed.Choices[0].Message.Content, nil
}
