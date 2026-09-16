package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSpawnReceiptIsSchedulingMetadata(t *testing.T) {
	for _, result := range []string{
		`{"details":{"status":"accepted","childSessionKey":"agent:main:subagent:child","runId":"child-run","modelApplied":true}}`,
		`{"content":[{"type":"text","text":"{\"status\":\"accepted\",\"childSessionKey\":\"agent:main:subagent:child\",\"runId\":\"child-run\",\"modelApplied\":true}"}]}`,
	} {
		var data map[string]interface{}
		if err := json.Unmarshal([]byte(`{"result":`+result+`}`), &data); err != nil {
			t.Fatal(err)
		}
		a := agentActivity{Tool: "sessions_spawn", Phase: "result", SubagentModel: "openai/requested"}
		enrichSpawnActivity(&a, data, "parent-session", "parent-run")
		if a.SubagentSpawnStatus != "accepted" || a.SubagentChildSession != "agent:main:subagent:child" || a.SubagentChildRun != "child-run" {
			t.Fatalf("missing receipt metadata: %+v", a)
		}
		if a.SubagentResolvedModel != "" || a.SubagentModel != "" {
			t.Fatalf("invented resolved model from modelApplied: %+v", a)
		}
		if a.SubagentParentSession != "parent-session" || a.SubagentParentRun != "parent-run" {
			t.Fatalf("missing parent correlation: %+v", a)
		}
	}
}

func TestSpawnRequestedAndResolvedModels(t *testing.T) {
	name, _, model, prompt := resolveSubagentFields("sessions_spawn", "start", map[string]interface{}{"input": map[string]interface{}{"label": "Inspect API", "task": "Check request handling", "model": "openai/gpt-worker"}})
	a := agentActivity{Tool: "sessions_spawn", Phase: "start", SubagentModel: model}
	enrichSpawnActivity(&a, nil, "parent", "run")
	if name != "Inspect API" || prompt != "Check request handling" || a.SubagentRequestedModel != "openai/gpt-worker" || a.SubagentRequestedProvider != "openai" {
		t.Fatalf("incorrect request metadata: %+v %s %s", a, name, prompt)
	}
	a.Phase = "completed"
	enrichSpawnActivity(&a, map[string]interface{}{"result": map[string]interface{}{"details": map[string]interface{}{"status": "accepted", "resolvedModel": "provider/actual", "modelProvider": "provider"}}}, "parent", "run")
	if a.SubagentResolvedModel != "provider/actual" || a.SubagentResolvedProvider != "provider" {
		t.Fatalf("missing resolved metadata: %+v", a)
	}
}

func TestSpawnRejectedAndMalformedReceipt(t *testing.T) {
	for _, tc := range []struct{ raw, status string }{
		{`{"result":{"details":{"status":"forbidden","error":"Model unavailable"}}}`, "failed"},
		{`{"result":{"content":[{"type":"text","text":"not JSON"}]}}`, "unknown"},
		{`{"isError":true,"result":{"details":{"status":"accepted"}}}`, "failed"},
		{`{"input":{"status":"accepted","childSessionKey":"not-a-result"}}`, "unknown"},
	} {
		var data map[string]interface{}
		if err := json.Unmarshal([]byte(tc.raw), &data); err != nil {
			t.Fatal(err)
		}
		a := agentActivity{Tool: "sessions_spawn", Phase: "result"}
		enrichSpawnActivity(&a, data, "parent", "run")
		if a.SubagentSpawnStatus != tc.status {
			t.Fatalf("status=%s, want %s", a.SubagentSpawnStatus, tc.status)
		}
		if tc.status == "failed" && a.Error == "" {
			t.Fatal("rejection has no error")
		}
	}
}

func TestSpawnStartAndReceiptShareCallIdentity(t *testing.T) {
	inf := &inFlightState{}
	start := agentActivity{Kind: "tool", Stream: "tool", Tool: "sessions_spawn", Phase: "start"}
	end := agentActivity{Kind: "tool", Stream: "tool", Tool: "sessions_spawn", Phase: "result"}
	at := time.Now()
	inf.resolveToolCall(&start, at)
	enrichSpawnActivity(&end, map[string]interface{}{"result": map[string]interface{}{"status": "accepted"}}, "parent", "run")
	inf.resolveToolCall(&end, at.Add(time.Second))
	if start.CallID == "" || start.CallID != end.CallID {
		t.Fatalf("spawn split into calls: %q %q", start.CallID, end.CallID)
	}
}
