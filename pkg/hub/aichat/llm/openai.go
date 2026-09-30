package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

type OpenAI struct{ Client }

func (p *OpenAI) Stream(ctx context.Context, req Request, token func(string) error) (Response, error) {
	messages := []any{map[string]any{"role": "system", "content": req.System}}
	for _, m := range req.Messages {
		message := map[string]any{"role": m.Role, "content": m.Content}
		if m.ToolCallID != "" {
			message["tool_call_id"] = m.ToolCallID
		}
		if len(m.ToolCalls) > 0 {
			calls := []any{}
			for _, call := range m.ToolCalls {
				calls = append(calls, map[string]any{"id": call.ID, "type": "function", "function": map[string]string{"name": call.Name, "arguments": call.Arguments}})
			}
			message["tool_calls"] = calls
		}
		messages = append(messages, message)
	}
	body := map[string]any{"model": p.Model(), "messages": messages, "stream": true, "stream_options": map[string]bool{"include_usage": true}}
	if len(req.Tools) > 0 {
		definitions := []any{}
		for _, tool := range req.Tools {
			definitions = append(definitions, map[string]any{"type": "function", "function": map[string]any{"name": tool.Name, "description": tool.Description, "parameters": tool.InputSchema}})
		}
		body["tools"] = definitions
	}
	var result Response
	calls := map[int]*ToolCall{}
	finished, done := false, false
	err := p.Client.stream(ctx, body, false, func(raw string) error {
		if raw == "[DONE]" {
			done = true
			return errStreamDone
		}
		var chunk struct {
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				Index        int     `json:"index"`
				FinishReason *string `json:"finish_reason"`
				Delta        struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage struct {
				Input  int `json:"prompt_tokens"`
				Output int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(raw), &chunk) != nil {
			return fmt.Errorf("invalid LLM stream event")
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			return fmt.Errorf("LLM stream failed")
		}
		if chunk.Usage.Input > 0 {
			result.InputTokens = chunk.Usage.Input
		}
		if chunk.Usage.Output > 0 {
			result.OutputTokens = chunk.Usage.Output
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				continue
			}
			if choice.Delta.Content != "" {
				result.Text += choice.Delta.Content
				if err := token(choice.Delta.Content); err != nil {
					return err
				}
			}
			for _, delta := range choice.Delta.ToolCalls {
				if delta.Index < 0 || delta.Index >= 128 {
					return fmt.Errorf("too many LLM tool calls")
				}
				call := calls[delta.Index]
				if call == nil {
					call = &ToolCall{}
					calls[delta.Index] = call
				}
				call.ID += delta.ID
				call.Name += delta.Function.Name
				call.Arguments += delta.Function.Arguments
				if len(call.Arguments) > 1024*1024 {
					return fmt.Errorf("LLM tool arguments too large")
				}
			}
			if choice.FinishReason != nil {
				switch *choice.FinishReason {
				case "stop", "tool_calls":
					finished = true
				case "length":
					finished, result.Truncated = true, true
				default:
					return fmt.Errorf("LLM response did not complete")
				}
			}
		}
		return nil
	})
	// A token limit may interrupt tool arguments; keep the text, not the calls.
	if result.Truncated {
		calls = nil
	}
	indices := make([]int, 0, len(calls))
	for i := range calls {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	for _, i := range indices {
		result.ToolCalls = append(result.ToolCalls, *calls[i])
	}
	if err == nil && (!finished || !done) {
		err = errIncomplete
	}
	if err == nil {
		err = validateCalls(result.ToolCalls)
	}
	return result, err
}
