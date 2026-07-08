// Package httpserver holds the hub's HTTP plumbing: the route table, the
// middleware chain (recovery, request ID, CORS, metrics) and the JSON
// response helpers. Handlers stay in their owning packages; the router
// references them through the Handlers struct and the Auth interface so the
// extraction of pkg/hub can proceed one subpackage at a time.
package httpserver

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/logger"
)

// APIError is the single JSON error envelope returned by API handlers.
// The frontend (web/lib/api.ts) parses the "error" field and falls back to
// plain text, so handlers can migrate to this envelope gradually.
type APIError struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// WriteErr writes the unified JSON error envelope with the given HTTP status.
// code is a stable machine-readable identifier (e.g. "not_found"); msg is a
// human-readable description safe to surface in the UI.
func WriteErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(APIError{Error: msg, Code: code})
}

// JSONOK writes v as a JSON response body with status 200.
func JSONOK(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// JSONError writes the JSON error envelope without a machine-readable code.
func JSONError(w http.ResponseWriter, status int, msg string) {
	WriteErr(w, status, "", msg)
}

// newRequestID returns a short random hex ID, enough to correlate the log
// lines of a single request.
func newRequestID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b[:])
}

// WithRequestID generates a short request ID for every incoming request,
// echoes it back in the X-Request-ID response header, and stores a logger
// carrying request_id in the request context. Downstream code retrieves it
// via logger.FromContext(ctx).
func WithRequestID(base *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set("X-Request-ID", id)
		l := base.With("request_id", id)
		next.ServeHTTP(w, r.WithContext(logger.NewContext(r.Context(), l)))
	})
}

// statusRecorder captures the response status code while delegating
// everything else — including Hijack (WebSocket upgrades) and Flush — to the
// wrapped ResponseWriter.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Hijack supports handlers (WebSocket upgrades) that type-assert
// http.Hijacker directly instead of going through http.ResponseController.
func (w *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if w.status == 0 {
		w.status = http.StatusSwitchingProtocols
	}
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

// Flush supports streaming handlers that type-assert http.Flusher directly.
func (w *statusRecorder) Flush() {
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// MetricsRecorder is the slice of the hub's Prometheus recorder that the
// metrics middleware needs. Defined here so httpserver does not depend on
// the hub package.
type MetricsRecorder interface {
	ObserveRequest(route, method string, status int, elapsed time.Duration)
	WebhookError(integration string)
}

// WithMetrics records a request counter and a latency histogram for every
// request, labeled by the ServeMux route pattern (never the raw path, to keep
// cardinality bounded). It must wrap the mux directly so r.Pattern is set on
// the same *http.Request it observes. Failed integration webhook deliveries
// (4xx/5xx) are additionally counted per integration.
func WithMetrics(m MetricsRecorder, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(rec, r)
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		m.ObserveRequest(route, r.Method, status, time.Since(start))
		if status >= 400 {
			if integration := webhookIntegration(route); integration != "" {
				m.WebhookError(integration)
			}
		}
	})
}

// webhookIntegration extracts the integration name from a webhook route
// pattern ("/api/integrations/linear/webhook" or
// "/api/workspaces/{workspace}/webhooks/linear"); it returns "" for
// non-webhook routes.
func webhookIntegration(route string) string {
	parts := strings.Split(strings.Trim(route, "/"), "/")
	switch {
	case len(parts) == 4 && parts[0] == "api" && parts[1] == "integrations" && parts[3] == "webhook":
		return parts[2]
	case len(parts) == 5 && parts[0] == "api" && parts[1] == "workspaces" && parts[3] == "webhooks":
		return parts[4]
	}
	return ""
}

// WithRecovery is the outermost middleware: it recovers from panics in any
// downstream handler, logs the stack trace with the request context available
// at this phase (method, path, remote addr, request_id via the context
// logger), and responds with a 500 JSON envelope.
func WithRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// http.ErrAbortHandler is the stdlib's sentinel to abort a
			// response without logging a stack trace; preserve that contract.
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			logger.FromContext(r.Context()).Info(
				fmt.Sprintf("panic recovered: %v (%s %s from %s)\n%s",
					rec, r.Method, r.URL.Path, r.RemoteAddr, debug.Stack()),
				"component", "hub")
			// Best effort: if the handler already wrote a response this is a
			// no-op apart from a superfluous-WriteHeader log line.
			WriteErr(w, http.StatusInternalServerError, "internal", "internal server error")
		}()
		next.ServeHTTP(w, r)
	})
}

// CORS adds permissive CORS headers so the web UI can connect from any
// origin (browser same-origin restrictions apply to REST + WS).
func CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, ngrok-skip-browser-warning")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
