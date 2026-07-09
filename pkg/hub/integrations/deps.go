package integrations

import (
	"context"
	"database/sql"
	"net/http"
	"sync"
	"time"

	"nhooyr.io/websocket"

	"github.com/elasticclaw/elasticclaw/pkg/hub/analytics"
	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// Deps carries the hub-owned state and helpers the integrations service
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
	// PromoteMu serializes PromotePendingClaws (TOCTOU protection).
	PromoteMu *sync.Mutex
	// WebhookDedupMu guards the webhook dedup map; WebhookDedup returns
	// the (hub-owned) map and must be called with WebhookDedupMu held.
	WebhookDedupMu *sync.Mutex
	WebhookDedup   func() map[string]time.Time

	// CloseClawConn closes and removes the WS connection for a claw, if
	// connected. It takes the hub's claw-registry lock itself.
	CloseClawConn func(clawID string, code websocket.StatusCode, reason string)
	// ClawGatewayReady reports whether the claw is connected and its
	// gateway finished bootstrapping. Takes the registry lock itself.
	ClawGatewayReady func(clawID string) bool

	// Base URL overrides for tests (default to the public APIs).
	GithubBaseURL   func() string
	LinearBaseURL   func() string
	ShortcutBaseURL func() string
	JiraBaseURL     func() string
	// GHBaseURL is the hub's resolved GitHub API base helper.
	GHBaseURL func() string

	// CronFinishRunByClawID marks a cron-scheduled workflow run finished.
	// nil when no cron scheduler is configured.
	CronFinishRunByClawID func(clawID, status, detail string)

	// Claw lifecycle and messaging (owned by pkg/hub until the claws/
	// extraction).
	BroadcastToUsers            func(tenantID string, msg types.WSMessage)
	InjectHubMessageByID        func(clawID, content string)
	InjectUserMessage           func(clawID, content string)
	StopAgentWithReason         func(clawID, reason string, skipVMTerminate bool)
	CheckpointBeforeTermination func(clawID, reason string)
	TerminateVM                 func(provider, vmID string)
	SyncWorkflowVolumes         func(clawID string)
	ClawHubURL                  func() string
	ProvisionDaytona            func(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, files map[string][]byte, env map[string]string) error
	ProvisionExedev             func(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, files map[string][]byte, env map[string]string) error
	ProvisionLambdaMicroVMs     func(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, files map[string][]byte) error
	ProvisionReplicated         func(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, env map[string]string) error

	// Factory/workflow claw creation.
	CreateClawFromFactory  func(factory *types.FactoryConfig, issueID string, inputs map[string]string, prebuiltTemplateFiles map[string]string, reason string) (string, bool, error)
	CreateClawFromWorkflow func(workspace *types.WorkspaceConfig, workflow *types.WorkflowConfig, opts WorkflowCreateOptions) (string, bool, error)
	ResolveFactories       func() []*types.FactoryConfig
	ResolveTemplateFiles   func(name string) (map[string]string, error)
	ResolveProvider        func(factory *types.FactoryConfig, tmplCfg *types.TemplateConfig, hubDefault string) (string, error)

	// Factory trigger claim table (factory_triggers.go).
	ClaimFactoryTrigger    func(factoryName, integration, triggerKey, source string, payload any) (bool, error)
	CompleteFactoryTrigger func(factoryName, integration, triggerKey, clawID string) error
	FailFactoryTrigger     func(factoryName, integration, triggerKey string)

	// Factory analytics/event log.
	TrackFactoryCreationSuccess func(factoryName, issueID, clawID string)
	TrackFactoryCreationFailure func(factoryName, issueID, detail string)
	TrackFactoryTermination     func(factoryName, issueID, clawID, reason string)
	TrackDoneSignal             func(factoryName, issueID, clawID string, prCount int)
	RecordTaskRunEventForClaw   func(clawID string, input analytics.TaskRunEvent) error
	TaskRunRequiresPRForClaw    func(clawID string) bool

	// Pipeline engine (pipeline_runner.go).
	FindPipelineContextForIssue        func(issueID string) (pipeline.Context, bool)
	PipelineStageForMessageContains    func(clawID, message string) (pipeline.Context, *pipeline.Stage, bool)
	TransitionPipelineStage            func(clawID string, stage pipeline.Stage, factory *types.FactoryConfig, issueID string) bool
	TransitionPipelineStageWithContext func(clawID string, stage pipeline.Stage, ctx pipeline.Context) bool
	ParsePipelineForContext            func(ctx pipeline.Context) *pipeline.Pipeline
	ParsePipelineForFactory            func(factory *types.FactoryConfig) *pipeline.Pipeline
	HasFailedRequiredGate              func(clawID string) bool

	// PR watcher helpers (pr_watcher.go).
	StorePRMention                     func(clawID, repo string, prNumber int, prURL string)
	ForwardHumanReviewComment          func(clawID, repo string, prNumber int, prURL string, id int64, login, body, htmlURL, path string, line int)
	ForwardHumanRequestedChangesReview func(clawID, repo string, prNumber int, prURL string, id int64, login, body, htmlURL string)
	IsHumanGitHubActor                 func(login, userType string) bool
	GithubAPIWithBase                  func(baseURL, path, token string) (map[string]interface{}, error)
	GithubAPIListWithBase              func(baseURL, path, token string) ([]interface{}, error)

	// Agent failure feedback (agent_failure_feedback.go / failure_summary.go).
	HandleAgentFailureFeedback func(feedback AgentFailureFeedback, token string)
	ClassifyAgentFailure       func(reason string) AgentFailureMessage

	// Tokens and secrets.
	ResolveGitHubToken        func() string
	ResolveGitHubTokenForRepo func(repo string) string

	// Workspace-managed config (workspace_managed.go, external_storage.go).
	FindWorkspaceIssueTracker           func(workspace, trackerType, name string) (WorkspaceIssueTracker, bool)
	WorkspaceIssueTrackerWebhookSecrets func(workspace, trackerType string) []string
	LoadExternalWorkflowsByIntegration  func(integration string) ([]*types.WorkspaceConfig, error)
	FilterWorkflowWorkspacesByName      func(workspaces []*types.WorkspaceConfig, workspaceName string) []*types.WorkspaceConfig

	// Misc helpers still owned by pkg/hub.
	InjectFigmaAPIDocs        func(files map[string]string, env map[string]string) map[string]string
	MergeTags                 func(templateName string, configTags []string, cliTags []string) []string
	CloneStringMap            func(values map[string]string) map[string]string
	ResolveDefaultModelForKey func(hubCfg *types.HubConfig, key *types.LLMKeyConfig) string
}

// Convenience accessors so the mechanically-moved code keeps its original
// shape (s.deps.X() everywhere would obscure the diff).

func (s *Service) hubCfg() *types.HubConfig { return s.deps.HubCfg() }

// Forwarders to hub-owned methods, keeping the pre-extraction call sites
// unchanged. They disappear as their targets move into their own
// subpackages (claws/, workflows/, checkpoints/) in later phase-2 steps.

func (s *Service) broadcastToUsers(tenantID string, msg types.WSMessage) {
	s.deps.BroadcastToUsers(tenantID, msg)
}

func (s *Service) injectHubMessageByID(clawID, content string) {
	s.deps.InjectHubMessageByID(clawID, content)
}

func (s *Service) injectUserMessage(clawID, content string) {
	s.deps.InjectUserMessage(clawID, content)
}

func (s *Service) stopAgentWithReason(clawID, reason string, skipVMTerminate bool) {
	s.deps.StopAgentWithReason(clawID, reason, skipVMTerminate)
}

func (s *Service) checkpointBeforeTermination(clawID, reason string) {
	s.deps.CheckpointBeforeTermination(clawID, reason)
}

func (s *Service) terminateVM(provider, vmID string) { s.deps.TerminateVM(provider, vmID) }

func (s *Service) syncWorkflowVolumes(clawID string) { s.deps.SyncWorkflowVolumes(clawID) }

func (s *Service) clawHubURL() string { return s.deps.ClawHubURL() }

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

func (s *Service) createClawFromFactory(factory *types.FactoryConfig, issueID string, inputs map[string]string, prebuiltTemplateFiles map[string]string, reason string) (string, bool, error) {
	return s.deps.CreateClawFromFactory(factory, issueID, inputs, prebuiltTemplateFiles, reason)
}

func (s *Service) createClawFromWorkflowWithOptions(workspace *types.WorkspaceConfig, workflow *types.WorkflowConfig, opts WorkflowCreateOptions) (string, bool, error) {
	return s.deps.CreateClawFromWorkflow(workspace, workflow, opts)
}

func (s *Service) resolveFactories() []*types.FactoryConfig { return s.deps.ResolveFactories() }

func (s *Service) resolveTemplateFiles(name string) (map[string]string, error) {
	return s.deps.ResolveTemplateFiles(name)
}

func (s *Service) claimFactoryTrigger(factoryName, integration, triggerKey, source string, payload any) (bool, error) {
	return s.deps.ClaimFactoryTrigger(factoryName, integration, triggerKey, source, payload)
}

func (s *Service) completeFactoryTrigger(factoryName, integration, triggerKey, clawID string) error {
	return s.deps.CompleteFactoryTrigger(factoryName, integration, triggerKey, clawID)
}

func (s *Service) failFactoryTrigger(factoryName, integration, triggerKey string) {
	s.deps.FailFactoryTrigger(factoryName, integration, triggerKey)
}

func (s *Service) trackFactoryCreationSuccess(factoryName, issueID, clawID string) {
	s.deps.TrackFactoryCreationSuccess(factoryName, issueID, clawID)
}

func (s *Service) trackFactoryCreationFailure(factoryName, issueID, detail string) {
	s.deps.TrackFactoryCreationFailure(factoryName, issueID, detail)
}

func (s *Service) trackFactoryTermination(factoryName, issueID, clawID, reason string) {
	s.deps.TrackFactoryTermination(factoryName, issueID, clawID, reason)
}

func (s *Service) trackDoneSignal(factoryName, issueID, clawID string, prCount int) {
	s.deps.TrackDoneSignal(factoryName, issueID, clawID, prCount)
}

func (s *Service) recordTaskRunEventForClaw(clawID string, input analytics.TaskRunEvent) error {
	return s.deps.RecordTaskRunEventForClaw(clawID, input)
}

func (s *Service) taskRunRequiresPRForClaw(clawID string) bool {
	return s.deps.TaskRunRequiresPRForClaw(clawID)
}

func (s *Service) findPipelineContextForIssue(issueID string) (pipeline.Context, bool) {
	return s.deps.FindPipelineContextForIssue(issueID)
}

func (s *Service) pipelineStageForMessageContains(clawID, message string) (pipeline.Context, *pipeline.Stage, bool) {
	return s.deps.PipelineStageForMessageContains(clawID, message)
}

func (s *Service) transitionPipelineStage(clawID string, stage pipeline.Stage, factory *types.FactoryConfig, issueID string) bool {
	return s.deps.TransitionPipelineStage(clawID, stage, factory, issueID)
}

func (s *Service) transitionPipelineStageWithContext(clawID string, stage pipeline.Stage, ctx pipeline.Context) bool {
	return s.deps.TransitionPipelineStageWithContext(clawID, stage, ctx)
}

func (s *Service) hasFailedRequiredGate(clawID string) bool {
	return s.deps.HasFailedRequiredGate(clawID)
}

func (s *Service) storePRMention(clawID, repo string, prNumber int, prURL string) {
	s.deps.StorePRMention(clawID, repo, prNumber, prURL)
}

func (s *Service) isHumanGitHubActor(login, userType string) bool {
	return s.deps.IsHumanGitHubActor(login, userType)
}

func (s *Service) handleAgentFailureFeedback(feedback AgentFailureFeedback, token string) {
	s.deps.HandleAgentFailureFeedback(feedback, token)
}

func (s *Service) resolveGitHubToken() string { return s.deps.ResolveGitHubToken() }

func (s *Service) resolveGitHubTokenForRepo(repo string) string {
	return s.deps.ResolveGitHubTokenForRepo(repo)
}

func (s *Service) ghBaseURL() string { return s.deps.GHBaseURL() }

// now mirrors the hub's monotonic-free clock helper (db.go).
func now() time.Time { return time.Now().UTC() }

// firstNonEmpty mirrors the shared helper kept unexported on both sides of
// the analytics extraction.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// issueTrackerHTTPClient mirrors the hub's shared client for tracker API
// calls (30s timeout).
var issueTrackerHTTPClient = &http.Client{Timeout: 30 * time.Second}
