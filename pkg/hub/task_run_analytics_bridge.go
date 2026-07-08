package hub

// This file is the compatibility bridge left behind by the extraction of
// task-run analytics into pkg/hub/analytics (phase-2 hub reorganization).
// It keeps the ~200 existing call sites in this package (including tests)
// unchanged while the pkg/hub split proceeds one subpackage at a time: the
// aliases below simply forward to the analytics package. New code should use
// pkg/hub/analytics directly; callers drop these aliases as they migrate to
// their own subpackages.

import (
	"net/http"

	"github.com/elasticclaw/elasticclaw/pkg/hub/analytics"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// taskRuns returns the task-run analytics service bound to this server's
// database. The service is stateless besides its dependencies, so building
// it per call is cheap and keeps hand-built test servers (&Server{...})
// working without extra wiring.
func (s *Server) taskRuns() *analytics.Service {
	return analytics.New(s.db, tenantFromCtx, logf)
}

// Types moved to pkg/hub/analytics.
type (
	TaskRunStart = analytics.TaskRunStart
	TaskRunEvent = analytics.TaskRunEvent
	TaskRunPR    = analytics.TaskRunPR

	taskRunAnalyticsSummaryResponse       = analytics.TaskRunAnalyticsSummaryResponse
	taskRunAnalyticsRunsResponse          = analytics.TaskRunAnalyticsRunsResponse
	taskRunAnalyticsRunDetailResponse     = analytics.TaskRunAnalyticsRunDetailResponse
	taskRunAnalyticsAttemptsResponse      = analytics.TaskRunAnalyticsAttemptsResponse
	taskRunAnalyticsEventsResponse        = analytics.TaskRunAnalyticsEventsResponse
	taskRunAnalyticsPRsResponse           = analytics.TaskRunAnalyticsPRsResponse
	taskRunAnalyticsFilterOptionsResponse = analytics.TaskRunAnalyticsFilterOptionsResponse
)

// Constants moved to pkg/hub/analytics.
const (
	taskRunActorAgent                    = analytics.TaskRunActorAgent
	taskRunActorHuman                    = analytics.TaskRunActorHuman
	taskRunActorSystem                   = analytics.TaskRunActorSystem
	taskRunEventAgentStopped             = analytics.TaskRunEventAgentStopped
	taskRunEventClawCreated              = analytics.TaskRunEventClawCreated
	taskRunEventDoneWithoutPR            = analytics.TaskRunEventDoneWithoutPR
	taskRunEventHumanDashboardMessage    = analytics.TaskRunEventHumanDashboardMessage
	taskRunEventHumanPRComment           = analytics.TaskRunEventHumanPRComment
	taskRunEventHumanRequestedChanges    = analytics.TaskRunEventHumanRequestedChanges
	taskRunEventHumanReviewComment       = analytics.TaskRunEventHumanReviewComment
	taskRunEventManualStopBeforeDelivery = analytics.TaskRunEventManualStopBeforeDelivery
	taskRunEventPRAssociated             = analytics.TaskRunEventPRAssociated
	taskRunEventPRClosedUnmerged         = analytics.TaskRunEventPRClosedUnmerged
	taskRunEventPRMerged                 = analytics.TaskRunEventPRMerged
	taskRunEventPROpened                 = analytics.TaskRunEventPROpened
	taskRunEventProvisionStarted         = analytics.TaskRunEventProvisionStarted
	taskRunEventRunQueued                = analytics.TaskRunEventRunQueued
	taskRunEventTaskCompleted            = analytics.TaskRunEventTaskCompleted
	taskRunEventTaskStart                = analytics.TaskRunEventTaskStart
	taskRunFailureAgentStopped           = analytics.TaskRunFailureAgentStopped
	taskRunFailureDoneWithoutPR          = analytics.TaskRunFailureDoneWithoutPR
	taskRunFailureManualStopDelivery     = analytics.TaskRunFailureManualStopDelivery
	taskRunFailurePRClosedUnmerged       = analytics.TaskRunFailurePRClosedUnmerged
	taskRunFailureTimeout                = analytics.TaskRunFailureTimeout
	taskRunInteractionAllowedStart       = analytics.TaskRunInteractionAllowedStart
	taskRunInteractionNeutral            = analytics.TaskRunInteractionNeutral
	taskRunInteractionTerminal           = analytics.TaskRunInteractionTerminal
	taskRunInteractionWarning            = analytics.TaskRunInteractionWarning
	taskRunKindCodeTask                  = analytics.TaskRunKindCodeTask
	taskRunKindPRTask                    = analytics.TaskRunKindPRTask
	taskRunOwnerFactory                  = analytics.TaskRunOwnerFactory
	taskRunOwnerWorkflow                 = analytics.TaskRunOwnerWorkflow
	taskRunPRStateClosed                 = analytics.TaskRunPRStateClosed
	taskRunPRStateOpen                   = analytics.TaskRunPRStateOpen
	taskRunPRStateUnknown                = analytics.TaskRunPRStateUnknown
	taskRunPhaseAgentRunning             = analytics.TaskRunPhaseAgentRunning
	taskRunPhaseProvisioning             = analytics.TaskRunPhaseProvisioning
	taskRunPhaseQueued                   = analytics.TaskRunPhaseQueued
	taskRunPhaseTerminal                 = analytics.TaskRunPhaseTerminal
	taskRunPhaseWaitingForMerge          = analytics.TaskRunPhaseWaitingForMerge
	taskRunSourceDashboard               = analytics.TaskRunSourceDashboard
	taskRunSourceFactory                 = analytics.TaskRunSourceFactory
	taskRunSourceGitHub                  = analytics.TaskRunSourceGitHub
	taskRunSourceHub                     = analytics.TaskRunSourceHub
	taskRunSourceWorkflow                = analytics.TaskRunSourceWorkflow
	taskRunStatusCleanSuccess            = analytics.TaskRunStatusCleanSuccess
	taskRunStatusFailed                  = analytics.TaskRunStatusFailed
	taskRunStatusRunning                 = analytics.TaskRunStatusRunning
	taskRunStatusWarningSuccess          = analytics.TaskRunStatusWarningSuccess
	taskRunWarningHumanDashboardMessage  = analytics.TaskRunWarningHumanDashboardMessage
	taskRunWarningHumanPRComment         = analytics.TaskRunWarningHumanPRComment
	taskRunWarningHumanReviewComment     = analytics.TaskRunWarningHumanReviewComment
)

// Server methods moved to analytics.Service.

func (s *Server) ensureTaskRunForClaw(clawID string, opts TaskRunStart) (string, string, error) {
	return s.taskRuns().EnsureTaskRunForClaw(clawID, opts)
}

func (s *Server) recordTaskRunEvent(input TaskRunEvent) error {
	return s.taskRuns().RecordTaskRunEvent(input)
}

func (s *Server) associateTaskRunPR(input TaskRunPR) error {
	return s.taskRuns().AssociateTaskRunPR(input)
}

func (s *Server) taskRunContextForClaw(clawID string) (tenantID, runID, attemptID string, ok bool, err error) {
	return s.taskRuns().TaskRunContextForClaw(clawID)
}

func (s *Server) recordTaskRunEventForClaw(clawID string, input TaskRunEvent) error {
	return s.taskRuns().RecordTaskRunEventForClaw(clawID, input)
}

func (s *Server) recordTaskRunHumanEventForClaw(clawID, eventType, eventKey, actorLogin, targetURL string, detail map[string]any) {
	s.taskRuns().RecordTaskRunHumanEventForClaw(clawID, eventType, eventKey, actorLogin, targetURL, detail)
}

func (s *Server) recordTaskRunDashboardMessage(clawID, actorLogin, messageID string) {
	s.taskRuns().RecordTaskRunDashboardMessage(clawID, actorLogin, messageID)
}

func (s *Server) recordTaskRunManualStopBeforeDelivery(clawID, actorLogin string) {
	s.taskRuns().RecordTaskRunManualStopBeforeDelivery(clawID, actorLogin)
}

func (s *Server) taskRunRequiresPRForClaw(clawID string) bool {
	return s.taskRuns().TaskRunRequiresPRForClaw(clawID)
}

func (s *Server) handleTaskRunAnalyticsSummary(w http.ResponseWriter, r *http.Request) {
	s.taskRuns().HandleTaskRunAnalyticsSummary(w, r)
}

func (s *Server) handleTaskRunAnalyticsRuns(w http.ResponseWriter, r *http.Request) {
	s.taskRuns().HandleTaskRunAnalyticsRuns(w, r)
}

func (s *Server) handleTaskRunAnalyticsFilterOptions(w http.ResponseWriter, r *http.Request) {
	s.taskRuns().HandleTaskRunAnalyticsFilterOptions(w, r)
}

// Package-level helpers moved to pkg/hub/analytics.

func taskRunKindForFactory(factory *types.FactoryConfig) string {
	return analytics.TaskRunKindForFactory(factory)
}

func taskRunKindForWorkflow(workflow *types.WorkflowConfig) string {
	return analytics.TaskRunKindForWorkflow(workflow)
}

func taskRunAnalyticsContractForFactory(factory *types.FactoryConfig) (enabled bool, requiresPR bool, excludedReason string) {
	return analytics.TaskRunAnalyticsContractForFactory(factory)
}

func taskRunAnalyticsContractForWorkflow(workflow *types.WorkflowConfig) (enabled bool, requiresPR bool, excludedReason string) {
	return analytics.TaskRunAnalyticsContractForWorkflow(workflow)
}

func sanitizeTaskRunDetail(detail map[string]any) (string, error) {
	return analytics.SanitizeTaskRunDetail(detail)
}

// firstNonEmpty and boolInt were shared helpers defined in
// task_run_analytics.go before the extraction; the analytics package keeps
// its own unexported copies, and these remain for the hub call sites.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
