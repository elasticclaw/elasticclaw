package llm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type streamTransport string

func (body streamTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
}

func TestTokenLimitCompletion(t *testing.T) {
	for _, anthropic := range []bool{false, true} {
		for _, partialTool := range []bool{false, true} {
			for _, terminal := range []bool{false, true} {
				t.Run(fmt.Sprintf("anthropic=%v/tool=%v/terminal=%v", anthropic, partialTool, terminal), func(t *testing.T) {
					var events []string
					if anthropic {
						events = []string{
							`{"type":"message_start","message":{"usage":{"input_tokens":12}}}`,
							`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial answer"}}`,
						}
						if partialTool {
							events = append(events,
								`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call1","name":"read","input":{}}}`,
								`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{"}}`)
						}
						events = append(events, `{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":20}}`)
						if terminal {
							events = append(events, `{"type":"message_stop"}`)
						}
					} else {
						events = []string{`{"choices":[{"index":0,"delta":{"content":"partial answer"}}]}`}
						if partialTool {
							events = append(events, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call1","function":{"name":"read","arguments":"{"}}]}}]}`)
						}
						events = append(events,
							`{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
							`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":20}}`)
						if terminal {
							events = append(events, "[DONE]")
						}
					}
					body := "data: " + strings.Join(events, "\n\ndata: ") + "\n\n"
					client := Client{HTTP: &http.Client{Transport: streamTransport(body)}, Endpoint: "https://llm.test", ModelName: "model"}
					var p Provider = &OpenAI{Client: client}
					if anthropic {
						p = &Anthropic{Client: client}
					}
					var text strings.Builder
					response, err := p.Stream(context.Background(), Request{}, func(token string) error { text.WriteString(token); return nil })
					if (err == nil) != terminal {
						t.Fatalf("terminal=%v err=%v", terminal, err)
					}
					if !response.Truncated || response.Text != "partial answer" || text.String() != response.Text || response.InputTokens != 12 || response.OutputTokens != 20 || len(response.ToolCalls) != 0 {
						t.Fatalf("response=%+v text=%q", response, text.String())
					}
				})
			}
		}
	}
}
