package hub

import (
	"encoding/json"
	"log"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// parsePipelineForFactory parses the PipelineYAML from a factory config.
// Returns nil (and logs a warning) if the YAML is empty or invalid.
func parsePipelineForFactory(factory *types.FactoryConfig) *pipeline.Pipeline {
	if factory == nil || factory.PipelineYAML == "" {
		return nil
	}
	p, err := pipeline.Parse([]byte(factory.PipelineYAML))
	if err != nil {
		log.Printf("[pipeline] factory %q: failed to parse pipeline_yaml: %v", factory.Name, err)
		return nil
	}
	return p
}

// runOnEnter executes the on_enter actions for a given stage.
//
// - stage.OnEnter.Inject: injects a user message into the claw
// - stage.OnEnter.MoveIssue: moves the Linear/Shortcut issue to the named status
//
// factory and issueID are required for MoveIssue; if either is nil/empty the
// move is skipped silently.
func (s *Server) runOnEnter(clawID string, stage pipeline.Stage, factory *types.FactoryConfig, issueID string) {
	if stage.OnEnter.Inject != "" {
		s.injectHubMessageByID(clawID, strings.TrimRight(stage.OnEnter.Inject, "\n"))
	}

	if stage.OnEnter.MergePR {
		go s.mergePRForClaw(clawID)
	}

	if stage.OnEnter.MoveIssue == "" || factory == nil || issueID == "" {
		return
	}

	targetStatus := stage.OnEnter.MoveIssue

	if strings.HasPrefix(issueID, "sc-") {
		// Shortcut story
		scToken := s.resolveShortcutToken(factory.Workspace)
		if scToken == "" {
			log.Printf("[pipeline] factory %q: no Shortcut token for workspace %q, skipping move_issue", factory.Name, factory.Workspace)
			return
		}
		if err := moveShortcutStory(scToken, issueID, targetStatus); err != nil {
			log.Printf("[pipeline] failed to move story %s to %q: %v", issueID, targetStatus, err)
		} else {
			log.Printf("[pipeline] moved story %s to %q", issueID, targetStatus)
		}
	} else {
		// Linear issue
		linearToken := s.resolveLinearTokenForFactory(factory)
		if linearToken == "" {
			log.Printf("[pipeline] factory %q: no Linear token for workspace %q, skipping move_issue", factory.Name, factory.Workspace)
			return
		}
		if err := s.moveLinearIssueOnServer(linearToken, issueID, targetStatus); err != nil {
			log.Printf("[pipeline] failed to move issue %s to %q: %v", issueID, targetStatus, err)
		} else {
			log.Printf("[pipeline] moved issue %s to %q", issueID, targetStatus)
		}
	}
}

// transitionPipelineStage sets the claw's current pipeline stage and runs on_enter.
func (s *Server) transitionPipelineStage(clawID string, stage pipeline.Stage, factory *types.FactoryConfig, issueID string) {
	s.setPipelineStage(clawID, stage.ID)
	log.Printf("[pipeline] claw %s → stage %q (%s)", clawID[:8], stage.ID, stage.Label)
	s.runOnEnter(clawID, stage, factory, issueID)
}

// initializePipelineEntryIfNeeded transitions a factory claw into its entry stage
// exactly once, after the claw is connected and ready.
// Returns true when entry on_enter inject should be used as the initial wake-up.
func (s *Server) initializePipelineEntryIfNeeded(clawID string) bool {
	// Entry runs only once; if a stage is already set we are done.
	if s.getPipelineStage(clawID) != "" {
		return false
	}

	factory, issueID := s.findFactoryForClaw(clawID)
	log.Printf("[pipeline] initializePipelineEntryIfNeeded: claw=%s factory=%v issueID=%q", clawID[:8], factory != nil, issueID)
	if factory == nil {
		return false
	}
	pl := parsePipelineForFactory(factory)
	if pl == nil {
		return false
	}
	entry := pl.EntryStage()
	if entry == nil {
		return false
	}

	s.transitionPipelineStage(clawID, *entry, factory, issueID)
	return strings.TrimSpace(entry.OnEnter.Inject) != ""
}

// findFactoryForClaw looks up the factory that created a claw by its claw ID.
// It uses the factory:<name> tag stored on the claw to identify the factory.
func (s *Server) findFactoryForClaw(clawID string) (*types.FactoryConfig, string) {
	var issueID, tagsJSON string
	if err := s.db.QueryRow(`SELECT COALESCE(linear_issue_id,''), COALESCE(tags,'[]') FROM claws WHERE id=?`, clawID).Scan(&issueID, &tagsJSON); err != nil {
		return nil, ""
	}

	if issueID != "" {
		if factory := s.findFactoryForIssue(issueID); factory != nil {
			return factory, issueID
		}
	}

	var tags []string
	if err := json.Unmarshal([]byte(tagsJSON), &tags); err != nil {
		return nil, issueID
	}
	s.mu.RLock()
	factories := s.hubCfg.Factories
	s.mu.RUnlock()
	for _, tag := range tags {
		if !strings.HasPrefix(tag, "factory:") {
			continue
		}
		factoryName := strings.TrimPrefix(tag, "factory:")
		for _, factory := range factories {
			if factory.Name == factoryName {
				return factory, issueID
			}
		}
	}
	return nil, issueID
}
