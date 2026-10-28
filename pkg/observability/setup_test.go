package observability

import (
	"context"
	"errors"
	"log"
	"net/http"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestSetupDisabled(t *testing.T) {
	tp, mp, lp := otel.GetTracerProvider(), otel.GetMeterProvider(), global.GetLoggerProvider()
	propagation, errorsHandler := otel.GetTextMapPropagator(), otel.GetErrorHandler()
	transport, writer, logger := http.DefaultTransport, log.Writer(), Logger()
	for _, cfg := range []*types.ObservabilityConfig{nil, {}, {Enabled: false, OTLPEndpoint: ":invalid"}, {Enabled: true, MetricsInterval: "bad"}} {
		shutdown, err := Setup(t.Context(), cfg, "test", "commit")
		if cfg != nil && cfg.Enabled {
			if err == nil {
				t.Fatal("invalid config accepted")
			}
		} else if err != nil {
			t.Fatal(err)
		}
		if err := shutdown(t.Context()); err != nil {
			t.Fatal(err)
		}
		if tp != otel.GetTracerProvider() || mp != otel.GetMeterProvider() || lp != global.GetLoggerProvider() || propagation != otel.GetTextMapPropagator() || errorsHandler != otel.GetErrorHandler() || transport != http.DefaultTransport || writer != log.Writer() || logger != Logger() {
			t.Fatal("disabled setup changed global state")
		}
	}
}

func TestEndpointEnvironmentPrecedence(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	if !endpointFallback("TRACES") {
		t.Fatal("missing YAML fallback")
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
	if endpointFallback("TRACES") {
		t.Fatal("common endpoint ignored")
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://trace:4318/v1/traces")
	if endpointFallback("TRACES") {
		t.Fatal("signal endpoint ignored")
	}
}

func TestBoundedShutdown(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := boundedShutdown(ctx, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestHTTPMetricCardinality(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithView(httpMetricView()))
	defer provider.Shutdown(t.Context())
	histogram, err := provider.Meter("test").Float64Histogram("http.client.request.duration")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"one", "two"} {
		histogram.Record(t.Context(), 1, metric.WithAttributes(attribute.String("server.address", value), attribute.String("url.path", value), attribute.String("error.type", value), attribute.String("http.request.method", "GET")))
	}
	var data metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &data); err != nil {
		t.Fatal(err)
	}
	points := data.ScopeMetrics[0].Metrics[0].Data.(metricdata.Histogram[float64]).DataPoints
	if len(points) != 1 || points[0].Count != 2 || points[0].Attributes.Len() != 1 {
		t.Fatalf("unbounded attributes survived: %#v", points)
	}
}
