package llm

import (
	"context"
	"encoding/json"
	"errors"
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

func TestOutputTokenLimitErrors(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		status        int
		limit         bool
	}{
		{"openai", "max_completion_tokens is too large: this model supports at most 4096 completion tokens. SECRET-SENTINEL", 400, true},
		{"anthropic", "max_tokens: 8192 > 4096, the maximum allowed number of output tokens SECRET-SENTINEL", 400, true},
		{"context", "maximum context length exceeded SECRET-SENTINEL", 400, false},
		{"unsupported", "unsupported parameter max_completion_tokens SECRET-SENTINEL", 400, false},
		{"auth", "max_tokens exceeds maximum SECRET-SENTINEL", 401, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := Client{Endpoint: "https://model.test", ModelName: "model", HTTP: &http.Client{Transport: budgetTransport(func(r *http.Request) (*http.Response, error) {
				body, _ := json.Marshal(map[string]any{"error": map[string]string{"message": tc.message}})
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			})}}
			_, err := (&OpenAI{Client: client}).Stream(context.Background(), Request{MaxTokens: 8192}, func(string) error { return nil })
			if err == nil || errors.Is(err, ErrOutputTokenLimit) != tc.limit || strings.Contains(err.Error(), "SECRET-SENTINEL") {
				t.Fatalf("unexpected public error: %v", err)
			}
		})
	}
}
