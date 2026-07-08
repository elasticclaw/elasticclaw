package workflows

import (
	"net/http"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// This file exports the identifiers pkg/hub still uses across the package
// boundary after the mechanical extraction (phase-2 hub reorganization,
// item 2.2 step 6). The wrappers delegate to the unexported moved code so
// the moved files keep their pre-extraction shape; hub call sites reach
// them through pkg/hub/workflows_bridge.go. They shrink as later phase-2
// steps move the remaining callers (claws/, hub.go composition).

// CronScheduler manages scheduled workflow runs using cron expressions.
// The hub's Server holds one instance built via NewCronScheduler.
type CronScheduler = cronScheduler

// NewCronScheduler builds the scheduler bound to the given service.
func NewCronScheduler(s *Service) *CronScheduler { return newCronScheduler(s) }

// Start initializes the cron scheduler and loads all cron workflows.
func (cs *cronScheduler) Start() error { return cs.start() }

// Stop gracefully shuts down the cron scheduler.
func (cs *cronScheduler) Stop() { cs.stop() }

// Reload rescans workspaces and re-registers cron workflows.
func (cs *cronScheduler) Reload() error { return cs.reload() }

// FinishRunByClawID marks a workflow run as finished by claw ID.
func (cs *cronScheduler) FinishRunByClawID(clawID, status, result string) {
	cs.finishRunByClawID(clawID, status, result)
}

// SetCronForTest replaces the underlying cron instance without starting
// it. Used by pkg/hub tests that previously assigned the unexported field.
func (cs *cronScheduler) SetCronForTest(c *cron.Cron) { cs.cron = c }

// Entries exposes the scheduled workflow entry map. Used by pkg/hub tests
// that previously read the unexported entries field.
func (cs *cronScheduler) Entries() map[string]cron.EntryID { return cs.entries }

// PipelineRunResult is the captured output of a workflow run action.
type PipelineRunResult = pipelineRunResult

// PipelineContext is the pipeline evaluation context (workflow or factory).
type PipelineContext = pipelineContext

// ClawPR is a tracked pull request row from claw_prs.
type ClawPR = clawPR

// PRCommentOptions controls how checkPRComments treats bot comments.
type PRCommentOptions = prCommentOptions

// Package-level helpers still called from pkg/hub.

func ValidateFactoryInputs(inputs []types.FactoryInput, values map[string]interface{}) (map[string]string, error) {
	return validateFactoryInputs(inputs, values)
}

func ResolveProvider(factory *types.FactoryConfig, tmplCfg *types.TemplateConfig, hubDefault string) (string, error) {
	return resolveProvider(factory, tmplCfg, hubDefault)
}

func GithubAPIWithBase(baseURL, path, token string) (map[string]interface{}, error) {
	return githubAPIWithBase(baseURL, path, token)
}

func GithubAPIListWithBase(baseURL, path, token string) ([]interface{}, error) {
	return githubAPIListWithBase(baseURL, path, token)
}

func GithubAPIAddLabel(baseURL, repo string, issueNumber int, label, token string) error {
	return githubAPIAddLabel(baseURL, repo, issueNumber, label, token)
}

func ParsePipelineForFactory(factory *types.FactoryConfig) *pipeline.Pipeline {
	return parsePipelineForFactory(factory)
}

func ParsePipelineForContext(ctx PipelineContext) *pipeline.Pipeline {
	return parsePipelineForContext(ctx)
}

func NormalizeIssueLabels(labels []string) []string { return normalizeIssueLabels(labels) }

// IssueLabelsTemplateFile is the reserved template file that carries issue
// labels into the claw workspace.
const IssueLabelsTemplateFile = issueLabelsTemplateFile

// Service methods still called from pkg/hub (HTTP routing, WS message
// loop, webhook triggers, claw lifecycle).

func (s *Service) HandleFactoriesCRUD(w http.ResponseWriter, r *http.Request) {
	s.handleFactoriesCRUD(w, r)
}

func (s *Service) HandleFactoryTrigger(w http.ResponseWriter, r *http.Request) {
	s.handleFactoryTrigger(w, r)
}

func (s *Service) HandleCronWorkflowTrigger(w http.ResponseWriter, r *http.Request) {
	s.handleCronWorkflowTrigger(w, r)
}

func (s *Service) HandleCronWorkflowRuns(w http.ResponseWriter, r *http.Request) {
	s.handleCronWorkflowRuns(w, r)
}

func (s *Service) HandleCronWorkflowNextRun(w http.ResponseWriter, r *http.Request) {
	s.handleCronWorkflowNextRun(w, r)
}

func (s *Service) HandleClawSubresource(w http.ResponseWriter, r *http.Request) {
	s.handleClawSubresource(w, r)
}

func (s *Service) CreateClawFromFactory(factory *types.FactoryConfig, issueID string, inputs map[string]string, prebuiltTemplateFiles map[string]string, reason string) (string, bool, error) {
	return s.createClawFromFactory(factory, issueID, inputs, prebuiltTemplateFiles, reason)
}

func (s *Service) ClaimFactoryTrigger(factoryName, integration, triggerKey, source string, payload any) (bool, error) {
	return s.claimFactoryTrigger(factoryName, integration, triggerKey, source, payload)
}

func (s *Service) CompleteFactoryTrigger(factoryName, integration, triggerKey, clawID string) error {
	return s.completeFactoryTrigger(factoryName, integration, triggerKey, clawID)
}

func (s *Service) FailFactoryTrigger(factoryName, integration, triggerKey string) {
	s.failFactoryTrigger(factoryName, integration, triggerKey)
}

func (s *Service) StorePRMention(clawID, repo string, prNumber int, prURL string) {
	s.storePRMention(clawID, repo, prNumber, prURL)
}

func (s *Service) ScanMessageForPRs(clawID, content string) { s.scanMessageForPRs(clawID, content) }

func (s *Service) StartPRWatcher() { s.startPRWatcher() }

func (s *Service) PollAllPRs() { s.pollAllPRs() }

func (s *Service) ResolveGitHubToken() string { return s.resolveGitHubToken() }

func (s *Service) ResolveGitHubTokenForRepo(repo string) string {
	return s.resolveGitHubTokenForRepo(repo)
}

func (s *Service) ForwardHumanReviewComment(pr ClawPR, id int64, login, body, htmlURL, path string, line int) {
	s.forwardHumanReviewComment(pr, id, login, body, htmlURL, path, line)
}

func (s *Service) ForwardHumanRequestedChangesReview(pr ClawPR, id int64, login, body, htmlURL string) {
	s.forwardHumanRequestedChangesReview(pr, id, login, body, htmlURL)
}

func (s *Service) IsHumanGitHubActor(login, userType string) bool {
	return s.isHumanGitHubActor(login, userType)
}

func (s *Service) InjectUserMessage(clawID, content string) { s.injectUserMessage(clawID, content) }

func (s *Service) InjectHubMessageByID(clawID, content string) {
	s.injectHubMessageByID(clawID, content)
}

func (s *Service) ExecutePipelineCommand(clawID, command string, timeout time.Duration) (*PipelineRunResult, error) {
	return s.executePipelineCommand(clawID, command, timeout)
}

func (s *Service) PersistPipelineOutput(clawID, stageID, outputName string, result *PipelineRunResult) {
	s.persistPipelineOutput(clawID, stageID, outputName, result)
}

func (s *Service) TransitionPipelineStage(clawID string, stage pipeline.Stage, factory *types.FactoryConfig, issueID string) bool {
	return s.transitionPipelineStage(clawID, stage, factory, issueID)
}

func (s *Service) TransitionPipelineStageWithContext(clawID string, stage pipeline.Stage, ctx PipelineContext) bool {
	return s.transitionPipelineStageWithContext(clawID, stage, ctx)
}

func (s *Service) CheckPipelineMessageTriggers(clawID, message string) bool {
	return s.checkPipelineMessageTriggers(clawID, message)
}

func (s *Service) PipelineStageForMessageContains(clawID, message string) (PipelineContext, *pipeline.Stage, bool) {
	return s.pipelineStageForMessageContains(clawID, message)
}

func (s *Service) HasFailedRequiredGate(clawID string) bool { return s.hasFailedRequiredGate(clawID) }

func (s *Service) InitializePipelineEntryIfNeeded(clawID string) bool {
	return s.initializePipelineEntryIfNeeded(clawID)
}

func (s *Service) StopAgentWithReason(clawID, reason string, skipVMTerminate bool) {
	s.stopAgentWithReason(clawID, reason, skipVMTerminate)
}

func (s *Service) FindFactoryForClaw(clawID string) (*types.FactoryConfig, string) {
	return s.findFactoryForClaw(clawID)
}

func (s *Service) FindPipelineContextForIssue(issueID string) (PipelineContext, bool) {
	return s.findPipelineContextForIssue(issueID)
}

func (s *Service) ClawIssueAndTags(clawID string) (string, []string) {
	return s.clawIssueAndTags(clawID)
}

// Identifiers below are exported for the pkg/hub tests that stayed behind
// (they exercise hub Server internals and hand-built test servers); the
// hub bridge re-aliases them under the original unexported names.

func ParseJudgeResponse(raw string) (*JudgeResult, error) { return parseJudgeResponse(raw) }

func JudgeTimeoutForTest(timeoutStr string) time.Duration { return judgeTimeout(timeoutStr) }

func TruncateString(s string, maxLen int) string { return truncateString(s, maxLen) }

func ParsePipelineOutputJSON(stdout string) (map[string]interface{}, bool) {
	return parsePipelineOutputJSON(stdout)
}

func ValidateScriptCommand(command string) error { return validateScriptCommand(command) }

func GithubAPIDeleteLabel(baseURL, repo string, issueNumber int, label, token string) error {
	return githubAPIDeleteLabel(baseURL, repo, issueNumber, label, token)
}

func LoadWorkflowPipelineContext(workspaceName, workflowName string) (*types.WorkspaceConfig, *types.WorkflowConfig, bool) {
	return loadWorkflowPipelineContext(workspaceName, workflowName)
}

func TriggerPayloadJSON(payload any) string { return triggerPayloadJSON(payload) }

func BuildManualTriggerContext(factory *types.FactoryConfig, inputs map[string]string) string {
	return buildManualTriggerContext(factory, inputs)
}

func (s *Service) CommentWorkflowAgentStopToTracker(clawID string, ctx PipelineContext, reason string) {
	s.commentWorkflowAgentStopToTracker(clawID, ctx, reason)
}

func (s *Service) EvaluateGate(clawID, stageID string, gate *pipeline.Gate) *GateEvaluationResult {
	return s.evaluateGate(clawID, stageID, gate)
}

func (s *Service) LoadGateResult(clawID, stageID string) *GateEvaluationResult {
	return s.loadGateResult(clawID, stageID)
}

func (s *Service) LoadPipelineOutputs(clawID string) map[string]map[string]interface{} {
	return s.loadPipelineOutputs(clawID)
}

func (s *Service) RunOnEnter(clawID string, stage pipeline.Stage, ctx PipelineContext) bool {
	return s.runOnEnter(clawID, stage, ctx)
}

func (s *Service) LoadIssueLabelsForClaw(clawID string) ([]string, bool) {
	return s.loadIssueLabelsForClaw(clawID)
}

func (s *Service) InjectTemplateData(clawID string, baseData interface{}) interface{} {
	return s.injectTemplateData(clawID, baseData)
}

func (s *Service) AutoTransitionAfterJudge(clawID, verdict string, ctx PipelineContext) {
	s.autoTransitionAfterJudge(clawID, verdict, ctx)
}

func (s *Service) AutoTransitionAfterGate(clawID, stageID, verdict string, ctx PipelineContext) {
	s.autoTransitionAfterGate(clawID, stageID, verdict, ctx)
}

func (s *Service) CheckPRComments(pr ClawPR, commentsData []interface{}, opts PRCommentOptions) {
	s.checkPRComments(pr, commentsData, opts)
}

func (s *Service) CheckPRReviews(pr ClawPR, reviewsData []interface{}) {
	s.checkPRReviews(pr, reviewsData)
}

func (s *Service) CheckGreptileReviewComments(pr ClawPR, reviewCommentsData []interface{}) {
	s.checkGreptileReviewComments(pr, reviewCommentsData)
}

func (s *Service) ResolveGitHubIssuesTokenForPipeline(ctx PipelineContext) string {
	return s.resolveGitHubIssuesTokenForPipeline(ctx)
}
