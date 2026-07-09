package claws

// Agent-activity payload helpers, moved verbatim from pkg/hub server.go.
// NormalizeAgentActivityPayload and IsBusyAgentActivity are exported because
// the pkg/hub tests that stayed behind exercise them through aliases.

import (
	"encoding/json"
	"strings"
)

func activityContent(activity map[string]interface{}) string {
	for _, key := range []string{"error", "command", "path", "url", "detail"} {
		if value, ok := activity[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	if value, ok := activity["message"].(string); ok && strings.TrimSpace(value) != "" && !isPhaseActivityText(value) {
		return strings.TrimSpace(value)
	}
	if value, ok := activity["tool"].(string); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	for _, key := range []string{"phase", "stream"} {
		if value, ok := activity[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return "Activity"
}

// NormalizeAgentActivityPayload unmarshals a raw "agent_activity" WS payload
// into a generic activity map. It returns the wire bytes untouched (they are
// persisted verbatim in the message format column). ok is false when the
// payload is absent, null, or not a JSON object.
func NormalizeAgentActivityPayload(raw json.RawMessage) (map[string]interface{}, []byte, bool) {
	var activity map[string]interface{}
	if err := json.Unmarshal(raw, &activity); err != nil || activity == nil {
		return nil, nil, false
	}
	return activity, raw, true
}

func IsBusyAgentActivity(activity map[string]interface{}) bool {
	kind, _ := activity["kind"].(string)
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "model_started":
		return true
	case "tool":
		phase, _ := activity["phase"].(string)
		switch strings.ToLower(strings.TrimSpace(phase)) {
		case "completed", "complete", "done", "failed", "error", "cancelled", "canceled":
			return false
		default:
			return true
		}
	default:
		return false
	}
}

func isPhaseActivityText(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "running", "completed", "complete", "done", "failed", "error":
		return true
	default:
		return false
	}
}

func isUnhelpfulActivityContent(activity map[string]interface{}, content string) bool {
	if kind, _ := activity["kind"].(string); kind == "still_working" {
		return true
	}
	return strings.HasPrefix(content, "No streamed output")
}

// DetectToolLoop reports whether a finalized message shows the agent hitting
// the same tool error 3+ times (moved verbatim from pkg/hub server.go;
// exported for the pkg/hub test alias).
func DetectToolLoop(content string) bool {
	patterns := []string{
		"edit failed:", "write failed:", "read failed:",
		"exec failed:", "elevated is not available", "tool-policy",
	}
	for _, p := range patterns {
		if strings.Count(strings.ToLower(content), p) >= 3 {
			return true
		}
	}
	return false
}
