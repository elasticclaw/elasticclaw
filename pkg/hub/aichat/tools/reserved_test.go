package tools

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestReservedBlockValidation(t *testing.T) {
	tests := []struct {
		name, raw string
		valid     bool
	}{
		{"tiles", `{"type":"stat_tiles","data":{"tiles":[{"label":"Users","value":"42","caption":"PostHog"}]}}`, true},
		{"callout", `{"type":"callout","data":{"tone":"warning","title":"Check","text":"Evidence"}}`, true},
		{"table", `{"type":"table","data":{"columns":["A","B"],"rows":[["","b"]]}}`, true},
		{"unknown type", `{"type":"chart","data":{}}`, false},
		{"mismatched type and data", `{"type":"stat_tiles","data":{"tone":"info","text":"Evidence"}}`, false},
		{"mixed data variants", `{"type":"callout","data":{"tone":"info","text":"Evidence","tiles":[{"label":"Users","value":"42"}]}}`, false},
		{"unknown field", `{"type":"callout","data":{"tone":"info","text":"ok","html":"bad"}}`, false},
		{"missing data", `{"type":"callout"}`, false},
		{"null data", `{"type":"callout","data":null}`, false},
		{"empty tiles", `{"type":"stat_tiles","data":{"tiles":[]}}`, false},
		{"many tiles", `{"type":"stat_tiles","data":{"tiles":[{"label":"a","value":"1"},{"label":"b","value":"2"},{"label":"c","value":"3"},{"label":"d","value":"4"},{"label":"e","value":"5"}]}}`, false},
		{"missing label", `{"type":"stat_tiles","data":{"tiles":[{"value":"1"}]}}`, false},
		{"wrong value type", `{"type":"stat_tiles","data":{"tiles":[{"label":"a","value":1}]}}`, false},
		{"null caption", `{"type":"stat_tiles","data":{"tiles":[{"label":"a","value":"1","caption":null}]}}`, false},
		{"duplicate nested", `{"type":"stat_tiles","data":{"tiles":[{"label":"a","label":"b","value":"1"}]}}`, false},
		{"field casing", `{"type":"stat_tiles","data":{"tiles":[{"Label":"a","value":"1"}]}}`, false},
		{"tone", `{"type":"callout","data":{"tone":"bad","text":"ok"}}`, false},
		{"missing text", `{"type":"callout","data":{"tone":"info"}}`, false},
		{"no columns", `{"type":"table","data":{"columns":[],"rows":[[]]}}`, false},
		{"many columns", `{"type":"table","data":{"columns":["1","2","3","4","5","6","7"],"rows":[["1"]]}}`, false},
		{"no rows", `{"type":"table","data":{"columns":["a"],"rows":[]}}`, false},
		{"ragged rows", `{"type":"table","data":{"columns":["a","b"],"rows":[["one"]]}}`, false},
		{"null cell", `{"type":"table","data":{"columns":["a"],"rows":[[null]]}}`, false},
		{"malformed", `{"type":"callout"`, false},
		{"trailing", `{"type":"callout","data":{}} {}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseBlock(json.RawMessage(tc.raw))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if err != nil {
				var arg ArgError
				if !errors.As(err, &arg) {
					t.Fatalf("not ArgError: %v", err)
				}
			}
		})
	}
	for _, field := range []struct {
		kind, key string
		limit     int
	}{
		{"stat_tiles", "label", 60}, {"stat_tiles", "value", 24}, {"stat_tiles", "caption", 80}, {"callout", "title", 80}, {"callout", "text", 600}, {"table", "column", 40}, {"table", "cell", 120},
	} {
		t.Run(field.key+" length", func(t *testing.T) {
			for _, extra := range []int{0, 1} {
				long := strings.Repeat("界", field.limit+extra)
				var data any
				switch field.kind {
				case "stat_tiles":
					tile := map[string]string{"label": "a", "value": "1"}
					tile[field.key] = long
					data = map[string]any{"tiles": []any{tile}}
				case "callout":
					v := map[string]string{"tone": "info", "text": "ok"}
					v[field.key] = long
					data = v
				case "table":
					columns := []string{"a"}
					rows := [][]string{{"b"}}
					if field.key == "column" {
						columns[0] = long
					} else {
						rows[0][0] = long
					}
					data = map[string]any{"columns": columns, "rows": rows}
				}
				raw, _ := json.Marshal(map[string]any{"type": field.kind, "data": data})
				_, err := ParseBlock(raw)
				if (err == nil) != (extra == 0) {
					t.Fatalf("extra=%d err=%v", extra, err)
				}
			}
		})
	}
	rows := make([][]string, 21)
	for i := range rows {
		rows[i] = []string{"x"}
	}
	raw, _ := json.Marshal(map[string]any{"type": "table", "data": Table{Columns: []string{"a"}, Rows: rows}})
	if _, err := ParseBlock(raw); err == nil {
		t.Fatal("accepted 21 rows")
	}
}

func TestReservedInterviewValidation(t *testing.T) {
	valid := `{"title":"Framing","steps":[{"questions":[{"prompt":"When?","kind":"single","options":[{"label":"Now"},{"label":"Later"},{"label":"OTHER"}]},{"id":"q1","prompt":"Who?","kind":"multi","options":[{"label":"Me"},{"label":"Us"}]}]}]}`
	interview, err := ParseInterview(json.RawMessage(valid))
	if err != nil {
		t.Fatal(err)
	}
	q := interview.Steps[0].Questions
	if q[0].ID != "q2" || q[1].ID != "q1" || !q[0].Other || len(q[0].Options) != 2 || !interview.Steps[0].Skippable {
		t.Fatalf("%+v", interview)
	}
	for _, raw := range []string{
		strings.Replace(valid, `"title":"Framing"`, `"title":"`+strings.Repeat("x", 61)+`"`, 1),
		strings.Replace(valid, `"prompt":"When?"`, `"id":"q1","prompt":"When?"`, 1),
		strings.Replace(valid, `"kind":"single"`, `"kind":"text"`, 1),
		strings.Replace(valid, `{"label":"Now"},`, "", 1),
		strings.Replace(valid, `"prompt":"When?"`, `"prompt":"When?","other":true`, 1),
		`{"title":"x","steps":[]}`, `{"title":"x","steps":[{"questions":[]}]}`,
		strings.Replace(valid, `"label":"Later"`, `"label":"Now"`, 1),
	} {
		if _, err := ParseInterview(json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
func TestReservedActionValidation(t *testing.T) {
	for _, raw := range []string{`{"actions":[]}`, `{"actions":["artifact","artifact"]}`, `{"actions":["panel"]}`, `{"actions":null}`} {
		if _, err := ParseActions(json.RawMessage(raw)); err == nil {
			t.Fatal(raw)
		}
	}
	if _, err := ParseActions(json.RawMessage(`{"actions":["artifact","ticket"]}`)); err != nil {
		t.Fatal(err)
	}
	for _, definition := range ReservedDefinitions() {
		if !json.Valid(definition.InputSchema) {
			t.Fatal(definition.Name)
		}
	}
}

func TestReservedFullUnicodeTable(t *testing.T) {
	table := Table{Columns: []string{"a", "b", "c", "d", "e", "f"}}
	for i := 0; i < 20; i++ {
		row := []string{}
		for j := 0; j < 6; j++ {
			row = append(row, strings.Repeat("😀", 120))
		}
		table.Rows = append(table.Rows, row)
	}
	raw, _ := json.Marshal(map[string]any{"type": "table", "data": table})
	if _, err := ParseBlock(raw); err != nil {
		t.Fatal(err)
	}
	escaped := strings.ReplaceAll(string(raw), "😀", `\ud83d\ude00`)
	if _, err := ParseBlock(json.RawMessage(escaped)); err != nil {
		t.Fatal(err)
	}
}
