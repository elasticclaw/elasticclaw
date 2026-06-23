package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
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
	httpStatusRE      = regexp.MustCompile(`(?i)\b(?:HTTP|status|github API error|Linear API error|github API [A-Z]+ [^:]+:)\s+([1-5][0-9]{2})\b`)
)

type agentFailureKind string

const (
	agentFailureUnknown            agentFailureKind = "unknown"
	agentFailureTrackerRead        agentFailureKind = "tracker_read"
	agentFailureTemplate           agentFailureKind = "template"
	agentFailureProviderConfig     agentFailureKind = "provider_config"
	agentFailureHubPersistence     agentFailureKind = "hub_persistence"
	agentFailureWorkspaceSecrets   agentFailureKind = "workspace_secrets"
	agentFailureTriggerClaim       agentFailureKind = "trigger_claim"
	agentFailureTaskRunAnalytics   agentFailureKind = "task_run_analytics"
	agentFailureProvisioning       agentFailureKind = "provisioning"
	agentFailureBootstrap          agentFailureKind = "bootstrap"
	agentFailureWorkspaceReadiness agentFailureKind = "workspace_readiness"
	agentFailureWorkspaceFiles     agentFailureKind = "workspace_files"
	agentFailureGitHubCredentials  agentFailureKind = "github_credentials"
	agentFailureWorkflowVolume     agentFailureKind = "workflow_volume"
	agentFailureRestore            agentFailureKind = "restore"
	agentFailureWorkflowCommand    agentFailureKind = "workflow_command"
	agentFailureDependencyUpdate   agentFailureKind = "dependency_update"
	agentFailureJudge              agentFailureKind = "judge"
	agentFailureGate               agentFailureKind = "gate"
	agentFailurePullRequestAction  agentFailureKind = "pull_request_action"
	agentFailureIssueAction        agentFailureKind = "issue_action"
	agentFailureSandboxTerminated  agentFailureKind = "sandbox_terminated"
	agentFailureTrackerAPI         agentFailureKind = "tracker_api"
)

type agentFailureMessage struct {
	Kind        agentFailureKind
	StatusCode  int
	Title       string
	UserMessage string
	NextStep    string
	SafeDetail  string
}

type agentFailureRule struct {
	kind    agentFailureKind
	signals []string
}

var agentFailureRules = []agentFailureRule{
	{agentFailureTrackerRead, []string{"cannot read issue", "fetchgithubissuedetails", "fetchlinearissuedetails"}},
	{agentFailureTemplate, []string{"template \"", "template '", "resolvetemplatefiles"}},
	{agentFailureProviderConfig, []string{"no provider configured", "provider \"", " is not configured", "unsupported provider", "unknown provider"}},
	{agentFailureHubPersistence, []string{"no tenant", "db insert", "artifact storage", "hub identity"}},
	{agentFailureWorkspaceSecrets, []string{"load workspace secrets", "secret_ref"}},
	{agentFailureTriggerClaim, []string{"claim workflow trigger", "claim factory trigger", "complete workflow trigger", "complete factory trigger"}},
	{agentFailureTaskRunAnalytics, []string{"task run analytics"}},
	{agentFailureProvisioning, []string{"provisioning failed", "factory provision failed", "workflow provision failed", "restore provision failed", "provision failed"}},
	{agentFailureBootstrap, []string{"bootstrap failed", "daytona bootstrap failed", "exedev bootstrap failed", "install openclaw failed", "start claw-bridge", "connector download"}},
	{agentFailureWorkspaceReadiness, []string{"workspace readiness failed", "workspace incomplete"}},
	{agentFailureWorkspaceFiles, []string{"could not write workspace files", "workspace files incomplete", "template file staging failed", "docker file copy failed", "invalid template file path", "path must stay inside workspace"}},
	{agentFailureGitHubCredentials, []string{"could not configure github credentials", "auth gh cli failed", "verify gh auth failed", "fetch github bootstrap token"}},
	{agentFailureWorkflowVolume, []string{"workflow volume attach failed"}},
	{agentFailureRestore, []string{"restore failed", "restore checkpoint failed", "checkpoint not found", "checkpoint is not ready", "checkpoint has no manifest", "checkpoint blob", "checkpoint tree"}},
	{agentFailureWorkflowCommand, []string{"workflow command failed", "invalid run timeout", "does not support workflow run actions", "script command", "command failed"}},
	{agentFailureDependencyUpdate, []string{"dependency update step failed"}},
	{agentFailureJudge, []string{"judge stage failed", "judge llm call failed", "judge response parse failed", "no judge inputs", "no llm keys configured for judge", "invalid verdict", "missing verdict"}},
	{agentFailureGate, []string{"gate error", "gate failed", "required gate"}},
	{agentFailurePullRequestAction, []string{"merge_pr: failed", "pr validation failed", "pr closed without merge"}},
	{agentFailureIssueAction, []string{"close_issue: failed", "failed to move github issue", "failed to move issue", "failed to add label", "failed to remove label"}},
	{agentFailureSandboxTerminated, []string{"sandbox terminated", "ttl expired", "external shutdown"}},
	{agentFailureTrackerAPI, []string{"github api error", "linear api error", "graphql error", "commentcreate returned success=false", "issue or state not found"}},
}

func classifyAgentFailure(reason string) agentFailureMessage {
	sanitized := sanitizeFailureDetails(reason)
	lower := strings.ToLower(sanitized)
	kind := agentFailureUnknown
	for _, rule := range agentFailureRules {
		if containsAny(lower, rule.signals) {
			kind = rule.kind
			break
		}
	}
	msg := agentFailureMessage{
		Kind:        kind,
		StatusCode:  extractFailureStatusCode(sanitized),
		UserMessage: agentFailureUserMessage(kind),
		NextStep:    agentFailureNextStep(kind),
		SafeDetail:  agentFailureSafeDetail(sanitized),
	}
	msg.Title = msg.UserMessage
	return msg
}

func containsAny(s string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(s, strings.ToLower(needle)) {
			return true
		}
	}
	return false
}

func extractFailureStatusCode(s string) int {
	for _, match := range httpStatusRE.FindAllStringSubmatch(s, -1) {
		if len(match) < 2 {
			continue
		}
		code, err := strconv.Atoi(match[1])
		if err == nil && code >= 100 && code <= 599 {
			return code
		}
	}
	return http.StatusInternalServerError
}

func agentFailureUserMessage(kind agentFailureKind) string {
	switch kind {
	case agentFailureTrackerRead:
		return "ElasticClaw could not read the source issue/ticket."
	case agentFailureTemplate:
		return "ElasticClaw could not load the workspace template."
	case agentFailureProviderConfig:
		return "ElasticClaw could not find a valid execution provider."
	case agentFailureHubPersistence:
		return "ElasticClaw could not save or load required hub data."
	case agentFailureWorkspaceSecrets:
		return "ElasticClaw could not load a required workspace secret."
	case agentFailureTriggerClaim:
		return "ElasticClaw could not reserve or finalize this automation run."
	case agentFailureTaskRunAnalytics:
		return "ElasticClaw could not create the run tracking record."
	case agentFailureProvisioning:
		return "ElasticClaw could not start the agent workspace."
	case agentFailureBootstrap:
		return "ElasticClaw started the workspace but could not finish preparing it."
	case agentFailureWorkspaceReadiness:
		return "ElasticClaw could not verify that the workspace was ready."
	case agentFailureWorkspaceFiles:
		return "ElasticClaw could not place the required files in the workspace."
	case agentFailureGitHubCredentials:
		return "ElasticClaw could not configure GitHub access inside the workspace."
	case agentFailureWorkflowVolume:
		return "ElasticClaw could not attach a required workflow volume."
	case agentFailureRestore:
		return "ElasticClaw could not restore the previous agent checkpoint."
	case agentFailureWorkflowCommand:
		return "A workflow command failed while the agent was running."
	case agentFailureDependencyUpdate:
		return "The dependency update step did not complete successfully."
	case agentFailureJudge:
		return "ElasticClaw could not complete an automated review step."
	case agentFailureGate:
		return "A workflow gate blocked the run."
	case agentFailurePullRequestAction:
		return "ElasticClaw could not complete the pull request action."
	case agentFailureIssueAction:
		return "ElasticClaw could not update the source issue/ticket."
	case agentFailureSandboxTerminated:
		return "The agent workspace stopped before ElasticClaw could finish."
	case agentFailureTrackerAPI:
		return "ElasticClaw received an error from the issue tracker API."
	default:
		return "ElasticClaw hit an unexpected error while running the agent."
	}
}

func agentFailureNextStep(kind agentFailureKind) string {
	switch kind {
	case agentFailureTrackerRead, agentFailureTrackerAPI:
		return "Check the issue tracker token permissions, then re-trigger the workflow."
	case agentFailureTemplate, agentFailureProviderConfig, agentFailureWorkspaceSecrets, agentFailureGitHubCredentials, agentFailureWorkflowVolume:
		return "Check the ElasticClaw workspace/workflow configuration, then re-trigger the workflow."
	case agentFailureProvisioning, agentFailureBootstrap, agentFailureWorkspaceReadiness, agentFailureWorkspaceFiles, agentFailureRestore, agentFailureSandboxTerminated:
		return "Check the ElasticClaw run logs and provider status, then retry the workflow."
	case agentFailureWorkflowCommand, agentFailureDependencyUpdate, agentFailureJudge, agentFailureGate, agentFailurePullRequestAction, agentFailureIssueAction:
		return "Review the workflow result, fix the command/review/PR issue, then retry from the appropriate stage."
	default:
		return "Review the failure details and retry after the configuration or hub issue is fixed."
	}
}

func agentFailureSafeDetail(sanitized string) string {
	return firstUsefulFailureLines(sanitized, 2)
}

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
		return defaultFireworksModel
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
		baseURL := os.Getenv("OLLAMA_BASE_URL")
		if baseURL == "" {
			baseURL = "http://ollama:11434"
		}
		return openAICompatibleProvider{Name: "Ollama", BaseURL: strings.TrimRight(baseURL, "/") + "/v1"}
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
