package logger

import (
	"fmt"
	"log/slog"
	"regexp"
)

// componentTagRe matches the manual "[component] " prefix historically used by
// log.Printf call sites (e.g. "[hub] ...", "[cron] ...").
var componentTagRe = regexp.MustCompile(`^\[([A-Za-z0-9_.-]+)\] ?`)

// Msgf is the printf-style bridge used by the mechanical log.Printf → slog
// migration. The message text is preserved verbatim, except that a leading
// "[component] " tag is lifted into a component attribute. It is shared by
// pkg/hub and the packages extracted from it (phase-2 reorganization) so the
// log line format stays identical across the split.
func Msgf(l *slog.Logger, format string, args ...any) {
	if m := componentTagRe.FindStringSubmatch(format); m != nil {
		l.Info(fmt.Sprintf(format[len(m[0]):], args...), "component", m[1])
		return
	}
	l.Info(fmt.Sprintf(format, args...))
}
