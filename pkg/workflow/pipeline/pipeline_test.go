package pipeline_test

import (
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/workflow/pipeline"
)

const sampleYAML = `
stages:
  - id: working
    label: "Working"
    entry: true
    on_enter:
      inject: |
        Read your BOOTSTRAP.md and start working on the issue.

  - id: pr_opened
    label: "PR Opened"
    triggers:
      - message_contains: "[DONE]"
    on_enter:
      move_issue: "In Review"
      inject: |
        PR created. Watch for CI results and review comments.

  - id: merged
    label: "Merged"
    triggers:
      - pr_merged:
    terminal: true

  - id: closed_no_merge
    label: "Closed Without Merge"
    triggers:
      - pr_closed:
    on_enter:
      inject: |
        PR was closed without merging. Decide: reopen, new PR, or ask the user.
`

func TestParse(t *testing.T) {
	p, err := pipeline.Parse([]byte(sampleYAML))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if len(p.Stages) != 4 {
		t.Fatalf("expected 4 stages, got %d", len(p.Stages))
	}
}

func TestParseMoveIssueObject(t *testing.T) {
	p, err := pipeline.Parse([]byte(`
stages:
  - id: working
    entry: true
    on_enter:
      move_issue:
        status: "Agent Needs Review"
        issue_id: "{{.Inputs.issue_id}}"
`))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	stage := p.EntryStage()
	if stage == nil {
		t.Fatal("expected entry stage")
	}
	if stage.OnEnter.MoveIssue.Status != "Agent Needs Review" {
		t.Fatalf("status = %q, want Agent Needs Review", stage.OnEnter.MoveIssue.Status)
	}
	if stage.OnEnter.MoveIssue.IssueID != "{{.Inputs.issue_id}}" {
		t.Fatalf("issue_id = %q, want template", stage.OnEnter.MoveIssue.IssueID)
	}
}

func TestParseRunAction(t *testing.T) {
	p, err := pipeline.Parse([]byte(`
stages:
  - id: pre_commit
    on_enter:
      run:
        command: clawpatch ci --format json --output .elasticclaw/clawpatch/report.json
        continue_on_error: true
        timeout: 15m
`))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if len(p.Stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(p.Stages))
	}
	run := p.Stages[0].OnEnter.Run
	if run.Command != "clawpatch ci --format json --output .elasticclaw/clawpatch/report.json" {
		t.Fatalf("command = %q", run.Command)
	}
	if !run.ContinueOnError {
		t.Fatal("expected continue_on_error")
	}
	if run.Timeout != "15m" {
		t.Fatalf("timeout = %q, want 15m", run.Timeout)
	}
}

func TestEntryStage(t *testing.T) {
	p, _ := pipeline.Parse([]byte(sampleYAML))
	entry := p.EntryStage()
	if entry == nil {
		t.Fatal("expected entry stage, got nil")
	}
	if entry.ID != "working" {
		t.Errorf("expected entry stage id 'working', got %q", entry.ID)
	}
}

func TestStageForMessageContains(t *testing.T) {
	p, _ := pipeline.Parse([]byte(sampleYAML))
	s := p.StageForMessageContains("Great work! [DONE] https://github.com/org/repo/pull/1")
	if s == nil {
		t.Fatal("expected to match message_contains trigger")
	}
	if s.ID != "pr_opened" {
		t.Errorf("expected 'pr_opened', got %q", s.ID)
	}
}

func TestStageForPRMerged(t *testing.T) {
	p, _ := pipeline.Parse([]byte(sampleYAML))
	s := p.StageForPRMerged()
	if s == nil {
		t.Fatal("expected pr_merged stage")
	}
	if s.ID != "merged" {
		t.Errorf("expected 'merged', got %q", s.ID)
	}
}

func TestStageForPRClosed(t *testing.T) {
	p, _ := pipeline.Parse([]byte(sampleYAML))
	s := p.StageForPRClosed()
	if s == nil {
		t.Fatal("expected pr_closed stage")
	}
	if s.ID != "closed_no_merge" {
		t.Errorf("expected 'closed_no_merge', got %q", s.ID)
	}
}

func TestParseEmpty(t *testing.T) {
	_, err := pipeline.Parse([]byte(""))
	if err != nil {
		t.Fatalf("unexpected error parsing empty yaml: %v", err)
	}
}

func TestParseInvalid(t *testing.T) {
	// A tab character at the start of a line is a YAML parse error
	_, err := pipeline.Parse([]byte("stages:\n\t- id: bad"))
	if err == nil {
		t.Fatal("expected parse error for invalid yaml")
	}
}

func TestParseJudgeAction(t *testing.T) {
	p, err := pipeline.Parse([]byte(`
stages:
  - id: review
    label: "Review"
    on_enter:
      judge:
        model: anthropic/claude-sonnet-4-6
        inputs:
          - issue
          - git_diff
          - test_output
        require:
          verdict: pass
        instructions: |
          Review the diff for correctness, security, regressions, and missing tests.
          Return pass/fail with specific required fixes.
        output: review_result
        continue_on_error: false
        max_tokens: 4096
        timeout: 2m
`))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if len(p.Stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(p.Stages))
	}
	judge := p.Stages[0].OnEnter.Judge
	if judge.Instructions == "" {
		t.Fatal("expected judge instructions")
	}
	if judge.Model != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("model = %q, want anthropic/claude-sonnet-4-6", judge.Model)
	}
	if len(judge.Inputs) != 3 {
		t.Fatalf("expected 3 inputs, got %d", len(judge.Inputs))
	}
	if judge.Inputs[0] != pipeline.JudgeInputIssue {
		t.Fatalf("input[0] = %q, want issue", judge.Inputs[0])
	}
	if judge.Inputs[1] != pipeline.JudgeInputGitDiff {
		t.Fatalf("input[1] = %q, want git_diff", judge.Inputs[1])
	}
	if judge.Inputs[2] != pipeline.JudgeInputTestOutput {
		t.Fatalf("input[2] = %q, want test_output", judge.Inputs[2])
	}
	if judge.Require.Verdict != "pass" {
		t.Fatalf("require.verdict = %q, want pass", judge.Require.Verdict)
	}
	if judge.Output != "review_result" {
		t.Fatalf("output = %q, want review_result", judge.Output)
	}
	if judge.ContinueOnError {
		t.Fatal("expected continue_on_error=false")
	}
	if judge.MaxTokens != 4096 {
		t.Fatalf("max_tokens = %d, want 4096", judge.MaxTokens)
	}
	if judge.Timeout != "2m" {
		t.Fatalf("timeout = %q, want 2m", judge.Timeout)
	}
}

func TestParseJudgeActionMinimal(t *testing.T) {
	p, err := pipeline.Parse([]byte(`
stages:
  - id: review
    on_enter:
      judge:
        inputs:
          - issue
        instructions: "Review this"
`))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	judge := p.Stages[0].OnEnter.Judge
	if judge.Instructions != "Review this" {
		t.Fatalf("instructions = %q", judge.Instructions)
	}
	if judge.Model != "" {
		t.Fatalf("expected empty model, got %q", judge.Model)
	}
	if judge.Require.Verdict != "" {
		t.Fatalf("expected empty require.verdict, got %q", judge.Require.Verdict)
	}
}

func TestJudgeInputConstants(t *testing.T) {
	if pipeline.JudgeInputIssue != "issue" {
		t.Fatalf("JudgeInputIssue = %q", pipeline.JudgeInputIssue)
	}
	if pipeline.JudgeInputGitDiff != "git_diff" {
		t.Fatalf("JudgeInputGitDiff = %q", pipeline.JudgeInputGitDiff)
	}
	if pipeline.JudgeInputTestOutput != "test_output" {
		t.Fatalf("JudgeInputTestOutput = %q", pipeline.JudgeInputTestOutput)
	}
	if pipeline.JudgeInputFiles != "files" {
		t.Fatalf("JudgeInputFiles = %q", pipeline.JudgeInputFiles)
	}
}

func TestStageForJudgeVerdictPass(t *testing.T) {
	p, err := pipeline.Parse([]byte(`
stages:
  - id: review
    on_enter:
      judge:
        instructions: "Review"
        inputs: [issue]
  - id: pr
    triggers:
      - judge_verdict: pass
    on_enter:
      inject: "Create PR"
  - id: fix
    triggers:
      - judge_verdict: fail
    on_enter:
      inject: "Fix issues"
`))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	passStage := p.StageForJudgeVerdict("pass")
	if passStage == nil {
		t.Fatal("expected stage for pass verdict")
	}
	if passStage.ID != "pr" {
		t.Fatalf("pass stage id = %q, want pr", passStage.ID)
	}
	failStage := p.StageForJudgeVerdict("fail")
	if failStage == nil {
		t.Fatal("expected stage for fail verdict")
	}
	if failStage.ID != "fix" {
		t.Fatalf("fail stage id = %q, want fix", failStage.ID)
	}
}

func TestStageForJudgeVerdictNoMatch(t *testing.T) {
	p, err := pipeline.Parse([]byte(`
stages:
  - id: review
    on_enter:
      judge:
        instructions: "Review"
        inputs: [issue]
`))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if p.StageForJudgeVerdict("pass") != nil {
		t.Fatal("expected nil for no match")
	}
}

func TestStageForJudgeVerdictCaseInsensitive(t *testing.T) {
	p, err := pipeline.Parse([]byte(`
stages:
  - id: pr
    triggers:
      - judge_verdict: PASS
`))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	stage := p.StageForJudgeVerdict("pass")
	if stage == nil {
		t.Fatal("expected case-insensitive match")
	}
}

func TestParseGateAction(t *testing.T) {
	p, err := pipeline.Parse([]byte(`
stages:
  - id: android_validation
    label: "Android Validation"
    on_enter:
      run:
        command: python3 scripts/run_android_codebuild.py --source-dir next_mobile
        output: android_validation
    gate:
      output: android_validation
      pass:
        path: status
        values:
          - passed
          - skipped
      fail:
        path: status
        values:
          - failed
          - error
      required: true
      treat_skipped_as_pass: true
`))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if len(p.Stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(p.Stages))
	}
	stage := p.Stages[0]
	if stage.Gate == nil {
		t.Fatal("expected gate")
	}
	if stage.Gate.Output != "android_validation" {
		t.Fatalf("gate.output = %q, want android_validation", stage.Gate.Output)
	}
	if stage.Gate.Pass.Path != "status" {
		t.Fatalf("gate.pass.path = %q, want status", stage.Gate.Pass.Path)
	}
	if len(stage.Gate.Pass.Values) != 2 {
		t.Fatalf("expected 2 pass values, got %d", len(stage.Gate.Pass.Values))
	}
	if stage.Gate.Pass.Values[0] != "passed" {
		t.Fatalf("gate.pass.values[0] = %q", stage.Gate.Pass.Values[0])
	}
	if stage.Gate.Fail.Path != "status" {
		t.Fatalf("gate.fail.path = %q, want status", stage.Gate.Fail.Path)
	}
	if len(stage.Gate.Fail.Values) != 2 {
		t.Fatalf("expected 2 fail values, got %d", len(stage.Gate.Fail.Values))
	}
	if !stage.Gate.Required {
		t.Fatal("expected gate.required=true")
	}
	if !stage.Gate.TreatSkippedAsPass {
		t.Fatal("expected gate.treat_skipped_as_pass=true")
	}
}

func TestParseGateResultTrigger(t *testing.T) {
	p, err := pipeline.Parse([]byte(`
stages:
  - id: create_pr
    triggers:
      - gate_result:
          stage: android_validation
          verdict: pass
  - id: fix_android
    triggers:
      - gate_result:
          stage: android_validation
          verdict: fail
`))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if len(p.Stages) != 2 {
		t.Fatalf("expected 2 stages, got %d", len(p.Stages))
	}
	passStage := p.StageForGateResult("android_validation", "pass")
	if passStage == nil {
		t.Fatal("expected stage for gate_result pass")
	}
	if passStage.ID != "create_pr" {
		t.Fatalf("pass stage id = %q, want create_pr", passStage.ID)
	}
	failStage := p.StageForGateResult("android_validation", "fail")
	if failStage == nil {
		t.Fatal("expected stage for gate_result fail")
	}
	if failStage.ID != "fix_android" {
		t.Fatalf("fail stage id = %q, want fix_android", failStage.ID)
	}
}

func TestStageForGateResultCaseInsensitive(t *testing.T) {
	p, err := pipeline.Parse([]byte(`
stages:
  - id: pr
    triggers:
      - gate_result:
          stage: android_validation
          verdict: PASS
`))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	stage := p.StageForGateResult("android_validation", "pass")
	if stage == nil {
		t.Fatal("expected case-insensitive match")
	}
}

func TestParseOutputMatchesTrigger(t *testing.T) {
	p, err := pipeline.Parse([]byte(`
stages:
  - id: create_pr
    triggers:
      - output_matches:
          output: android_validation
          path: status
          any_of:
            - passed
            - skipped
`))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if len(p.Stages) != 1 {
		t.Fatalf("expected 1 stage, got %d", len(p.Stages))
	}
	triggers := p.Stages[0].Triggers
	if len(triggers) != 1 {
		t.Fatalf("expected 1 trigger, got %d", len(triggers))
	}
	om := triggers[0].OutputMatches
	if om == nil {
		t.Fatal("expected output_matches trigger")
	}
	if om.Output != "android_validation" {
		t.Fatalf("output = %q, want android_validation", om.Output)
	}
	if om.Path != "status" {
		t.Fatalf("path = %q, want status", om.Path)
	}
	if len(om.AnyOf) != 2 {
		t.Fatalf("expected 2 any_of values, got %d", len(om.AnyOf))
	}
	if om.AnyOf[0] != "passed" {
		t.Fatalf("any_of[0] = %q", om.AnyOf[0])
	}
}

func TestStageForOutputMatches(t *testing.T) {
	p, err := pipeline.Parse([]byte(`
stages:
  - id: create_pr
    triggers:
      - output_matches:
          output: build_info
          path: status
          any_of:
            - success
            - skipped
`))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	outputs := map[string]map[string]interface{}{
		"build_info": {"status": "success"},
	}
	stage := p.StageForOutputMatches(outputs)
	if stage == nil {
		t.Fatal("expected stage for output_matches")
	}
	if stage.ID != "create_pr" {
		t.Fatalf("stage id = %q, want create_pr", stage.ID)
	}
	// No match case
	outputs2 := map[string]map[string]interface{}{
		"build_info": {"status": "failed"},
	}
	if p.StageForOutputMatches(outputs2) != nil {
		t.Fatal("expected nil for no match")
	}
}

func TestGetJSONPath(t *testing.T) {
	m := map[string]interface{}{
		"status": "passed",
		"details": map[string]interface{}{
			"duration": "45s",
			"nested": map[string]interface{}{
				"value": 42,
			},
		},
	}
	if v := pipeline.GetJSONPath(m, "status"); v != "passed" {
		t.Fatalf("status = %v, want passed", v)
	}
	if v := pipeline.GetJSONPath(m, "details.duration"); v != "45s" {
		t.Fatalf("details.duration = %v, want 45s", v)
	}
	if v := pipeline.GetJSONPath(m, "details.nested.value"); v != 42 {
		t.Fatalf("details.nested.value = %v, want 42", v)
	}
	if v := pipeline.GetJSONPath(m, "missing"); v != nil {
		t.Fatalf("missing = %v, want nil", v)
	}
	if v := pipeline.GetJSONPath(m, "details.missing"); v != nil {
		t.Fatalf("details.missing = %v, want nil", v)
	}
}
