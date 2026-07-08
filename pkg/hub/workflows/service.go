// Package workflows implements the hub's workflow automation surface:
// the pipeline runner (stage transitions, run/judge/gate actions, issue
// moves), factory CRUD and issue-trigger evaluation, the cron scheduler
// with its API, and the PR watcher that follows agent pull requests.
//
// The package was extracted mechanically from pkg/hub (pipeline_runner.go,
// factory_api.go, factory_creator.go, factory_trigger.go,
// factory_triggers.go, cron_api.go, cron_scheduler.go and pr_watcher.go)
// as part of the phase-2 hub reorganization; behavior is unchanged. It
// shares the hub's state (config mutex, DB, claw registry, cron scheduler
// slot) through injected hooks so it does not import pkg/hub (which would
// create an import cycle).
package workflows

// Service hosts the pipeline, factory, cron and PR-watcher logic. It is
// stateless: all mutable state stays on the hub's Server behind the
// injected Deps hooks, so it can be rebuilt per call cheaply.
type Service struct {
	deps Deps
}

// New builds a Service bound to the given hub state.
func New(deps Deps) *Service {
	return &Service{deps: deps}
}
