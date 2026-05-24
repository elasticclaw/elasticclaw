package pipeline_test

import (
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
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
