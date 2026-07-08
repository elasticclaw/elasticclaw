// Package analytics implements task-run analytics for the hub: it records
// lifecycle events for factory- and workflow-owned task runs, materializes
// per-run summaries, and serves the /api/task-run-analytics endpoints.
//
// The package was extracted mechanically from pkg/hub (task_run_analytics.go
// and task_run_analytics_api.go) as part of the phase-2 hub reorganization;
// behavior is unchanged.
package analytics

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Service owns task-run analytics persistence and HTTP handlers. It only
// needs the hub's database plus two small hooks so it does not depend on the
// hub package (which would create an import cycle).
type Service struct {
	db *sql.DB

	// tenantFromRequest resolves the authenticated tenant ID from a request.
	// The hub injects its context-based resolver here.
	tenantFromRequest func(r *http.Request) string

	// logf is the printf-style slog bridge injected by the hub so log lines
	// keep the exact same format and component attribution after the move.
	logf func(format string, args ...any)
}

// New creates a Service. tenantFromRequest and logf may be nil; safe
// defaults are installed so hand-built test servers keep working.
func New(db *sql.DB, tenantFromRequest func(r *http.Request) string, logf func(format string, args ...any)) *Service {
	if tenantFromRequest == nil {
		tenantFromRequest = func(*http.Request) string { return "" }
	}
	if logf == nil {
		logf = func(format string, args ...any) {
			slog.Default().Info(fmt.Sprintf(format, args...))
		}
	}
	return &Service{db: db, tenantFromRequest: tenantFromRequest, logf: logf}
}

// now mirrors the hub package's clock helper so timestamps stay identical.
func now() time.Time { return time.Now().UTC() }
