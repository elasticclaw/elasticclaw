package observability

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Middleware instruments hub routes. Call it only when observability is enabled.
func Middleware(mux *http.ServeMux, opts ...otelhttp.Option) http.Handler {
	return middleware(mux, otel.GetMeterProvider(), opts...)
}

func middleware(mux *http.ServeMux, provider metric.MeterProvider, opts ...otelhttp.Option) http.Handler {
	connections, _ := provider.Meter("github.com/elasticclaw/elasticclaw/pkg/observability").Int64UpDownCounter(
		"elasticclaw.hub.websocket.connections",
		metric.WithDescription("Active hub WebSocket connections"), metric.WithUnit("{connection}"),
	)
	routed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
		// ServeMux fills Pattern during dispatch. Never use URL.Path as a fallback.
		if r.Pattern == "" {
			r.Pattern = "unmatched"
		}
		route := attribute.String("http.route", r.Pattern)
		labeler, _ := otelhttp.LabelerFromContext(r.Context())
		labeler.Add(route)
		trace.SpanFromContext(r.Context()).SetAttributes(route)
	})
	opts = append(opts, otelhttp.WithMeterProvider(provider), otelhttp.WithServerName("elasticclaw-hub:0"))
	instrumented := otelhttp.NewHandler(routed, "hub", opts...)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
			_, pattern := mux.Handler(r)
			switch pattern {
			case "/claw/ws", "/claw/control/ws", "/api/ws":
				option := metric.WithAttributes(attribute.String("endpoint", pattern))
				connections.Add(r.Context(), 1, option)
				defer connections.Add(r.Context(), -1, option)
				// Leave the original writer intact, including http.Hijacker.
				mux.ServeHTTP(w, r)
				return
			}
		}
		instrumented.ServeHTTP(w, r)
	})
}
