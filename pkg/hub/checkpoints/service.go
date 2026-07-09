// Package checkpoints implements the hub's durability surface: claw
// checkpoints (idle/bootstrap/termination snapshots, the internal
// checkpoint API and blob store, restore-to-provider flows), workflow
// volume leases and archives, and the external on-disk storage for
// templates, factories and workspaces.
//
// The package was extracted mechanically from pkg/hub (checkpoints.go,
// volumes.go and external_storage.go) as part of the phase-2 hub
// reorganization; behavior is unchanged. It shares the hub's state
// (config mutex, DB, claw registry, waiter maps, artifact store) through
// injected hooks so it does not import pkg/hub (which would create an
// import cycle). Artifact blobs already live in their own package,
// pkg/hub/artifact, which this package consumes for volume storage.
package checkpoints

import "context"

// Service hosts the checkpoint, volume and external-storage logic. It is
// stateless: all mutable state stays on the hub's Server behind the
// injected Deps hooks, so it can be rebuilt per call cheaply.
type Service struct {
	deps Deps
}

// New builds a Service bound to the given hub state.
func New(deps Deps) *Service {
	return &Service{deps: deps}
}

// baseCtx returns the hub root context (context.Background() when the
// hook is unset, e.g. hand-built test servers). Background work that must
// outlive a request derives from it.
func (s *Service) baseCtx() context.Context {
	if s.deps.BaseCtx != nil {
		return s.deps.BaseCtx()
	}
	return context.Background()
}
