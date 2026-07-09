// Package claws implements the hub's claw-core surface: the claw bridge
// WebSocket handler (/claw/ws — registration, heartbeats, streaming chunks,
// message finalization, file/volume acks, HTTP proxy), the streaming buffer
// persistence, file upload/view over the claw channel, the SSH terminal
// proxy, and the per-claw message queue (inject/drain).
//
// The package was extracted mechanically from pkg/hub (the clawConn type,
// handleClawWS, flushStreamingSegment, files.go, handleTerminal,
// injectHubMessage and sendNextQueuedMessage) as part of the phase-2 hub
// reorganization; behavior is unchanged. It shares the hub's state (config
// mutex, DB, claw registry map, file-ack waiter maps) through injected
// hooks so it does not import pkg/hub (which would create an import cycle).
package claws

import (
	"database/sql"
	"sync"

	"github.com/elasticclaw/elasticclaw/pkg/hub/store"
)

// Service hosts the claw-core logic. All mutable state stays on the hub's
// Server behind the injected Deps hooks (the registry map and its mutex are
// the hub's own), so it can be rebuilt per call cheaply and hand-built test
// servers keep working without extra wiring.
type Service struct {
	deps Deps

	// db, cfgMu and fileAckMu mirror the hub Server fields of the same
	// names so the mechanically-moved method bodies keep their original
	// shape. st wraps the same database with the store repositories
	// (phase-2 item 2.4); the claw/message persistence goes through it.
	// reg is the claw-connection registry, which owns its own lock
	// (phase-2 item 2.3).
	db        *sql.DB
	st        *store.Store
	reg       *Registry
	cfgMu     *sync.RWMutex
	fileAckMu *sync.Mutex

	// metrics keeps the moved bodies' s.metrics.wsMessage(...) call sites
	// unchanged; the hook is nil-safe like the hub's *serverMetrics.
	metrics metricsHook
}

// New builds a Service bound to the given hub state.
func New(deps Deps) *Service {
	return &Service{
		deps:      deps,
		db:        deps.DB,
		st:        store.New(deps.DB),
		reg:       deps.Registry,
		cfgMu:     deps.CfgMu,
		fileAckMu: deps.FileAckMu,
		metrics:   metricsHook{ws: deps.WSMessageMetric},
	}
}
