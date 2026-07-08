// Package integrations implements the hub's issue-tracker and webhook
// surface: the Linear, GitHub, Jira and Shortcut webhooks, the generic
// external webhook endpoint, and the shared integration poller.
//
// The package was extracted mechanically from pkg/hub (linear.go,
// github_webhook.go, github_issues.go, jira.go, shortcut.go,
// external_webhook.go and integration_poller.go) as part of the phase-2
// hub reorganization; behavior is unchanged. It shares the hub's state
// (config mutex, DB, webhook dedup map, claw registry) through injected
// hooks so it does not import pkg/hub (which would create an import
// cycle).
package integrations

// Service hosts the integration webhook handlers and pollers. It is
// stateless: all mutable state stays on the hub's Server behind the
// injected Deps hooks, so it can be rebuilt per call cheaply.
type Service struct {
	deps Deps
}

// New builds a Service bound to the given hub state.
func New(deps Deps) *Service {
	return &Service{deps: deps}
}
