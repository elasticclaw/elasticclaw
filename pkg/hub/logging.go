package hub

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/elasticclaw/elasticclaw/pkg/hub/logger"
)

// componentTagRe matches the manual "[component] " prefix historically used by
// log.Printf call sites (e.g. "[hub] ...", "[cron] ...").
var componentTagRe = regexp.MustCompile(`^\[([A-Za-z0-9_.-]+)\] ?`)

// logf is the printf-style bridge used by the mechanical log.Printf → slog
// migration for call sites with no context available (boot code, background
// goroutines). The message text is preserved verbatim, except that a leading
// "[component] " tag is lifted into a component attribute.
func logf(format string, args ...any) {
	logMsg(slog.Default(), format, args...)
}

// logfCtx is the context-aware variant of logf: it logs through the
// request-scoped logger, so lines carry request_id (and tenant_id after auth).
func logfCtx(ctx context.Context, format string, args ...any) {
	logMsg(logger.FromContext(ctx), format, args...)
}

func logMsg(l *slog.Logger, format string, args ...any) {
	if m := componentTagRe.FindStringSubmatch(format); m != nil {
		l.Info(fmt.Sprintf(format[len(m[0]):], args...), "component", m[1])
		return
	}
	l.Info(fmt.Sprintf(format, args...))
}
