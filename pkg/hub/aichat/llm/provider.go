// Package llm provides streaming text and tool-use adapters shared by hub assistants.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}
type Message struct {
	Role       string
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
	IsError    bool
}
type Request struct {
	System   string
	Messages []Message
	Tools    []Tool
}
type Response struct {
	Text         string
	Truncated    bool
	ToolCalls    []ToolCall
	InputTokens  int
	OutputTokens int
}

type Provider interface {
	Model() string
	Stream(context.Context, Request, func(string) error) (Response, error)
}

// Client may use the hub's HTTP transport or a fake server in tests.
type Client struct {
	HTTP      *http.Client
	Endpoint  string
	APIKey    string
	ModelName string
}

func (c Client) Model() string { return c.ModelName }

func (c Client) stream(ctx context.Context, body any, anthropic bool, consume func(string) error) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("invalid LLM endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if anthropic {
		req.Header.Set("x-api-key", c.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("LLM connection failed")
	}
	defer res.Body.Close()
	// Provider error bodies may echo credentials or request data.
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("LLM returned HTTP %d", res.StatusCode)
	}
	scanner := bufio.NewScanner(res.Body)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var lines []string
	size := 0
	dispatch := func() error {
		if len(lines) == 0 {
			return nil
		}
		raw := strings.Join(lines, "\n")
		lines, size = nil, 0
		return consume(raw)
	}
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				if errors.Is(err, errStreamDone) {
					return nil
				}
				return err
			}
		} else if strings.HasPrefix(line, "data:") {
			size += len(line)
			if size > 1024*1024 {
				return fmt.Errorf("LLM event too large")
			}
			lines = append(lines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("LLM stream interrupted")
	}
	err = dispatch()
	if errors.Is(err, errStreamDone) {
		return nil
	}
	return err
}

func validateCalls(calls []ToolCall) error {
	seen := map[string]bool{}
	for _, call := range calls {
		var args map[string]json.RawMessage
		if call.ID == "" || call.Name == "" || seen[call.ID] || json.Unmarshal([]byte(call.Arguments), &args) != nil || args == nil {
			return fmt.Errorf("invalid LLM tool call")
		}
		seen[call.ID] = true
	}
	return nil
}

var errIncomplete = io.ErrUnexpectedEOF
var errStreamDone = errors.New("LLM stream complete")
