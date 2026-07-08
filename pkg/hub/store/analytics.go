package store

import "context"

// AnalyticsRepo is the repository for the analytics aggregate. It only
// carries the retention pruning today; the task-run analytics queries
// are the largest remaining raw-SQL surface and migrate here in a
// follow-up (see the phase-2 item 2.4 residual).
type AnalyticsRepo struct {
	st *Store
}

// PruneFactoryAnalytics deletes factory_analytics rows older than 1
// year. Called periodically from the hub's background pruner.
func (r *AnalyticsRepo) PruneFactoryAnalytics(ctx context.Context) error {
	_, err := r.st.exec(ctx, `DELETE FROM factory_analytics WHERE created_at < datetime('now', '-1 year')`)
	return err
}
