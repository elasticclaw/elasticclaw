// Package telemetry wires optional OpenTelemetry tracing for the hub.
//
// Tracing is opt-in: it activates only when ELASTICCLAW_OTLP_ENDPOINT is set.
// When the variable is unset, no SDK tracer provider is installed — the OTel
// global stays on its no-op implementation and every helper in this package
// short-circuits, so the cost of an unconfigured hub is effectively zero.
package telemetry

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// EndpointEnv is the environment variable that enables tracing. It accepts
// either a full URL ("http://localhost:4318") or a bare host:port
// ("localhost:4318", sent over plain HTTP).
const EndpointEnv = "ELASTICCLAW_OTLP_ENDPOINT"

// tracerName identifies the hub's spans in exported traces.
const tracerName = "github.com/elasticclaw/elasticclaw/pkg/hub"

var enabled atomic.Bool

// Setup installs a global OTLP/HTTP tracer provider when EndpointEnv is set.
// It returns a shutdown function that flushes buffered spans; when tracing is
// disabled the returned function is a no-op and nothing is installed.
func Setup(ctx context.Context) (func(context.Context) error, error) {
	endpoint := strings.TrimSpace(os.Getenv(EndpointEnv))
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	var opts []otlptracehttp.Option
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		opts = append(opts, otlptracehttp.WithEndpointURL(endpoint))
	} else {
		opts = append(opts, otlptracehttp.WithEndpoint(endpoint), otlptracehttp.WithInsecure())
	}
	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("otlp trace exporter: %w", err)
	}

	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", "elasticclaw-hub"),
	))
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	enabled.Store(true)
	return tp.Shutdown, nil
}

// Enabled reports whether Setup installed a real tracer provider.
func Enabled() bool { return enabled.Load() }

// StartProviderSpan starts a span around a provider operation (the slow calls
// that matter: create/exec/destroy). It returns the span context and an end
// function that records err (if any) and closes the span. When tracing is
// disabled both are cheap no-ops.
func StartProviderSpan(ctx context.Context, op, provider string) (context.Context, func(err error)) {
	if !enabled.Load() {
		return ctx, func(error) {}
	}
	ctx, span := otel.Tracer(tracerName).Start(ctx, "provider."+op,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("elasticclaw.provider", provider)),
	)
	return ctx, func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}
}
