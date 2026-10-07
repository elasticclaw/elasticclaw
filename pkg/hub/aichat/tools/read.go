package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/llm"
	"io"
	"strings"
)

// ReadTool adapts a fixed read operation; callers cannot select HTTP methods or endpoints.
type ReadTool struct {
	Name    llm.Tool
	Source  string
	Execute func(context.Context, json.RawMessage) (Result, error)
}

func (t ReadTool) Definition() llm.Tool { return t.Name }
func (t ReadTool) Provider() string     { return t.Source }
func (t ReadTool) Run(ctx context.Context, args json.RawMessage) (Result, error) {
	return t.Execute(ctx, args)
}

func Define(name, description string, properties map[string]any, required ...string) llm.Tool {
	if required == nil {
		required = []string{}
	}
	schema, _ := json.Marshal(map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false})
	return llm.Tool{Name: name, Description: description, InputSchema: schema}
}

// ArgError describes a caller-correctable validation failure, never a provider error.
type ArgError string

func (e ArgError) Error() string { return string(e) }

func DecodeArgs(raw json.RawMessage, out any) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > MaxResultBytes || raw[0] != '{' {
		return ArgError("invalid tool arguments")
	}
	fields := json.NewDecoder(bytes.NewReader(raw))
	_, _ = fields.Token()
	seen := make(map[string]bool)
	for fields.More() {
		key, err := fields.Token()
		if err != nil {
			return ArgError("invalid tool arguments")
		}
		name, ok := key.(string)
		name = strings.ToLower(name)
		if !ok || seen[name] {
			return ArgError("invalid tool arguments")
		}
		seen[name] = true
		var value json.RawMessage
		if fields.Decode(&value) != nil || string(value) == "null" {
			return ArgError("invalid tool arguments")
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return ArgError("invalid tool arguments")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return ArgError("invalid tool arguments")
	}
	return nil
}
