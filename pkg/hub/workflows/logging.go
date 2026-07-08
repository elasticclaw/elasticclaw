package workflows

import (
	"log/slog"

	"github.com/elasticclaw/elasticclaw/pkg/hub/logger"
)

// logf is the printf-style slog bridge, identical to the one in pkg/hub, so
// the log lines produced by the extracted workflow code keep the exact same
// format and component attribution.
func logf(format string, args ...any) {
	logger.Msgf(slog.Default(), format, args...)
}
