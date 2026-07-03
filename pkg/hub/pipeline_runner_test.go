package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
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

func TestTransitionPipelineStageSkipIfIssueLabelMatchesBeforeOnEnter(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-skip-if-label"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "workflow claw", "elasticclaw", "connected", "working",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	pipelineYAML := `
stages:
  - id: review_loop
    skip_if:
      issue_labels:
        labels: [no review loop]
      go_to: detect_android_changes
    on_enter:
      inject: "review loop should not run"
  - id: detect_android_changes
    on_enter:
      inject: "detect android changes"
`
	pl, err := pipeline.Parse([]byte(pipelineYAML))
	if err != nil {
		t.Fatalf("parse pipeline: %v", err)
	}
	ctx := pipelineContext{
		Workflow:             &types.WorkflowConfig{PipelineYAML: pipelineYAML},
		IssueLabelsAvailable: true,
		IssueLabels:          []string{"No Review Loop"},
	}
	if !s.transitionPipelineStageWithContext(clawID, pl.Stages[0], ctx) {
		t.Fatalf("stage transition returned false")
	}
	if got := s.getPipelineStage(clawID); got != "detect_android_changes" {
		t.Fatalf("pipeline stage = %q, want detect_android_changes", got)
	}

	var messages []string
	rows, err := db.Query(`SELECT content FROM messages WHERE claw_id=? ORDER BY created_at, id`, clawID)
	if err != nil {
		t.Fatalf("select messages: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			t.Fatalf("scan message: %v", err)
		}
		messages = append(messages, content)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("message rows: %v", err)
	}
	joined := strings.Join(messages, "\n")
	if strings.Contains(joined, "review loop should not run") {
		t.Fatalf("skipped stage on_enter ran unexpectedly:\n%s", joined)
	}
	if !strings.Contains(joined, "detect android changes") {
		t.Fatalf("target stage on_enter did not run:\n%s", joined)
	}
}

func TestTransitionPipelineStageIssueLabelSkipAvailabilityDefaults(t *testing.T) {
	tests := []struct {
		name                   string
		pipelineYAML           string
		issueLabels            []string
		issueLabelsAvailable   bool
		templateFileLabelsJSON string
		wantStage              string
		wantMessage            string
		rejectMessage          string
	}{
		{
			name: "skip_if does not skip without labels",
			pipelineYAML: `
stages:
  - id: review_loop
    skip_if:
      issue_labels:
        labels: [no review loop]
      go_to: detect_android_changes
    on_enter:
      inject: "review loop ran"
  - id: detect_android_changes
    on_enter:
      inject: "detect android changes"
`,
			wantStage:     "review_loop",
			wantMessage:   "review loop ran",
			rejectMessage: "detect android changes",
		},
		{
			name: "skip_if skips with labels loaded from claw snapshot",
			pipelineYAML: `
stages:
  - id: review_loop
    skip_if:
      issue_labels:
        labels: [no review loop]
      go_to: detect_android_changes
    on_enter:
      inject: "review loop ran"
  - id: detect_android_changes
    on_enter:
      inject: "detect android changes"
`,
			templateFileLabelsJSON: `["no review loop"]`,
			wantStage:              "detect_android_changes",
			wantMessage:            "detect android changes",
			rejectMessage:          "review loop ran",
		},
		{
			name: "skip_unless skips without labels",
			pipelineYAML: `
stages:
  - id: review_loop
    skip_unless:
      issue_labels:
        labels: [with review loop]
      go_to: detect_android_changes
    on_enter:
      inject: "review loop ran"
  - id: detect_android_changes
    on_enter:
      inject: "detect android changes"
`,
			wantStage:     "detect_android_changes",
			wantMessage:   "detect android changes",
			rejectMessage: "review loop ran",
		},
		{
			name: "skip_unless skips when available labels do not match",
			pipelineYAML: `
stages:
  - id: review_loop
    skip_unless:
      issue_labels:
        labels: [with review loop]
      go_to: detect_android_changes
    on_enter:
      inject: "review loop ran"
  - id: detect_android_changes
    on_enter:
      inject: "detect android changes"
`,
			issueLabelsAvailable: true,
			issueLabels:          []string{"other label"},
			wantStage:            "detect_android_changes",
			wantMessage:          "detect android changes",
			rejectMessage:        "review loop ran",
		},
		{
			name: "skip_unless does not skip when available labels match",
			pipelineYAML: `
stages:
  - id: review_loop
    skip_unless:
      issue_labels:
        labels: [with review loop]
      go_to: detect_android_changes
    on_enter:
      inject: "review loop ran"
  - id: detect_android_changes
    on_enter:
      inject: "detect android changes"
`,
			issueLabelsAvailable: true,
			issueLabels:          []string{"With Review Loop"},
			wantStage:            "review_loop",
			wantMessage:          "review loop ran",
			rejectMessage:        "detect android changes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

			clawID := "claw-" + strings.ReplaceAll(tt.name, " ", "-")
			templateFiles := "{}"
			if tt.templateFileLabelsJSON != "" {
				templateFiles = `{"` + issueLabelsTemplateFile + `":` + strconv.Quote(tt.templateFileLabelsJSON) + `}`
			}
			_, err := db.Exec(
				`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, template_files, created_at) VALUES(?,?,?,?,?,?,?,datetime('now'))`,
				clawID, "test-tenant-id", "workflow claw", "elasticclaw", "connected", "working", templateFiles,
			)
			if err != nil {
				t.Fatalf("insert claw: %v", err)
			}
			pl, err := pipeline.Parse([]byte(tt.pipelineYAML))
			if err != nil {
				t.Fatalf("parse pipeline: %v", err)
			}
			ctx := pipelineContext{
				Workflow:             &types.WorkflowConfig{PipelineYAML: tt.pipelineYAML},
				IssueLabelsAvailable: tt.issueLabelsAvailable,
				IssueLabels:          tt.issueLabels,
			}
			if !s.transitionPipelineStageWithContext(clawID, pl.Stages[0], ctx) {
				t.Fatalf("stage transition returned false")
			}
			if got := s.getPipelineStage(clawID); got != tt.wantStage {
				t.Fatalf("pipeline stage = %q, want %s", got, tt.wantStage)
			}
			var content string
			if err := db.QueryRow(`SELECT COALESCE(group_concat(content, '\n'),'') FROM messages WHERE claw_id=?`, clawID).Scan(&content); err != nil {
				t.Fatalf("select messages: %v", err)
			}
			if !strings.Contains(content, tt.wantMessage) {
				t.Fatalf("messages did not contain %q:\n%s", tt.wantMessage, content)
			}
			if strings.Contains(content, tt.rejectMessage) {
				t.Fatalf("messages unexpectedly contained %q:\n%s", tt.rejectMessage, content)
			}
		})
	}
}

func TestWorkflowIssueLabelsDoNotReplaceWorkspaceFile(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		Token:     "test-token",
		ClawToken: "test-claw-token",
		Providers: map[string]types.ProviderConfig{
			"noop": {Type: "noop"},
		},
	}, "", "", "")

	workspace := &types.WorkspaceConfig{
		SchemaVersion: "v1",
		Name:          "workspace-with-label-file",
		Files: map[string]string{
			"elasticclaw-config.yaml": "schema_version: v1\nname: workspace-with-label-file\nprovider: noop\n",
			"ISSUE_LABELS.json":       `{"user":"workspace file"}`,
		},
	}
	workflow := &types.WorkflowConfig{Name: "workflow", Provider: "noop"}

	clawID, _, err := s.createClawFromWorkflowWithOptions(workspace, workflow, workflowCreateOptions{
		issueLabelsAvailable: true,
		issueLabels:          []string{"with review loop"},
		reason:               "test",
	})
	if err != nil {
		t.Fatalf("createClawFromWorkflowWithOptions: %v", err)
	}

	var filesJSON string
	if err := db.QueryRow(`SELECT template_files FROM claws WHERE id=?`, clawID).Scan(&filesJSON); err != nil {
		t.Fatalf("select template files: %v", err)
	}
	var files map[string]string
	if err := json.Unmarshal([]byte(filesJSON), &files); err != nil {
		t.Fatalf("unmarshal template files: %v", err)
	}
	if got := files["ISSUE_LABELS.json"]; got != `{"user":"workspace file"}` {
		t.Fatalf("workspace ISSUE_LABELS.json = %q, want user file", got)
	}
	labels, ok := s.loadIssueLabelsForClaw(clawID)
	if !ok {
		t.Fatalf("expected issue labels metadata to be available")
	}
	if len(labels) != 1 || labels[0] != "with review loop" {
		t.Fatalf("issue labels = %#v, want [with review loop]", labels)
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

func TestParsePipelineOutputJSONUsesLastJSONLine(t *testing.T) {
	stdout := `$ git rev-parse --is-inside-work-tree
$ git status --porcelain
{"reason": "next_mobile matches main", "source_dir": "/home/daytona/.openclaw/workspace/next_mobile", "status": "skipped"}
`
	parsed, ok := parsePipelineOutputJSON(stdout)
	if !ok {
		t.Fatal("expected noisy stdout with final JSON line to parse")
	}
	if parsed["status"] != "skipped" {
		t.Fatalf("status = %v, want skipped", parsed["status"])
	}
	if parsed["reason"] != "next_mobile matches main" {
		t.Fatalf("reason = %v, want next_mobile matches main", parsed["reason"])
	}
}

func TestPersistPipelineOutputNoisyJSON(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-output-noisy-json"
	_, err := s.db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test-claw", "base", "connected", "",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	s.persistPipelineOutput(clawID, "stage-1", "android_validation", &pipelineRunResult{
		ExitCode: 0,
		Stdout: `$ git status --porcelain
{"status":"skipped","reason":"next_mobile matches main"}`,
	})

	outputs := s.loadPipelineOutputs(clawID)
	validation, ok := outputs["android_validation"]
	if !ok {
		t.Fatal("expected android_validation in outputs")
	}
	if validation["status"] != "skipped" {
		t.Fatalf("status = %v, want skipped", validation["status"])
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

func TestTerminalPipelineFailureStageMarksWorkflowRunFailed(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	s.cronScheduler = newCronScheduler(s)

	const clawID = "claw-terminal-failure"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "workflow claw", "elasticclaw", "connected", "validation",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}
	_, err = db.Exec(
		`INSERT INTO workflow_runs(id, tenant_id, workflow_name, workspace_name, trigger_type, status, claw_id, run_context, started_at, created_at)
		 VALUES(?,?,?,?,?,?,?,?,datetime('now'),datetime('now'))`,
		"run-terminal-failure", "test-tenant-id", "nightly", "engineering", "cron", "running", clawID, "{}",
	)
	if err != nil {
		t.Fatalf("insert workflow run: %v", err)
	}

	stage := pipeline.Stage{
		ID:       "failed",
		Label:    "Failed",
		Terminal: true,
		Triggers: []pipeline.Trigger{{
			GateResult: &pipeline.GateResultTrigger{
				Stage:   "validation",
				Verdict: "fail",
			},
		}},
	}
	if !s.transitionPipelineStageWithContext(clawID, stage, pipelineContext{}) {
		t.Fatalf("terminal stage transition returned false")
	}

	var status, result string
	if err := db.QueryRow(`SELECT status, result FROM workflow_runs WHERE id=?`, "run-terminal-failure").Scan(&status, &result); err != nil {
		t.Fatalf("select workflow run: %v", err)
	}
	if status != "failed" {
		t.Fatalf("workflow run status = %q, want failed (result=%q)", status, result)
	}
	if result != "pipeline terminal stage failed" {
		t.Fatalf("workflow run result = %q, want pipeline terminal stage failed", result)
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

	// Insert an error verdict required gate — should also block
	_, err = db.Exec(`
		INSERT INTO pipeline_gate_results(claw_id, stage_id, output_name, verdict, matched_path, matched_value, required, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		clawID, "test-stage-2", "build_info", "error", "", "", 1, time.Now().UTC())
	if err != nil {
		t.Fatalf("insert gate result: %v", err)
	}
	if !s.hasFailedRequiredGate(clawID) {
		t.Fatal("expected failed required gate for error verdict")
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

func TestCheckPipelineMessageTriggersOutputMatches(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-output-matches-trigger"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, tags, created_at) VALUES(?,?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test-claw", "base", "connected", "", `["factory:test-factory"]`,
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	// Store a pipeline output that should match the output_matches trigger
	s.persistPipelineOutput(clawID, "build-stage", "build_info", &pipelineRunResult{
		ExitCode: 0,
		Stdout:   `{"status":"success","duration":"45s"}`,
	})

	// Create a factory with a pipeline that has an output_matches trigger
	factory := &types.FactoryConfig{
		Name:     "test-factory",
		Template: "base",
		PipelineYAML: `
stages:
  - id: working
    label: "Working"
    entry: true
    on_enter:
      inject: "Start working"
  - id: pr_ready
    label: "PR Ready"
    triggers:
      - output_matches:
          output: build_info
          path: status
          any_of:
            - success
            - passed
    on_enter:
      inject: "Build succeeded, create PR"
`,
	}

	// Set the factory on the server config
	s.hubCfg.Factories = []*types.FactoryConfig{factory}

	// The claw should transition from working to pr_ready because build_info.status == "success"
	// First, set the claw to the working stage
	_, err = db.Exec(`UPDATE claws SET pipeline_stage='working' WHERE id=?`, clawID)
	if err != nil {
		t.Fatalf("update stage: %v", err)
	}

	// Now check triggers — this should find the output_matches trigger and transition
	// We need to call checkPipelineMessageTriggers with a message that doesn't match message_contains
	// so it falls through to output_matches
	transitioned := s.checkPipelineMessageTriggers(clawID, "some random message that doesn't match anything")
	if !transitioned {
		t.Fatal("expected output_matches trigger to transition the stage")
	}

	// Verify the stage transitioned
	var stage string
	if err := db.QueryRow(`SELECT pipeline_stage FROM claws WHERE id=?`, clawID).Scan(&stage); err != nil {
		t.Fatalf("get stage: %v", err)
	}
	if stage != "pr_ready" {
		t.Fatalf("stage = %q, want pr_ready", stage)
	}
}

func TestGateEvaluatesNonzeroExitWithValidJSON(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-gate-nonzero"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test-claw", "base", "connected", "",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	// Simulate a run that exited nonzero but still emitted valid JSON
	result := &pipelineRunResult{
		ExitCode: 1, // nonzero exit — but valid JSON output
		Stdout:   `{"status":"failed","reason":"CodeBuild failed"}`,
		Stderr:   "exit status 1",
	}
	s.persistPipelineOutput(clawID, "validate", "android_validation", result)

	gate := &pipeline.Gate{
		Output: "android_validation",
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

	gateResult := s.evaluateGate(clawID, "validate", gate)
	if gateResult.Verdict != "fail" {
		t.Fatalf("verdict = %q, want fail (nonzero exit with valid JSON should still evaluate gate)", gateResult.Verdict)
	}
	if gateResult.MatchedPath != "status" {
		t.Fatalf("matched_path = %q, want status", gateResult.MatchedPath)
	}
	if gateResult.MatchedValue != "failed" {
		t.Fatalf("matched_value = %q, want failed", gateResult.MatchedValue)
	}

	// Verify the gate result was persisted
	var verdict string
	if err := db.QueryRow(`SELECT verdict FROM pipeline_gate_results WHERE claw_id=? AND stage_id=?`, clawID, "validate").Scan(&verdict); err != nil {
		t.Fatalf("select gate result: %v", err)
	}
	if verdict != "fail" {
		t.Fatalf("db verdict = %q, want fail", verdict)
	}
}

func TestGateErrorOnNonzeroExitNoValidJSON(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-gate-nonzero-nojson"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test-claw", "base", "connected", "",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	// Simulate a run that exited nonzero with no valid JSON output
	result := &pipelineRunResult{
		ExitCode: 1,
		Stdout:   "Error: could not connect to CodeBuild",
		Stderr:   "exit status 1",
	}
	s.persistPipelineOutput(clawID, "validate", "android_validation", result)

	gate := &pipeline.Gate{
		Output: "android_validation",
		Pass: pipeline.GateCondition{
			Path:   "status",
			Values: []string{"passed"},
		},
		Fail: pipeline.GateCondition{
			Path:   "status",
			Values: []string{"failed"},
		},
		Required: true,
	}

	gateResult := s.evaluateGate(clawID, "validate", gate)
	if gateResult.Verdict != "error" {
		t.Fatalf("verdict = %q, want error (nonzero exit with no valid JSON should produce error verdict)", gateResult.Verdict)
	}
}

func TestGateSkippedAsPassAutoTransition(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-gate-skipped-pass"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, tags, created_at) VALUES(?,?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test-claw", "base", "connected", "", `["factory:skipped-factory"]`,
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	// No output stored — output is missing, but treat_skipped_as_pass is true
	gate := &pipeline.Gate{
		Output:             "missing_output",
		TreatSkippedAsPass: true,
	}

	result := s.evaluateGate(clawID, "validate", gate)
	if result.Verdict != "skipped" {
		t.Fatalf("verdict = %q, want skipped", result.Verdict)
	}

	// Create a pipeline with a gate_result: pass trigger that should match
	factory := &types.FactoryConfig{
		Name:     "skipped-factory",
		Template: "base",
		PipelineYAML: `
stages:
  - id: validate
    label: "Validate"
    entry: true
    on_enter:
      inject: "Start validating"
    gate:
      output: missing_output
      treat_skipped_as_pass: true
  - id: create_pr
    label: "Create PR"
    triggers:
      - gate_result:
          stage: validate
          verdict: pass
    on_enter:
      inject: "Skipped treated as pass, create PR"
`,
	}
	s.hubCfg.Factories = []*types.FactoryConfig{factory}

	// Set claw to validate stage
	_, err = db.Exec(`UPDATE claws SET pipeline_stage='validate' WHERE id=?`, clawID)
	if err != nil {
		t.Fatalf("update stage: %v", err)
	}

	// Simulate the auto-transition that would happen after gate evaluation
	// The skipped verdict should be normalised to pass for auto-transition
	ctx := pipelineContext{Factory: factory, IssueID: ""}
	s.autoTransitionAfterGate(clawID, "validate", "pass", ctx)

	// Verify the stage transitioned to create_pr
	var stage string
	if err := db.QueryRow(`SELECT pipeline_stage FROM claws WHERE id=?`, clawID).Scan(&stage); err != nil {
		t.Fatalf("get stage: %v", err)
	}
	if stage != "create_pr" {
		t.Fatalf("stage = %q, want create_pr (skipped should be normalised to pass for auto-transition)", stage)
	}
}
