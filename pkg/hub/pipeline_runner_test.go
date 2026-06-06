package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestTransitionPipelineStageSkipsDuplicateCurrentStage(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-duplicate-stage"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "MAR-56", "elasticclaw", "connected", "pr_opened",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	stage := pipeline.Stage{
		ID:    "pr_opened",
		Label: "PR Opened",
		OnEnter: pipeline.OnEnter{
			Inject: "PR created. Watch for CI results and review comments.",
		},
	}
	if s.transitionPipelineStageWithContext(clawID, stage, pipelineContext{}) {
		t.Fatalf("duplicate stage transition returned true")
	}

	var messageCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=?`, clawID).Scan(&messageCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if messageCount != 0 {
		t.Fatalf("duplicate stage transition injected %d messages, want 0", messageCount)
	}
	if got := s.getPipelineStage(clawID); got != "pr_opened" {
		t.Fatalf("pipeline stage = %q, want pr_opened", got)
	}
}

func TestPipelineEntryInjectIncludesInitialPlanInstruction(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-initial-plan-pipeline"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, tags, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "workflow claw", "elasticclaw", "connected", `["workflow:github-issue"]`, "",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	stage := pipeline.Stage{
		ID:    "entry",
		Label: "Entry",
		OnEnter: pipeline.OnEnter{
			Inject: "Read the GitHub issue and start work.",
		},
	}
	if !s.transitionPipelineStageWithContext(clawID, stage, pipelineContext{}) {
		t.Fatalf("stage transition returned false")
	}

	var content string
	if err := db.QueryRow(`SELECT content FROM messages WHERE claw_id=? AND role='hub'`, clawID).Scan(&content); err != nil {
		t.Fatalf("select injected message: %v", err)
	}
	if !strings.Contains(content, initialPlanWakeContent) || !strings.Contains(content, "Task context:\nRead the GitHub issue and start work.") {
		t.Fatalf("pipeline inject did not include initial plan and task context:\n%s", content)
	}
	if !s.hasSystemMarker(clawID, initialPlanRequiredMarker) {
		t.Fatalf("initial plan required marker was not inserted")
	}
}

func TestGitHubAPIDeleteLabelIgnoresMissingLabel(t *testing.T) {
	var sawDelete bool
	var handlerErr string
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			handlerErr = "method = " + r.Method + ", want DELETE"
			http.Error(w, handlerErr, http.StatusInternalServerError)
			return
		}
		if r.URL.Path != "/repos/elasticclaw/elasticclaw/issues/305/labels/agent-ready" {
			handlerErr = "path = " + r.URL.Path
			http.Error(w, handlerErr, http.StatusInternalServerError)
			return
		}
		sawDelete = true
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Label does not exist","status":"404"}`))
	}))
	t.Cleanup(github.Close)

	if err := githubAPIDeleteLabel(github.URL, "elasticclaw/elasticclaw", 305, "agent-ready", "test-token"); err != nil {
		t.Fatalf("githubAPIDeleteLabel returned error for missing label: %v", err)
	}
	if handlerErr != "" {
		t.Fatal(handlerErr)
	}
	if !sawDelete {
		t.Fatal("test server did not receive DELETE")
	}
}

func TestPersistPipelineOutputStoresAndLoadsJSON(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-output-test"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test-claw", "base", "connected", "",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	result := &pipelineRunResult{
		ExitCode: 0,
		Stdout:   `{"branch":"feat/foo","commit":"abc123"}`,
		Stderr:   "",
	}
	s.persistPipelineOutput(clawID, "stage-1", "git_info", result)

	// Verify it was stored
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pipeline_outputs WHERE claw_id=?`, clawID).Scan(&count); err != nil {
		t.Fatalf("count outputs: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 output row, got %d", count)
	}

	// Verify load returns parsed JSON
	outputs := s.loadPipelineOutputs(clawID)
	if outputs == nil {
		t.Fatal("loadPipelineOutputs returned nil")
	}
	gitInfo, ok := outputs["git_info"]
	if !ok {
		t.Fatal("expected git_info in outputs")
	}
	if gitInfo["branch"] != "feat/foo" {
		t.Fatalf("expected branch=feat/foo, got %v", gitInfo["branch"])
	}
	if gitInfo["commit"] != "abc123" {
		t.Fatalf("expected commit=abc123, got %v", gitInfo["commit"])
	}
}

func TestPersistPipelineOutputNonJSON(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-output-nonjson"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test-claw", "base", "connected", "",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	result := &pipelineRunResult{
		ExitCode: 0,
		Stdout:   "plain text output",
		Stderr:   "",
	}
	s.persistPipelineOutput(clawID, "stage-1", "plain", result)

	outputs := s.loadPipelineOutputs(clawID)
	if outputs == nil {
		t.Fatal("loadPipelineOutputs returned nil")
	}
	plain, ok := outputs["plain"]
	if !ok {
		t.Fatal("expected plain in outputs")
	}
	// Non-JSON stdout should result in empty parsed_json
	if len(plain) != 0 {
		t.Fatalf("expected empty parsed map for non-JSON, got %v", plain)
	}
}

func TestPersistPipelineOutputOverwrite(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-output-overwrite"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test-claw", "base", "connected", "",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	// First write
	s.persistPipelineOutput(clawID, "stage-1", "version", &pipelineRunResult{
		ExitCode: 0,
		Stdout:   `{"v":"1.0.0"}`,
	})

	// Second write with same output name — should overwrite
	s.persistPipelineOutput(clawID, "stage-2", "version", &pipelineRunResult{
		ExitCode: 0,
		Stdout:   `{"v":"2.0.0"}`,
	})

	outputs := s.loadPipelineOutputs(clawID)
	if outputs == nil {
		t.Fatal("loadPipelineOutputs returned nil")
	}
	version, ok := outputs["version"]
	if !ok {
		t.Fatal("expected version in outputs")
	}
	if version["v"] != "2.0.0" {
		t.Fatalf("expected v=2.0.0 after overwrite, got %v", version["v"])
	}

	// Verify only one row
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pipeline_outputs WHERE claw_id=?`, clawID).Scan(&count); err != nil {
		t.Fatalf("count outputs: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 output row after overwrite, got %d", count)
	}
}

func TestValidateScriptCommandBlocksTraversal(t *testing.T) {
	cases := []struct {
		name    string
		cmd     string
		wantErr bool
	}{
		{"normal script", "python scripts/analyze.py", false},
		{"script with args", "bash scripts/deploy.sh --env=prod", false},
		{"path traversal scripts/..", "python scripts/../etc/passwd", true},
		{"scripts/.. traversal deep", "bash scripts/../../etc/shadow", true},
		{"direct traversal", "cat ../../.ssh/id_rsa", true},
		{"traversal with slash", "cat ../config.yaml", true},
		{"empty command", "", false},
		{"absolute path", "/bin/ls", false},
		{"flag with dot", "python -m ..module", true}, // flag values with .. are still rejected
		{"script in subdir", "python scripts/utils/helper.py", false},
		{"flag value traversal", "sometool --file ../../.ssh/id_rsa", true},
		{"module flag value traversal", "python -m ../../evil.py", true},
		{"inline flag value traversal", "sometool --output=../../.ssh/id_rsa", true},
		{"inline flag safe", "sometool --output=results.json", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateScriptCommand(tc.cmd)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %q, got nil", tc.cmd)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.cmd, err)
			}
		})
	}
}

func TestInjectTemplateDataMergesOutputs(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-template-outputs"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test-claw", "base", "connected", "",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	// Store an output
	s.persistPipelineOutput(clawID, "stage-1", "build_info", &pipelineRunResult{
		ExitCode: 0,
		Stdout:   `{"status":"success","duration":"45s"}`,
	})

	// Build template data
	baseData := map[string]interface{}{
		"Issue": map[string]string{"Title": "Test Issue"},
	}
	data := s.injectTemplateData(clawID, baseData)

	// Should be a map with both Issue and Outputs
	m, ok := data.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map[string]interface{}, got %T", data)
	}

	if _, ok := m["Issue"]; !ok {
		t.Fatal("expected Issue key in merged data")
	}

	outputs, ok := m["Outputs"].(map[string]map[string]interface{})
	if !ok {
		t.Fatalf("expected Outputs map, got %T", m["Outputs"])
	}

	buildInfo, ok := outputs["build_info"]
	if !ok {
		t.Fatal("expected build_info in outputs")
	}
	if buildInfo["status"] != "success" {
		t.Fatalf("expected status=success, got %v", buildInfo["status"])
	}
}

func TestTransitionPipelineStageConcurrentCallsRunOnEnterOnce(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-concurrent-stage"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "MAR-56", "elasticclaw", "connected", "working",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	stage := pipeline.Stage{
		ID:    "pr_opened",
		Label: "PR Opened",
		OnEnter: pipeline.OnEnter{
			Inject: "PR created. Watch for CI results and review comments.",
		},
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	transitionCount := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.transitionPipelineStageWithContext(clawID, stage, pipelineContext{}) {
				mu.Lock()
				transitionCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if transitionCount != 1 {
		t.Fatalf("concurrent stage transitions returned true %d times, want 1", transitionCount)
	}

	var messageCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=?`, clawID).Scan(&messageCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if messageCount != 1 {
		t.Fatalf("concurrent stage transitions injected %d messages, want 1", messageCount)
	}
	if got := s.getPipelineStage(clawID); got != "pr_opened" {
		t.Fatalf("pipeline stage = %q, want pr_opened", got)
	}
}

func TestParseJudgeResponseValid(t *testing.T) {
	raw := `{"verdict":"pass","summary":"Looks good","findings":[],"required_fixes":[]}`
	result, err := parseJudgeResponse(raw)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if result.Verdict != "pass" {
		t.Fatalf("verdict = %q, want pass", result.Verdict)
	}
	if result.Summary != "Looks good" {
		t.Fatalf("summary = %q", result.Summary)
	}
}

func TestParseJudgeResponseFail(t *testing.T) {
	raw := `{"verdict":"fail","summary":"Issues found","findings":[{"file":"main.go","line":"42","comment":"nil pointer","severity":"high"}],"required_fixes":["fix nil pointer"]}`
	result, err := parseJudgeResponse(raw)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if result.Verdict != "fail" {
		t.Fatalf("verdict = %q, want fail", result.Verdict)
	}
	if len(result.Findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(result.Findings))
	}
	if result.Findings[0].File != "main.go" {
		t.Fatalf("finding.file = %q", result.Findings[0].File)
	}
	if len(result.RequiredFixes) != 1 {
		t.Fatalf("expected 1 required fix, got %d", len(result.RequiredFixes))
	}
}

func TestParseJudgeResponseWithMarkdownFences(t *testing.T) {
	raw := "```json\n{\"verdict\":\"pass\",\"summary\":\"Good\",\"findings\":[],\"required_fixes\":[]}\n```"
	result, err := parseJudgeResponse(raw)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if result.Verdict != "pass" {
		t.Fatalf("verdict = %q, want pass", result.Verdict)
	}
}

func TestParseJudgeResponseWithTrailingText(t *testing.T) {
	// LLM may add trailing text after the JSON object
	raw := `{"verdict":"pass","summary":"Looks good"} Some trailing text here`
	result, err := parseJudgeResponse(raw)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if result.Verdict != "pass" {
		t.Fatalf("verdict = %q, want pass", result.Verdict)
	}
	if result.Summary != "Looks good" {
		t.Fatalf("summary = %q, want Looks good", result.Summary)
	}
}

func TestParseJudgeResponseWithNestedBraces(t *testing.T) {
	// Nested braces in strings should not confuse the parser
	raw := `{"verdict":"pass","summary":"Check {nested} braces","findings":[]}`
	result, err := parseJudgeResponse(raw)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if result.Verdict != "pass" {
		t.Fatalf("verdict = %q, want pass", result.Verdict)
	}
	if result.Summary != "Check {nested} braces" {
		t.Fatalf("summary = %q", result.Summary)
	}
}

func TestParseJudgeResponseWithEscapedQuotes(t *testing.T) {
	// Escaped quotes should not confuse the parser
	raw := `{"verdict":"pass","summary":"It said \"hello\"","findings":[]}`
	result, err := parseJudgeResponse(raw)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if result.Verdict != "pass" {
		t.Fatalf("verdict = %q, want pass", result.Verdict)
	}
	if result.Summary != `It said "hello"` {
		t.Fatalf("summary = %q", result.Summary)
	}
}

func TestParseJudgeResponseMissingVerdict(t *testing.T) {
	raw := `{"summary":"No verdict"}`
	_, err := parseJudgeResponse(raw)
	if err == nil {
		t.Fatal("expected error for missing verdict")
	}
}

func TestParseJudgeResponseInvalidVerdict(t *testing.T) {
	raw := `{"verdict":"maybe","summary":"Unclear"}`
	_, err := parseJudgeResponse(raw)
	if err == nil {
		t.Fatal("expected error for invalid verdict")
	}
}

func TestJudgeTimeoutDefault(t *testing.T) {
	if d := judgeTimeout(""); d != 2*time.Minute {
		t.Fatalf("default timeout = %v, want 2m", d)
	}
}

func TestJudgeTimeoutCustom(t *testing.T) {
	if d := judgeTimeout("5m"); d != 5*time.Minute {
		t.Fatalf("custom timeout = %v, want 5m", d)
	}
}

func TestJudgeTimeoutInvalid(t *testing.T) {
	if d := judgeTimeout("invalid"); d != 2*time.Minute {
		t.Fatalf("invalid timeout fallback = %v, want 2m", d)
	}
}

func TestTruncateString(t *testing.T) {
	if truncateString("hello", 10) != "hello" {
		t.Fatal("short string should not be truncated")
	}
	if truncateString("hello world", 5) != "hello..." {
		t.Fatalf("truncated = %q", truncateString("hello world", 5))
	}
}

func TestRunOnEnterJudgeBlocksOnFail(t *testing.T) {
	// This test verifies that when a judge stage fails and continue_on_error=false,
	// runOnEnter returns early and does not process subsequent actions.
	// We can't easily mock the LLM call, so we test the parsing and timeout
	// functions directly above. The integration with runOnEnter is tested
	// via the judge action being present in the stage and the flow logic.
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-judge-block"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test-claw", "base", "connected", "",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	// Insert a test output for the judge to reference
	s.persistPipelineOutput(clawID, "test-stage", "test_output", &pipelineRunResult{
		ExitCode: 0,
		Stdout:   "PASS\nok  github.com/elasticclaw/elasticclaw/pkg/hub\t0.042s",
	})

	// Verify the output is stored
	outputs := s.loadPipelineOutputs(clawID)
	if outputs == nil {
		t.Fatal("expected outputs")
	}
	if _, ok := outputs["test_output"]; !ok {
		t.Fatal("expected test_output in outputs")
	}

	// Verify judge stage with required verdict would be processed
	stage := pipeline.Stage{
		ID:    "review",
		Label: "Review",
		OnEnter: pipeline.OnEnter{
			Judge: pipeline.JudgeAction{
				Instructions: "Review the code",
				Inputs:       []pipeline.JudgeInput{pipeline.JudgeInputIssue},
				Require:      pipeline.JudgeRequire{Verdict: "pass"},
				Output:       "review_result",
			},
			Inject: "Continue to PR",
		},
	}

	// The judge will fail because no LLM keys are configured, and
	// continue_on_error=false, so runOnEnter should return early.
	// We verify this by checking that no inject message was sent.
	s.runOnEnter(clawID, stage, pipelineContext{})

	var messageCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=?`, clawID).Scan(&messageCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	// Should have 1 error message from the judge failure, not the inject message
	if messageCount != 1 {
		t.Fatalf("expected 1 message (judge error), got %d", messageCount)
	}
}

func TestRunOnEnterJudgeContinueOnError(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-judge-continue"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test-claw", "base", "connected", "",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	stage := pipeline.Stage{
		ID:    "review",
		Label: "Review",
		OnEnter: pipeline.OnEnter{
			Judge: pipeline.JudgeAction{
				Instructions:    "Review the code",
				Inputs:          []pipeline.JudgeInput{pipeline.JudgeInputIssue},
				Require:         pipeline.JudgeRequire{Verdict: "pass"},
				Output:          "review_result",
				ContinueOnError: true,
			},
			Inject: "Continue to PR",
		},
	}

	s.runOnEnter(clawID, stage, pipelineContext{})

	var messageCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=?`, clawID).Scan(&messageCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	// Should have 2 messages: judge error + inject message (because continue_on_error=true)
	if messageCount != 2 {
		t.Fatalf("expected 2 messages (judge error + inject), got %d", messageCount)
	}
}

func TestAutoTransitionAfterJudge(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-auto-judge"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test-claw", "base", "connected", "review",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	// autoTransitionAfterJudge with no pipeline context should do nothing
	s.autoTransitionAfterJudge(clawID, "pass", pipelineContext{})

	// Stage should remain "review" since no pipeline was configured
	var stage string
	if err := db.QueryRow(`SELECT pipeline_stage FROM claws WHERE id=?`, clawID).Scan(&stage); err != nil {
		t.Fatalf("get stage: %v", err)
	}
	if stage != "review" {
		t.Fatalf("stage = %q, want review", stage)
	}
}

func TestEvaluateGatePass(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-gate-pass"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test-claw", "base", "connected", "",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	// Store a test output
	s.persistPipelineOutput(clawID, "test-stage", "build_info", &pipelineRunResult{
		ExitCode: 0,
		Stdout:   `{"status":"passed","duration":"45s"}`,
	})

	gate := &pipeline.Gate{
		Output: "build_info",
		Pass: pipeline.GateCondition{
			Path:   "status",
			Values: []string{"passed", "skipped"},
		},
		Fail: pipeline.GateCondition{
			Path:   "status",
			Values: []string{"failed", "error"},
		},
		Required: true,
	}

	result := s.evaluateGate(clawID, "test-stage", gate)
	if result.Verdict != "pass" {
		t.Fatalf("verdict = %q, want pass", result.Verdict)
	}
	if result.MatchedPath != "status" {
		t.Fatalf("matched_path = %q, want status", result.MatchedPath)
	}
	if result.MatchedValue != "passed" {
		t.Fatalf("matched_value = %q, want passed", result.MatchedValue)
	}

	// Verify persisted in DB
	var verdict string
	if err := db.QueryRow(`SELECT verdict FROM pipeline_gate_results WHERE claw_id=? AND stage_id=?`, clawID, "test-stage").Scan(&verdict); err != nil {
		t.Fatalf("select gate result: %v", err)
	}
	if verdict != "pass" {
		t.Fatalf("db verdict = %q, want pass", verdict)
	}
}

func TestEvaluateGateFail(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-gate-fail"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test-claw", "base", "connected", "",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	s.persistPipelineOutput(clawID, "test-stage", "build_info", &pipelineRunResult{
		ExitCode: 0,
		Stdout:   `{"status":"failed","error":"compile error"}`,
	})

	gate := &pipeline.Gate{
		Output: "build_info",
		Pass: pipeline.GateCondition{
			Path:   "status",
			Values: []string{"passed", "skipped"},
		},
		Fail: pipeline.GateCondition{
			Path:   "status",
			Values: []string{"failed", "error"},
		},
		Required: true,
	}

	result := s.evaluateGate(clawID, "test-stage", gate)
	if result.Verdict != "fail" {
		t.Fatalf("verdict = %q, want fail", result.Verdict)
	}
	if result.MatchedPath != "status" {
		t.Fatalf("matched_path = %q, want status", result.MatchedPath)
	}
	if result.MatchedValue != "failed" {
		t.Fatalf("matched_value = %q, want failed", result.MatchedValue)
	}

	var verdict string
	if err := db.QueryRow(`SELECT verdict FROM pipeline_gate_results WHERE claw_id=? AND stage_id=?`, clawID, "test-stage").Scan(&verdict); err != nil {
		t.Fatalf("select gate result: %v", err)
	}
	if verdict != "fail" {
		t.Fatalf("db verdict = %q, want fail", verdict)
	}
}

func TestEvaluateGateSkippedAsPass(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-gate-skipped"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test-claw", "base", "connected", "",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	// No output stored — output is missing
	gate := &pipeline.Gate{
		Output:             "missing_output",
		TreatSkippedAsPass: true,
	}

	result := s.evaluateGate(clawID, "test-stage", gate)
	if result.Verdict != "skipped" {
		t.Fatalf("verdict = %q, want skipped", result.Verdict)
	}

	var verdict string
	if err := db.QueryRow(`SELECT verdict FROM pipeline_gate_results WHERE claw_id=? AND stage_id=?`, clawID, "test-stage").Scan(&verdict); err != nil {
		t.Fatalf("select gate result: %v", err)
	}
	if verdict != "skipped" {
		t.Fatalf("db verdict = %q, want skipped", verdict)
	}
}

func TestEvaluateGateErrorNoMatch(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-gate-error"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test-claw", "base", "connected", "",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	s.persistPipelineOutput(clawID, "test-stage", "build_info", &pipelineRunResult{
		ExitCode: 0,
		Stdout:   `{"status":"unknown"}`,
	})

	gate := &pipeline.Gate{
		Output: "build_info",
		Pass: pipeline.GateCondition{
			Path:   "status",
			Values: []string{"passed"},
		},
		Fail: pipeline.GateCondition{
			Path:   "status",
			Values: []string{"failed"},
		},
	}

	result := s.evaluateGate(clawID, "test-stage", gate)
	if result.Verdict != "error" {
		t.Fatalf("verdict = %q, want error", result.Verdict)
	}

	var verdict string
	if err := db.QueryRow(`SELECT verdict FROM pipeline_gate_results WHERE claw_id=? AND stage_id=?`, clawID, "test-stage").Scan(&verdict); err != nil {
		t.Fatalf("select gate result: %v", err)
	}
	if verdict != "error" {
		t.Fatalf("db verdict = %q, want error", verdict)
	}
}

func TestHasFailedRequiredGate(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-required-gate"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test-claw", "base", "connected", "",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	if s.hasFailedRequiredGate(clawID) {
		t.Fatal("expected no failed required gate initially")
	}

	// Insert a failed required gate
	_, err = db.Exec(`
		INSERT INTO pipeline_gate_results(claw_id, stage_id, output_name, verdict, matched_path, matched_value, required, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		clawID, "test-stage", "build_info", "fail", "status", "failed", 1, time.Now().UTC())
	if err != nil {
		t.Fatalf("insert gate result: %v", err)
	}

	if !s.hasFailedRequiredGate(clawID) {
		t.Fatal("expected failed required gate")
	}
}

func TestLoadGateResult(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-load-gate"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test-claw", "base", "connected", "",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	// No gate result yet
	if s.loadGateResult(clawID, "test-stage") != nil {
		t.Fatal("expected nil for missing gate result")
	}

	_, err = db.Exec(`
		INSERT INTO pipeline_gate_results(claw_id, stage_id, output_name, verdict, matched_path, matched_value, required, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		clawID, "test-stage", "build_info", "pass", "status", "passed", 1, time.Now().UTC())
	if err != nil {
		t.Fatalf("insert gate result: %v", err)
	}

	result := s.loadGateResult(clawID, "test-stage")
	if result == nil {
		t.Fatal("expected gate result")
	}
	if result.Verdict != "pass" {
		t.Fatalf("verdict = %q, want pass", result.Verdict)
	}
	if result.MatchedPath != "status" {
		t.Fatalf("matched_path = %q, want status", result.MatchedPath)
	}
	if result.MatchedValue != "passed" {
		t.Fatalf("matched_value = %q, want passed", result.MatchedValue)
	}
}
