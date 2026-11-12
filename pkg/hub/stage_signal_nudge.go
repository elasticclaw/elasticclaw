package hub

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

func (s *Server) pipelineRejectsMessageSignal(clawID, token string) bool {
	ctx, ok := s.findPipelineContextForClaw(clawID)
	if !ok {
		return false
	}
	pl := parsePipelineForContext(ctx)
	return pl != nil && len(pl.RejectedMessageContainsTokens(token, s.getPipelineStage(clawID))) > 0
}

func (s *Server) nudgeRejectedPipelineSignals(clawID, message string) {
	ctx, ok := s.findPipelineContextForClaw(clawID)
	if !ok {
		return
	}
	pl := parsePipelineForContext(ctx)
	if pl == nil {
		return
	}
	currentStage := s.getPipelineStage(clawID)
	label := currentStage
	if stage := pl.StageByID(currentStage); stage != nil && strings.TrimSpace(stage.Label) != "" {
		label = stage.Label
	}
	if label == "" {
		label = "(not started)"
	}
	allowed := strings.Join(pl.MessageContainsTokens(currentStage), ", ")
	if allowed == "" {
		allowed = "(no message signals)"
	}
	for _, token := range pl.RejectedMessageContainsTokens(message, currentStage) {
		text := fmt.Sprintf("[hub] %s is not accepted in stage %s. This stage ends with: %s.", token, label, allowed)
		// The message ID deduplicates across concurrent turns, delivery, stage
		// revisits and hub restarts, independently of labels or allowed tokens.
		key := "pipeline-signal-rejection\x00" + clawID + "\x00" + currentStage + "\x00" + strings.ToUpper(token)
		messageID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(key)).String()
		s.injectMessageWithID(clawID, text, "hub", messageID)
	}
}
