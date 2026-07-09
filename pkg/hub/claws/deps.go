package claws

import (
	"context"
	"database/sql"
	"net/http"
	"sync"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// Deps carries the hub-owned state and helpers the claws service needs.
// Everything is injected so the package does not depend on pkg/hub (which
// would create an import cycle). Hooks that read mutable Server fields are
// closures so live config/test overrides stay visible.
type Deps struct {
	// DB is the hub database.
	DB *sql.DB
	// BaseCtx returns the hub's root context. Background work that must
	// outlive an HTTP request derives from it — never from a bare
	// context.Background() — so cancellation propagates once graceful
	// shutdown wires a cancelable root. May be nil in hand-built test
	// servers; use the service's baseCtx() accessor.
	BaseCtx func() context.Context
	// Registry is the hub-owned claw-connection registry. It carries its
	// own RWMutex (per-subsystem locking, phase-2 item 2.3).
	Registry *Registry
	// Pool bounds the goroutines spawned per WS message (nil is not
	// allowed; the hub owns one WorkerPool for its lifetime).
	Pool *WorkerPool
	// CfgMu guards the hub's live config pointer (the settings-subsystem
	// lock). It must be the same mutex the hub's config writers use.
	CfgMu *sync.RWMutex
	// HubCfg reads the hub's live config. Called with CfgMu held where
	// concurrent config hot-reloads are possible.
	HubCfg func() *types.HubConfig
	// Mux reads the hub's HTTP mux (set in Run) for the bridge HTTP proxy.
	Mux func() http.Handler
	// WSMessageMetric counts one WS message (the hub's
	// serverMetrics.wsMessage, which is nil-receiver-safe).
	WSMessageMetric func(direction, peer string)

	// HTTP response writers injected by the composition root
	// (httpserver.JSONOK / httpserver.WriteDomainErr) so claws never
	// imports httpserver — the phase-2 dependency rule says httpserver
	// knows the services, never the reverse.
	JSONOK         func(w http.ResponseWriter, v interface{})
	WriteDomainErr func(w http.ResponseWriter, err error)

	// Auth helpers still owned by pkg/hub.
	TenantByClawToken func(token string) (string, error)
	TenantByToken     func(token string) (string, error)
	TenantFromRequest func(r *http.Request) string
	// RedeemAuthTicket resolves a single-use ?ticket= (issued by
	// POST /api/auth/ticket) to a tenant. Browser WS upgrades cannot set
	// an Authorization header, so the terminal handler authenticates with
	// a ticket instead. May be nil in hand-built test servers.
	RedeemAuthTicket func(ticket string) (tenantID string, ok bool)

	// FileAckMu guards the file/volume ack waiter maps below; the
	// accessors return the hub-owned maps and must be called with
	// FileAckMu held (they are shared with pkg/hub/checkpoints, whose
	// bridge lazily initializes the volume maps under the same lock).
	FileAckMu           *sync.Mutex
	FileAckWaiters      func() map[string]chan types.FileAck
	FileReadWaiters     func() map[string]chan types.FileReadResp
	VolumeAttachWaiters func() map[string]chan types.VolumeAttachAck
	VolumeSyncWaiters   func() map[string]chan types.VolumeSyncAck

	// User-session fanout (owned by pkg/hub until users get their own
	// subpackage).
	BroadcastToUsers func(tenantID string, msg types.WSMessage)

	// Wake/workflow glue still owned by pkg/hub.
	StartWorkflowAfterVolumes func(ctx context.Context, cc *Conn, clawID string)

	// Checkpoint/volume hooks (pkg/hub bridge over pkg/hub/checkpoints).
	HasRecentCheckpoint        func(clawID string, minAge time.Duration) bool
	RequestBootstrapCheckpoint func(clawID string)
	RequestCheckpoint          func(ctx context.Context, clawID, reason, createdBy string, wait bool, timeout time.Duration) (string, error)
	// CheckpointRequestTimeout mirrors the hub's checkpointRequestTimeout
	// constant (checkpoints.RequestTimeout).
	CheckpointRequestTimeout      time.Duration
	DrainPendingCheckpoint        func(clawID string)
	HeartbeatWorkflowVolumeLeases func(clawID string)

	// Message injection and initial-plan handling still owned by pkg/hub
	// (workflows bridge and server.go).
	InjectHubMessageByID      func(clawID, content string)
	HandleInitialPlanActivity func(clawID, tenantID string, activity map[string]interface{})
	HandleInitialPlanResponse func(clawID, tenantID, content string)

	// EvaluatePipelineMessageTriggers runs the pipeline [DONE]/message
	// trigger block for a finalized claw message and reports whether a
	// pipeline explicitly handled the [DONE] signal. The body (previously
	// inline in handleClawWS) stays in pkg/hub because it builds
	// workflows-package pipeline contexts, which claws must not import.
	// The returned job, if non-nil, is the async pipeline work (stage
	// transition or message-trigger check); the caller runs it through the
	// bounded WS worker pool so a chatty bridge cannot spawn unbounded
	// goroutines around the semaphore.
	EvaluatePipelineMessageTriggers func(clawID, content string) (handledDone bool, job func())

	// Done/terminate signal handling (pkg/hub bridge over integrations/
	// workflows).
	HandleClawDoneSignal      func(clawID, content string)
	HandleClawTerminateSignal func(clawID, content string)
	ScanMessageForPRs         func(clawID, content string)

	// SSH terminal helpers (hub identity + known-hosts verification).
	SSHSigner          func() gossh.Signer
	SSHHostKeyCallback func(host string) gossh.HostKeyCallback
}

// metricsHook keeps the moved bodies' s.metrics.wsMessage(...) call sites
// unchanged. Like the hub's *serverMetrics, it is safe to call when no
// metrics backend is wired (hand-built test servers).
type metricsHook struct {
	ws func(direction, peer string)
}

func (m metricsHook) wsMessage(direction, peer string) {
	if m.ws != nil {
		m.ws(direction, peer)
	}
}

// Convenience accessors so the mechanically-moved code keeps its original
// shape.

func (s *Service) hubCfg() *types.HubConfig { return s.deps.HubCfg() }

// baseCtx returns the hub root context (context.Background() when the
// hook is unset, e.g. hand-built test servers).
func (s *Service) baseCtx() context.Context {
	if s.deps.BaseCtx != nil {
		return s.deps.BaseCtx()
	}
	return context.Background()
}

func (s *Service) mux() http.Handler { return s.deps.Mux() }

func (s *Service) fileAckWaiters() map[string]chan types.FileAck { return s.deps.FileAckWaiters() }

func (s *Service) fileReadWaiters() map[string]chan types.FileReadResp {
	return s.deps.FileReadWaiters()
}

func (s *Service) volumeAttachWaiters() map[string]chan types.VolumeAttachAck {
	return s.deps.VolumeAttachWaiters()
}

func (s *Service) volumeSyncWaiters() map[string]chan types.VolumeSyncAck {
	return s.deps.VolumeSyncWaiters()
}

// Forwarders to hub-owned methods, keeping the pre-extraction call sites
// unchanged. They disappear as their targets move into their own
// subpackages in later phase-2 steps.

func (s *Service) tenantByClawToken(token string) (string, error) {
	return s.deps.TenantByClawToken(token)
}

func (s *Service) tenantByToken(token string) (string, error) {
	return s.deps.TenantByToken(token)
}

func (s *Service) redeemAuthTicket(ticket string) (string, bool) {
	if s.deps.RedeemAuthTicket == nil {
		return "", false
	}
	return s.deps.RedeemAuthTicket(ticket)
}

func (s *Service) broadcastToUsers(tenantID string, msg types.WSMessage) {
	s.deps.BroadcastToUsers(tenantID, msg)
}

func (s *Service) startWorkflowAfterVolumes(ctx context.Context, cc *Conn, clawID string) {
	s.deps.StartWorkflowAfterVolumes(ctx, cc, clawID)
}

func (s *Service) hasRecentCheckpoint(clawID string, minAge time.Duration) bool {
	return s.deps.HasRecentCheckpoint(clawID, minAge)
}

func (s *Service) requestBootstrapCheckpoint(clawID string) {
	s.deps.RequestBootstrapCheckpoint(clawID)
}

func (s *Service) requestCheckpoint(ctx context.Context, clawID, reason, createdBy string, wait bool, timeout time.Duration) (string, error) {
	return s.deps.RequestCheckpoint(ctx, clawID, reason, createdBy, wait, timeout)
}

func (s *Service) drainPendingCheckpoint(clawID string) {
	s.deps.DrainPendingCheckpoint(clawID)
}

func (s *Service) heartbeatWorkflowVolumeLeases(clawID string) {
	s.deps.HeartbeatWorkflowVolumeLeases(clawID)
}

func (s *Service) injectHubMessageByID(clawID, content string) {
	s.deps.InjectHubMessageByID(clawID, content)
}

func (s *Service) handleInitialPlanActivity(clawID, tenantID string, activity map[string]interface{}) {
	s.deps.HandleInitialPlanActivity(clawID, tenantID, activity)
}

func (s *Service) handleInitialPlanResponse(clawID, tenantID, content string) {
	s.deps.HandleInitialPlanResponse(clawID, tenantID, content)
}

func (s *Service) handleClawDoneSignal(clawID, content string) {
	s.deps.HandleClawDoneSignal(clawID, content)
}

func (s *Service) handleClawTerminateSignal(clawID, content string) {
	s.deps.HandleClawTerminateSignal(clawID, content)
}

func (s *Service) scanMessageForPRs(clawID, content string) {
	s.deps.ScanMessageForPRs(clawID, content)
}

func (s *Service) tenantFromCtx(r *http.Request) string { return s.deps.TenantFromRequest(r) }

func (s *Service) sshHostKeyCallback(host string) gossh.HostKeyCallback {
	return s.deps.SSHHostKeyCallback(host)
}

// shortID mirrors checkpoints.ShortID (kept local so claws does not import
// a sibling service package).
func shortID(id string) string {
	if len(id) < 8 {
		return id
	}
	return id[:8]
}

// now mirrors the hub's monotonic-free clock helper (db.go).
func now() time.Time { return time.Now().UTC() }
