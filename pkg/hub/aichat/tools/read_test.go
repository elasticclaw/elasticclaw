package tools

import (
	"encoding/json"
	"testing"
)

func TestStrictReadArguments(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{"query":"a","query":"b"}`, `{"query":"a","Query":"b"}`, `{"query":null}`, `{"unknown":true}`, `{"query":"a"} {}`} {
		var args struct {
			Query string `json:"query"`
		}
		if DecodeArgs(json.RawMessage(raw), &args) == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	var args struct {
		Query string `json:"query"`
	}
	if err := DecodeArgs(json.RawMessage(`{"query":"a"}`), &args); err != nil || args.Query != "a" {
		t.Fatalf("valid args: %+v %v", args, err)
	}
}
