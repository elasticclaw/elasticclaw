package observability

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func httpTestProviders(t *testing.T) (*sdkmetric.MeterProvider, *sdkmetric.ManualReader, *sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	spans := tracetest.NewSpanRecorder()
	tracer := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	t.Cleanup(func() {
		_ = meter.Shutdown(context.Background())
		_ = tracer.Shutdown(context.Background())
	})
	return meter, reader, tracer, spans
}

func collectHTTPMetrics(t *testing.T, reader *sdkmetric.ManualReader) []metricdata.Metrics {
	t.Helper()
	var resource metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &resource); err != nil {
		t.Fatal(err)
	}
	var metrics []metricdata.Metrics
	for _, scope := range resource.ScopeMetrics {
		metrics = append(metrics, scope.Metrics...)
	}
	return metrics
}

func TestMiddlewareRouteMetrics(t *testing.T) {
	for _, tc := range []struct{ name, pattern, path, route string }{
		{"parameter", "/api/factories/{name}/trigger", "/api/factories/private-name/trigger", "/api/factories/{name}/trigger"},
		{"unmatched", "", "/arbitrary/private/path", "unmatched"},
		{"static", "/", "/assets/arbitrary-hash.js", "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meter, reader, tracer, spans := httpTestProviders(t)
			mux := http.NewServeMux()
			if tc.pattern != "" {
				mux.HandleFunc(tc.pattern, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
			}
			handler := middleware(mux, meter, otelhttp.WithTracerProvider(tracer))
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Host = "unbounded-user-host.example:12345"
			handler.ServeHTTP(httptest.NewRecorder(), req)
			found := false
			for _, m := range collectHTTPMetrics(t, reader) {
				if m.Name != "http.server.request.duration" {
					continue
				}
				found = true
				points := m.Data.(metricdata.Histogram[float64]).DataPoints
				if len(points) != 1 {
					t.Fatalf("points = %d", len(points))
				}
				attrs := points[0].Attributes
				route, _ := attrs.Value(attribute.Key("http.route"))
				if route.AsString() != tc.route {
					t.Errorf("route = %q, want %q", route.AsString(), tc.route)
				}
				host, _ := attrs.Value(attribute.Key("server.address"))
				if host.AsString() != "elasticclaw-hub" {
					t.Errorf("unbounded host: %v", host)
				}
				if points[0].Count != 1 {
					t.Errorf("count = %d", points[0].Count)
				}
			}
			if !found {
				t.Fatal("missing HTTP duration histogram")
			}
			ended := spans.Ended()
			if len(ended) != 1 {
				t.Fatalf("spans = %d", len(ended))
			}
			attrs := attribute.NewSet(ended[0].Attributes()...)
			route, _ := attrs.Value(attribute.Key("http.route"))
			if route.AsString() != tc.route {
				t.Errorf("span route = %q", route.AsString())
			}
		})
	}
}

// No socket is opened: this writer verifies that the upgrade handler can hijack.
type hijackRecorder struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (w *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	return nil, nil, errors.New("test hijack")
}

func TestMiddlewareWebSocket(t *testing.T) {
	for _, tc := range []struct{ endpoint, path string }{
		{"/claw/ws", "/claw/ws"},
		{"/claw/control/ws", "/claw/control/ws"},
		{"/api/ws", "/api/ws"},
		{"/api/terminal/", "/api/terminal/claw-1"},
	} {
		endpoint := tc.endpoint
		t.Run(endpoint, func(t *testing.T) {
			meter, reader, tracer, spans := httpTestProviders(t)
			assertConnections := func(want int64) {
				t.Helper()
				found := false
				for _, m := range collectHTTPMetrics(t, reader) {
					if m.Name == "http.server.request.duration" {
						t.Error("WebSocket recorded HTTP duration")
					}
					if m.Name != "elasticclaw.hub.streaming.connections" {
						continue
					}
					found = true
					points := m.Data.(metricdata.Sum[int64]).DataPoints
					if len(points) != 1 {
						t.Fatalf("points = %d", len(points))
					}
					if points[0].Value != want {
						t.Errorf("connections = %d, want %d", points[0].Value, want)
					}
					attr, _ := points[0].Attributes.Value(attribute.Key("endpoint"))
					protocol, _ := points[0].Attributes.Value(attribute.Key("protocol"))
					if protocol.AsString() != "websocket" {
						t.Errorf("protocol = %q", protocol.AsString())
					}
					if attr.AsString() != endpoint {
						t.Errorf("endpoint = %q", attr.AsString())
					}
				}
				if !found {
					t.Error("missing WebSocket counter")
				}
			}
			mux := http.NewServeMux()
			mux.HandleFunc(endpoint, func(w http.ResponseWriter, r *http.Request) {
				assertConnections(1)
				if len(spans.Started()) != 0 {
					t.Error("WebSocket started server span")
				}
				hijacker, ok := w.(http.Hijacker)
				if !ok {
					t.Fatal("lost http.Hijacker")
				}
				_, _, _ = hijacker.Hijack()
			})
			handler := middleware(mux, meter, otelhttp.WithTracerProvider(tracer))
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Upgrade", "WebSocket")
			w := &hijackRecorder{ResponseRecorder: httptest.NewRecorder()}
			handler.ServeHTTP(w, req)
			if !w.hijacked {
				t.Error("handler did not hijack")
			}
			assertConnections(0)
			if len(spans.Ended()) != 0 {
				t.Error("WebSocket recorded span")
			}
		})
	}
}

func TestIsWebSocketUpgrade(t *testing.T) {
	for _, tc := range []struct {
		values []string
		want   bool
	}{
		{[]string{"websocket"}, true},
		{[]string{"WebSocket"}, true},
		{[]string{"h2c, websocket"}, true},
		{[]string{"h2c", " websocket "}, true},
		{[]string{"h2c"}, false},
		{nil, false},
	} {
		h := http.Header{}
		for _, v := range tc.values {
			h.Add("Upgrade", v)
		}
		if got := isWebSocketUpgrade(h); got != tc.want {
			t.Errorf("Upgrade %q = %v, want %v", tc.values, got, tc.want)
		}
	}
}

func TestMiddlewarePublicEndpoint(t *testing.T) {
	meter, _, tracer, spans := httpTestProviders(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {})
	handler := middleware(mux, meter, otelhttp.WithTracerProvider(tracer), otelhttp.WithPropagators(propagation.TraceContext{}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("traceparent", "00-0102030405060708090a0b0c0d0e0f10-0102030405060708-01")
	handler.ServeHTTP(httptest.NewRecorder(), req)
	ended := spans.Ended()
	if len(ended) != 1 {
		t.Fatalf("spans = %d", len(ended))
	}
	if ended[0].Parent().IsValid() {
		t.Error("untrusted traceparent became parent")
	}
	links := ended[0].Links()
	remoteTrace, _ := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	if len(links) != 1 || links[0].SpanContext.TraceID() != remoteTrace {
		t.Fatalf("missing incoming trace link: %v", links)
	}
}

func TestMiddlewareSSE(t *testing.T) {
	meter, reader, tracer, spans := httpTestProviders(t)
	assertConnections := func(want int64) {
		t.Helper()
		found := false
		for _, m := range collectHTTPMetrics(t, reader) {
			if m.Name == "http.server.request.duration" {
				t.Error("SSE recorded HTTP duration")
			}
			if m.Name != "elasticclaw.hub.streaming.connections" {
				continue
			}
			found = true
			points := m.Data.(metricdata.Sum[int64]).DataPoints
			if len(points) != 1 || points[0].Value != want {
				t.Fatalf("connections = %#v, want %d", points, want)
			}
			endpoint, _ := points[0].Attributes.Value(attribute.Key("endpoint"))
			protocol, _ := points[0].Attributes.Value(attribute.Key("protocol"))
			if endpoint.AsString() != "/stream/{id}" || protocol.AsString() != "sse" {
				t.Errorf("attributes = %v", points[0].Attributes)
			}
		}
		if !found {
			t.Error("missing SSE counter")
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/stream/{id}", func(w http.ResponseWriter, r *http.Request) {
		assertConnections(1)
		if len(spans.Started()) != 0 {
			t.Error("SSE started server span")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
	})
	handler := middleware(mux, meter, otelhttp.WithTracerProvider(tracer))
	req := httptest.NewRequest(http.MethodGet, "/stream/private-id", nil)
	req.Header.Set("Accept", "application/json, text/event-stream; charset=utf-8")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if !w.Flushed {
		t.Error("handler did not flush")
	}
	assertConnections(0)
	if len(spans.Ended()) != 0 {
		t.Error("SSE recorded span")
	}
}

func TestStreamingProtocol(t *testing.T) {
	for _, tc := range []struct {
		accept []string
		want   string
	}{
		{nil, ""},
		{[]string{"application/json"}, ""},
		{[]string{"text/event-stream"}, "sse"},
		{[]string{"application/json", "text/event-stream; charset=utf-8"}, "sse"},
		{[]string{"application/json, text/event-stream;q=0.5"}, "sse"},
		{[]string{"text/event-stream;q=0.0"}, ""},
		{[]string{"text/event-stream;q=invalid"}, ""},
	} {
		h := http.Header{"Accept": tc.accept}
		if got := streamingProtocol(h); got != tc.want {
			t.Errorf("Accept %q = %q, want %q", tc.accept, got, tc.want)
		}
	}
}
