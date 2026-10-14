package aichat

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/internal/readhttp"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/llm"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
	"github.com/google/uuid"
)

func redactedJSON(value any, secrets []string) json.RawMessage {
	raw, _ := json.Marshal(value)
	return json.RawMessage(readhttp.RedactJSON(string(raw), secrets))
}

func (r *Runner) runReserved(ctx context.Context, m *Message, seq int, call llm.ToolCall, emit Emit, available, secrets []string, at int) (llm.Message, bool, error) {
	run := ToolRun{ID: uuid.NewString(), MessageID: m.ID, Seq: seq, Tool: readhttp.Redact(call.Name, secrets), Provider: "AI Chat", Args: readhttp.RedactJSON(call.Arguments, secrets)}
	started := time.Now()
	var blocks []tools.Block
	_ = json.Unmarshal(m.Blocks, &blocks)
	var block tools.Block
	var interview tools.Interview
	var toolErr error
	var event string
	var payload json.RawMessage
	stop := false
	switch call.Name {
	case tools.EmitBlock:
		block, toolErr = tools.ParseBlock(json.RawMessage(call.Arguments))
		if toolErr == nil && len(blocks) >= 6 {
			toolErr = tools.ArgError("Block limit reached.")
		}
		run.Summary = "Showed " + strings.ReplaceAll(block.Type, "_", " ")
	case tools.AskInterview:
		interview, toolErr = tools.ParseInterview(json.RawMessage(call.Arguments))
		if toolErr == nil && len(m.Interview) > 0 {
			toolErr = tools.ArgError("Interview already asked.")
		}
		if toolErr == nil {
			interview.QuestionSetID = uuid.NewString()
			sanitized := redactedJSON(interview, secrets)
			_ = json.Unmarshal(sanitized, &interview)
			if !uniqueInterviewValues(interview) {
				toolErr = tools.ArgError("Question ids and option labels must remain unique after redaction.")
				break
			}
			m.Interview = sanitized
			count := 0
			for _, step := range interview.Steps {
				count += len(step.Questions)
			}
			run.Summary = fmt.Sprintf("Asked %d questions", count)
			event = EventInterview
			payload = redactedJSON(map[string]any{"questionSetId": interview.QuestionSetID, "title": interview.Title, "step": 1, "total": len(interview.Steps), "steps": interview.Steps}, secrets)
			stop = true
		}
	case tools.OfferActions:
		if len(available) == 0 {
			toolErr = tools.ArgError("Tool not available in this release.")
			break
		}
		var requested []string
		requested, toolErr = tools.ParseActions(json.RawMessage(call.Arguments))
		if toolErr == nil {
			for _, b := range blocks {
				if b.Type == "actions" {
					toolErr = tools.ArgError("Wrap-up already offered.")
				}
			}
			if len(blocks) >= 6 {
				toolErr = tools.ArgError("Block limit reached.")
			}
			for _, a := range requested {
				if slices.Contains(available, a) {
					block.Actions = append(block.Actions, a)
				}
			}
			if len(block.Actions) == 0 {
				toolErr = tools.ArgError("Tool not available in this release.")
			}
			block.Type = "actions"
			run.Summary = "Offered wrap-up"
		}
	default:
		toolErr = tools.ArgError("Tool not available in this release.")
	}
	if toolErr == nil && block.Type != "" {
		block.ID = uuid.NewString()
		block.At = at
		payload = redactedJSON(block, secrets)
		_ = json.Unmarshal(payload, &block)
		blocks = append(blocks, block)
		m.Blocks = redactedJSON(blocks, secrets)
		event = EventBlock
	}
	content := "Shown to the user."
	if toolErr != nil {
		run.Error = readhttp.Redact(toolErr.Error(), secrets)
		run.Summary = ""
		content = run.Error
	}
	run.Result = string(redactedJSON(content, secrets))
	run.DurationMS = time.Since(started).Milliseconds()
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := r.Store.RecordToolRun(saveCtx, run); err != nil {
		return llm.Message{}, false, err
	}
	if event != "" {
		if err := emit(event, payload); err != nil {
			return llm.Message{}, false, err
		}
	}
	return llm.Message{Role: "tool", ToolCallID: readhttp.Redact(call.ID, secrets), Content: content, IsError: toolErr != nil}, stop, nil
}

func historyContent(m Message) string {
	parts := []string{m.Content}
	var blocks []tools.Block
	if json.Unmarshal(m.Blocks, &blocks) == nil {
		for _, b := range blocks {
			text := ""
			label := "Shown"
			switch b.Type {
			case "stat_tiles":
				var v tools.StatTiles
				if json.Unmarshal(b.Data, &v) == nil {
					for _, tile := range v.Tiles {
						text += tile.Label + ": " + tile.Value
						if tile.Caption != "" {
							text += " (" + tile.Caption + ")"
						}
						text += "; "
					}
				}
			case "callout":
				var v tools.Callout
				if json.Unmarshal(b.Data, &v) == nil {
					text = v.Title + ": " + v.Text
				}
			case "table":
				var v tools.Table
				if json.Unmarshal(b.Data, &v) == nil {
					text = strings.Join(v.Columns, " | ")
					for _, row := range v.Rows {
						text += "\n" + strings.Join(row, " | ")
					}
				}
			case "actions":
				label = "Offered wrap-up"
				text = strings.Join(b.Actions, ", ")
			}
			if text != "" {
				parts = append(parts, "["+label+": "+capRendering(text)+"]")
			}
		}
	}
	var interview tools.Interview
	if m.Role == "assistant" && json.Unmarshal(m.Interview, &interview) == nil {
		for _, step := range interview.Steps {
			for _, q := range step.Questions {
				options := []string{}
				for _, o := range q.Options {
					options = append(options, o.Label)
				}
				options = append(options, "other")
				parts = append(parts, "[Asked: "+capRendering(q.Prompt+" ("+strings.Join(options, " | ")+")")+"]")
			}
		}
	}
	return strings.Join(parts, "\n")
}
func capRendering(s string) string {
	if len(s) <= 1024 {
		return s
	}
	end := 1024
	for !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "…"
}

func uniqueInterviewValues(interview tools.Interview) bool {
	ids := map[string]bool{}
	for _, step := range interview.Steps {
		for _, q := range step.Questions {
			if ids[q.ID] {
				return false
			}
			ids[q.ID] = true
			labels := map[string]bool{}
			for _, o := range q.Options {
				if labels[o.Label] {
					return false
				}
				labels[o.Label] = true
			}
		}
	}
	return true
}
