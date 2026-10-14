package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/llm"
)

type Block struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Data    json.RawMessage `json:"data,omitempty"`
	Actions []string        `json:"actions,omitempty"`
	At      int             `json:"at"`
}
type StatTile struct {
	Label   string `json:"label"`
	Value   string `json:"value"`
	Caption string `json:"caption,omitempty"`
}
type StatTiles struct {
	Tiles []StatTile `json:"tiles"`
}
type Callout struct {
	Tone  string `json:"tone"`
	Title string `json:"title,omitempty"`
	Text  string `json:"text"`
}
type Table struct {
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
}
type InterviewOption struct {
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
}
type InterviewQuestion struct {
	ID      string            `json:"id,omitempty"`
	Prompt  string            `json:"prompt"`
	Label   string            `json:"label,omitempty"`
	Kind    string            `json:"kind"`
	Options []InterviewOption `json:"options"`
	Other   bool              `json:"other"`
}
type InterviewStep struct {
	Questions []InterviewQuestion `json:"questions"`
	Skippable bool                `json:"skippable"`
}
type Interview struct {
	QuestionSetID string          `json:"questionSetId"`
	Title         string          `json:"title"`
	Steps         []InterviewStep `json:"steps"`
	Answered      bool            `json:"answered"`
}

func stringSchema(max int) map[string]any {
	return map[string]any{"type": "string", "minLength": 1, "maxLength": max}
}
func enumSchema(values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values}
}
func arraySchema(items any, min, max int) map[string]any {
	return map[string]any{"type": "array", "items": items, "minItems": min, "maxItems": max}
}
func objectSchema(properties map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}

// ReservedDefinitions includes the future panel seam; the runner selects what is available.
func ReservedDefinitions() []llm.Tool {
	tile := objectSchema(map[string]any{"label": stringSchema(60), "value": stringSchema(24), "caption": stringSchema(80)}, "label", "value")
	cell := stringSchema(120)
	cell["minLength"] = 0
	data := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tiles": arraySchema(tile, 1, 4),
			"tone":  enumSchema("info", "warning", "success"), "title": stringSchema(80), "text": stringSchema(600),
			"columns": arraySchema(stringSchema(40), 1, 6), "rows": arraySchema(arraySchema(cell, 1, 6), 1, 20),
		},
		"additionalProperties": false,
		"description":          "Fields by type: stat_tiles requires tiles; callout requires tone and text with optional title; table requires columns and rows. Use only fields for the selected type.",
	}
	blockSchema, _ := json.Marshal(objectSchema(map[string]any{"type": enumSchema("stat_tiles", "callout", "table"), "data": data}, "type", "data"))
	option := objectSchema(map[string]any{"label": stringSchema(60), "detail": stringSchema(90)}, "label")
	labelSchema := stringSchema(24)
	labelSchema["description"] = "Short name for the question in the answers summary, e.g. \"When\" or \"Who\"."
	question := objectSchema(map[string]any{"id": stringSchema(60), "prompt": stringSchema(160), "label": labelSchema, "kind": enumSchema("single", "multi"), "options": arraySchema(option, 2, 5)}, "prompt", "kind", "options")
	step := objectSchema(map[string]any{"questions": arraySchema(question, 1, 3)}, "questions")
	actions := arraySchema(enumSchema("artifact", "ticket"), 1, 2)
	actions["uniqueItems"] = true
	return []llm.Tool{
		{Name: EmitBlock, Description: "Show plain-text inline stat tiles, a callout or a table. Use 1–4 key numbers only when backed by a read tool result, never invent them. Stat data is {tiles:[{label,value,caption?}]}. Table rows must match the columns. No HTML or Markdown.", InputSchema: blockSchema},
		Define(AskInterview, "End this turn with a short interview at the start of exploration, only if the answers change your advice and sources cannot answer them. Ask at most once. Do not include Other or Skip: the runner adds these. IDs, if supplied, must be unique.", map[string]any{"title": stringSchema(60), "steps": arraySchema(step, 1, 3)}, "title", "steps"),
		Define(OfferActions, "Offer available wrap-up actions after giving your answer. This only shows buttons; it never creates an artifact or ticket.", map[string]any{"actions": actions}, "actions"),
		Define(SetPanel, "Reserved for v1. Not available in Lite.", map[string]any{"mode": enumSchema("explore_idea", "validate_hypothesis", "ask_the_data", "write_brief"), "text": stringSchema(600)}, "mode", "text"),
	}
}

func validText(s string, max int) bool {
	return strings.TrimSpace(s) != "" && utf8.RuneCountInString(s) <= max
}
func optionalText(s string, max int) bool { return s == "" || validText(s, max) }

func ParseBlock(raw json.RawMessage) (Block, error) {
	var args struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := decodeReserved(raw, &args); err != nil {
		return Block{}, err
	}
	var data any
	switch args.Type {
	case "stat_tiles":
		var v StatTiles
		if err := decodeReserved(args.Data, &v); err != nil {
			return Block{}, err
		}
		if len(v.Tiles) < 1 || len(v.Tiles) > 4 {
			return Block{}, ArgError("stat_tiles requires 1–4 tiles")
		}
		for _, tile := range v.Tiles {
			if !validText(tile.Label, 60) || !validText(tile.Value, 24) || !optionalText(tile.Caption, 80) {
				return Block{}, ArgError("tile requires label (1–60 chars), value (1–24) and optional caption (up to 80)")
			}
		}
		data = v
	case "callout":
		var v Callout
		if err := decodeReserved(args.Data, &v); err != nil {
			return Block{}, err
		}
		if v.Tone != "info" && v.Tone != "warning" && v.Tone != "success" {
			return Block{}, ArgError("callout tone must be info, warning or success")
		}
		if !validText(v.Text, 600) || !optionalText(v.Title, 80) {
			return Block{}, ArgError("callout requires text (1–600 chars) and optional title (up to 80)")
		}
		data = v
	case "table":
		var v Table
		if err := decodeReserved(args.Data, &v); err != nil {
			return Block{}, err
		}
		if len(v.Columns) < 1 || len(v.Columns) > 6 || len(v.Rows) < 1 || len(v.Rows) > 20 {
			return Block{}, ArgError("table requires 1–6 columns and 1–20 rows")
		}
		for _, c := range v.Columns {
			if !validText(c, 40) {
				return Block{}, ArgError("table column must contain 1–40 chars")
			}
		}
		for _, row := range v.Rows {
			if len(row) != len(v.Columns) {
				return Block{}, ArgError("each table row must match the column count")
			}
			for _, cell := range row {
				if utf8.RuneCountInString(cell) > 120 {
					return Block{}, ArgError("table cell exceeds 120 chars")
				}
			}
		}
		data = v
	default:
		return Block{}, ArgError("block type must be stat_tiles, callout or table")
	}
	encoded, _ := json.Marshal(data)
	return Block{Type: args.Type, Data: encoded}, nil
}

func ParseInterview(raw json.RawMessage) (Interview, error) {
	// Input types deliberately exclude runner-owned fields.
	type question struct {
		ID      string            `json:"id,omitempty"`
		Prompt  string            `json:"prompt"`
		Label   string            `json:"label,omitempty"`
		Kind    string            `json:"kind"`
		Options []InterviewOption `json:"options"`
	}
	var args struct {
		Title string `json:"title"`
		Steps []struct {
			Questions []question `json:"questions"`
		} `json:"steps"`
	}
	if err := decodeReserved(raw, &args); err != nil {
		return Interview{}, err
	}
	if !validText(args.Title, 60) || len(args.Steps) < 1 || len(args.Steps) > 3 {
		return Interview{}, ArgError("interview requires title (1–60 chars) and 1–3 steps")
	}
	ids := map[string]bool{}
	for _, s := range args.Steps {
		for _, q := range s.Questions {
			if q.ID != "" {
				if !validText(q.ID, 60) || ids[q.ID] {
					return Interview{}, ArgError("question ids must be unique, up to 60 chars")
				}
				ids[q.ID] = true
			}
		}
	}
	result := Interview{Title: args.Title}
	next := 1
	for _, s := range args.Steps {
		if len(s.Questions) < 1 || len(s.Questions) > 3 {
			return Interview{}, ArgError("each step requires 1–3 questions")
		}
		step := InterviewStep{Skippable: true}
		for _, q := range s.Questions {
			if !validText(q.Prompt, 160) || (q.Kind != "single" && q.Kind != "multi") {
				return Interview{}, ArgError("question requires prompt (1–160 chars) and kind single or multi")
			}
			if !optionalText(q.Label, 24) {
				return Interview{}, ArgError("question label is up to 24 chars")
			}
			options := []InterviewOption{}
			labels := map[string]bool{}
			for _, o := range q.Options {
				if strings.EqualFold(strings.TrimSpace(o.Label), "other") {
					continue
				}
				if !validText(o.Label, 60) || !optionalText(o.Detail, 90) || labels[o.Label] {
					return Interview{}, ArgError("options require unique labels (1–60 chars) and optional detail (up to 90)")
				}
				labels[o.Label] = true
				options = append(options, o)
			}
			if len(options) < 2 || len(options) > 5 {
				return Interview{}, ArgError("each question requires 2–5 options excluding Other")
			}
			if q.ID == "" {
				for ids[fmt.Sprintf("q%d", next)] {
					next++
				}
				q.ID = fmt.Sprintf("q%d", next)
				ids[q.ID] = true
				next++
			}
			step.Questions = append(step.Questions, InterviewQuestion{ID: q.ID, Prompt: q.Prompt, Label: q.Label, Kind: q.Kind, Options: options, Other: true})
		}
		result.Steps = append(result.Steps, step)
	}
	return result, nil
}

func ParseActions(raw json.RawMessage) ([]string, error) {
	var args struct {
		Actions []string `json:"actions"`
	}
	if err := decodeReserved(raw, &args); err != nil {
		return nil, err
	}
	if len(args.Actions) == 0 {
		return nil, ArgError("actions must not be empty")
	}
	seen := map[string]bool{}
	for _, a := range args.Actions {
		if (a != "artifact" && a != "ticket") || seen[a] {
			return nil, ArgError("actions must be unique artifact or ticket values")
		}
		seen[a] = true
	}
	return args.Actions, nil
}

// Reject nulls and duplicate nested keys before decoding into Go zero values.
func decodeReserved(raw json.RawMessage, out any) error {
	raw = bytes.TrimSpace(raw)
	// Full tables, including JSON-escaped Unicode cells, can exceed read-tool limits.
	if len(raw) == 0 || len(raw) > 256*1024 || raw[0] != '{' {
		return ArgError("invalid tool arguments")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	var walk func() error
	walk = func() error {
		token, err := d.Token()
		if err != nil || token == nil {
			return ArgError("arguments must not contain null or malformed values")
		}
		if delim, ok := token.(json.Delim); ok {
			if delim != '{' && delim != '[' {
				return ArgError("invalid tool arguments")
			}
			seen := map[string]bool{}
			for d.More() {
				if delim == '{' {
					key, err := d.Token()
					if err != nil {
						return ArgError("invalid tool arguments")
					}
					name, ok := key.(string)
					if !ok || seen[name] {
						return ArgError("duplicate argument field")
					}
					seen[name] = true
				}
				if err := walk(); err != nil {
					return err
				}
			}
			if _, err := d.Token(); err != nil {
				return ArgError("invalid tool arguments")
			}
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return ArgError("invalid tool arguments")
	}
	if err := canonicalFields(raw, reflect.TypeOf(out).Elem()); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return ArgError("invalid tool arguments")
	}
	return nil
}

func canonicalFields(raw json.RawMessage, typ reflect.Type) error {
	if typ == reflect.TypeOf(json.RawMessage{}) {
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			return ArgError("expected an object")
		}
		allowed := map[string]reflect.Type{}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			allowed[strings.Split(field.Tag.Get("json"), ",")[0]] = field.Type
		}
		for key, value := range fields {
			field, ok := allowed[key]
			if !ok {
				return ArgError("unknown argument field")
			}
			if err := canonicalFields(value, field); err != nil {
				return err
			}
		}
	case reflect.Slice:
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return ArgError("expected an array")
		}
		for _, item := range items {
			if err := canonicalFields(item, typ.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}
