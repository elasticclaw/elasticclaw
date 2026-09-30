package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These integration tests intentionally use fake HTTP servers. Environments
// that cannot bind ports should compile them and leave execution to CI.
func TestOpenAIStreamingToolUse(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing authorization")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "text/event-stream")
		if len(bodies) == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Checking\",\"tool_calls\":[{\"index\":0,\"id\":\"call1\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"q\\\":\"}}]}}]}\r\n\r\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"test\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
		} else {
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Answer\"},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":4}}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	p := &OpenAI{Client: Client{HTTP: server.Client(), Endpoint: server.URL, APIKey: "secret", ModelName: "model"}}
	req := Request{System: "system", Messages: []Message{{Role: "user", Content: "question"}}, Tools: []Tool{{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	var text strings.Builder
	response, err := p.Stream(context.Background(), req, func(token string) error { text.WriteString(token); return nil })
	if err != nil || response.Text != "Checking" || text.String() != response.Text || response.InputTokens != 12 || response.OutputTokens != 4 || len(response.ToolCalls) != 1 {
		t.Fatalf("%+v %v", response, err)
	}
	call := response.ToolCalls[0]
	if call.ID != "call1" || call.Name != "read" || call.Arguments != `{"q":"test"}` {
		t.Fatal(call)
	}
	req.Messages = append(req.Messages, Message{Role: "assistant", Content: response.Text, ToolCalls: response.ToolCalls}, Message{Role: "tool", ToolCallID: call.ID, Content: "result"})
	_, err = p.Stream(context.Background(), req, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	messages := bodies[1]["messages"].([]any)
	last := messages[len(messages)-1].(map[string]any)
	if last["role"] != "tool" || last["tool_call_id"] != "call1" || bodies[0]["model"] != "model" || bodies[0]["tools"] == nil {
		t.Fatal(bodies)
	}
}

func TestAnthropicStreamingToolUse(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "secret" || r.Header.Get("anthropic-version") == "" {
			t.Error("missing headers")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":8,"output_tokens":1}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Checking"}}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call1","name":"read","input":{}}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"test\"}"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`,
			`{"type":"message_stop"}`,
		}
		for _, event := range events {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
	}))
	defer server.Close()
	p := &Anthropic{Client: Client{HTTP: server.Client(), Endpoint: server.URL, APIKey: "secret", ModelName: "model"}}
	req := Request{System: "system", Messages: []Message{{Role: "user", Content: "question"}}, Tools: []Tool{{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	response, err := p.Stream(context.Background(), req, func(string) error { return nil })
	if err != nil || response.Text != "Checking" || response.InputTokens != 8 || response.OutputTokens != 5 || len(response.ToolCalls) != 1 || response.ToolCalls[0].Arguments != `{"q":"test"}` {
		t.Fatalf("%+v %v", response, err)
	}
	req.Messages = append(req.Messages, Message{Role: "assistant", ToolCalls: response.ToolCalls}, Message{Role: "tool", ToolCallID: "call1", Content: "result"})
	if _, err := p.Stream(context.Background(), req, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	messages := bodies[1]["messages"].([]any)
	last := messages[len(messages)-1].(map[string]any)
	block := last["content"].([]any)[0].(map[string]any)
	if last["role"] != "user" || block["type"] != "tool_result" || block["tool_use_id"] != "call1" {
		t.Fatal(last)
	}
}

func TestStreamErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		anthropic  bool
	}{
		{"http", `secret echoed`, 401, false},
		{"openai error", "data: {\"error\":{\"message\":\"secret echoed\"}}\n\n", 200, false},
		{"truncated", "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n", 200, false},
		{"anthropic error", "data: {\"type\":\"error\",\"error\":{\"message\":\"secret echoed\"}}\n\n", 200, true},
		{"anthropic truncated", "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n", 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
			defer server.Close()
			client := Client{HTTP: server.Client(), Endpoint: server.URL, ModelName: "model"}
			var provider Provider = &OpenAI{Client: client}
			if tc.anthropic {
				provider = &Anthropic{Client: client}
			}
			_, err := provider.Stream(context.Background(), Request{}, func(string) error { return nil })
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestStreamCancelledMidRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	provider := &OpenAI{Client: Client{HTTP: server.Client(), Endpoint: server.URL, ModelName: "model"}}
	_, err := provider.Stream(ctx, Request{}, func(string) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context.Canceled", err)
	}
}
