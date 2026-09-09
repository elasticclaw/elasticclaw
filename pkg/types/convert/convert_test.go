package convert_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types/convert"
	v2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

const sampleWorkspaceV1 = `
schema_version: v1
name: elasticclaw

provider: daytona
nix: true
docker: true

repositories:
  - repo: elasticclaw/elasticclaw
    permissions: write
  - repo: elasticclaw/elasticclaw.ai
    permissions: write

env:
  LINEAR_API_KEY:
    secret: LINEAR_API_KEY
  ENVIRONMENT: dev
`

const sampleWorkflowV1 = `
schema_version: v1
name: github-issue

trigger:
  github_issues:
    event: issue_labeled
    repositories:
      - elasticclaw/elasticclaw
    states:
      - open
    labels:
      - agent-ready

stages:
  - id: working
    label: Working
    entry: true
    on_enter:
      inject: |
        Start work. Say [READY_TO_COMMIT] when done.
  - id: pre_commit
    label: Pre-Commit Checks
    triggers:
      - message_contains: "[READY_TO_COMMIT]"
    on_enter:
      run:
        command: make test
  - id: pr_opened
    label: PR Opened
    triggers:
      - message_contains: "[DONE]"
  - id: merged
    label: Merged
    terminal: true
    triggers:
      - pr_merged: {}
  - id: closed_no_merge
    label: Closed Without Merge
    terminal: true
    triggers:
      - pr_closed: {}
`

func TestConvertWorkspaceV1ToV2(t *testing.T) {
	res, err := convert.Convert(convert.KindWorkspace, []byte(sampleWorkspaceV1), convert.Options{To: "2"})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if res.From != "v1" || res.To != "2" {
		t.Fatalf("from/to = %s/%s", res.From, res.To)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("expected migration warnings")
	}

	resolved, err := v2.ParseAndValidateWorkspace(res.Output)
	if err != nil {
		t.Fatalf("validate converted: %v\n%s", err, res.Output)
	}
	if resolved.Workspace.Name != "elasticclaw" {
		t.Fatalf("name = %q", resolved.Workspace.Name)
	}
	if resolved.Workspace.Execution == nil || resolved.Workspace.Execution.Provider != "daytona" {
		t.Fatalf("execution = %#v", resolved.Workspace.Execution)
	}
	if !resolved.Workspace.HasRepository("elasticclaw-elasticclaw") {
		t.Fatalf("missing repo key, repos=%v\n%s", resolved.Workspace.Repositories, res.Output)
	}
	if !resolved.Workspace.HasCredential("linear_api_key") {
		t.Fatalf("expected linear_api_key credential\n%s", res.Output)
	}
	// Inline env dropped with warning
	joined := strings.Join(res.Warnings, "\n")
	if !strings.Contains(joined, "env.ENVIRONMENT") {
		t.Fatalf("expected warning about inline env, got:\n%s", joined)
	}
}

func TestConvertWorkflowV1ToV2(t *testing.T) {
	res, err := convert.Convert(convert.KindWorkflow, []byte(sampleWorkflowV1), convert.Options{To: "2"})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	resolved, err := v2.ParseAndValidateWorkflow(res.Output)
	if err != nil {
		t.Fatalf("validate converted: %v\n%s", err, res.Output)
	}
	if resolved.Workflow.InitialState != "working" {
		t.Fatalf("initial_state = %q", resolved.Workflow.InitialState)
	}
	if !resolved.Workflow.States["merged"].Terminal {
		t.Fatal("merged should be terminal")
	}
	if resolved.Workflow.Enabled || !strings.Contains(string(res.Output), "enabled: false") {
		t.Fatalf("converted workflow must be an explicitly inactive draft:\n%s", res.Output)
	}

	// Deterministic PR edges present; message_contains not turned into trusted transitions.
	hasMerged := false
	hasClosed := false
	for _, tr := range resolved.Workflow.Transitions {
		if tr.On == "pull_request.merged" && tr.To == "merged" {
			hasMerged = true
		}
		if tr.On == "pull_request.closed" && tr.To == "closed_no_merge" {
			hasClosed = true
		}
		if strings.Contains(tr.On, "message") || strings.Contains(tr.On, "DONE") {
			t.Fatalf("must not create trusted transition from message control: %#v", tr)
		}
	}
	if !hasMerged || !hasClosed {
		t.Fatalf("expected pr_merged/pr_closed transitions, got %#v\n%s", resolved.Workflow.Transitions, res.Output)
	}

	joined := strings.Join(res.Warnings, "\n")
	for _, want := range []string{"message_contains", "[DONE]", "[READY_TO_COMMIT]"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected warning mentioning %q, got:\n%s", want, joined)
		}
	}
}

func TestConvertWorkflowPRStartedUsesVerifiedDeliveryEdge(t *testing.T) {
	input := []byte(`
schema_version: v1
name: pr-lifecycle
stages:
  - id: build
    entry: true
  - id: review
    triggers:
      - pr_opened: {}
`)
	res, err := convert.Convert(convert.KindWorkflow, input, convert.Options{To: "2"})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := v2.ParseAndValidateWorkflow(res.Output)
	if err != nil {
		t.Fatalf("validate converted workflow: %v\n%s", err, res.Output)
	}
	found := false
	for _, transition := range resolved.Workflow.Transitions {
		if transition.To != "review" {
			continue
		}
		found = true
		if transition.On != "delivery.verified" {
			t.Fatalf("PR-start transition event = %q, want delivery.verified", transition.On)
		}
		delivery, ok := transition.When["delivery"].(map[string]interface{})
		if !ok {
			t.Fatalf("PR-start transition predicate = %#v", transition.When)
		}
		open, ok := delivery["open"].(map[string]interface{})
		if !ok || open["not_equals"] == nil {
			t.Fatalf("PR-start transition predicate = %#v", transition.When)
		}
	}
	if !found {
		t.Fatalf("converted workflow has no transition to review:\n%s", res.Output)
	}
}

func TestConvertWorkflowPairWithConvertedWorkspace(t *testing.T) {
	ws, err := convert.Convert(convert.KindWorkspace, []byte(sampleWorkspaceV1), convert.Options{To: "2"})
	if err != nil {
		t.Fatal(err)
	}
	wf, err := convert.Convert(convert.KindWorkflow, []byte(sampleWorkflowV1), convert.Options{
		To:            "2",
		WorkspaceYAML: ws.Output,
	})
	if err != nil {
		t.Fatalf("pair convert: %v", err)
	}
	if _, _, err := v2.ParseAndValidateWorkflowPair(wf.Output, ws.Output); err != nil {
		t.Fatalf("pair validate: %v", err)
	}
}

func TestConvertWorkflowV1CommentIssueEmitsWarning(t *testing.T) {
	const commentIssueWorkflow = `
schema_version: v1
name: comment-issue-workflow

trigger:
  linear:
    team_key: ELA
    labels:
      - agent-ready

stages:
  - id: working
    label: Working
    entry: true
    on_enter:
      inject: |
        Start work.
      comment_issue: "Starting work on {{.Issue.Identifier}}"
  - id: done
    label: Done
    terminal: true
`
	res, err := convert.Convert(convert.KindWorkflow, []byte(commentIssueWorkflow), convert.Options{To: "2"})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	joined := strings.Join(res.Warnings, "\n")
	if !strings.Contains(joined, "on_enter.comment_issue: not auto-converted") {
		t.Fatalf("expected comment_issue warning, got:\n%s", joined)
	}
	// Sibling inject must still convert into an agent.task effect.
	resolved, err := v2.ParseAndValidateWorkflow(res.Output)
	if err != nil {
		t.Fatalf("validate converted: %v\n%s", err, res.Output)
	}
	if resolved.Workflow.States["working"].OnEnter == nil || len(resolved.Workflow.States["working"].OnEnter.Effects) == 0 {
		t.Fatalf("expected working state to retain converted inject effect:\n%s", res.Output)
	}
}

func TestConvertRejectsUnknownTarget(t *testing.T) {
	_, err := convert.Convert(convert.KindWorkspace, []byte(sampleWorkspaceV1), convert.Options{To: "3"})
	if err == nil || !strings.Contains(err.Error(), "no converter") {
		t.Fatalf("error = %v, want no converter", err)
	}
}

func TestConvertRejectsAlreadyTarget(t *testing.T) {
	ws, err := convert.Convert(convert.KindWorkspace, []byte(sampleWorkspaceV1), convert.Options{To: "2"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = convert.Convert(convert.KindWorkspace, ws.Output, convert.Options{To: "2"})
	if err == nil || !strings.Contains(err.Error(), "already schema version") {
		t.Fatalf("error = %v", err)
	}
}

func TestExampleGitHubIssueWorkflowConverts(t *testing.T) {
	path := filepath.Join("..", "..", "..", "examples", "workflows", "github-issue.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		// try from module root relative to this package: pkg/types/convert -> ../../../examples
		path = filepath.Join("..", "..", "..", "examples", "workflows", "github-issue.yaml")
		data, err = os.ReadFile(path)
		if err != nil {
			t.Skipf("example workflow not found: %v", err)
		}
	}
	res, err := convert.Convert(convert.KindWorkflow, data, convert.Options{To: "2"})
	if err != nil {
		t.Fatalf("convert example: %v", err)
	}
	if _, err := v2.ParseAndValidateWorkflow(res.Output); err != nil {
		t.Fatalf("validate example conversion: %v\n%s", err, res.Output)
	}
}
