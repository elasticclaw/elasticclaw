// Wake messages and the initial-plan protocol for newly connected claws.
//
// Split out of the former server.go; same package, no behavior changes.
package hub

import (
	"context"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
	"nhooyr.io/websocket/wsjson"
)

const (
	wakeMessageMarker               = "__WAKE_MESSAGE__"
	initialPlanRequiredMarker       = "__INITIAL_PLAN_REQUIRED__"
	initialPlanAcceptedMarker       = "__INITIAL_PLAN_ACCEPTED__"
	initialPlanCorrectionSentMarker = "__INITIAL_PLAN_CORRECTION_SENT__"
	defaultWakeContent              = "Introduce yourself briefly and let the user know you're ready to help."
	initialPlanWakeContent          = `Initial plan required before implementation.

Before editing files, running builds, or doing broad tool exploration, send one visible assistant message that contains:
1. Your understanding of the issue or task.
2. The likely area of the codebase or behavior involved.
3. A rough implementation plan.
4. What you will verify or test.

This first message must be a normal assistant message visible to the user. Tool calls, activity rows, and update_plan do not count. After that visible plan, wait for the hub's proceed message, then start implementation and continue sending visible progress updates.`
	initialPlanProceedContent    = `[hub] Initial plan received. Proceed with implementation. Keep sending visible progress updates before and after substantial work; tool calls and activity rows do not count as user communication.`
	initialPlanCorrectionContent = `[hub] Initial plan is required before implementation. Pause tool work and send a visible assistant message with your understanding of the issue, likely code area, rough plan, and verification approach.`
)

// sendWakeMessage sends a silent system message to wake the agent.
// For factory claws, it sends a task-specific prompt.
// A marker is stored in DB so reconnects after hub restart don't re-introduce.
func (s *Server) sendWakeMessage(cc *clawConn, clawID string) {
	wakeContent := defaultWakeContent
	if s.clawNeedsInitialPlan(clawID) {
		wakeContent = initialPlanWakeContent
		_ = s.insertSystemMarker(clawID, cc.TenantID, initialPlanRequiredMarker)
	}
	wakeMsg := types.HubMessage{
		ID:        uuid.New().String(),
		ClawID:    clawID,
		TenantID:  cc.TenantID,
		Role:      "system",
		Content:   wakeMessageMarker,
		CreatedAt: now(),
	}
	_ = s.st().Messages().Insert(context.Background(), wakeMsg)
	wakeMsg.Content = wakeContent
	_ = wsjson.Write(context.Background(), cc.WS, types.WSMessage{Type: "message", Payload: wakeMsg})

	// Note: We don't call sendNextQueuedMessage here because sendWakeMessage is launched
	// with 'go' (asynchronously). The normal end-of-turn path in handleClawWS read loop
	// will drain the queue once the claw finishes the wake response. This prevents race
	// conditions where both goroutines try to dequeue messages concurrently.
}

func (s *Server) sendInitialPlanInstruction(cc *clawConn, clawID string) {
	if cc == nil || !s.clawNeedsInitialPlan(clawID) || s.hasSystemMarker(clawID, initialPlanAcceptedMarker) {
		return
	}
	if !s.insertSystemMarker(clawID, cc.TenantID, initialPlanRequiredMarker) {
		return
	}
	msg := types.HubMessage{
		ID:        uuid.New().String(),
		ClawID:    clawID,
		TenantID:  cc.TenantID,
		Role:      "system",
		Content:   initialPlanWakeContent,
		CreatedAt: now(),
	}
	_ = wsjson.Write(context.Background(), cc.WS, types.WSMessage{Type: "message", Payload: msg})
}

func (s *Server) clawNeedsInitialPlan(clawID string) bool {
	issueID, tags := s.clawIssueAndTags(clawID)
	if issueID != "" {
		return true
	}
	for _, tag := range tags {
		if strings.HasPrefix(tag, "factory:") || strings.HasPrefix(tag, "workflow:") {
			return true
		}
	}
	return false
}

func (s *Server) tenantIDForClaw(clawID string) string {
	return s.st().Claws().TenantID(context.Background(), clawID)
}

func (s *Server) hasSystemMarker(clawID, marker string) bool {
	has, _ := s.st().Messages().HasSystemMarker(context.Background(), clawID, marker)
	return has
}

func (s *Server) insertSystemMarker(clawID, tenantID, marker string) bool {
	if clawID == "" || tenantID == "" || marker == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hasSystemMarker(clawID, marker) {
		return false
	}
	err := s.st().Messages().Insert(context.Background(), types.HubMessage{
		ID: uuid.New().String(), ClawID: clawID, TenantID: tenantID,
		Role: "system", Content: marker, CreatedAt: now(),
	})
	return err == nil
}

func (s *Server) handleInitialPlanResponse(clawID, tenantID, content string) {
	if !s.hasSystemMarker(clawID, initialPlanRequiredMarker) || s.hasSystemMarker(clawID, initialPlanAcceptedMarker) {
		return
	}
	if isValidInitialPlan(content) {
		_ = s.insertSystemMarker(clawID, tenantID, initialPlanAcceptedMarker)
		s.injectHubMessageByID(clawID, initialPlanProceedContent)
		return
	}
	if !s.hasSystemMarker(clawID, initialPlanCorrectionSentMarker) {
		_ = s.insertSystemMarker(clawID, tenantID, initialPlanCorrectionSentMarker)
		s.injectHubMessageByID(clawID, initialPlanCorrectionContent)
	}
}

func (s *Server) handleInitialPlanActivity(clawID, tenantID string, activity map[string]interface{}) {
	if !s.hasSystemMarker(clawID, initialPlanRequiredMarker) ||
		s.hasSystemMarker(clawID, initialPlanAcceptedMarker) ||
		s.hasSystemMarker(clawID, initialPlanCorrectionSentMarker) {
		return
	}
	kind, _ := activity["kind"].(string)
	if kind != "tool" {
		return
	}
	_ = s.insertSystemMarker(clawID, tenantID, initialPlanCorrectionSentMarker)
	s.injectHubMessageByID(clawID, initialPlanCorrectionContent)
}

func isValidInitialPlan(content string) bool {
	content = strings.TrimSpace(content)
	if len(content) < 120 || len(strings.Fields(content)) < 35 {
		return false
	}
	lower := strings.ToLower(content)
	hasUnderstanding := strings.Contains(lower, "understand") ||
		strings.Contains(lower, "issue") ||
		strings.Contains(lower, "task") ||
		strings.Contains(lower, "problem")
	hasPlan := strings.Contains(lower, "plan") ||
		strings.Contains(lower, "step") ||
		strings.Contains(lower, "approach")
	hasVerification := strings.Contains(lower, "test") ||
		strings.Contains(lower, "verify") ||
		strings.Contains(lower, "check") ||
		strings.Contains(lower, "build")
	hasCodeArea := strings.Contains(lower, "file") ||
		strings.Contains(lower, "code") ||
		strings.Contains(lower, "package") ||
		strings.Contains(lower, "component") ||
		strings.Contains(lower, "backend") ||
		strings.Contains(lower, "frontend")
	return hasUnderstanding && hasPlan && hasVerification && hasCodeArea
}

// clawHasMessages returns true if the claw already has message history.
// Used to suppress the intro wake message on reconnect.
func (s *Server) clawHasMessages(clawID string) bool {
	count, _ := s.st().Messages().CountByClaw(context.Background(), clawID)
	return count > 0
}
