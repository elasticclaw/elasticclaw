package hub

// This file is the compatibility bridge left behind by the extraction of the
// issue-tracker/webhook surface (linear, github, jira, shortcut, external
// webhook, integration poller) into pkg/hub/integrations (phase-2 hub
// reorganization). It keeps the existing call sites in this package
// (including tests) unchanged while the pkg/hub split proceeds one subpackage
// at a time: the aliases below simply forward to the integrations package.
// New code should use pkg/hub/integrations directly; callers drop these
// aliases as they migrate to their own subpackages.

import (
	"net/http"
	"time"

	"nhooyr.io/websocket"

	"github.com/elasticclaw/elasticclaw/pkg/hub/integrations"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// integrationsSvc returns the integrations service bound to this server's
// state. The service itself is stateless; all mutable state (config, claw
// registry, webhook dedup) stays on the Server behind the injected hooks, so
// building it per call is cheap and keeps hand-built test servers
// (&Server{...}) working without extra wiring.
func (s *Server) integrationsSvc() *integrations.Service {
	return integrations.New(integrations.Deps{
		DB:        s.db,
		Mu:        &s.mu,
		HubCfg:    func() *types.HubConfig { return s.hubCfg },
		PromoteMu: &s.promoteMu,

		WebhookDedupMu: &s.webhookDedupMu,
		WebhookDedup: func() map[string]time.Time {
			if s.webhookDedup == nil {
				s.webhookDedup = map[string]time.Time{}
			}
			return s.webhookDedup
		},

		CloseClawConn: func(clawID string, code websocket.StatusCode, reason string) {
			s.mu.Lock()
			if cc, ok := s.claws[clawID]; ok {
				cc.conn.Close(code, reason)
				delete(s.claws, clawID)
			}
			s.mu.Unlock()
		},
		ClawGatewayReady: func(clawID string) bool {
			s.mu.RLock()
			cc, connected := s.claws[clawID]
			ready := connected && cc.gatewayReady
			s.mu.RUnlock()
			return ready
		},

		GithubBaseURL:   func() string { return s.githubBaseURL },
		LinearBaseURL:   func() string { return s.linearBaseURL },
		ShortcutBaseURL: func() string { return s.shortcutBaseURL },
		JiraBaseURL:     func() string { return s.jiraBaseURL },
		GHBaseURL:       s.ghBaseURL,

		CronFinishRunByClawID: func(clawID, status, detail string) {
			if s.cronScheduler != nil {
				s.cronScheduler.FinishRunByClawID(clawID, status, detail)
			}
		},

		BroadcastToUsers:            s.broadcastToUsers,
		InjectHubMessageByID:        s.injectHubMessageByID,
		InjectUserMessage:           s.injectUserMessage,
		StopAgentWithReason:         s.stopAgentWithReason,
		CheckpointBeforeTermination: s.checkpointBeforeTermination,
		TerminateVM:                 s.terminateVM,
		SyncWorkflowVolumes:         s.syncWorkflowVolumes,
		ClawHubURL:                  s.clawHubURL,
		ProvisionDaytona:            s.provisionDaytona,
		ProvisionExedev:             s.provisionExedev,
		ProvisionLambdaMicroVMs:     s.provisionLambdaMicroVMs,
		ProvisionReplicated:         s.provisionReplicated,

		CreateClawFromFactory:  s.createClawFromFactory,
		CreateClawFromWorkflow: s.createClawFromWorkflowWithOptions,
		ResolveFactories:       s.resolveFactories,
		ResolveTemplateFiles:   s.resolveTemplateFiles,
		ResolveProvider:        resolveProvider,

		ClaimFactoryTrigger:    s.claimFactoryTrigger,
		CompleteFactoryTrigger: s.completeFactoryTrigger,
		FailFactoryTrigger:     s.failFactoryTrigger,

		TrackFactoryCreationSuccess: s.trackFactoryCreationSuccess,
		TrackFactoryCreationFailure: s.trackFactoryCreationFailure,
		TrackFactoryTermination:     s.trackFactoryTermination,
		TrackDoneSignal:             s.trackDoneSignal,
		RecordTaskRunEventForClaw:   s.recordTaskRunEventForClaw,
		TaskRunRequiresPRForClaw:    s.taskRunRequiresPRForClaw,

		FindPipelineContextForIssue:        s.findPipelineContextForIssue,
		PipelineStageForMessageContains:    s.pipelineStageForMessageContains,
		TransitionPipelineStage:            s.transitionPipelineStage,
		TransitionPipelineStageWithContext: s.transitionPipelineStageWithContext,
		ParsePipelineForContext:            parsePipelineForContext,
		ParsePipelineForFactory:            parsePipelineForFactory,
		HasFailedRequiredGate:              s.hasFailedRequiredGate,

		StorePRMention: s.storePRMention,
		ForwardHumanReviewComment: func(clawID, repo string, prNumber int, prURL string, id int64, login, body, htmlURL, path string, line int) {
			pr := clawPR{ClawID: clawID, Repo: repo, PRNumber: prNumber, PRURL: prURL}
			s.forwardHumanReviewComment(pr, id, login, body, htmlURL, path, line)
		},
		ForwardHumanRequestedChangesReview: func(clawID, repo string, prNumber int, prURL string, id int64, login, body, htmlURL string) {
			pr := clawPR{ClawID: clawID, Repo: repo, PRNumber: prNumber, PRURL: prURL}
			s.forwardHumanRequestedChangesReview(pr, id, login, body, htmlURL)
		},
		IsHumanGitHubActor:    s.isHumanGitHubActor,
		GithubAPIWithBase:     githubAPIWithBase,
		GithubAPIListWithBase: githubAPIListWithBase,

		HandleAgentFailureFeedback: s.handleAgentFailureFeedback,
		ClassifyAgentFailure:       classifyAgentFailure,

		ResolveGitHubToken:        s.resolveGitHubToken,
		ResolveGitHubTokenForRepo: s.resolveGitHubTokenForRepo,

		FindWorkspaceIssueTracker:           findWorkspaceIssueTracker,
		WorkspaceIssueTrackerWebhookSecrets: workspaceIssueTrackerWebhookSecrets,
		LoadExternalWorkflowsByIntegration:  loadExternalWorkflowsByIntegration,
		FilterWorkflowWorkspacesByName:      filterWorkflowWorkspacesByName,

		InjectFigmaAPIDocs:        injectFigmaAPIDocs,
		MergeTags:                 mergeTags,
		CloneStringMap:            cloneStringMap,
		ResolveDefaultModelForKey: resolveDefaultModelForKey,
	})
}

// Types moved to pkg/hub/integrations.
type (
	workspaceIssueTracker      = integrations.WorkspaceIssueTracker
	triggerActor               = integrations.TriggerActor
	agentFailureFeedback       = integrations.AgentFailureFeedback
	agentFailureMessage        = integrations.AgentFailureMessage
	agentFailureKind           = integrations.AgentFailureKind
	workflowCreateOptions      = integrations.WorkflowCreateOptions
	linearIssueDetails         = integrations.LinearIssueDetails
	linearWebhookPayload       = integrations.LinearWebhookPayload
	githubPRPayload            = integrations.GitHubPRPayload
	githubIssueComment         = integrations.GitHubIssueComment
	githubIssueCommentUser     = integrations.GitHubIssueCommentUser
	shortcutAction             = integrations.ShortcutAction
	externalWebhookPayload     = integrations.ExternalWebhookPayload
	githubIssuesPollItem       = integrations.GitHubIssuesPollItem
	githubIssuesWebhookPayload = integrations.GitHubIssuesWebhookPayload
)

// defaultFactoryPRPolicy moved to pkg/hub/integrations.
const defaultFactoryPRPolicy = integrations.DefaultFactoryPRPolicy

// Package-level helpers moved to pkg/hub/integrations.
var (
	errFactoryTriggerAlreadyClaimed       = integrations.ErrFactoryTriggerAlreadyClaimed
	isFactoryTriggerAlreadyClaimed        = integrations.IsFactoryTriggerAlreadyClaimed
	factoryTriggerKey                     = integrations.FactoryTriggerKey
	extractDonePRURLs                     = integrations.ExtractDonePRURLs
	appendDefaultFactoryPRPolicy          = integrations.AppendDefaultFactoryPRPolicy
	commentGitHubIssue                    = integrations.CommentGitHubIssue
	commentGitHubIssueWithBase            = integrations.CommentGitHubIssueWithBase
	githubAPIPostWithBase                 = integrations.GithubAPIPostWithBase
	moveGitHubIssue                       = integrations.MoveGitHubIssue
	moveShortcutStory                     = integrations.MoveShortcutStory
	commentShortcutIssue                  = integrations.CommentShortcutIssue
	githubIssuesWorkflowTriggerRepos      = integrations.GitHubIssuesWorkflowTriggerRepos
	buildGitHubIssuesPollPayloadForAction = integrations.BuildGitHubIssuesPollPayloadForAction
	buildLinearContext                    = integrations.BuildLinearContext
	buildShortcutContext                  = integrations.BuildShortcutContext
	buildGitHubPRContext                  = integrations.BuildGitHubPRContext
	buildGitHubIssuesContext              = integrations.BuildGitHubIssuesContext
	buildExternalEventContext             = integrations.BuildExternalEventContext
	fetchGitHubIssueComments              = integrations.FetchGitHubIssueComments
	githubIssuesWorkflowTriggerMatches    = integrations.GitHubIssuesWorkflowTriggerMatches
)

// Server methods moved to integrations.Service (HTTP handlers and helpers).

func (s *Server) handleLinearWebhook(w http.ResponseWriter, r *http.Request) {
	s.integrationsSvc().HandleLinearWebhook(w, r)
}

func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	s.integrationsSvc().HandleGitHubWebhook(w, r)
}

func (s *Server) handleGitHubIssuesWebhook(w http.ResponseWriter, r *http.Request) {
	s.integrationsSvc().HandleGitHubIssuesWebhook(w, r)
}

func (s *Server) handleJiraWebhook(w http.ResponseWriter, r *http.Request) {
	s.integrationsSvc().HandleJiraWebhook(w, r)
}

func (s *Server) handleShortcutWebhook(w http.ResponseWriter, r *http.Request) {
	s.integrationsSvc().HandleShortcutWebhook(w, r)
}

func (s *Server) handleExternalWebhook(w http.ResponseWriter, r *http.Request) {
	s.integrationsSvc().HandleExternalWebhook(w, r)
}

func (s *Server) handleFactoryEvents(w http.ResponseWriter, r *http.Request) {
	s.integrationsSvc().HandleFactoryEvents(w, r)
}

func (s *Server) handleClawDoneSignal(clawID, rawMessage string) {
	s.integrationsSvc().HandleClawDoneSignal(clawID, rawMessage)
}

func (s *Server) handleClawTerminateSignal(clawID, rawMessage string) {
	s.integrationsSvc().HandleClawTerminateSignal(clawID, rawMessage)
}

func (s *Server) promotePendingClaws() {
	s.integrationsSvc().PromotePendingClaws()
}

func (s *Server) startIntegrationPoller() {
	s.integrationsSvc().StartIntegrationPoller()
}

func (s *Server) pollTick() {
	s.integrationsSvc().PollTick()
}

func (s *Server) mergePRForClaw(clawID string) {
	s.integrationsSvc().MergePRForClaw(clawID)
}

func (s *Server) isOwnAppBot(login string) bool {
	return s.integrationsSvc().IsOwnAppBot(login)
}

func (s *Server) findFactoryForIssue(issueID string) *types.FactoryConfig {
	return s.integrationsSvc().FindFactoryForIssue(issueID)
}

func (s *Server) defaultProvider() string {
	return s.integrationsSvc().DefaultProvider()
}

func (s *Server) countActiveClawsInGroup(groupName string) int {
	return s.integrationsSvc().CountActiveClawsInGroup(groupName)
}

func (s *Server) resolveGroupLimit(factory *types.FactoryConfig) (string, int) {
	return s.integrationsSvc().ResolveGroupLimit(factory)
}

func (s *Server) resolveSecretRef(ref types.SecretRef, factory *types.FactoryConfig) (string, string, bool) {
	return s.integrationsSvc().ResolveSecretRef(ref, factory)
}

func (s *Server) resolveLinearTokenForFactory(factory *types.FactoryConfig) string {
	return s.integrationsSvc().ResolveLinearTokenForFactory(factory)
}

func (s *Server) resolveLinearTokenForWorkflow(workspaceName string, workflow *types.WorkflowConfig) string {
	return s.integrationsSvc().ResolveLinearTokenForWorkflow(workspaceName, workflow)
}

func (s *Server) resolveShortcutToken(workspace string) string {
	return s.integrationsSvc().ResolveShortcutToken(workspace)
}

func (s *Server) resolveShortcutBaseURL() string {
	return s.integrationsSvc().ResolveShortcutBaseURL()
}

func (s *Server) resolveGitHubIssuesTokenForFactory(factory *types.FactoryConfig) string {
	return s.integrationsSvc().ResolveGitHubIssuesTokenForFactory(factory)
}

func (s *Server) resolveGitHubIssuesTokenForWorkflow(workspaceName string, workflow *types.WorkflowConfig) string {
	return s.integrationsSvc().ResolveGitHubIssuesTokenForWorkflow(workspaceName, workflow)
}

func (s *Server) resolveJiraTrackerForFactory(factory *types.FactoryConfig) (workspaceIssueTracker, bool) {
	return s.integrationsSvc().ResolveJiraTrackerForFactory(factory)
}

func (s *Server) resolveJiraTrackerForWorkflow(workspaceName string, workflow *types.WorkflowConfig) (workspaceIssueTracker, bool) {
	return s.integrationsSvc().ResolveJiraTrackerForWorkflow(workspaceName, workflow)
}

func (s *Server) moveLinearIssueOnServer(token, issueIdentifier, targetStateName string) error {
	return s.integrationsSvc().MoveLinearIssueOnServer(token, issueIdentifier, targetStateName)
}

func (s *Server) moveJiraIssue(tracker workspaceIssueTracker, key, targetStatus string) error {
	return s.integrationsSvc().MoveJiraIssue(tracker, key, targetStatus)
}

func (s *Server) commentLinearIssue(token, issueIdentifier, body string) error {
	return s.integrationsSvc().CommentLinearIssue(token, issueIdentifier, body)
}

func (s *Server) commentJiraIssue(tracker workspaceIssueTracker, key, text string) error {
	return s.integrationsSvc().CommentJiraIssue(tracker, key, text)
}

func (s *Server) fetchLinearIssueDetails(token, issueIdentifier string) (*linearIssueDetails, error) {
	return s.integrationsSvc().FetchLinearIssueDetails(token, issueIdentifier)
}

func (s *Server) createClawForGitHubIssueWorkflow(workspace *types.WorkspaceConfig, workflow *types.WorkflowConfig, payload githubIssuesWebhookPayload, reason string) (string, bool, error) {
	return s.integrationsSvc().CreateClawForGitHubIssueWorkflow(workspace, workflow, payload, reason)
}

func (s *Server) validateDonePRs(clawID string, prURLs []string, ghToken string) (bool, string) {
	return s.integrationsSvc().ValidateDonePRs(clawID, prURLs, ghToken)
}

func (s *Server) resolveGitHubIssuesTokenForRepo(repoFullName string) string {
	return s.integrationsSvc().ResolveGitHubIssuesTokenForRepo(repoFullName)
}

func (s *Server) closeGitHubIssueForClaw(clawID string) {
	s.integrationsSvc().CloseGitHubIssueForClaw(clawID)
}
