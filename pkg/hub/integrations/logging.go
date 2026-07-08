package integrations

import (
	"context"
	"log/slog"

	"github.com/elasticclaw/elasticclaw/pkg/hub/logger"
)

// logf is the printf-style slog bridge, identical to the one in pkg/hub, so
// the log lines produced by the extracted integration code keep the exact
// same format and component attribution.
func logf(format string, args ...any) {
	logger.Msgf(slog.Default(), format, args...)
}

// logfCtx is the context-aware variant of logf: it logs through the
// request-scoped logger, so lines carry request_id (and tenant_id after auth).
func logfCtx(ctx context.Context, format string, args ...any) {
	logger.Msgf(logger.FromContext(ctx), format, args...)
}
