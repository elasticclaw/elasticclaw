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

// executeNotifyAction sends a workflow notification. Notification failures are
// deliberately advisory: they are logged and surfaced in the claw conversation
// without blocking the rest of the stage transition.
func (s *Server) executeNotifyAction(clawID string, stage pipeline.Stage, action pipeline.NotifyAction, pipelineCtx pipelineContext) {
	workspace, err := s.workspaceForNotifyAction(clawID, pipelineCtx)
	if err != nil {
		s.warnNotifyAction(clawID, stage.ID, err)
		return
	}
	config, ok := workspace.Notifiers[action.Via]
	if !ok {
		s.warnNotifyAction(clawID, stage.ID, fmt.Errorf("workspace %q has no notifier %q", workspace.Name, action.Via))
		return
	}

	workspaceSecrets, err := loadWorkspaceSecrets(workspace.Name)
	if err != nil {
		s.warnNotifyAction(clawID, stage.ID, fmt.Errorf("load workspace secrets: %w", err))
		return
	}
	s.mu.RLock()
	hubSecrets := s.hubCfg.Secrets
	s.mu.RUnlock()
	// Workspace-scoped secrets take precedence over hub secrets, matching the
	// resolution order used when creating workflow claws (workflow_creator.go).
	resolver := func(name string) (string, bool) {
		if value, ok := workspaceSecrets[name]; ok {
			return value, true
		}
		value, ok := hubSecrets[name]
		return value, ok
	}
	n, err := notify.New(config.Type, config.Settings, resolver)
	if err != nil {
		s.warnNotifyAction(clawID, stage.ID, fmt.Errorf("create notifier %q: %w", action.Via, err))
		return
	}

	data := s.notifyTemplateData(clawID)
	message := notify.Message{
		Text:    renderInjectWithData(clawID, action.Text, data),
		Subject: renderInjectWithData(clawID, action.Subject, data),
		Target:  renderInjectWithData(clawID, action.Target, data),
		Options: action.Options,
	}
	ctx, cancel := context.WithTimeout(context.Background(), notifyActionTimeout)
	defer cancel()
	if err := n.Send(ctx, message); err != nil {
		s.warnNotifyAction(clawID, stage.ID, fmt.Errorf("send via notifier %q: %w", action.Via, err))
		return
	}
	log.Printf("[pipeline] notify action sent for claw %s stage %q via %q", clawID, stage.ID, action.Via)
}

func (s *Server) workspaceForNotifyAction(clawID string, pipelineCtx pipelineContext) (*types.WorkspaceConfig, error) {
	// Prefer the context the runner already resolved; only re-resolve from the
	// claw's tags (and, failing that, the DB) when the caller had none.
	if pipelineCtx.Workspace == nil && pipelineCtx.Factory == nil {
		if found, ok := s.findPipelineContextForClaw(clawID); ok {
			pipelineCtx = found
		}
	}
	if pipelineCtx.Workspace != nil {
		return pipelineCtx.Workspace, nil
	}
	if pipelineCtx.Factory != nil {
		return loadNotifyWorkspace(pipelineCtx.Factory)
	}

	var factoryName string
	if err := s.db.QueryRow(`SELECT COALESCE(factory_name,'') FROM claws WHERE id=?`, clawID).Scan(&factoryName); err != nil {
		return nil, fmt.Errorf("resolve workspace for claw %q: %w", clawID, err)
	}
	for _, factory := range s.resolveFactories() {
		if factory != nil && factory.Name == factoryName {
			return loadNotifyWorkspace(factory)
		}
	}
	if factoryName == "" {
		return nil, fmt.Errorf("resolve workspace for claw %q: claw has no factory", clawID)
	}
	return nil, fmt.Errorf("resolve workspace for claw %q: factory %q was not found", clawID, factoryName)
}

func loadNotifyWorkspace(factory *types.FactoryConfig) (*types.WorkspaceConfig, error) {
	workspaceName := strings.TrimSpace(factory.Workspace)
	if workspaceName == "" {
		return nil, fmt.Errorf("factory %q has no workspace configured", factory.Name)
	}
	workspace, err := loadExternalWorkspace(workspaceName)
	if err != nil {
		return nil, fmt.Errorf("load workspace %q for factory %q: %w", workspaceName, factory.Name, err)
	}
	return workspace, nil
}

func (s *Server) notifyTemplateData(clawID string) interface{} {
	data := map[string]interface{}{}
	if inputs := s.loadManualTriggerInputs(clawID); inputs != nil {
		data["Inputs"] = inputs
	}

	pipelineCtx, _ := s.findPipelineContextForClaw(clawID)
	issueID := pipelineCtx.IssueID
	if issueID != "" {
		data["Issue"] = s.notifyIssueTemplateData(clawID, pipelineCtx, issueID)
	}
	return s.injectTemplateData(clawID, data)
}

func (s *Server) notifyIssueTemplateData(clawID string, pipelineCtx pipelineContext, issueID string) interface{} {
	if strings.Contains(issueID, "/") {
		details := fallbackGitHubIssueDetails(issueID)
		token := s.resolveGitHubIssuesTokenForPipeline(pipelineCtx)
		parts := strings.Split(issueID, "/")
		if token != "" && len(parts) == 3 {
			var issueNumber int
			if _, err := fmt.Sscanf(parts[2], "%d", &issueNumber); err == nil {
				base := s.githubBaseURL
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
