package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

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
		{"absolute path", "/bin/ls", true},
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
