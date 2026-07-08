// Package logger provides the hub's structured logging setup on top of
// log/slog, plus context helpers so request-scoped attributes (request_id,
// tenant_id) travel with a context.Context.
package logger

import (
	"context"
	"log/slog"
	"os"
)

// ctxKey is the private context key under which the request-scoped logger is
// stored.
type ctxKey struct{}

// New builds the root logger for the hub process. Output format is controlled
// by ELASTICCLAW_LOG_FORMAT: "text" produces human-readable lines (dev),
// anything else (including unset) produces JSON.
func New() *slog.Logger {
	var h slog.Handler
	switch os.Getenv("ELASTICCLAW_LOG_FORMAT") {
	case "text":
		h = slog.NewTextHandler(os.Stderr, nil)
	default:
		h = slog.NewJSONHandler(os.Stderr, nil)
	}
	return slog.New(h)
}

// NewContext returns a copy of ctx carrying l as the request-scoped logger.
func NewContext(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext returns the request-scoped logger stored in ctx (with
// request_id and, after auth, tenant_id already attached). It falls back to
// slog.Default() when ctx carries no logger, so it is always safe to call.
func FromContext(ctx context.Context) *slog.Logger {
	if ctx != nil {
		if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && l != nil {
			return l
		}
	}
	return slog.Default()
}
