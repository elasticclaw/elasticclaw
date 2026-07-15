package hub

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestSanitizeFailureDetailsRedactsSecretsAndOpaqueData(t *testing.T) {
	raw := `Factory provision failed:
ANTHROPIC_API_KEY=sk-ant-thisshouldnotappear
GITHUB_TOKEN=ghp_thisshouldnotappearincomments
Authorization: Bearer abcdefghijklmnopqrstuvwxyz1234567890
payload=QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVoQUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVoQUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo=
real error: curl exited with status 22`

	got := sanitizeFailureDetails(raw)
	for _, forbidden := range []string{
		"sk-ant-thisshouldnotappear",
		"ghp_thisshouldnotappearincomments",
		"abcdefghijklmnopqrstuvwxyz1234567890",
		"QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("sanitized output leaked %q in:\n%s", forbidden, got)
		}
	}
	if !strings.Contains(got, "real error: curl exited with status 22") {
		t.Fatalf("sanitized output lost useful detail:\n%s", got)
	}
}

func TestSanitizeFailureDetailsStripsHTML(t *testing.T) {
	raw := `<html><body><h1>403 Forbidden</h1><script>secret()</script><p>replicated API denied request</p></body></html>`
	got := sanitizeFailureDetails(raw)
	if strings.Contains(got, "<html>") || strings.Contains(got, "<script>") || strings.Contains(got, "</") {
		t.Fatalf("sanitized output still contains HTML:\n%s", got)
	}
	if !strings.Contains(got, "403 Forbidden") || !strings.Contains(got, "replicated API denied request") {
		t.Fatalf("sanitized output lost useful HTML text:\n%s", got)
	}
}

func TestFallbackFailureSummaryDoesNotDumpRawLogs(t *testing.T) {
	raw := strings.Repeat("A", 200) + "\nOPENAI_API_KEY=sk-secret\nuseful: ssh timed out"
	sanitized := sanitizeFailureDetails(raw)
	got := fallbackFailureSummary("12345678-1234", sanitized)

	if strings.Contains(got, "sk-secret") || strings.Contains(got, strings.Repeat("A", 80)) {
		t.Fatalf("fallback leaked raw secret or opaque log:\n%s", got)
	}
	if !strings.Contains(got, "sanitized fallback") || !strings.Contains(got, "useful: ssh timed out") {
		t.Fatalf("fallback missing expected summary/detail:\n%s", got)
	}
}

func TestClampFailureCommentPreservesUTF8(t *testing.T) {
	got := clampFailureComment(strings.Repeat("é", failureCommentLimit+10))
	if !utf8.ValidString(got) {
		t.Fatalf("clamped comment is not valid UTF-8")
	}
	if !strings.Contains(got, "[truncated]") {
		t.Fatalf("expected truncation marker in:\n%s", got)
	}
}

func TestClassifyAgentFailureMapsKnownErrorsToGenericMessages(t *testing.T) {
	tests := []struct {
		name       string
		reason     string
		wantKind   agentFailureKind
		wantStatus int
		wantMsg    string
		wantNext   string
	}{
		{
			name:       "tracker read",
			reason:     "cannot read issue ELA-123 from Linear (check token/workspace access): Linear API error 403",
			wantKind:   agentFailureTrackerRead,
			wantStatus: 403,
			wantMsg:    "ElasticClaw could not read the source issue/ticket.",
			wantNext:   "Check the issue tracker token permissions, then re-trigger the workflow.",
		},
		{
			name:       "template",
			reason:     `template "missing" not found: not found`,
			wantKind:   agentFailureTemplate,
			wantStatus: 500,
			wantMsg:    "ElasticClaw could not load the workspace template.",
			wantNext:   "Check the ElasticClaw workspace/workflow configuration, then re-trigger the workflow.",
		},
		{
			name:       "provider config",
			reason:     `provider "daytona" is not configured on this hub`,
			wantKind:   agentFailureProviderConfig,
			wantStatus: 500,
			wantMsg:    "ElasticClaw could not find a valid execution provider.",
			wantNext:   "Check the ElasticClaw workspace/workflow configuration, then re-trigger the workflow.",
		},
		{
			name:       "bootstrap",
			reason:     "Bootstrap failed: install openclaw failed: status 22",
			wantKind:   agentFailureBootstrap,
			wantStatus: 500,
			wantMsg:    "ElasticClaw started the workspace but could not finish preparing it.",
			wantNext:   "Check the ElasticClaw run logs and provider status, then retry the workflow.",
		},
		{
			name:       "watchdog gateway health",
			reason:     "agent process unhealthy for 12 consecutive heartbeats",
			wantKind:   agentFailureWorkspaceReadiness,
			wantStatus: 500,
			wantMsg:    "ElasticClaw could not verify that the workspace was ready.",
			wantNext:   "Check the ElasticClaw run logs and provider status, then retry the workflow.",
		},
		{
			name:       "watchdog silent death",
			reason:     "no status updates for 10 minutes, agent presumed dead",
			wantKind:   agentFailureWorkspaceReadiness,
			wantStatus: 500,
			wantMsg:    "ElasticClaw could not verify that the workspace was ready.",
			wantNext:   "Check the ElasticClaw run logs and provider status, then retry the workflow.",
		},
		{
			name:       "watchdog stuck turn",
			reason:     "agent repeatedly stuck mid-turn",
			wantKind:   agentFailureWorkspaceReadiness,
			wantStatus: 500,
			wantMsg:    "ElasticClaw could not verify that the workspace was ready.",
			wantNext:   "Check the ElasticClaw run logs and provider status, then retry the workflow.",
		},
		{
			name:       "judge",
			reason:     "Judge stage failed: no LLM keys configured for judge",
			wantKind:   agentFailureJudge,
			wantStatus: 500,
			wantMsg:    "ElasticClaw could not complete an automated review step.",
			wantNext:   "Review the workflow result, fix the command/review/PR issue, then retry from the appropriate stage.",
		},
		{
			name:       "tracker api",
			reason:     "github API error 404: label not found",
			wantKind:   agentFailureTrackerAPI,
			wantStatus: 404,
			wantMsg:    "ElasticClaw received an error from the issue tracker API.",
			wantNext:   "Check the issue tracker token permissions, then re-trigger the workflow.",
		},
		{
			name:       "unknown",
			reason:     "unexpected internal failure",
			wantKind:   agentFailureUnknown,
			wantStatus: 500,
			wantMsg:    "ElasticClaw hit an unexpected error while running the agent.",
			wantNext:   "Review the failure details and retry after the configuration or hub issue is fixed.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyAgentFailure(tt.reason)
			if got.Kind != tt.wantKind {
				t.Fatalf("Kind = %q, want %q", got.Kind, tt.wantKind)
			}
			if got.StatusCode != tt.wantStatus {
				t.Fatalf("StatusCode = %d, want %d", got.StatusCode, tt.wantStatus)
			}
			if got.UserMessage != tt.wantMsg {
				t.Fatalf("UserMessage = %q, want %q", got.UserMessage, tt.wantMsg)
			}
			if got.NextStep != tt.wantNext {
				t.Fatalf("NextStep = %q, want %q", got.NextStep, tt.wantNext)
			}
		})
	}
}

func TestClassifyAgentFailureDoesNotExposeRawSecrets(t *testing.T) {
	got := classifyAgentFailure("Bootstrap failed: OPENAI_API_KEY=sk-secret-token\nreal error: HTTP 401")
	if got.StatusCode != 401 {
		t.Fatalf("StatusCode = %d, want 401", got.StatusCode)
	}
	for _, forbidden := range []string{"sk-secret-token", "OPENAI_API_KEY=sk-secret-token"} {
		if strings.Contains(got.SafeDetail, forbidden) {
			t.Fatalf("SafeDetail leaked %q in:\n%s", forbidden, got.SafeDetail)
		}
	}
	if got.UserMessage == "" || got.NextStep == "" {
		t.Fatalf("expected generic user message and next step: %#v", got)
	}
}

func TestCloneLLMKeysCopiesStructValues(t *testing.T) {
	original := types.LLMKeysList{
		{Name: "openai-main", Provider: "openai", APIKey: "old-key", DefaultModel: "gpt-4o-mini"},
	}
	cloned := cloneLLMKeys(original)
	original[0].APIKey = "new-key"
	original[0].DefaultModel = "gpt-4o"

	if cloned[0] == original[0] {
		t.Fatal("clone reused original LLM key pointer")
	}
	if cloned[0].APIKey != "old-key" || cloned[0].DefaultModel != "gpt-4o-mini" {
		t.Fatalf("clone changed after original mutation: %#v", cloned[0])
	}
}

func TestSelectFailureSummaryModelSupportsOpenAICompatibleProviders(t *testing.T) {
	keys := types.LLMKeysList{
		{Name: "fireworks-main", Provider: "fireworks", APIKey: "key", Default: true, DefaultModel: "accounts/fireworks/models/kimi-k2p6"},
	}
	key, model, err := selectFailureSummaryModel(keys, "")
	if err != nil {
		t.Fatalf("select model: %v", err)
	}
	if key.Provider != "fireworks" || model != "fireworks/accounts/fireworks/models/kimi-k2p6" {
		t.Fatalf("provider/model = %s/%s", key.Provider, model)
	}
}

func TestSelectFailureSummaryModelSupportsOllama(t *testing.T) {
	keys := types.LLMKeysList{
		{Name: "ollama-main", Provider: "ollama", APIKey: "ollama-local", Default: true},
	}
	key, model, err := selectFailureSummaryModel(keys, "")
	if err != nil {
		t.Fatalf("select model: %v", err)
	}
	if key.Provider != "ollama" || model != "ollama/qwen2.5-coder:1.5b" {
		t.Fatalf("provider/model = %s/%s", key.Provider, model)
	}
	provider := openAICompatibleConfig("ollama")
	if provider.Name != "Ollama" || provider.BaseURL != "http://ollama:11434/v1" {
		t.Fatalf("ollama provider config = %#v", provider)
	}
}

func TestOpenAICompatibleConfigUsesOllamaBaseURLEnv(t *testing.T) {
	t.Setenv("OLLAMA_BASE_URL", "http://localhost:11434/")

	provider := openAICompatibleConfig("ollama")
	if provider.Name != "Ollama" || provider.BaseURL != "http://localhost:11434/v1" {
		t.Fatalf("ollama provider config = %#v", provider)
	}
}

func TestSelectFailureSummaryModelAllowsBlankOllamaAPIKey(t *testing.T) {
	keys := types.LLMKeysList{
		{Name: "ollama-main", Provider: "ollama", Default: true},
	}
	key, model, err := selectFailureSummaryModel(keys, "")
	if err != nil {
		t.Fatalf("select model: %v", err)
	}
	if key.Provider != "ollama" || model != "ollama/qwen2.5-coder:1.5b" {
		t.Fatalf("provider/model = %s/%s", key.Provider, model)
	}
}

func TestSelectFailureSummaryModelRejectsBlankExternalAPIKey(t *testing.T) {
	keys := types.LLMKeysList{
		{Name: "openai-main", Provider: "openai", Default: true},
	}
	if _, _, err := selectFailureSummaryModel(keys, ""); err == nil {
		t.Fatal("expected blank external API key to be rejected")
	}
}
