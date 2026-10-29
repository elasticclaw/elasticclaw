// Package observability provides opt-in telemetry for the hub.
package observability

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
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
)

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
	tp := sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithBatcher(te), sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(*c.TracesSampleRatio))))
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
	var once sync.Once
	var shutdownErr error
	return func(ctx context.Context) error {
		once.Do(func() {
			// Only the log output is restored: log.SetOutput is synchronized, while
			// swapping http.DefaultTransport or the OTel globals back would race with
			// pollers still running. Shut-down providers turn into no-ops instead.
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

// Do not let HTTP hosts, URLs, or exporter-provided error strings become labels.
func httpMetricView() sdkmetric.View {
	return sdkmetric.NewView(sdkmetric.Instrument{Name: "http.*"}, sdkmetric.Stream{AttributeFilter: func(kv attribute.KeyValue) bool {
		switch kv.Key {
		case "http.route", "http.request.method", "http.response.status_code", "http.method", "http.status_code":
			return true
		default:
			return false
		}
	}})
}
