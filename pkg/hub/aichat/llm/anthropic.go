package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

type Anthropic struct{ Client }

func (p *Anthropic) Stream(ctx context.Context, req Request, token func(string) error) (Response, error) {
	messages := []map[string]any{}
	for _, m := range req.Messages {
		role := m.Role
		blocks := []any{}
		if m.ToolCallID != "" {
			role = "user"
			blocks = append(blocks, map[string]any{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": m.Content, "is_error": m.IsError})
		} else {
			if m.Content != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
			}
			for _, call := range m.ToolCalls {
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": json.RawMessage(call.Arguments)})
			}
		}
		if len(blocks) == 0 {
			continue
		}
		// Multiple results for one assistant tool-use message form one user message.
		if len(messages) > 0 && messages[len(messages)-1]["role"] == role {
			previous := messages[len(messages)-1]
			previous["content"] = append(previous["content"].([]any), blocks...)
		} else {
			messages = append(messages, map[string]any{"role": role, "content": blocks})
		}
	}
	body := map[string]any{"model": p.Model(), "max_tokens": 4096, "system": req.System, "messages": messages, "stream": true}
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
	}
	var result Response
	calls := map[int]*ToolCall{}
	partial := map[int]string{}
	stopped, finished := false, false
	err := p.Client.stream(ctx, body, true, func(raw string) error {
		var event struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message struct {
				Usage struct {
					Input  int `json:"input_tokens"`
					Output int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
			ContentBlock struct {
				Type  string          `json:"type"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
				Text  string          `json:"text"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				Input  int `json:"input_tokens"`
				Output int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(raw), &event) != nil {
			return fmt.Errorf("invalid LLM stream event")
		}
		switch event.Type {
		case "error":
			return fmt.Errorf("LLM stream failed")
		case "message_start":
			result.InputTokens = event.Message.Usage.Input
			result.OutputTokens = event.Message.Usage.Output
		case "content_block_start":
			if event.ContentBlock.Type == "tool_use" {
				if len(calls) >= 128 {
					return fmt.Errorf("too many LLM tool calls")
				}
				calls[event.Index] = &ToolCall{ID: event.ContentBlock.ID, Name: event.ContentBlock.Name, Arguments: string(event.ContentBlock.Input)}
			} else if event.ContentBlock.Type == "text" && event.ContentBlock.Text != "" {
				result.Text += event.ContentBlock.Text
				return token(event.ContentBlock.Text)
			}
		case "content_block_delta":
			if event.Delta.Type == "text_delta" {
				result.Text += event.Delta.Text
				return token(event.Delta.Text)
			}
			if event.Delta.Type == "input_json_delta" {
				if calls[event.Index] == nil {
					return fmt.Errorf("invalid LLM tool delta")
				}
				partial[event.Index] += event.Delta.PartialJSON
				if len(partial[event.Index]) > 1024*1024 {
					return fmt.Errorf("LLM tool arguments too large")
				}
			}
		case "message_delta":
			if event.Usage.Input > 0 {
				result.InputTokens = event.Usage.Input
			}
			result.OutputTokens = event.Usage.Output
			switch event.Delta.StopReason {
			case "end_turn", "tool_use", "stop_sequence":
				finished = true
			case "max_tokens":
				finished, result.Truncated = true, true
			case "":
			default:
				return fmt.Errorf("LLM response did not complete")
			}
		case "message_stop":
			stopped = true
			return errStreamDone
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
		call := *calls[i]
		if partial[i] != "" {
			call.Arguments = partial[i]
		}
		result.ToolCalls = append(result.ToolCalls, call)
	}
	if err == nil && (!stopped || !finished) {
		err = errIncomplete
	}
	if err == nil {
		err = validateCalls(result.ToolCalls)
	}
	return result, err
}
