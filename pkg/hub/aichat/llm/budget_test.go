package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type budgetTransport func(*http.Request) (*http.Response, error)

func (f budgetTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestArtifactOutputBudget(t *testing.T) {
	for _, anthropic := range []bool{false, true} {
		for _, budget := range []int{0, 32000} {
			client := Client{Endpoint: "https://model.test", ModelName: "fake", HTTP: &http.Client{Transport: budgetTransport(func(r *http.Request) (*http.Response, error) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				field := "max_completion_tokens"
				if anthropic {
					field = "max_tokens"
				}
				expected := budget
				if budget == 0 && anthropic {
					expected = 4096
				}
				if expected == 0 {
					if _, ok := body[field]; ok {
						t.Fatal(body)
					}
				} else if body[field] != float64(expected) {
					t.Fatal(body)
				}
				events := "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
				if anthropic {
					events = "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(events))}, nil
			})}}
			var p Provider = &OpenAI{Client: client}
			if anthropic {
				p = &Anthropic{Client: client}
			}
			if _, err := p.Stream(context.Background(), Request{MaxTokens: budget}, func(string) error { return nil }); err != nil {
				t.Fatal(err)
			}
		}
	}
}
