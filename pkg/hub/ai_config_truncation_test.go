package hub

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAIConfigStreamAcceptsTokenLimit(t *testing.T) {
	answer := "```yaml\nname: updated\n```"
	encoded, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"anthropic", "openai"} {
		t.Run(provider, func(t *testing.T) {
			body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":" + string(encoded) + "},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n"
			if provider == "anthropic" {
				body = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":" + string(encoded) + "}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
			}
			previous := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			t.Cleanup(func() { http.DefaultClient = previous })
			var text strings.Builder
			onToken := func(token string) { text.WriteString(token) }
			var err error
			if provider == "anthropic" {
				err = streamAnthropic(context.Background(), "key", "system", nil, onToken)
			} else {
				err = streamOpenAI(context.Background(), openAICompatibleConfig("openai"), "key", "system", nil, onToken, "model")
			}
			if err != nil || text.String() != answer {
				t.Fatalf("text=%q err=%v", text.String(), err)
			}
		})
	}
}
