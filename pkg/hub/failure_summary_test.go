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
