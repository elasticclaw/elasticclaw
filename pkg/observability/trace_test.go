package observability

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientSpanRedactsQuery(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(redactingSpanProcessor{sdktrace.NewSimpleSpanProcessor(exporter)}))
	defer provider.Shutdown(t.Context())
	transport := otelhttp.NewTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.RawQuery != "token=private&search=secret" {
			t.Errorf("outbound query was changed: %q", r.URL.RawQuery)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}, nil
	}), otelhttp.WithTracerProvider(provider))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.example.com/v1/items?token=private&search=secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := res.Body.Close(); err != nil {
		t.Fatal(err)
	}
	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d", len(spans))
	}
	attrs := attribute.NewSet(spans[0].Attributes...)
	url, ok := attrs.Value("url.full")
	if !ok || url.AsString() != "https://api.example.com/v1/items" {
		t.Fatalf("url.full = %q", url.AsString())
	}
}
