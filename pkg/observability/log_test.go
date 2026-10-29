package observability

import (
	"bytes"
	"context"
	"log"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/trace"
)

type logRecorder struct{ records []sdklog.Record }

func (e *logRecorder) Export(_ context.Context, records []sdklog.Record) error {
	for _, record := range records {
		e.records = append(e.records, record.Clone())
	}
	return nil
}
func (*logRecorder) Shutdown(context.Context) error   { return nil }
func (*logRecorder) ForceFlush(context.Context) error { return nil }

func TestLogTee(t *testing.T) {
	for _, flags := range []int{0, log.LstdFlags, log.Ldate | log.Lmicroseconds, log.LstdFlags | log.Lshortfile | log.Lmsgprefix} {
		t.Run(string(rune('A'+flags)), func(t *testing.T) {
			var original bytes.Buffer
			exporter := &logRecorder{}
			provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))
			defer provider.Shutdown(t.Context())
			tee := &logTee{writer: &original, logger: provider.Logger("test"), flags: flags, prefix: "prefix "}
			logger := log.New(tee, "prefix ", flags)
			logger.Print("[bridge] hello")
			if len(exporter.records) != 1 {
				t.Fatalf("records: %d", len(exporter.records))
			}
			record := exporter.records[0]
			if record.Body().AsString() != "[bridge] hello" {
				t.Fatalf("body: %q", record.Body().AsString())
			}
			if record.Severity() != otellog.SeverityInfo {
				t.Fatalf("severity: %v", record.Severity())
			}
			component := ""
			record.WalkAttributes(func(kv attribute.KeyValue) bool {
				if kv.Key == "log.component" {
					component = kv.Value.AsString()
				}
				return true
			})
			if component != "bridge" {
				t.Fatalf("component: %q", component)
			}
			if !strings.Contains(original.String(), "[bridge] hello\n") {
				t.Fatalf("original: %q", original.String())
			}
			original.Reset()
			input := []byte("2026/10/28 12:30:45 [hub] panic: example\n")
			n, err := tee.Write(input)
			if err != nil || n != len(input) || !bytes.Equal(original.Bytes(), input) {
				t.Fatalf("tee changed bytes: %q, %v", original.Bytes(), err)
			}
		})
	}
}

func TestStructuredLoggerCorrelation(t *testing.T) {
	var output bytes.Buffer
	exporter := &logRecorder{}
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))
	defer provider.Shutdown(t.Context())
	logger := slog.New(fanoutHandler{slog.NewTextHandler(&output, nil), otelslog.NewHandler("test", otelslog.WithLoggerProvider(provider))}).With("component", "test").WithGroup("operation")
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled})
	logger.InfoContext(trace.ContextWithSpanContext(t.Context(), sc), "hello", "key", "value")
	if len(exporter.records) != 1 {
		t.Fatalf("records: %d", len(exporter.records))
	}
	record := exporter.records[0]
	if record.TraceID() != sc.TraceID() || record.SpanID() != sc.SpanID() {
		t.Fatal("missing trace correlation")
	}
	if !strings.Contains(output.String(), "operation.key=value") {
		t.Fatalf("stderr: %s", output.String())
	}
}
