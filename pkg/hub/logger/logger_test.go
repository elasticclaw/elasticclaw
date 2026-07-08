package logger

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestFromContextFallsBackToDefault(t *testing.T) {
	if got := FromContext(context.Background()); got != slog.Default() {
		t.Fatalf("expected slog.Default() fallback, got %v", got)
	}
}

func TestNewContextRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, nil)).With("request_id", "abc123")
	ctx := NewContext(context.Background(), l)
	FromContext(ctx).Info("hello")
	if out := buf.String(); !strings.Contains(out, "request_id=abc123") || !strings.Contains(out, "hello") {
		t.Fatalf("logged line missing request_id or message: %q", out)
	}
}

func TestNewFormatSelection(t *testing.T) {
	t.Setenv("ELASTICCLAW_LOG_FORMAT", "text")
	if _, ok := New().Handler().(*slog.TextHandler); !ok {
		t.Fatalf("expected TextHandler for ELASTICCLAW_LOG_FORMAT=text")
	}
	t.Setenv("ELASTICCLAW_LOG_FORMAT", "")
	if _, ok := New().Handler().(*slog.JSONHandler); !ok {
		t.Fatalf("expected JSONHandler by default")
	}
}
