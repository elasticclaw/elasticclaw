// Package tools keeps model-readable tools separate from user-initiated writes.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/llm"
)

const Timeout = 30 * time.Second
const MaxResultBytes = 32 * 1024
const (
	EmitBlock    = "emit_block"
	AskInterview = "ask_interview"
	OfferActions = "offer_actions"
	SetPanel     = "set_panel"
)

func Reserved(name string) bool {
	switch name {
	case EmitBlock, AskInterview, OfferActions, SetPanel:
		return true
	}
	return false
}

type Result struct {
	Summary  string
	Data     string
	RowCount int
}

// Tool implementations must perform only reads, honor context cancellation, and
// return summaries/data/errors without credentials. Credentials never enter args.
type Tool interface {
	Definition() llm.Tool
	Provider() string
	Run(context.Context, json.RawMessage) (Result, error)
}

type ReadRegistry struct{ tools map[string]Tool }

func NewReadRegistry(reads ...Tool) (*ReadRegistry, error) {
	r := &ReadRegistry{tools: map[string]Tool{}}
	for _, tool := range reads {
		name := tool.Definition().Name
		if name == "" || Reserved(name) || r.tools[name] != nil {
			return nil, fmt.Errorf("invalid or duplicate read tool %q", name)
		}
		r.tools[name] = tool
	}
	return r, nil
}
func (r *ReadRegistry) Lookup(name string) (Tool, bool) {
	if r == nil {
		return nil, false
	}
	tool, ok := r.tools[name]
	return tool, ok
}
func (r *ReadRegistry) Definitions() []llm.Tool {
	definitions := []llm.Tool{}
	if r != nil {
		for _, tool := range r.tools {
			definitions = append(definitions, tool.Definition())
		}
	}
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Name < definitions[j].Name })
	return definitions
}

// WriteRegistry is intentionally empty. It is never passed to the model loop.
// Artifact and ticket actions will be added through a separate user-action API.
type WriteRegistry struct{}

func Execute(ctx context.Context, tool Tool, args json.RawMessage) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	result, err := tool.Run(ctx, args)
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, err
}

func CapResult(text string) string {
	if len(text) <= MaxResultBytes {
		return text
	}
	const suffix = "\n[truncated]"
	end := MaxResultBytes - len(suffix)
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + suffix
}
