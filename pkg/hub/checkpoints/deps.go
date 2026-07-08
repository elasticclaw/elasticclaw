package checkpoints

import (
	"context"
	"database/sql"
	"net/http"
	"sync"
	"time"

	gossh "golang.org/x/crypto/ssh"
	"nhooyr.io/websocket"

	"github.com/elasticclaw/elasticclaw/pkg/hub/artifact"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// Conn is the subset of the hub's per-claw WS connection state the
// checkpoints service needs. It is implemented by the hub's *clawConn via
// thin bridge methods, so the exact same mutex and fields keep guarding
// the checkpoint/volume state as before the extraction. Methods with the
// Locked suffix must be called with the connection lock held (Lock or
// RLock for getters, Lock for setters), mirroring the pre-extraction
// direct field access.
type Conn interface {
	ID() string

	Lock()
	Unlock()
	RLock()
	RUnlock()

	LastUserMessageAtLocked() time.Time
	// StreamingLocked reports whether a streaming turn is in flight
	// (streamingStartedAt set or a streaming message ID assigned).
	StreamingLocked() bool
	CheckpointInProgressLocked() bool
	SetCheckpointInProgressLocked(bool)
	PendingCheckpointIDLocked() string
	PendingCheckpointReasonLocked() string
	SetPendingCheckpointReasonLocked(reason string)
	SetPendingCheckpointLocked(id, reason, by string)

	// WriteWS writes a WS message to the claw connection (wsjson.Write on
	// the underlying conn). Called without the connection lock held, as
	// before the extraction.
	WriteWS(ctx context.Context, msg types.WSMessage) error
}

// ConnEntry pairs a claw ID with its connection for registry snapshots.
type ConnEntry struct {
	ID   string
	Conn Conn
}

// Deps carries the hub-owned state and helpers the checkpoints service
// needs. Everything is injected so the package does not depend on pkg/hub
// (which would create an import cycle). Hooks that read mutable Server
// fields are closures so live config/test overrides stay visible.
type Deps struct {
	// DB is the hub database.
	DB *sql.DB
	// Mu is the hub's config/claw-registry mutex. It must be the same
	// mutex the hub uses so reads/writes keep the exact same
	// synchronization as before the extraction.
	Mu *sync.RWMutex
	// HubCfg reads the hub's live config. Called with Mu held where the
	// pre-extraction code held it.
	HubCfg func() *types.HubConfig

	// CheckpointMu guards the checkpoint waiter map; CheckpointWaiters
	// returns the (hub-owned) map and must be called with CheckpointMu
	// held. The accessor lazily initializes the map, mirroring the nil
	// check the pre-extraction code did under the same lock.
	CheckpointMu      *sync.Mutex
	CheckpointWaiters func() map[string]chan error

	// FileAckMu guards the volume attach/sync waiter maps (shared with
	// the hub's file-ack waiters); the accessors return the hub-owned
	// maps and must be called with FileAckMu held.
	FileAckMu           *sync.Mutex
	VolumeAttachWaiters func() map[string]chan types.VolumeAttachAck
	VolumeSyncWaiters   func() map[string]chan types.VolumeSyncAck

	// Artifacts reads the hub's artifact store (volume layers/manifests).
	Artifacts func() artifact.Store

	// ClawConn returns the connection for a claw, or nil when the claw is
	// not connected. Takes the registry lock itself.
	ClawConn func(clawID string) Conn
	// ConnectedClaws snapshots the claw registry under the registry lock.
	ConnectedClaws func() []ConnEntry
	// CloseClawConn closes and removes the WS connection for a claw, if
	// connected. It takes the hub's claw-registry lock itself.
	CloseClawConn func(clawID string, code websocket.StatusCode, reason string)

	// Claw lifecycle and messaging (owned by pkg/hub until the claws/
	// extraction).
	BroadcastToUsers        func(tenantID string, msg types.WSMessage)
	StopAgentWithReason     func(clawID, reason string, skipVMTerminate bool)
	TerminateVM             func(provider, vmID string)
	SetBootstrapStatus      func(clawID, status string)
	ClawHubURL              func() string
	TenantByClawToken       func(token string) (string, error)
	ProvisionDaytona        func(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, files map[string][]byte, env map[string]string) error
	ProvisionExedev         func(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, files map[string][]byte, env map[string]string) error
	ProvisionLambdaMicroVMs func(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, files map[string][]byte) error
	ProvisionReplicated     func(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, env map[string]string) error

	// Request auth/identity helpers still owned by pkg/hub.
	TenantFromRequest      func(r *http.Request) string
	GithubLoginFromContext func(ctx context.Context) string
	CanModifyClaw          func(cfg *types.AccessConfig, userLogin string, clawTags []string) bool

	// SSH restore helpers (hub identity + known-hosts verification).
	SSHSigner          func() gossh.Signer
	SSHHostKeyCallback func(host string) gossh.HostKeyCallback

	// Misc helpers still owned by pkg/hub.
	InjectFigmaAPIDocs func(files map[string]string, env map[string]string) map[string]string
	// HubVersion reads hub.Version (settable at link time / in tests).
	HubVersion func() string
}

// Convenience accessors so the mechanically-moved code keeps its original
// shape (s.deps.X() everywhere would obscure the diff).

func (s *Service) hubCfg() *types.HubConfig { return s.deps.HubCfg() }

func (s *Service) artifacts() artifact.Store { return s.deps.Artifacts() }

// Forwarders to hub-owned methods, keeping the pre-extraction call sites
// unchanged. They disappear as their targets move into their own
// subpackages (claws/, workflows/) in later phase-2 steps.

func (s *Service) broadcastToUsers(tenantID string, msg types.WSMessage) {
	s.deps.BroadcastToUsers(tenantID, msg)
}

func (s *Service) stopAgentWithReason(clawID, reason string, skipVMTerminate bool) {
	s.deps.StopAgentWithReason(clawID, reason, skipVMTerminate)
}

func (s *Service) terminateVM(provider, vmID string) { s.deps.TerminateVM(provider, vmID) }

func (s *Service) setBootstrapStatus(clawID, status string) {
	s.deps.SetBootstrapStatus(clawID, status)
}

func (s *Service) clawHubURL() string { return s.deps.ClawHubURL() }

func (s *Service) tenantByClawToken(token string) (string, error) {
	return s.deps.TenantByClawToken(token)
}

func (s *Service) provisionDaytona(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, files map[string][]byte, env map[string]string) error {
	return s.deps.ProvisionDaytona(ctx, clawID, req, cfg, files, env)
}

func (s *Service) provisionExedev(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, files map[string][]byte, env map[string]string) error {
	return s.deps.ProvisionExedev(ctx, clawID, req, cfg, files, env)
}

func (s *Service) provisionLambdaMicroVMs(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, files map[string][]byte) error {
	return s.deps.ProvisionLambdaMicroVMs(ctx, clawID, req, cfg, files)
}

func (s *Service) provisionReplicated(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, env map[string]string) error {
	return s.deps.ProvisionReplicated(ctx, clawID, req, cfg, env)
}

func (s *Service) sshHostKeyCallback(host string) gossh.HostKeyCallback {
	return s.deps.SSHHostKeyCallback(host)
}

// now mirrors the hub's monotonic-free clock helper (db.go).
func now() time.Time { return time.Now().UTC() }

// workspaceManagedDirName mirrors the constant in pkg/hub
// (workspace_managed.go): the reserved subdirectory of a workspace that
// holds hub-managed files and must survive workspace re-pushes.
const workspaceManagedDirName = ".elasticclaw-managed"
