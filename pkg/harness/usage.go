package harness

// UsageSnapshot is a harness session's usage so far. Token and cost fields are
// cumulative over the session and never decrease within one Epoch.
type UsageSnapshot struct {
	Epoch            string   `json:"epoch"` // changes when the producer restarts and loses its running totals
	SessionKey       string   `json:"session_key"`
	Model            string   `json:"model,omitempty"`
	ModelProvider    string   `json:"model_provider,omitempty"`
	InputTokens      *int     `json:"input_tokens,omitempty"`
	OutputTokens     *int     `json:"output_tokens,omitempty"`
	CostUSD          *float64 `json:"cost_usd,omitempty"` // nil = not reported in this sample; 0 with tokens = unpriced
	CacheReadTokens  *int     `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *int     `json:"cache_write_tokens,omitempty"`
}
