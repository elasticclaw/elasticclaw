// Package observability provides opt-in telemetry for the hub.
package observability

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	runtimeotel "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

var enabled atomic.Bool

// Enabled reports whether Setup activated telemetry. Callers must use it rather
// than the config flag, which stays as written in hub.yaml even when invalid.
func Enabled() bool { return enabled.Load() }

// Setup is called once at startup, before HTTP clients or handlers are started.
// Shutdown must be called after the server has drained its requests.
func Setup(ctx context.Context, cfg *types.ObservabilityConfig, version, commit string) (func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }
	if cfg == nil || !cfg.Enabled {
		return noop, nil
	}
	if err := cfg.Validate(); err != nil {
		return noop, err
	}
	c := cfg.WithDefaults()
	interval, _ := time.ParseDuration(c.MetricsInterval)
	hostname, _ := os.Hostname()
	attrs := []attribute.KeyValue{
		attribute.String("service.name", c.ServiceName), attribute.String("service.version", version),
		attribute.String("service.instance.id", hostname), attribute.String("host.name", hostname),
	}
	if commit != "" {
		attrs = append(attrs, attribute.String("service.commit", commit))
	}
	if c.Environment != "" {
		attrs = append(attrs, attribute.String("deployment.environment.name", c.Environment))
	}
	res := resource.NewSchemaless(attrs...)
	// Endpoint options override SDK environment settings, so only supply the YAML
	// fallback when neither the common nor signal-specific endpoint is set.
	var traceOpts []otlptracehttp.Option
	var metricOpts []otlpmetrichttp.Option
	var logOpts []otlploghttp.Option
	if endpointFallback("TRACES") {
		traceOpts = append(traceOpts, otlptracehttp.WithEndpointURL(strings.TrimRight(c.OTLPEndpoint, "/")+"/v1/traces"))
	}
	if endpointFallback("METRICS") {
		metricOpts = append(metricOpts, otlpmetrichttp.WithEndpointURL(strings.TrimRight(c.OTLPEndpoint, "/")+"/v1/metrics"))
	}
	if endpointFallback("LOGS") {
		logOpts = append(logOpts, otlploghttp.WithEndpointURL(strings.TrimRight(c.OTLPEndpoint, "/")+"/v1/logs"))
	}
	// Drop failed batches instead of retrying against an unavailable collector.
	traceOpts = append(traceOpts, otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}))
	metricOpts = append(metricOpts, otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{Enabled: false}))
	logOpts = append(logOpts, otlploghttp.WithRetry(otlploghttp.RetryConfig{Enabled: false}))
	te, err := otlptracehttp.New(ctx, traceOpts...)
	if err != nil {
		return noop, err
	}
	cleanups := []func(context.Context) error{te.Shutdown}
	cleanup := func() { boundedShutdown(context.Background(), cleanups...) }
	me, err := otlpmetrichttp.New(ctx, metricOpts...)
	if err != nil {
		cleanup()
		return noop, err
	}
	cleanups = append(cleanups, me.Shutdown)
	le, err := otlploghttp.New(ctx, logOpts...)
	if err != nil {
		cleanup()
		return noop, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithSpanProcessor(redactingSpanProcessor{sdktrace.NewBatchSpanProcessor(te)}), sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(*c.TracesSampleRatio))))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithView(httpMetricView()), sdkmetric.WithReader(sdkmetric.NewPeriodicReader(me, sdkmetric.WithInterval(interval))))
	lp := sdklog.NewLoggerProvider(sdklog.WithResource(res), sdklog.WithProcessor(sdklog.NewBatchProcessor(le)))
	cleanups = []func(context.Context) error{tp.Shutdown, mp.Shutdown, lp.Shutdown}
	if err := runtimeotel.Start(runtimeotel.WithMeterProvider(mp)); err != nil {
		cleanup()
		return noop, err
	}

	oldOutput, oldLogger := log.Writer(), Logger()
	// Report directly to the original writer: logging through the tee here would
	// create another failing export and a self-sustaining error loop.
	diagnostic := log.New(oldOutput, log.Prefix(), log.Flags())
	var errorMu sync.Mutex
	var lastError time.Time
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) {
		errorMu.Lock()
		defer errorMu.Unlock()
		if time.Since(lastError) >= time.Minute {
			lastError = time.Now()
			diagnostic.Print("[hub] observability export failed; telemetry dropped (further errors suppressed for 1m)")
		}
	}))
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	global.SetLoggerProvider(lp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	http.DefaultTransport = otelhttp.NewTransport(http.DefaultTransport)
	log.SetOutput(&logTee{writer: oldOutput, logger: lp.Logger("elasticclaw.hub"), flags: log.Flags(), prefix: log.Prefix()})
	structuredLogger.Store(slog.New(fanoutHandler{slog.NewTextHandler(os.Stderr, nil), otelslog.NewHandler("elasticclaw.hub", otelslog.WithLoggerProvider(lp))}))
	enabled.Store(true)
	var once sync.Once
	var shutdownErr error
	return func(ctx context.Context) error {
		once.Do(func() {
			// Only the log output is restored: log.SetOutput is synchronized, while
			// swapping http.DefaultTransport or the OTel globals back would race with
			// pollers still running. Shut-down providers turn into no-ops instead.
			enabled.Store(false)
			log.SetOutput(oldOutput)
			structuredLogger.Store(oldLogger)
			shutdownErr = boundedShutdown(ctx, cleanups...)
		})
		return shutdownErr
	}, nil
}

func endpointFallback(signal string) bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_"+signal+"_ENDPOINT") == ""
}

func boundedShutdown(ctx context.Context, shutdowns ...func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	results := make(chan error, len(shutdowns))
	for _, shutdown := range shutdowns {
		go func() { results <- shutdown(ctx) }()
	}
	var result error
	for range shutdowns {
		select {
		case err := <-results:
			result = errors.Join(result, err)
		case <-ctx.Done():
			return errors.Join(result, ctx.Err())
		}
	}
	return result
}

// Keep remote hosts on client metrics only; incoming hosts and URL/error strings
// must never create unbounded server metric labels.
func httpMetricView() sdkmetric.View {
	return func(instrument sdkmetric.Instrument) (sdkmetric.Stream, bool) {
		if !strings.HasPrefix(instrument.Name, "http.") {
			return sdkmetric.Stream{}, false
		}
		client := strings.HasPrefix(instrument.Name, "http.client.")
		return sdkmetric.Stream{Name: instrument.Name, Description: instrument.Description, Unit: instrument.Unit, AttributeFilter: func(kv attribute.KeyValue) bool {
			switch kv.Key {
			case "http.route", "http.request.method", "http.response.status_code", "http.method", "http.status_code":
				return true
			case "server.address":
				return client
			default:
				return false
			}
		}}, true
	}
}

// otelhttp sets url.full after OnStart. Sanitize the completed span before the
// batch processor snapshots it, without changing the actual outbound request.
type redactingSpanProcessor struct {
	sdktrace.SpanProcessor
}

func (p redactingSpanProcessor) OnEnd(span sdktrace.ReadOnlySpan) {
	if span.SpanKind() == trace.SpanKindClient {
		attrs := append([]attribute.KeyValue(nil), span.Attributes()...)
		for i, attr := range attrs {
			if attr.Key != "url.full" {
				continue
			}
			u, err := url.Parse(attr.Value.AsString())
			value := ""
			if err == nil {
				u.RawQuery, u.Fragment, u.RawFragment, u.ForceQuery, u.User = "", "", "", false, nil
				value = u.String()
			}
			attrs[i] = attribute.String("url.full", value)
		}
		span = redactedSpan{ReadOnlySpan: span, attributes: attrs}
	}
	p.SpanProcessor.OnEnd(span)
}

type redactedSpan struct {
	sdktrace.ReadOnlySpan
	attributes []attribute.KeyValue
}

func (s redactedSpan) Attributes() []attribute.KeyValue { return s.attributes }
