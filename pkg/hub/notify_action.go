package hub

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/notify"
	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

const notifyActionTimeout = 30 * time.Second

// executeNotifyAction sends a pipeline stage notification through a hub-level
// named notifier (notifications.notifiers in hub.yaml). Notification failures
// are deliberately advisory: they are logged and surfaced in the claw
// conversation without ever blocking the stage transition.
//
// Resolution and template rendering happen synchronously — the claw's context
// (issue, inputs, outputs) must be captured before a terminal stage tears the
// claw down — but the network send runs in its own goroutine. Stage
// transitions fire inline from the PR watcher's sequential poll loop, and the
// process-wide Slack limiter paces sends one second apart per channel, so a
// synchronous send would stall the whole tick (and drop notifications past
// the timeout) whenever many transitions hit one channel at once. MergePR and
// CloseIssue in the same on_enter make the same choice.
//
// A pipeline notification is a one-shot send: Message.Thread stays empty and
// no thread bookkeeping (slack_run_threads) is written or reused — that state
// belongs to the lifecycle notifier, whose thread roots track task runs, not
// stages. A stage that wants threading passes provider options instead
// (e.g. options: {thread_ts: ...} for Slack).
func (s *Server) executeNotifyAction(clawID string, stage pipeline.Stage, action pipeline.NotifyAction, pipelineCtx pipelineContext) {
	via := strings.TrimSpace(action.Via)
	if via == "" {
		// A blank via is intentionally not a pipeline parse error (a
		// notification typo must never disable the pipeline), so this is
		// where it surfaces.
		s.warnNotifyAction(clawID, stage.ID, fmt.Errorf("notify action has no via (the name of a notifier under notifications.notifiers)"))
		return
	}
	cfg := s.notificationsConfig()
	var nc types.NotifierConfig
	ok := false
	if cfg != nil {
		nc, ok = cfg.Notifiers[via]
	}
	if !ok {
		s.warnNotifyAction(clawID, stage.ID, fmt.Errorf("hub config has no notifier %q (notifications.notifiers)", via))
		return
	}

	notifier, err := s.notifierForPipeline(clawID, via, nc, pipelineCtx)
	if err != nil {
		s.warnNotifyAction(clawID, stage.ID, fmt.Errorf("build notifier %q: %w", via, err))
		return
	}

	data := s.notifyTemplateData(clawID, pipelineCtx)
	render := func(text string) string {
		if text == "" {
			return ""
		}
		return renderInjectWithData(clawID, text, data)
	}
	message := notify.Message{
		Text:    render(action.Text),
		Subject: render(action.Subject),
		Target:  render(action.Target),
		Options: action.Options,
	}
	s.safeGo("pipeline notify", func() {
		ctx, cancel := context.WithTimeout(context.Background(), notifyActionTimeout)
		defer cancel()
		if _, err := notifier.Send(ctx, message); err != nil {
			s.warnNotifyAction(clawID, stage.ID, fmt.Errorf("send via notifier %q: %w", via, err))
			return
		}
		log.Printf("[pipeline] notify action sent for claw %s stage %q via %q", clawID, stage.ID, via)
	})
}

// notifierForPipeline resolves the named hub notifier for a pipeline notify
// action through the shared notifierFor cache, so the action reuses the same
// instance as the lifecycle notifier (and, with it, the process-wide Slack
// pacing).
//
// Secret resolution prefers workspace-scoped secrets over hub secrets when
// the claw has a workspace, matching the workflow secret contract
// (workflow_creator.go). Because notifierFor caches per name with a key that
// digests the resolved secrets, a workspace that overrides one of the
// notifier's secrets gets its own workspace-scoped cache entry: it can never
// poison the hub-scoped entry or another workspace's, and the hub entry never
// thrashes against workspace sends. When the workspace overrides none of the
// notifier's secrets, resolution is identical to hub resolution and the
// hub-scoped instance is shared outright.
func (s *Server) notifierForPipeline(clawID, via string, nc types.NotifierConfig, pipelineCtx pipelineContext) (notify.Notifier, error) {
	workspaceName := s.notifyWorkspaceName(clawID, pipelineCtx)
	if workspaceName == "" {
		return s.notifierFor(via, nc, s.hubSecretResolver())
	}
	workspaceSecrets, err := loadWorkspaceSecrets(workspaceName)
	if err != nil {
		return nil, fmt.Errorf("load workspace %q secrets: %w", workspaceName, err)
	}
	hubSecrets := s.hubSecretResolver()
	overrides := false
	for _, settingKey := range notify.SecretSettings(nc.Type) {
		if secretName, ok := nc.Settings[settingKey].(string); ok {
			if _, ok := workspaceSecrets[secretName]; ok {
				overrides = true
				break
			}
		}
	}
	if !overrides {
		return s.notifierFor(via, nc, hubSecrets)
	}
	resolver := func(name string) (string, bool) {
		if value, ok := workspaceSecrets[name]; ok {
			return value, true
		}
		return hubSecrets(name)
	}
	return s.notifierFor(via+"\x00workspace\x00"+workspaceName, nc, resolver)
}

// notifyWorkspaceName resolves the ElasticClaw workspace whose secrets scope
// a notify action's secret resolution. Only a workflow claw has one
// (pipelineCtx.Workspace, the same WorkspaceConfig.Name workflow_creator.go
// scopes secrets by). Factory and ad-hoc claws resolve hub secrets alone:
// FactoryConfig.Workspace is the integration/tracker workspace slug
// (integrations.<type>[].workspace — see pipelineContext.TrackerName), not an
// ElasticClaw workspace name, so treating it as one would either silently
// resolve no workspace secrets at all or, worse, pick up an unrelated
// workspace's secrets whenever the two names collide.
func (s *Server) notifyWorkspaceName(clawID string, pipelineCtx pipelineContext) string {
	// Prefer the context the runner already resolved; only re-resolve from the
	// claw's tags when the caller had none.
	if pipelineCtx.Workspace == nil && pipelineCtx.Factory == nil {
		if found, ok := s.findPipelineContextForClaw(clawID); ok {
			pipelineCtx = found
		}
	}
	if pipelineCtx.Workspace != nil {
		return pipelineCtx.Workspace.Name
	}
	return ""
}

// notifyTemplateData builds the template data for a notify action's text,
// subject and target: the same {{.Issue.*}}, {{.Inputs.*}} and {{.Outputs.*}}
// variables the inject action renders.
func (s *Server) notifyTemplateData(clawID string, pipelineCtx pipelineContext) interface{} {
	data := map[string]interface{}{}
	if inputs := s.loadManualTriggerInputs(clawID); inputs != nil {
		data["Inputs"] = inputs
	}

	if pipelineCtx.IssueID == "" {
		if found, ok := s.findPipelineContextForClaw(clawID); ok {
			pipelineCtx = found
		}
	}
	if issueID := pipelineCtx.IssueID; issueID != "" {
		data["Issue"] = s.notifyIssueTemplateData(clawID, pipelineCtx, issueID)
	}
	return s.injectTemplateData(clawID, data)
}

func (s *Server) notifyIssueTemplateData(clawID string, pipelineCtx pipelineContext, issueID string) interface{} {
	if strings.Contains(issueID, "/") {
		// GitHub issue (owner/repo/number).
		details := fallbackGitHubIssueDetails(issueID)
		token := s.resolveGitHubIssuesTokenForPipeline(pipelineCtx)
		parts := strings.Split(issueID, "/")
		if token != "" && len(parts) == 3 {
			var issueNumber int
			if _, err := fmt.Sscanf(parts[2], "%d", &issueNumber); err == nil {
				base := s.githubBaseURL
				if base == "" {
					base = "https://api.github.com"
				}
				if fetched, err := s.fetchGitHubIssueDetails(token, parts[0]+"/"+parts[1], issueNumber, base); err == nil && fetched != nil {
					return fetched
				} else if err != nil {
					log.Printf("[pipeline] notify issue template lookup failed for claw %s: %v", clawID, err)
				}
			}
		}
		return details
	}

	details := &linearIssueDetails{Identifier: issueID}
	if !strings.HasPrefix(issueID, "sc-") {
		if token := s.resolveLinearTokenForPipeline(pipelineCtx); token != "" {
			if fetched, err := s.fetchLinearIssueDetails(token, issueID); err == nil && fetched != nil {
				return fetched
			} else if err != nil {
				log.Printf("[pipeline] notify issue template lookup failed for claw %s: %v", clawID, err)
			}
		}
	}
	return details
}

func (s *Server) warnNotifyAction(clawID, stageID string, err error) {
	msg := fmt.Sprintf("Notify action failed for stage %q: %v", stageID, err)
	log.Printf("[pipeline] %s", msg)
	s.injectHubMessageByID(clawID, "[hub] Warning: "+msg)
}
