package main

import (
	"encoding/json"
	"strings"
)

// Spawn receipts describe scheduling, not completion of the child run. Only
// explicit result metadata may identify the resolved model; modelApplied alone
// does not establish which model the child actually used.
func enrichSpawnActivity(a *agentActivity, data map[string]interface{}, parentSession, parentRun string) {
	if !strings.EqualFold(strings.TrimSpace(a.Tool), "sessions_spawn") {
		return
	}
	a.SubagentParentSession = parentSession
	a.SubagentParentRun = parentRun
	if !isToolTerminalPhase(a.Phase) {
		a.SubagentRequestedModel = a.SubagentModel
		a.SubagentRequestedProvider = modelProvider(a.SubagentRequestedModel)
		return
	}
	// Generic input extraction also searches metadata. Never label terminal
	// model metadata as a request or copy it into the legacy requested field.
	a.SubagentModel = ""
	result := spawnResultObject(data["result"])
	if result == nil {
		result = spawnResultObject(data["output"])
	}
	a.SubagentSpawnStatus = "unknown"
	if result != nil {
		a.SubagentChildSession = stringField(result, "childSessionKey", "child_session_key")
		a.SubagentChildRun = stringField(result, "runId", "childRunId", "child_run_id")
		a.SubagentResolvedModel = stringField(result, "resolvedModel", "model")
		a.SubagentResolvedProvider = firstNonEmpty(stringField(result, "resolvedProvider", "modelProvider", "provider"), modelProvider(a.SubagentResolvedModel))
		switch strings.ToLower(stringField(result, "status")) {
		case "accepted":
			a.SubagentSpawnStatus = "accepted"
		case "error", "failed", "forbidden", "denied":
			a.SubagentSpawnStatus = "failed"
			a.Error = firstNonEmpty(a.Error, stringField(result, "error", "message"), "Subagent launch failed")
		}
		if a.Result == "" {
			if encoded, err := json.Marshal(result); err == nil {
				a.Result = string(encoded)
			}
		}
	}
	failed, _ := data["isError"].(bool)
	if failed {
		a.Error = firstNonEmpty(a.Error, "Subagent launch failed")
	}
	if failed || strings.EqualFold(a.Phase, "failed") || strings.EqualFold(a.Phase, "error") || a.Error != "" {
		a.SubagentSpawnStatus = "failed"
	}
}

func modelProvider(model string) string {
	if provider, _, ok := strings.Cut(model, "/"); ok {
		return provider
	}
	return ""
}

func stringField(data map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value, ok := data[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

// OpenClaw tool results expose structured details, or JSON in a text content
// block. Limit traversal to these result envelopes, never tool arguments.
func spawnResultObject(value interface{}) map[string]interface{} {
	switch result := value.(type) {
	case map[string]interface{}:
		if details, ok := result["details"].(map[string]interface{}); ok {
			return details
		}
		if _, ok := result["status"].(string); ok {
			return result
		}
		if content, ok := result["content"].([]interface{}); ok {
			for _, block := range content {
				if item, ok := block.(map[string]interface{}); ok && item["type"] == "text" {
					if parsed := spawnResultObject(item["text"]); parsed != nil {
						return parsed
					}
				}
			}
		}
	case string:
		var parsed map[string]interface{}
		if json.Unmarshal([]byte(result), &parsed) == nil {
			return parsed
		}
	}
	return nil
}
