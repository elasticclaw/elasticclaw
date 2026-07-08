package hub

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"runtime/debug"

	"github.com/elasticclaw/elasticclaw/pkg/hub/logger"
)

// apiError is the single JSON error envelope returned by API handlers.
// The frontend (web/lib/api.ts) parses the "error" field and falls back to
// plain text, so handlers can migrate to this envelope gradually.
type apiError struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// writeErr writes the unified JSON error envelope with the given HTTP status.
// code is a stable machine-readable identifier (e.g. "not_found"); msg is a
// human-readable description safe to surface in the UI.
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiError{Error: msg, Code: code})
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

// withRequestID generates a short request ID for every incoming request,
// echoes it back in the X-Request-ID response header, and stores a logger
// carrying request_id in the request context. Downstream code retrieves it
// via logger.FromContext(ctx).
func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set("X-Request-ID", id)
		l := s.logger.With("request_id", id)
		next.ServeHTTP(w, r.WithContext(logger.NewContext(r.Context(), l)))
	})
}

// withRecovery is the outermost middleware: it recovers from panics in any
// downstream handler, logs the stack trace with the request context available
// at this phase (method, path, remote addr, request_id via the context
// logger), and responds with a 500 JSON envelope.
func withRecovery(next http.Handler) http.Handler {
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
			logfCtx(r.Context(), "[hub] panic recovered: %v (%s %s from %s)\n%s",
				rec, r.Method, r.URL.Path, r.RemoteAddr, debug.Stack())
			// Best effort: if the handler already wrote a response this is a
			// no-op apart from a superfluous-WriteHeader log line.
			writeErr(w, http.StatusInternalServerError, "internal", "internal server error")
		}()
		next.ServeHTTP(w, r)
	})
}
