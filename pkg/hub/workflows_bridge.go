package hub

// This file is the compatibility bridge left behind by the extraction of the
// workflow automation surface (pipeline_runner.go, factory_api.go,
// factory_creator.go, factory_trigger.go, factory_triggers.go, cron_api.go,
// cron_scheduler.go, pr_watcher.go) into pkg/hub/workflows (phase-2 hub
// reorganization, item 2.2 step 6). It keeps the existing call sites in this
// package (including tests) unchanged while the pkg/hub split proceeds one
// subpackage at a time: the aliases below simply forward to the workflows
// package. New code should use pkg/hub/workflows directly; callers drop
// these aliases as they migrate to their own subpackages.

import (
	"context"
	"net/http"
	"time"

	"nhooyr.io/websocket"

	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
	"github.com/elasticclaw/elasticclaw/pkg/hub/workflows"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// workflowsSvc returns the workflows service bound to this server's state.
// The service itself is stateless; all mutable state (config, claw registry,
// cron scheduler slot, promotion mutex) stays on the Server behind the
// injected hooks, so building it per call is cheap and keeps hand-built test
// servers (&Server{...}) working without extra wiring.
func (s *Server) workflowsSvc() *workflows.Service {
	return workflows.New(workflows.Deps{
		DB:            s.db,
		Mu:            &s.mu,
		HubCfg:        func() *types.HubConfig { return s.hubCfg },
		GithubBaseURL: func() string { return s.githubBaseURL },
		PromoteMu:     &s.promoteMu,
		CronSchedulerInst: func() *workflows.CronScheduler {
			return s.cronScheduler
		},

		ClawConn: func(clawID string) workflows.Conn {
			s.mu.RLock()
			cc := s.claws[clawID]
			s.mu.RUnlock()
			if cc == nil {
				return nil
			}
			return cc
		},
		ClawStreaming: func(clawID string) bool {
			s.mu.RLock()
			cc, connected := s.claws[clawID]
			streaming := connected && cc.streamingBuf.Len() > 0
			s.mu.RUnlock()
			return streaming
		},
		CloseClawConn: func(clawID string, code websocket.StatusCode, reason string) {
			s.mu.Lock()
			if cc, ok := s.claws[clawID]; ok {
				cc.conn.Close(code, reason)
				delete(s.claws, clawID)
			}
			s.mu.Unlock()
		},
		SendNextQueuedMessage: func(cc workflows.Conn) {
			s.sendNextQueuedMessage(cc.(*clawConn))
		},

		BroadcastToUsers:        s.broadcastToUsers,
		TerminateVM:             s.terminateVM,
		ClawHubURL:              s.clawHubURL,
		PromotePendingClaws:     s.promotePendingClaws,
		SendClawCheckpoints:     s.handleClawCheckpoints,
		ProvisionDaytona:        s.provisionDaytona,
		ProvisionExedev:         s.provisionExedev,
		ProvisionLambdaMicroVMs: s.provisionLambdaMicroVMs,
		ProvisionReplicated:     s.provisionReplicated,
		SSHRunWithTimeout:       s.sshRunWithTimeout,
		DefaultProvider:         s.defaultProvider,
		CountActiveClawsInGroup: s.countActiveClawsInGroup,
		ResolveGroupLimit:       s.resolveGroupLimit,
		TenantIDForClaw:         s.tenantIDForClaw,
		ClawNeedsInitialPlan:    s.clawNeedsInitialPlan,
		InsertSystemMarker:      s.insertSystemMarker,
		MergePRForClaw:          s.mergePRForClaw,
		CloseGitHubIssueForClaw: s.closeGitHubIssueForClaw,
		IsOwnAppBot:             s.isOwnAppBot,

		CheckpointBeforeTermination: s.checkpointBeforeTermination,
		SyncWorkflowVolumes:         s.syncWorkflowVolumes,
		ResolveFactories:            s.resolveFactories,

		GetPipelineStage:             s.getPipelineStage,
		RecordPipelineStageVisit:     s.recordPipelineStageVisit,
		HasVisitedPipelineStage:      s.hasVisitedPipelineStage,
		ClaimPipelineStageTransition: s.claimPipelineStageTransition,

		LoadManualTriggerInputs: s.loadManualTriggerInputs,

		CreateClawFromWorkflowWithOptions: s.createClawFromWorkflowWithOptions,

		FetchLinearIssueDetails:             s.fetchLinearIssueDetails,
		MoveLinearIssueOnServer:             s.moveLinearIssueOnServer,
		CommentLinearIssue:                  s.commentLinearIssue,
		MoveJiraIssue:                       s.moveJiraIssue,
		CommentJiraIssue:                    s.commentJiraIssue,
		ResolveJiraTrackerForFactory:        s.resolveJiraTrackerForFactory,
		ResolveJiraTrackerForWorkflow:       s.resolveJiraTrackerForWorkflow,
		ResolveLinearTokenForFactory:        s.resolveLinearTokenForFactory,
		ResolveLinearTokenForWorkflow:       s.resolveLinearTokenForWorkflow,
		ResolveGitHubIssuesTokenForFactory:  s.resolveGitHubIssuesTokenForFactory,
		ResolveGitHubIssuesTokenForWorkflow: s.resolveGitHubIssuesTokenForWorkflow,
		ResolveShortcutToken:                s.resolveShortcutToken,
		ResolveShortcutBaseURL:              s.resolveShortcutBaseURL,
		FindWorkspaceIssueTracker:           findWorkspaceIssueTracker,
		FindFactoryForIssue:                 s.findFactoryForIssue,

		HandleAgentFailureFeedback: s.handleAgentFailureFeedback,
		BuildAgentStopComment:      s.buildAgentStopComment,
		ClassifyAgentFailure:       classifyAgentFailure,
		SanitizeBootstrapOutput:    sanitizeBootstrapOutput,
		SanitizeFailureDetails:     sanitizeFailureDetails,
		FirstUsefulFailureLines:    firstUsefulFailureLines,
		TriggerActorForClaw:        s.triggerActorForClaw,

		EnsureTaskRunForClaw:               s.ensureTaskRunForClaw,
		TaskRunContextForClaw:              s.taskRunContextForClaw,
		AssociateTaskRunPR:                 s.associateTaskRunPR,
		RecordTaskRunEventForClaw:          s.recordTaskRunEventForClaw,
		RecordTaskRunHumanEventForClaw:     s.recordTaskRunHumanEventForClaw,
		TaskRunKindForFactory:              taskRunKindForFactory,
		TaskRunAnalyticsContractForFactory: taskRunAnalyticsContractForFactory,
		TrackFactoryCreationSuccess:        s.trackFactoryCreationSuccess,
		TrackFactoryCreationFailure:        s.trackFactoryCreationFailure,
		TrackPROpened:                      s.trackPROpened,
		TrackPRMerged:                      s.trackPRMerged,
		TrackPRClosed:                      s.trackPRClosed,

		ExecuteDependencyUpdatesAction: s.executeDependencyUpdatesAction,
		DependencyUpdatesConfigured:    dependencyUpdatesConfigured,
		DependencyUpdatesOutputName:    dependencyUpdatesOutputName,

		StreamLLMWithSystemPrompt: streamLLMWithSystemPrompt,

		TenantFromRequest:      tenantFromCtx,
		GithubLoginFromContext: githubLoginFromContext,
		CanViewClaw:            canViewClaw,

		NewDaytonaProvider:        newDaytonaProvider,
		NewExedevProvider:         newExedevProvider,
		NewLambdaMicroVMsProvider: newLambdaMicroVMsProvider,
		NewGitHubTokenProvider: func(cfg *types.GitHubAppConfig) (workflows.GitHubTokenProvider, error) {
			p, err := NewGitHubTokenProvider(cfg)
			if err != nil {
				return nil, err
			}
			return githubTokenProviderAdapter{p: p}, nil
		},

		MergeTags:                 mergeTags,
		ResolveDefaultModelForKey: resolveDefaultModelForKey,
		ResolveSecretRef:          s.resolveSecretRef,
		ResolveTemplateFiles:      s.resolveTemplateFiles,
		InjectFigmaAPIDocs:        injectFigmaAPIDocs,
		InitialPlanRequiredMarker: initialPlanRequiredMarker,
		InitialPlanWakeContent:    initialPlanWakeContent,
	})
}

// githubTokenProviderAdapter adapts the hub's *GitHubTokenProvider to the
// workflows.GitHubTokenProvider interface, converting the RepoAccess slice
// between the (structurally identical) hub and workflows types.
type githubTokenProviderAdapter struct {
	p *GitHubTokenProvider
}

func (a githubTokenProviderAdapter) InstallationToken(ctx context.Context, installationID int64, repos []workflows.RepoAccess) (string, time.Time, error) {
	var converted []RepoAccess
	if repos != nil {
		converted = make([]RepoAccess, len(repos))
		for i, r := range repos {
			converted[i] = RepoAccess(r)
		}
	}
	return a.p.InstallationToken(ctx, installationID, converted)
}

// workflows.Conn implementation on the hub's clawConn: thin accessors so
// the extracted PR-watcher message injection keeps using the exact same
// mutex and fields. Lock/Unlock/WriteWS are shared with the checkpoints
// bridge (checkpoints_bridge.go). Methods with the Locked suffix must be
// called with the connection lock held.

func (cc *clawConn) SetLastUserMessageAtLocked(t time.Time) { cc.lastUserMessageAt = t }

func (cc *clawConn) BusyLocked() bool { return cc.isBusyLocked() }

func (cc *clawConn) AppendMessageQueueLocked(msg types.HubMessage) int {
	cc.messageQueue = append(cc.messageQueue, msg)
	return len(cc.messageQueue)
}

func (cc *clawConn) PrependMessageQueueLocked(msg types.HubMessage) int {
	cc.messageQueue = append([]types.HubMessage{msg}, cc.messageQueue...)
	return len(cc.messageQueue)
}

// Types and constants moved to pkg/hub/workflows.
type (
	cronScheduler         = workflows.CronScheduler
	pipelineRunResult     = workflows.PipelineRunResult
	pipelineContext       = workflows.PipelineContext
	clawPR                = workflows.ClawPR
	FactoryTriggerRequest = workflows.FactoryTriggerRequest
)

const issueLabelsTemplateFile = workflows.IssueLabelsTemplateFile

// Package-level helpers moved to pkg/hub/workflows.
var (
	validateFactoryInputs   = workflows.ValidateFactoryInputs
	resolveProvider         = workflows.ResolveProvider
	githubAPIWithBase       = workflows.GithubAPIWithBase
	githubAPIListWithBase   = workflows.GithubAPIListWithBase
	githubAPIAddLabel       = workflows.GithubAPIAddLabel
	parsePipelineForFactory = workflows.ParsePipelineForFactory
	parsePipelineForContext = workflows.ParsePipelineForContext
	normalizeIssueLabels    = workflows.NormalizeIssueLabels
)

// newCronScheduler builds the cron scheduler bound to this server's state.
func newCronScheduler(srv *Server) *cronScheduler {
	return workflows.NewCronScheduler(srv.workflowsSvc())
}

// Server methods moved to workflows.Service.

func (s *Server) handleFactoriesCRUD(w http.ResponseWriter, r *http.Request) {
	s.workflowsSvc().HandleFactoriesCRUD(w, r)
}

func (s *Server) handleFactoryTrigger(w http.ResponseWriter, r *http.Request) {
	s.workflowsSvc().HandleFactoryTrigger(w, r)
}

func (s *Server) handleCronWorkflowTrigger(w http.ResponseWriter, r *http.Request) {
	s.workflowsSvc().HandleCronWorkflowTrigger(w, r)
}

func (s *Server) handleCronWorkflowRuns(w http.ResponseWriter, r *http.Request) {
	s.workflowsSvc().HandleCronWorkflowRuns(w, r)
}

func (s *Server) handleCronWorkflowNextRun(w http.ResponseWriter, r *http.Request) {
	s.workflowsSvc().HandleCronWorkflowNextRun(w, r)
}

func (s *Server) handleClawSubresource(w http.ResponseWriter, r *http.Request) {
	s.workflowsSvc().HandleClawSubresource(w, r)
}

func (s *Server) createClawFromFactory(factory *types.FactoryConfig, issueID string, inputs map[string]string, prebuiltTemplateFiles map[string]string, reason string) (string, bool, error) {
	return s.workflowsSvc().CreateClawFromFactory(factory, issueID, inputs, prebuiltTemplateFiles, reason)
}

func (s *Server) claimFactoryTrigger(factoryName, integration, triggerKey, source string, payload any) (bool, error) {
	return s.workflowsSvc().ClaimFactoryTrigger(factoryName, integration, triggerKey, source, payload)
}

func (s *Server) completeFactoryTrigger(factoryName, integration, triggerKey, clawID string) error {
	return s.workflowsSvc().CompleteFactoryTrigger(factoryName, integration, triggerKey, clawID)
}

func (s *Server) failFactoryTrigger(factoryName, integration, triggerKey string) {
	s.workflowsSvc().FailFactoryTrigger(factoryName, integration, triggerKey)
}

func (s *Server) storePRMention(clawID, repo string, prNumber int, prURL string) {
	s.workflowsSvc().StorePRMention(clawID, repo, prNumber, prURL)
}

func (s *Server) scanMessageForPRs(clawID, content string) {
	s.workflowsSvc().ScanMessageForPRs(clawID, content)
}

func (s *Server) startPRWatcher() {
	s.workflowsSvc().StartPRWatcher()
}

func (s *Server) pollAllPRs() {
	s.workflowsSvc().PollAllPRs()
}

func (s *Server) resolveGitHubToken() string {
	return s.workflowsSvc().ResolveGitHubToken()
}

func (s *Server) resolveGitHubTokenForRepo(repo string) string {
	return s.workflowsSvc().ResolveGitHubTokenForRepo(repo)
}

func (s *Server) forwardHumanReviewComment(pr clawPR, id int64, login, body, htmlURL, path string, line int) {
	s.workflowsSvc().ForwardHumanReviewComment(pr, id, login, body, htmlURL, path, line)
}

func (s *Server) forwardHumanRequestedChangesReview(pr clawPR, id int64, login, body, htmlURL string) {
	s.workflowsSvc().ForwardHumanRequestedChangesReview(pr, id, login, body, htmlURL)
}

func (s *Server) isHumanGitHubActor(login, userType string) bool {
	return s.workflowsSvc().IsHumanGitHubActor(login, userType)
}

func (s *Server) injectUserMessage(clawID, content string) {
	s.workflowsSvc().InjectUserMessage(clawID, content)
}

func (s *Server) injectHubMessageByID(clawID, content string) {
	s.workflowsSvc().InjectHubMessageByID(clawID, content)
}

func (s *Server) executePipelineCommand(clawID, command string, timeout time.Duration) (*pipelineRunResult, error) {
	return s.workflowsSvc().ExecutePipelineCommand(clawID, command, timeout)
}

func (s *Server) persistPipelineOutput(clawID, stageID, outputName string, result *pipelineRunResult) {
	s.workflowsSvc().PersistPipelineOutput(clawID, stageID, outputName, result)
}

func (s *Server) transitionPipelineStage(clawID string, stage pipeline.Stage, factory *types.FactoryConfig, issueID string) bool {
	return s.workflowsSvc().TransitionPipelineStage(clawID, stage, factory, issueID)
}

func (s *Server) transitionPipelineStageWithContext(clawID string, stage pipeline.Stage, ctx pipelineContext) bool {
	return s.workflowsSvc().TransitionPipelineStageWithContext(clawID, stage, ctx)
}

func (s *Server) checkPipelineMessageTriggers(clawID, message string) bool {
	return s.workflowsSvc().CheckPipelineMessageTriggers(clawID, message)
}

func (s *Server) pipelineStageForMessageContains(clawID, message string) (pipelineContext, *pipeline.Stage, bool) {
	return s.workflowsSvc().PipelineStageForMessageContains(clawID, message)
}

func (s *Server) hasFailedRequiredGate(clawID string) bool {
	return s.workflowsSvc().HasFailedRequiredGate(clawID)
}

func (s *Server) initializePipelineEntryIfNeeded(clawID string) bool {
	return s.workflowsSvc().InitializePipelineEntryIfNeeded(clawID)
}

func (s *Server) stopAgentWithReason(clawID, reason string, skipVMTerminate bool) {
	s.workflowsSvc().StopAgentWithReason(clawID, reason, skipVMTerminate)
}

func (s *Server) findFactoryForClaw(clawID string) (*types.FactoryConfig, string) {
	return s.workflowsSvc().FindFactoryForClaw(clawID)
}

func (s *Server) findPipelineContextForIssue(issueID string) (pipelineContext, bool) {
	return s.workflowsSvc().FindPipelineContextForIssue(issueID)
}

func (s *Server) clawIssueAndTags(clawID string) (string, []string) {
	return s.workflowsSvc().ClawIssueAndTags(clawID)
}

// Test bridge: the pkg/hub tests that stayed behind (they exercise hub
// Server internals) keep their original identifiers through these aliases
// and delegates.

type prCommentOptions = workflows.PRCommentOptions

var (
	parseJudgeResponse          = workflows.ParseJudgeResponse
	judgeTimeout                = workflows.JudgeTimeoutForTest
	truncateString              = workflows.TruncateString
	parsePipelineOutputJSON     = workflows.ParsePipelineOutputJSON
	validateScriptCommand       = workflows.ValidateScriptCommand
	githubAPIDeleteLabel        = workflows.GithubAPIDeleteLabel
	loadWorkflowPipelineContext = workflows.LoadWorkflowPipelineContext
	triggerPayloadJSON          = workflows.TriggerPayloadJSON
	buildManualTriggerContext   = workflows.BuildManualTriggerContext
)

func (s *Server) commentWorkflowAgentStopToTracker(clawID string, ctx pipelineContext, reason string) {
	s.workflowsSvc().CommentWorkflowAgentStopToTracker(clawID, ctx, reason)
}

func (s *Server) evaluateGate(clawID, stageID string, gate *pipeline.Gate) *workflows.GateEvaluationResult {
	return s.workflowsSvc().EvaluateGate(clawID, stageID, gate)
}

func (s *Server) loadGateResult(clawID, stageID string) *workflows.GateEvaluationResult {
	return s.workflowsSvc().LoadGateResult(clawID, stageID)
}

func (s *Server) loadPipelineOutputs(clawID string) map[string]map[string]interface{} {
	return s.workflowsSvc().LoadPipelineOutputs(clawID)
}

func (s *Server) runOnEnter(clawID string, stage pipeline.Stage, ctx pipelineContext) bool {
	return s.workflowsSvc().RunOnEnter(clawID, stage, ctx)
}

func (s *Server) loadIssueLabelsForClaw(clawID string) ([]string, bool) {
	return s.workflowsSvc().LoadIssueLabelsForClaw(clawID)
}

func (s *Server) injectTemplateData(clawID string, baseData interface{}) interface{} {
	return s.workflowsSvc().InjectTemplateData(clawID, baseData)
}

func (s *Server) autoTransitionAfterJudge(clawID, verdict string, ctx pipelineContext) {
	s.workflowsSvc().AutoTransitionAfterJudge(clawID, verdict, ctx)
}

func (s *Server) autoTransitionAfterGate(clawID, stageID, verdict string, ctx pipelineContext) {
	s.workflowsSvc().AutoTransitionAfterGate(clawID, stageID, verdict, ctx)
}

func (s *Server) checkPRComments(pr clawPR, commentsData []interface{}, opts prCommentOptions) {
	s.workflowsSvc().CheckPRComments(pr, commentsData, opts)
}

func (s *Server) checkPRReviews(pr clawPR, reviewsData []interface{}) {
	s.workflowsSvc().CheckPRReviews(pr, reviewsData)
}

func (s *Server) checkGreptileReviewComments(pr clawPR, reviewCommentsData []interface{}) {
	s.workflowsSvc().CheckGreptileReviewComments(pr, reviewCommentsData)
}

func (s *Server) resolveGitHubIssuesTokenForPipeline(ctx pipelineContext) string {
	return s.workflowsSvc().ResolveGitHubIssuesTokenForPipeline(ctx)
}
