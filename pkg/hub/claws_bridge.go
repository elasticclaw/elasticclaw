package hub

// This file is the compatibility bridge left behind by the extraction of the
// claw-core surface (the clawConn type, handleClawWS, flushStreamingSegment,
// files.go, handleTerminal, injectHubMessage, sendNextQueuedMessage) into
// pkg/hub/claws (phase-2 hub reorganization, item 2.2 step 7). It keeps the
// existing call sites in this package (including tests) unchanged while the
// pkg/hub split proceeds one subpackage at a time: the aliases below simply
// forward to the claws package. New code should use pkg/hub/claws directly;
// callers drop these aliases as they migrate to their own subpackages.

import (
	"context"
	"net/http"
	"strings"

	gossh "golang.org/x/crypto/ssh"

	"github.com/elasticclaw/elasticclaw/pkg/hub/claws"
	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// clawConn moved to pkg/hub/claws. The alias keeps the registry map, the
// user-WS fanout, the status watchdog and the hand-built test servers in
// this package on the exact same struct (same mutex, same fields — now
// exported) as before the extraction.
type clawConn = claws.Conn

// clawsSvc returns the claws service bound to this server's state. The
// service itself is stateless; all mutable state (config, claw registry,
// waiter maps) stays on the Server behind the injected hooks, so building
// it per call is cheap and keeps hand-built test servers (&Server{...})
// working without extra wiring.
func (s *Server) clawsSvc() *claws.Service {
	return claws.New(claws.Deps{
		DB:              s.db,
		Registry:        &s.clawReg,
		Pool:            &s.wsPool,
		CfgMu:           &s.cfgMu,
		HubCfg:          func() *types.HubConfig { return s.hubCfg },
		Mux:             func() http.Handler { return s.mux },
		WSMessageMetric: s.metrics.wsMessage,

		TenantByClawToken: s.tenantByClawToken,
		TenantByToken:     s.tenantByToken,
		TenantFromRequest: tenantFromCtx,

		FileAckMu: &s.fileAckMu,
		FileAckWaiters: func() map[string]chan types.FileAck {
			return s.fileAckWaiters
		},
		FileReadWaiters: func() map[string]chan types.FileReadResp {
			return s.fileReadWaiters
		},
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

		BroadcastToUsers: s.broadcastToUsers,

		StartWorkflowAfterVolumes: s.startWorkflowAfterVolumes,

		HasRecentCheckpoint:           s.hasRecentCheckpoint,
		RequestBootstrapCheckpoint:    s.requestBootstrapCheckpoint,
		RequestCheckpoint:             s.requestCheckpoint,
		CheckpointRequestTimeout:      checkpointRequestTimeout,
		DrainPendingCheckpoint:        s.drainPendingCheckpoint,
		HeartbeatWorkflowVolumeLeases: s.heartbeatWorkflowVolumeLeases,

		InjectHubMessageByID:      s.injectHubMessageByID,
		HandleInitialPlanActivity: s.handleInitialPlanActivity,
		HandleInitialPlanResponse: s.handleInitialPlanResponse,

		EvaluatePipelineMessageTriggers: s.evaluatePipelineMessageTriggers,

		HandleClawDoneSignal:      s.handleClawDoneSignal,
		HandleClawTerminateSignal: s.handleClawTerminateSignal,
		ScanMessageForPRs:         s.scanMessageForPRs,

		SSHSigner:          func() gossh.Signer { return s.identity.PrivateKey },
		SSHHostKeyCallback: s.sshHostKeyCallback,
	})
}

// evaluatePipelineMessageTriggers runs the pipeline trigger block for a
// finalized claw message and reports whether a pipeline explicitly handled
// the [DONE] signal. The body is the exact block that lived inline in
// handleClawWS before the claws extraction; it stays in pkg/hub because it
// builds workflows-package pipeline contexts, which claws must not import.
func (s *Server) evaluatePipelineMessageTriggers(clawID, content string) bool {
	pipelineHandledDone := false
	var pipelineDoneCtx pipelineContext
	var pipelineDoneStage *pipeline.Stage
	if strings.Contains(content, "[DONE]") {
		pipelineDoneCtx, pipelineDoneStage, pipelineHandledDone = s.pipelineStageForMessageContains(clawID, content)
	}
	if pipelineHandledDone {
		prURLs := extractDonePRURLs(content)
		s.trackDoneSignal(pipelineDoneCtx.Name(), pipelineDoneCtx.IssueID, clawID, len(prURLs))
		go s.transitionPipelineStageWithContext(clawID, *pipelineDoneStage, pipelineDoneCtx)
	} else if !strings.Contains(content, "[DONE]") {
		go s.checkPipelineMessageTriggers(clawID, content)
	}
	return pipelineHandledDone
}

// Package-level helpers moved to pkg/hub/claws.
var (
	detectToolLoop                = claws.DetectToolLoop
	allowWakeBeforeBootstrap      = claws.AllowWakeBeforeBootstrap
	normalizeAgentActivityPayload = claws.NormalizeAgentActivityPayload
	isBusyAgentActivity           = claws.IsBusyAgentActivity
)

// Server methods moved to claws.Service.

func (s *Server) handleClawWS(w http.ResponseWriter, r *http.Request) {
	s.clawsSvc().HandleClawWS(w, r)
}

func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	s.clawsSvc().HandleTerminal(w, r)
}

func (s *Server) handleFileUpload(w http.ResponseWriter, r *http.Request) {
	s.clawsSvc().HandleFileUpload(w, r)
}

func (s *Server) handleFileView(w http.ResponseWriter, r *http.Request) {
	s.clawsSvc().HandleFileView(w, r)
}

func (s *Server) flushStreamingSegment(clawID, tenantID string, cc *clawConn) error {
	return s.clawsSvc().FlushStreamingSegment(clawID, tenantID, cc)
}

func (s *Server) injectHubMessage(ctx context.Context, cc *clawConn, text string) {
	s.clawsSvc().InjectHubMessage(ctx, cc, text)
}

func (s *Server) sendNextQueuedMessage(cc *clawConn) {
	s.clawsSvc().SendNextQueuedMessage(cc)
}
