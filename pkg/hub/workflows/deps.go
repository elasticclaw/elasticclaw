package workflows

import (
	"context"
	"database/sql"
	"net/http"
	"sync"
	"time"

	"nhooyr.io/websocket"

	"github.com/elasticclaw/elasticclaw/pkg/hub/analytics"
	"github.com/elasticclaw/elasticclaw/pkg/hub/checkpoints"
	"github.com/elasticclaw/elasticclaw/pkg/hub/integrations"
	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
	"github.com/elasticclaw/elasticclaw/pkg/hub/settings"
	"github.com/elasticclaw/elasticclaw/pkg/provider/daytona"
	"github.com/elasticclaw/elasticclaw/pkg/provider/exedev"
	lambdamicrovms "github.com/elasticclaw/elasticclaw/pkg/provider/lambdamicrovms"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// Conn is the subset of the hub's per-claw WS connection state the
// workflows service needs (message injection from the PR watcher). It is
// implemented by the hub's *clawConn via thin bridge methods, so the exact
// same mutex and fields keep guarding that state as before the extraction.
// Methods with the Locked suffix must be called with the connection lock
// held, mirroring the pre-extraction direct field access.
type Conn interface {
	Lock()
	Unlock()

	SetLastUserMessageAtLocked(t time.Time)
	// BusyLocked reports whether the agent is mid-turn (the hub's
	// isBusyLocked), in which case injected messages are queued.
	BusyLocked() bool
	// AppendMessageQueueLocked appends msg to the connection's message
	// queue and returns the new queue length.
	AppendMessageQueueLocked(msg types.HubMessage) int
	// PrependMessageQueueLocked puts msg at the front of the message queue
	// (retry after a failed WS write) and returns the new queue length.
	PrependMessageQueueLocked(msg types.HubMessage) int

	// WriteWS writes a WS message to the claw connection (wsjson.Write on
	// the underlying conn). Called without the connection lock held, as
	// before the extraction.
	WriteWS(ctx context.Context, msg types.WSMessage) error
}

// GitHubTokenProvider mints GitHub App installation tokens; implemented by
// the hub's *GitHubTokenProvider (github.go) through a thin adapter.
type GitHubTokenProvider interface {
	InstallationToken(ctx context.Context, installationID int64, repos []RepoAccess) (string, time.Time, error)
}

// RepoAccess mirrors the hub's github.go RepoAccess (repo + permission
// level used when minting tokens); the bridge converts between the two.
type RepoAccess struct {
	Repo        string // "owner/repo"
	Permissions string // "read" or "write"
}

// Deps carries the hub-owned state and helpers the workflows service
// needs. Everything is injected so the package does not depend on pkg/hub
// (which would create an import cycle). Hooks that read mutable Server
// fields are closures so live config/test overrides stay visible.
type Deps struct {
	// DB is the hub database.
	DB *sql.DB
	// Mu is the hub's config/claw-registry mutex. It must be the same
	// mutex the hub uses so reads/writes keep the exact same
	// synchronization as before the extraction.
	CfgMu *sync.RWMutex
	// HubCfg reads the hub's live config. Called with Mu held where the
	// pre-extraction code held it.
	HubCfg func() *types.HubConfig
	// GithubBaseURL reads the hub's GitHub API base override (tests).
	GithubBaseURL func() string
	// PromoteMu is the hub's pending-claw promotion mutex (shared with
	// promotePendingClaws so concurrency-limit checks stay serialized).
	PromoteMu *sync.Mutex
	// CronSchedulerInst reads the hub's cron scheduler slot (nil until the
	// hub constructs it; also nil on hand-built test servers).
	CronSchedulerInst func() *CronScheduler

	// Claw registry access (owned by pkg/hub until the claws/ extraction).
	// ClawConn returns the connection for a claw, or nil when the claw is
	// not connected. Takes the registry lock itself.
	ClawConn func(clawID string) Conn
	// ClawStreaming reports whether the claw's WS connection has a
	// non-empty streaming buffer, under the registry lock (exactly the
	// pre-extraction check in the pipeline terminal-stage wait loop).
	ClawStreaming func(clawID string) bool
	// CloseClawConn closes and removes the WS connection for a claw, if
	// connected. It takes the hub's claw-registry lock itself.
	CloseClawConn func(clawID string, code websocket.StatusCode, reason string)
	// SendNextQueuedMessage delivers the next queued message for the
	// connection (hub's sendNextQueuedMessage).
	SendNextQueuedMessage func(cc Conn)

	// Claw lifecycle and messaging (owned by pkg/hub until the claws/
	// extraction).
	BroadcastToUsers        func(tenantID string, msg types.WSMessage)
	TerminateVM             func(provider, vmID string)
	ClawHubURL              func() string
	PromotePendingClaws     func()
	SendClawCheckpoints     func(w http.ResponseWriter, r *http.Request, clawID string)
	ProvisionDaytona        func(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, files map[string][]byte, env map[string]string) error
	ProvisionExedev         func(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, files map[string][]byte, env map[string]string) error
	ProvisionLambdaMicroVMs func(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, files map[string][]byte) error
	ProvisionReplicated     func(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, env map[string]string) error
	SSHRunWithTimeout       func(user, host, script string, timeout time.Duration) (string, error)
	DefaultProvider         func() string
	CountActiveClawsInGroup func(groupName string) int
	ResolveGroupLimit       func(factory *types.FactoryConfig) (string, int)
	TenantIDForClaw         func(clawID string) string
	ClawNeedsInitialPlan    func(clawID string) bool
	InsertSystemMarker      func(clawID, tenantID, marker string) bool
	MergePRForClaw          func(clawID string)
	CloseGitHubIssueForClaw func(clawID string)
	IsOwnAppBot             func(login string) bool

	// Checkpoint/volume hooks (pkg/hub bridge over pkg/hub/checkpoints).
	CheckpointBeforeTermination func(clawID, reason string)
	SyncWorkflowVolumes         func(clawID string)
	ResolveFactories            func() []*types.FactoryConfig

	// Pipeline stage-visit state (pkg/hub's pipeline_state.go).
	GetPipelineStage             func(clawID string) string
	RecordPipelineStageVisit     func(clawID, stageID string)
	HasVisitedPipelineStage      func(clawID, stageID string) bool
	ClaimPipelineStageTransition func(clawID, stageID string) bool

	// Manual trigger inputs (pkg/hub's manual_trigger.go).
	LoadManualTriggerInputs func(clawID string) map[string]string

	// Workflow claw creation (pkg/hub's workflow_creator.go).
	CreateClawFromWorkflowWithOptions func(workspace *types.WorkspaceConfig, workflow *types.WorkflowConfig, opts workflowCreateOptions) (string, bool, error)

	// Issue tracker helpers still owned by pkg/hub (integrations bridge).
	FetchLinearIssueDetails             func(token, issueIdentifier string) (*linearIssueDetails, error)
	MoveLinearIssueOnServer             func(token, issueIdentifier, targetStateName string) error
	CommentLinearIssue                  func(token, issueIdentifier, body string) error
	MoveJiraIssue                       func(tracker workspaceIssueTracker, key, targetStatus string) error
	CommentJiraIssue                    func(tracker workspaceIssueTracker, key, text string) error
	ResolveJiraTrackerForFactory        func(factory *types.FactoryConfig) (workspaceIssueTracker, bool)
	ResolveJiraTrackerForWorkflow       func(workspaceName string, workflow *types.WorkflowConfig) (workspaceIssueTracker, bool)
	ResolveLinearTokenForFactory        func(factory *types.FactoryConfig) string
	ResolveLinearTokenForWorkflow       func(workspaceName string, workflow *types.WorkflowConfig) string
	ResolveGitHubIssuesTokenForFactory  func(factory *types.FactoryConfig) string
	ResolveGitHubIssuesTokenForWorkflow func(workspaceName string, workflow *types.WorkflowConfig) string
	ResolveShortcutToken                func(workspace string) string
	ResolveShortcutBaseURL              func() string
	FindWorkspaceIssueTracker           func(workspace, trackerType, name string) (workspaceIssueTracker, bool)
	FindFactoryForIssue                 func(issueID string) *types.FactoryConfig

	// Agent failure feedback (pkg/hub's agent_failure_feedback.go and
	// failure_summary.go).
	HandleAgentFailureFeedback func(feedback agentFailureFeedback, token string)
	BuildAgentStopComment      func(clawID, reason string) string
	ClassifyAgentFailure       func(reason string) integrations.AgentFailureMessage
	SanitizeBootstrapOutput    func(out string) string
	SanitizeFailureDetails     func(raw string) string
	FirstUsefulFailureLines    func(s string, maxLines int) string
	TriggerActorForClaw        func(clawID string) integrations.TriggerActor

	// Task-run analytics hooks (pkg/hub bridge over pkg/hub/analytics).
	EnsureTaskRunForClaw               func(clawID string, opts TaskRunStart) (string, string, error)
	TaskRunContextForClaw              func(clawID string) (tenantID, runID, attemptID string, ok bool, err error)
	AssociateTaskRunPR                 func(input TaskRunPR) error
	RecordTaskRunEventForClaw          func(clawID string, input TaskRunEvent) error
	RecordTaskRunHumanEventForClaw     func(clawID, eventType, eventKey, actorLogin, targetURL string, detail map[string]any)
	TaskRunKindForFactory              func(factory *types.FactoryConfig) string
	TaskRunAnalyticsContractForFactory func(factory *types.FactoryConfig) (enabled bool, requiresPR bool, excludedReason string)
	TrackFactoryCreationSuccess        func(factoryName, issueID, clawID string)
	TrackFactoryCreationFailure        func(factoryName, issueID, detail string)
	TrackPROpened                      func(factoryName, issueID, clawID, repo string, prNumber int)
	TrackPRMerged                      func(factoryName, issueID, clawID, repo string, prNumber int)
	TrackPRClosed                      func(factoryName, issueID, clawID, repo string, prNumber int)

	// Dependency updates action (pkg/hub's dependency_updates.go).
	ExecuteDependencyUpdatesAction func(clawID, stageID string, action pipeline.DependencyUpdatesAction) (*pipelineRunResult, error)
	DependencyUpdatesConfigured    func(action pipeline.DependencyUpdatesAction) bool
	DependencyUpdatesOutputName    func(action pipeline.DependencyUpdatesAction) string

	// LLM judge call (pkg/hub bridge over pkg/hub/settings).
	StreamLLMWithSystemPrompt func(ctx context.Context, systemPrompt string, msgs []aiChatMessage, llmKeys types.LLMKeysList, defaultModel string, onToken func(string)) error

	// Request auth/identity helpers still owned by pkg/hub.
	TenantFromRequest      func(r *http.Request) string
	GithubLoginFromContext func(ctx context.Context) string
	CanViewClaw            func(cfg *types.AccessConfig, userLogin string, clawTags []string) bool

	// Provider construction (pkg/hub's providers.go and github.go).
	NewDaytonaProvider        func(cfg types.ProviderConfig) (*daytona.Provider, error)
	NewExedevProvider         func(cfg types.ProviderConfig) (*exedev.Provider, error)
	NewLambdaMicroVMsProvider func(cfg types.ProviderConfig) (*lambdamicrovms.Provider, error)
	NewGitHubTokenProvider    func(cfg *types.GitHubAppConfig) (GitHubTokenProvider, error)

	// Misc helpers still owned by pkg/hub.
	MergeTags                 func(templateName string, configTags []string, cliTags []string) []string
	ResolveDefaultModelForKey func(hubCfg *types.HubConfig, key *types.LLMKeyConfig) string
	ResolveSecretRef          func(ref types.SecretRef, factory *types.FactoryConfig) (string, string, bool)
	ResolveTemplateFiles      func(name string) (map[string]string, error)
	InjectFigmaAPIDocs        func(files map[string]string, env map[string]string) map[string]string
	// InitialPlanRequiredMarker/InitialPlanWakeContent mirror the pkg/hub
	// constants of the same (lowercased) names.
	InitialPlanRequiredMarker string
	InitialPlanWakeContent    string
}

// Convenience accessors so the mechanically-moved code keeps its original
// shape (s.deps.X() everywhere would obscure the diff).

func (s *Service) hubCfg() *types.HubConfig { return s.deps.HubCfg() }

func (s *Service) githubBaseURL() string { return s.deps.GithubBaseURL() }

func (s *Service) cronScheduler() *CronScheduler { return s.deps.CronSchedulerInst() }

// Forwarders to hub-owned methods, keeping the pre-extraction call sites
// unchanged. They disappear as their targets move into their own
// subpackages (claws/, store/) in later phase-2 steps.

func (s *Service) broadcastToUsers(tenantID string, msg types.WSMessage) {
	s.deps.BroadcastToUsers(tenantID, msg)
}

func (s *Service) terminateVM(provider, vmID string) { s.deps.TerminateVM(provider, vmID) }

func (s *Service) clawHubURL() string { return s.deps.ClawHubURL() }

func (s *Service) promotePendingClaws() { s.deps.PromotePendingClaws() }

func (s *Service) handleClawCheckpoints(w http.ResponseWriter, r *http.Request, clawID string) {
	s.deps.SendClawCheckpoints(w, r, clawID)
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

func (s *Service) sshRunWithTimeout(user, host, script string, timeout time.Duration) (string, error) {
	return s.deps.SSHRunWithTimeout(user, host, script, timeout)
}

func (s *Service) defaultProvider() string { return s.deps.DefaultProvider() }

func (s *Service) countActiveClawsInGroup(groupName string) int {
	return s.deps.CountActiveClawsInGroup(groupName)
}

func (s *Service) resolveGroupLimit(factory *types.FactoryConfig) (string, int) {
	return s.deps.ResolveGroupLimit(factory)
}

func (s *Service) tenantIDForClaw(clawID string) string { return s.deps.TenantIDForClaw(clawID) }

func (s *Service) clawNeedsInitialPlan(clawID string) bool {
	return s.deps.ClawNeedsInitialPlan(clawID)
}

func (s *Service) insertSystemMarker(clawID, tenantID, marker string) bool {
	return s.deps.InsertSystemMarker(clawID, tenantID, marker)
}

func (s *Service) mergePRForClaw(clawID string) { s.deps.MergePRForClaw(clawID) }

func (s *Service) closeGitHubIssueForClaw(clawID string) { s.deps.CloseGitHubIssueForClaw(clawID) }

func (s *Service) isOwnAppBot(login string) bool { return s.deps.IsOwnAppBot(login) }

func (s *Service) checkpointBeforeTermination(clawID, reason string) {
	s.deps.CheckpointBeforeTermination(clawID, reason)
}

func (s *Service) syncWorkflowVolumes(clawID string) { s.deps.SyncWorkflowVolumes(clawID) }

func (s *Service) resolveFactories() []*types.FactoryConfig { return s.deps.ResolveFactories() }

func (s *Service) getPipelineStage(clawID string) string { return s.deps.GetPipelineStage(clawID) }

func (s *Service) recordPipelineStageVisit(clawID, stageID string) {
	s.deps.RecordPipelineStageVisit(clawID, stageID)
}

func (s *Service) hasVisitedPipelineStage(clawID, stageID string) bool {
	return s.deps.HasVisitedPipelineStage(clawID, stageID)
}

func (s *Service) claimPipelineStageTransition(clawID, stageID string) bool {
	return s.deps.ClaimPipelineStageTransition(clawID, stageID)
}

func (s *Service) loadManualTriggerInputs(clawID string) map[string]string {
	return s.deps.LoadManualTriggerInputs(clawID)
}

func (s *Service) createClawFromWorkflowWithOptions(workspace *types.WorkspaceConfig, workflow *types.WorkflowConfig, opts workflowCreateOptions) (string, bool, error) {
	return s.deps.CreateClawFromWorkflowWithOptions(workspace, workflow, opts)
}

func (s *Service) fetchLinearIssueDetails(token, issueIdentifier string) (*linearIssueDetails, error) {
	return s.deps.FetchLinearIssueDetails(token, issueIdentifier)
}

func (s *Service) moveLinearIssueOnServer(token, issueIdentifier, targetStateName string) error {
	return s.deps.MoveLinearIssueOnServer(token, issueIdentifier, targetStateName)
}

func (s *Service) commentLinearIssue(token, issueIdentifier, body string) error {
	return s.deps.CommentLinearIssue(token, issueIdentifier, body)
}

func (s *Service) moveJiraIssue(tracker workspaceIssueTracker, key, targetStatus string) error {
	return s.deps.MoveJiraIssue(tracker, key, targetStatus)
}

func (s *Service) commentJiraIssue(tracker workspaceIssueTracker, key, text string) error {
	return s.deps.CommentJiraIssue(tracker, key, text)
}

func (s *Service) resolveJiraTrackerForFactory(factory *types.FactoryConfig) (workspaceIssueTracker, bool) {
	return s.deps.ResolveJiraTrackerForFactory(factory)
}

func (s *Service) resolveJiraTrackerForWorkflow(workspaceName string, workflow *types.WorkflowConfig) (workspaceIssueTracker, bool) {
	return s.deps.ResolveJiraTrackerForWorkflow(workspaceName, workflow)
}

func (s *Service) resolveLinearTokenForFactory(factory *types.FactoryConfig) string {
	return s.deps.ResolveLinearTokenForFactory(factory)
}

func (s *Service) resolveLinearTokenForWorkflow(workspaceName string, workflow *types.WorkflowConfig) string {
	return s.deps.ResolveLinearTokenForWorkflow(workspaceName, workflow)
}

func (s *Service) resolveGitHubIssuesTokenForFactory(factory *types.FactoryConfig) string {
	return s.deps.ResolveGitHubIssuesTokenForFactory(factory)
}

func (s *Service) resolveGitHubIssuesTokenForWorkflow(workspaceName string, workflow *types.WorkflowConfig) string {
	return s.deps.ResolveGitHubIssuesTokenForWorkflow(workspaceName, workflow)
}

func (s *Service) resolveShortcutToken(workspace string) string {
	return s.deps.ResolveShortcutToken(workspace)
}

func (s *Service) resolveShortcutBaseURL() string { return s.deps.ResolveShortcutBaseURL() }

func (s *Service) findFactoryForIssue(issueID string) *types.FactoryConfig {
	return s.deps.FindFactoryForIssue(issueID)
}

func (s *Service) handleAgentFailureFeedback(feedback agentFailureFeedback, token string) {
	s.deps.HandleAgentFailureFeedback(feedback, token)
}

func (s *Service) buildAgentStopComment(clawID, reason string) string {
	return s.deps.BuildAgentStopComment(clawID, reason)
}

func (s *Service) triggerActorForClaw(clawID string) integrations.TriggerActor {
	return s.deps.TriggerActorForClaw(clawID)
}

func (s *Service) ensureTaskRunForClaw(clawID string, opts TaskRunStart) (string, string, error) {
	return s.deps.EnsureTaskRunForClaw(clawID, opts)
}

func (s *Service) taskRunContextForClaw(clawID string) (tenantID, runID, attemptID string, ok bool, err error) {
	return s.deps.TaskRunContextForClaw(clawID)
}

func (s *Service) associateTaskRunPR(input TaskRunPR) error {
	return s.deps.AssociateTaskRunPR(input)
}

func (s *Service) recordTaskRunEventForClaw(clawID string, input TaskRunEvent) error {
	return s.deps.RecordTaskRunEventForClaw(clawID, input)
}

func (s *Service) recordTaskRunHumanEventForClaw(clawID, eventType, eventKey, actorLogin, targetURL string, detail map[string]any) {
	s.deps.RecordTaskRunHumanEventForClaw(clawID, eventType, eventKey, actorLogin, targetURL, detail)
}

func (s *Service) trackFactoryCreationSuccess(factoryName, issueID, clawID string) {
	s.deps.TrackFactoryCreationSuccess(factoryName, issueID, clawID)
}

func (s *Service) trackFactoryCreationFailure(factoryName, issueID, detail string) {
	s.deps.TrackFactoryCreationFailure(factoryName, issueID, detail)
}

func (s *Service) trackPROpened(factoryName, issueID, clawID, repo string, prNumber int) {
	s.deps.TrackPROpened(factoryName, issueID, clawID, repo, prNumber)
}

func (s *Service) trackPRMerged(factoryName, issueID, clawID, repo string, prNumber int) {
	s.deps.TrackPRMerged(factoryName, issueID, clawID, repo, prNumber)
}

func (s *Service) trackPRClosed(factoryName, issueID, clawID, repo string, prNumber int) {
	s.deps.TrackPRClosed(factoryName, issueID, clawID, repo, prNumber)
}

func (s *Service) executeDependencyUpdatesAction(clawID, stageID string, action pipeline.DependencyUpdatesAction) (*pipelineRunResult, error) {
	return s.deps.ExecuteDependencyUpdatesAction(clawID, stageID, action)
}

func (s *Service) resolveSecretRef(ref types.SecretRef, factory *types.FactoryConfig) (string, string, bool) {
	return s.deps.ResolveSecretRef(ref, factory)
}

func (s *Service) resolveTemplateFiles(name string) (map[string]string, error) {
	return s.deps.ResolveTemplateFiles(name)
}

// Types and helpers now owned by the sibling extracted packages; local
// aliases keep the mechanically-moved code unchanged.

type (
	workspaceIssueTracker = integrations.WorkspaceIssueTracker
	linearIssueDetails    = integrations.LinearIssueDetails
	agentFailureFeedback  = integrations.AgentFailureFeedback
	workflowCreateOptions = integrations.WorkflowCreateOptions
	aiChatMessage         = settings.AIChatMessage

	// TaskRunStart/TaskRunEvent/TaskRunPR are the analytics inputs the
	// workflow code records.
	TaskRunStart = analytics.TaskRunStart
	TaskRunEvent = analytics.TaskRunEvent
	TaskRunPR    = analytics.TaskRunPR
)

var (
	moveGitHubIssue              = integrations.MoveGitHubIssue
	moveShortcutStory            = integrations.MoveShortcutStory
	commentGitHubIssue           = integrations.CommentGitHubIssue
	commentShortcutIssue         = integrations.CommentShortcutIssue
	appendDefaultFactoryPRPolicy = integrations.AppendDefaultFactoryPRPolicy

	shortID                = checkpoints.ShortID
	loadExternalFactory    = checkpoints.LoadExternalFactory
	loadExternalFactories  = checkpoints.LoadExternalFactories
	saveExternalFactory    = checkpoints.SaveExternalFactory
	deleteExternalFactory  = checkpoints.DeleteExternalFactory
	loadExternalWorkspace  = checkpoints.LoadExternalWorkspace
	loadExternalWorkspaces = checkpoints.LoadExternalWorkspaces
)

// Task-run analytics constants (pkg/hub/analytics).
const (
	taskRunActorSystem                = analytics.TaskRunActorSystem
	taskRunEventAgentStopped          = analytics.TaskRunEventAgentStopped
	taskRunEventHumanPRComment        = analytics.TaskRunEventHumanPRComment
	taskRunEventHumanRequestedChanges = analytics.TaskRunEventHumanRequestedChanges
	taskRunEventHumanReviewComment    = analytics.TaskRunEventHumanReviewComment
	taskRunFailureAgentStopped        = analytics.TaskRunFailureAgentStopped
	taskRunInteractionTerminal        = analytics.TaskRunInteractionTerminal
	taskRunOwnerFactory               = analytics.TaskRunOwnerFactory
	taskRunPRStateOpen                = analytics.TaskRunPRStateOpen
	taskRunSourceFactory              = analytics.TaskRunSourceFactory
	taskRunSourceHub                  = analytics.TaskRunSourceHub
)

// issueTrackerHTTPClient mirrors the hub's shared 30s-timeout HTTP client
// for issue tracker API calls (agent_failure_feedback.go), as the
// integrations extraction did.
var issueTrackerHTTPClient = &http.Client{Timeout: 30 * time.Second}

// now mirrors the hub's monotonic-free clock helper (db.go).
func now() time.Time { return time.Now().UTC() }
