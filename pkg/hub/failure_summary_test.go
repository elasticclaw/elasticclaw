package hub

import (
	"strings"
	"testing"

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
