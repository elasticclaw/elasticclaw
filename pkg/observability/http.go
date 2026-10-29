package observability

import (
	"mime"
	"net/http"
	"strconv"
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
		"elasticclaw.hub.streaming.connections",
		metric.WithDescription("Active hub streaming connections"), metric.WithUnit("{connection}"),
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
	opts = append(opts, otelhttp.WithMeterProvider(provider), otelhttp.WithServerName("elasticclaw-hub:0"), otelhttp.WithPublicEndpointFn(func(*http.Request) bool { return true }))
	instrumented := otelhttp.NewHandler(routed, "hub", opts...)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Long-lived WebSocket and SSE streams would skew request latency.
		protocol := streamingProtocol(r.Header)
		if protocol != "" {
			_, pattern := mux.Handler(r)
			if pattern == "" {
				pattern = "unmatched"
			}
			option := metric.WithAttributes(attribute.String("endpoint", pattern), attribute.String("protocol", protocol))
			connections.Add(r.Context(), 1, option)
			defer connections.Add(r.Context(), -1, option)
			// Leave the original writer intact, including http.Hijacker.
			mux.ServeHTTP(w, r)
			return
		}
		instrumented.ServeHTTP(w, r)
	})
}

// isWebSocketUpgrade matches the websocket library's handshake check: the token
// may appear anywhere in any Upgrade header value.
func isWebSocketUpgrade(h http.Header) bool {
	for _, value := range h.Values("Upgrade") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "websocket") {
				return true
			}
		}
	}
	return false
}

func streamingProtocol(h http.Header) string {
	if isWebSocketUpgrade(h) {
		return "websocket"
	}
	for _, value := range h.Values("Accept") {
		for _, part := range strings.Split(value, ",") {
			mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(part))
			if err == nil && strings.EqualFold(mediaType, "text/event-stream") {
				if q, ok := params["q"]; ok {
					quality, err := strconv.ParseFloat(q, 64)
					if err != nil || quality <= 0 || quality > 1 {
						continue
					}
				}
				return "sse"
			}
		}
	}
	return ""
}
