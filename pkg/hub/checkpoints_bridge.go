package hub

// This file is the compatibility bridge left behind by the extraction of the
// checkpoint/volume/external-storage surface (checkpoints.go, volumes.go,
// external_storage.go) into pkg/hub/checkpoints (phase-2 hub reorganization).
// It keeps the existing call sites in this package (including tests)
// unchanged while the pkg/hub split proceeds one subpackage at a time: the
// aliases below simply forward to the checkpoints package. New code should
// use pkg/hub/checkpoints directly; callers drop these aliases as they
// migrate to their own subpackages.

import (
	"context"
	"net/http"
	"time"

	gossh "golang.org/x/crypto/ssh"
	"nhooyr.io/websocket"

	"github.com/elasticclaw/elasticclaw/pkg/hub/artifact"
	"github.com/elasticclaw/elasticclaw/pkg/hub/checkpoints"
	daytonaProvider "github.com/elasticclaw/elasticclaw/pkg/provider/daytona"
	exedevProvider "github.com/elasticclaw/elasticclaw/pkg/provider/exedev"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// checkpointsSvc returns the checkpoints service bound to this server's
// state. The service itself is stateless; all mutable state (config, claw
// registry, waiter maps, artifact store) stays on the Server behind the
// injected hooks, so building it per call is cheap and keeps hand-built test
// servers (&Server{...}) working without extra wiring.
func (s *Server) checkpointsSvc() *checkpoints.Service {
	return checkpoints.New(checkpoints.Deps{
		DB:      s.db,
		BaseCtx: s.base,
		CfgMu:   &s.cfgMu,
		HubCfg:  func() *types.HubConfig { return s.hubCfg },

		CheckpointMu: &s.checkpointMu,
		CheckpointWaiters: func() map[string]chan error {
			if s.checkpointWaiters == nil {
				s.checkpointWaiters = make(map[string]chan error)
			}
			return s.checkpointWaiters
		},

		FileAckMu: &s.fileAckMu,
		VolumeAttachWaiters: func() map[string]chan types.VolumeAttachAck {
			if s.volumeAttachWaiters == nil {
				s.volumeAttachWaiters = make(map[string]chan types.VolumeAttachAck)
			}
			return s.volumeAttachWaiters
		},
		VolumeSyncWaiters: func() map[string]chan types.VolumeSyncAck {
			if s.volumeSyncWaiters == nil {
				s.volumeSyncWaiters = make(map[string]chan types.VolumeSyncAck)
			}
			return s.volumeSyncWaiters
		},

		Artifacts: func() artifact.Store { return s.artifacts },

		ClawConn: func(clawID string) checkpoints.Conn {
			cc := s.clawReg.Lookup(clawID)
			if cc == nil {
				return nil
			}
			return cc
		},
		ConnectedClaws: func() []checkpoints.ConnEntry {
			var items []checkpoints.ConnEntry
			s.clawReg.DoRead(func(conns map[string]*clawConn) {
				items = make([]checkpoints.ConnEntry, 0, len(conns))
				for id, cc := range conns {
					items = append(items, checkpoints.ConnEntry{ID: id, Conn: cc})
				}
			})
			return items
		},
		CloseClawConn: func(clawID string, code websocket.StatusCode, reason string) {
			s.clawReg.Do(func(conns map[string]*clawConn) {
				if cc, ok := conns[clawID]; ok {
					cc.WS.Close(code, reason)
					delete(conns, clawID)
				}
			})
		},

		BroadcastToUsers:        s.broadcastToUsers,
		StopAgentWithReason:     s.stopAgentWithReason,
		TerminateVM:             s.terminateVM,
		SetBootstrapStatus:      s.setBootstrapStatus,
		ClawHubURL:              s.clawHubURL,
		TenantByClawToken:       s.tenantByClawToken,
		ProvisionDaytona:        s.provisionDaytona,
		ProvisionExedev:         s.provisionExedev,
		ProvisionLambdaMicroVMs: s.provisionLambdaMicroVMs,
		ProvisionReplicated:     s.provisionReplicated,

		TenantFromRequest:      tenantFromCtx,
		GithubLoginFromContext: githubLoginFromContext,
		CanModifyClaw:          canModifyClaw,

		SSHSigner:          func() gossh.Signer { return s.identity.PrivateKey },
		SSHHostKeyCallback: s.sshHostKeyCallback,

		InjectFigmaAPIDocs: injectFigmaAPIDocs,
		HubVersion:         func() string { return Version },
	})
}

// Types and constants moved to pkg/hub/checkpoints.
type workflowVolumeRuntime = checkpoints.WorkflowVolumeRuntime

const checkpointRequestTimeout = checkpoints.RequestTimeout

// Package-level helpers moved to pkg/hub/checkpoints.
var (
	shortID                            = checkpoints.ShortID
	normalizeWorkflowVolumes           = checkpoints.NormalizeWorkflowVolumes
	hubConfigDir                       = checkpoints.HubConfigDir
	templatesDir                       = checkpoints.TemplatesDir
	workspacesDir                      = checkpoints.WorkspacesDir
	validateName                       = checkpoints.ValidateName
	loadExternalTemplate               = checkpoints.LoadExternalTemplate
	saveExternalTemplate               = checkpoints.SaveExternalTemplate
	deleteExternalTemplate             = checkpoints.DeleteExternalTemplate
	listExternalTemplates              = checkpoints.ListExternalTemplates
	loadExternalFactories              = checkpoints.LoadExternalFactories
	loadExternalFactory                = checkpoints.LoadExternalFactory
	saveExternalFactory                = checkpoints.SaveExternalFactory
	deleteExternalFactory              = checkpoints.DeleteExternalFactory
	loadExternalWorkspaces             = checkpoints.LoadExternalWorkspaces
	loadExternalWorkspace              = checkpoints.LoadExternalWorkspace
	loadExternalWorkflowsByIntegration = checkpoints.LoadExternalWorkflowsByIntegration
	filterWorkflowWorkspacesByName     = checkpoints.FilterWorkflowWorkspacesByName
	saveExternalWorkspace              = checkpoints.SaveExternalWorkspace
	saveExternalWorkflows              = checkpoints.SaveExternalWorkflows
	deleteExternalWorkspace            = checkpoints.DeleteExternalWorkspace
)

// Exported package API kept on pkg/hub for cmd/ (external storage setup and
// v2 migration); moved to pkg/hub/checkpoints.
var (
	EnsureExternalDirs     = checkpoints.EnsureExternalDirs
	HasMigratedV2          = checkpoints.HasMigratedV2
	MarkMigratedV2         = checkpoints.MarkMigratedV2
	MigrateLegacyFactories = checkpoints.MigrateLegacyFactories
)

// Server methods moved to checkpoints.Service.

func (s *Server) checkpointScheduler() {
	s.checkpointsSvc().CheckpointScheduler()
}

func (s *Server) hasRecentCheckpoint(clawID string, minAge time.Duration) bool {
	return s.checkpointsSvc().HasRecentCheckpoint(clawID, minAge)
}

func (s *Server) requestBootstrapCheckpoint(clawID string) {
	s.checkpointsSvc().RequestBootstrapCheckpoint(clawID)
}

func (s *Server) handleClawCheckpoints(w http.ResponseWriter, r *http.Request, clawID string) {
	s.checkpointsSvc().HandleClawCheckpoints(w, r, clawID)
}

func (s *Server) requestCheckpoint(ctx context.Context, clawID, reason, createdBy string, wait bool, timeout time.Duration) (string, error) {
	return s.checkpointsSvc().RequestCheckpoint(ctx, clawID, reason, createdBy, wait, timeout)
}

func (s *Server) dispatchCheckpoint(ctx context.Context, cc *clawConn, clawID, checkpointID, reason string, wait bool, timeout time.Duration) (string, error) {
	return s.checkpointsSvc().DispatchCheckpoint(ctx, cc, clawID, checkpointID, reason, wait, timeout)
}

func (s *Server) drainPendingCheckpoint(clawID string) {
	s.checkpointsSvc().DrainPendingCheckpoint(clawID)
}

func (s *Server) checkpointBeforeTermination(clawID, reason string) {
	s.checkpointsSvc().CheckpointBeforeTermination(clawID, reason)
}

func (s *Server) insertCheckpoint(id, tenantID, clawID, reason, createdBy, provider, providerID string) error {
	return s.checkpointsSvc().InsertCheckpoint(id, tenantID, clawID, reason, createdBy, provider, providerID)
}

func (s *Server) handleCheckpointInternal(w http.ResponseWriter, r *http.Request) {
	s.checkpointsSvc().HandleCheckpointInternal(w, r)
}

func (s *Server) handleCheckpointBlobUpload(w http.ResponseWriter, r *http.Request) {
	s.checkpointsSvc().HandleCheckpointBlobUpload(w, r)
}

func (s *Server) restoreCheckpointToDaytona(ctx context.Context, clawID, instanceID string, p *daytonaProvider.Provider) error {
	return s.checkpointsSvc().RestoreCheckpointToDaytona(ctx, clawID, instanceID, p)
}

func (s *Server) restoreCheckpointToExedev(ctx context.Context, clawID, vmName string, p *exedevProvider.Provider) error {
	return s.checkpointsSvc().RestoreCheckpointToExedev(ctx, clawID, vmName, p)
}

func (s *Server) restoreCheckpointToSSH(clawID, user, host string) error {
	return s.checkpointsSvc().RestoreCheckpointToSSH(clawID, user, host)
}

func (s *Server) acquireWorkflowVolumeLeases(ctx context.Context, clawID string, volumes []workflowVolumeRuntime) ([]workflowVolumeRuntime, error) {
	return s.checkpointsSvc().AcquireWorkflowVolumeLeases(ctx, clawID, volumes)
}

func (s *Server) releaseWorkflowVolumeLeases(clawID string) {
	s.checkpointsSvc().ReleaseWorkflowVolumeLeases(clawID)
}

func (s *Server) heartbeatWorkflowVolumeLeases(clawID string) {
	s.checkpointsSvc().HeartbeatWorkflowVolumeLeases(clawID)
}

func (s *Server) storeWorkflowVolumes(clawID string, volumes []workflowVolumeRuntime) error {
	return s.checkpointsSvc().StoreWorkflowVolumes(clawID, volumes)
}

func (s *Server) attachWorkflowVolumes(ctx context.Context, cc *clawConn, clawID string) error {
	return s.checkpointsSvc().AttachWorkflowVolumes(ctx, cc, clawID)
}

func (s *Server) syncWorkflowVolumes(clawID string) {
	s.checkpointsSvc().SyncWorkflowVolumes(clawID)
}

func (s *Server) handleVolumeArchive(w http.ResponseWriter, r *http.Request) {
	s.checkpointsSvc().HandleVolumeArchive(w, r)
}

func (s *Server) resolveFactories() []*types.FactoryConfig {
	return s.checkpointsSvc().ResolveFactories()
}

// MigrateLegacyTemplates migrates templates from the SQLite hub_templates
// table to the external templates/ directory, then drops the legacy table.
// Moved to pkg/hub/checkpoints; kept exported here for cmd/hub.go.
func (s *Server) MigrateLegacyTemplates() error {
	return s.checkpointsSvc().MigrateLegacyTemplates()
}
