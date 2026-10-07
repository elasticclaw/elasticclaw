package tools

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestStrictReadArguments(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{"query":"a","query":"b"}`, `{"query":"a","Query":"b"}`, `{"query":null}`, `{"unknown":true}`, `{"query":"a"} {}`} {
		var args struct {
			Query string `json:"query"`
		}
		var argErr ArgError
		if err := DecodeArgs(json.RawMessage(raw), &args); !errors.As(err, &argErr) {
			t.Errorf("argument error for %s: %v", raw, err)
		}
	}
	var args struct {
		Query string `json:"query"`
	}
	if err := DecodeArgs(json.RawMessage(`{"query":"a"}`), &args); err != nil || args.Query != "a" {
		t.Fatalf("valid args: %+v %v", args, err)
	}
}
