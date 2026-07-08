package hub

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub/logger"
)

func TestLogfComponentExtraction(t *testing.T) {
	var buf bytes.Buffer
	logger.Msgf(slog.New(slog.NewJSONHandler(&buf, nil)), "[claw] instance %s started", "abc")
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("log line is not JSON: %q", buf.String())
	}
	if entry["component"] != "claw" {
		t.Fatalf("expected component=claw, got %v", entry["component"])
	}
	if entry["msg"] != "instance abc started" {
		t.Fatalf("unexpected message: %v", entry["msg"])
	}

	buf.Reset()
	entry = map[string]any{}
	logger.Msgf(slog.New(slog.NewJSONHandler(&buf, nil)), "no tag here %d", 7)
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("log line is not JSON: %q", buf.String())
	}
	if _, ok := entry["component"]; ok {
		t.Fatalf("did not expect a component attribute: %v", entry)
	}
	if entry["msg"] != "no tag here 7" {
		t.Fatalf("unexpected message: %v", entry["msg"])
	}
}
