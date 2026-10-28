package observability

import (
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
)

var structuredLogger atomic.Pointer[slog.Logger]
var stderrLogger = slog.New(slog.NewTextHandler(os.Stderr, nil))

// Logger returns a structured stderr logger, with an OTLP tee when enabled.
func Logger() *slog.Logger {
	if logger := structuredLogger.Load(); logger != nil {
		return logger
	}
	return stderrLogger
}

type fanoutHandler []slog.Handler

func (h fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range h {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}
func (h fanoutHandler) Handle(ctx context.Context, record slog.Record) error {
	var result error
	for _, handler := range h {
		if handler.Enabled(ctx, record.Level) {
			result = errors.Join(result, handler.Handle(ctx, record.Clone()))
		}
	}
	return result
}
func (h fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	result := make(fanoutHandler, len(h))
	for i, handler := range h {
		result[i] = handler.WithAttrs(attrs)
	}
	return result
}
func (h fanoutHandler) WithGroup(name string) slog.Handler {
	result := make(fanoutHandler, len(h))
	for i, handler := range h {
		result[i] = handler.WithGroup(name)
	}
	return result
}

// The stdlib emits each Print call in one Write, including its trailing newline.
// Keep those bytes intact on the original writer and strip only its header from
// the independent OTLP copy. Source-location flags are supported as well.
type logTee struct {
	writer io.Writer
	logger otellog.Logger
	flags  int
	prefix string
}

var dateHeader = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} `)
var timeHeader = regexp.MustCompile(`^\d{2}:\d{2}:\d{2}(\.\d+)? `)
var fileHeader = regexp.MustCompile(`^.*?:\d+: `)

func (w *logTee) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	message := string(p)
	if w.flags&log.Lmsgprefix == 0 {
		message = strings.TrimPrefix(message, w.prefix)
	}
	if w.flags&log.Ldate != 0 {
		message = dateHeader.ReplaceAllString(message, "")
	}
	if w.flags&(log.Ltime|log.Lmicroseconds) != 0 {
		message = timeHeader.ReplaceAllString(message, "")
	}
	if w.flags&(log.Lshortfile|log.Llongfile) != 0 {
		message = fileHeader.ReplaceAllString(message, "")
	}
	if w.flags&log.Lmsgprefix != 0 {
		message = strings.TrimPrefix(message, w.prefix)
	}
	for _, line := range strings.Split(strings.TrimSuffix(message, "\n"), "\n") {
		var record otellog.Record
		record.SetTimestamp(time.Now())
		record.SetObservedTimestamp(time.Now())
		record.SetSeverity(otellog.SeverityInfo)
		record.SetSeverityText("INFO")
		body := line
		if strings.HasPrefix(body, "[") {
			if end := strings.IndexByte(body, ']'); end > 1 {
				record.AddAttributes(attribute.String("log.component", body[1:end]))
				body = strings.TrimSpace(body[end+1:])
			}
		}
		if body == "panic" || strings.HasPrefix(body, "panic:") || strings.HasPrefix(body, "panic ") {
			record.SetSeverity(otellog.SeverityError)
			record.SetSeverityText("ERROR")
		}
		record.SetBody(attribute.StringValue(line))
		w.logger.Emit(context.Background(), record)
	}
	return n, err
}
