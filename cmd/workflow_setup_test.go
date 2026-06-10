package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/workflowsetup"
)

func TestWorkflowSetupCLICreateGitHubIssueWritesDefaultWorkflowPath(t *testing.T) {
	withTempWorkingDir(t)
	if err := os.MkdirAll(filepath.Join(".elasticclaw", "workspaces", "engineering"), 0755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	out, err := executeWorkflowCommand(t,
		"create", "github-issue",
		"--workspace", "engineering",
		"--name", "issue-triage",
		"--repo", "owner/repo",
		"--concurrency-group", "repo:owner/repo",
		"--label", "agent-ready",
	)
	if err != nil {
		t.Fatalf("workflow create: %v", err)
	}

	path := filepath.Join(".elasticclaw", "workspaces", "engineering", "workflows", "issue-triage.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rendered workflow: %v", err)
	}
	rendered := string(data)
	for _, want := range []string{
		"name: issue-triage",
		"github_issues:",
		"owner/repo",
		"enable_manual_trigger: true",
		"name: issue_number",
		"concurrency_group: repo:owner/repo",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered workflow missing %q:\n%s", want, rendered)
		}
	}
	if !strings.Contains(out, path) {
		t.Fatalf("output %q did not mention path %q", out, path)
	}

	if _, err := executeWorkflowCommand(t, "validate", "--workspace", "engineering", path); err != nil {
		t.Fatalf("workflow validate generated file: %v", err)
	}
}

func TestWorkflowSetupCLICreateGitHubIssueAcceptsAdvancedLifecycleFlags(t *testing.T) {
	withTempWorkingDir(t)
	if err := os.MkdirAll(filepath.Join(".elasticclaw", "workspaces", "engineering"), 0755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	_, err := executeWorkflowCommand(t,
		"create", "github-issue",
		"--workspace", "engineering",
		"--name", "issue-precommit",
		"--repo", "owner/repo",
		"--include-pre-commit",
		"--pre-commit-command", "go test ./pkg/workflowsetup",
		"--pre-commit-ready-signal", "[READY_TO_TEST]",
		"--trigger-label", "agent-ready",
		"--working-label", "agent-working",
		"--review-label", "agent-review",
		"--done-label", "agent-done",
		"--closed-label", "agent-needs-attention",
		"--state", "open",
		"--labeler", "octocat",
	)
	if err != nil {
		t.Fatalf("workflow create: %v", err)
	}

	path := filepath.Join(".elasticclaw", "workspaces", "engineering", "workflows", "issue-precommit.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rendered workflow: %v", err)
	}
	rendered := string(data)
	for _, want := range []string{
		"id: pre_commit",
		"command: go test ./pkg/workflowsetup",
		"[READY_TO_TEST]",
		"octocat",
		"agent-review",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered workflow missing %q:\n%s", want, rendered)
		}
	}
}

func TestWorkflowSetupCLICreateLinearAcceptsStatusFlags(t *testing.T) {
	withTempWorkingDir(t)
	if err := os.MkdirAll(filepath.Join(".elasticclaw", "workspaces", "engineering"), 0755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	_, err := executeWorkflowCommand(t,
		"create", "linear-status",
		"--workspace", "engineering",
		"--name", "linear-lifecycle",
		"--integration-workspace", "product",
		"--team", "ENG",
		"--trigger-status", "Ready for Agent",
		"--working-status", "In Progress",
		"--pr-opened-status", "In Review",
		"--merged-status", "Done",
		"--closed-no-merge-status", "Needs Attention",
	)
	if err != nil {
		t.Fatalf("workflow create: %v", err)
	}

	path := filepath.Join(".elasticclaw", "workspaces", "engineering", "workflows", "linear-lifecycle.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rendered workflow: %v", err)
	}
	rendered := string(data)
	for _, want := range []string{
		"team: ENG",
		"Ready for Agent",
		"status: In Progress",
		"status: In Review",
		"status: Done",
		"status: Needs Attention",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered workflow missing %q:\n%s", want, rendered)
		}
	}
}

func TestWorkflowSetupCLICreateRequiresExistingWorkspaceOrCreateWorkspace(t *testing.T) {
	withTempWorkingDir(t)

	_, err := executeWorkflowCommand(t,
		"create", "manual-task",
		"--workspace", "missing",
		"--name", "manual-run",
	)
	if err == nil {
		t.Fatalf("workflow create error = nil, want missing workspace error")
	}
	if !strings.Contains(err.Error(), "elasticclaw workspace create") {
		t.Fatalf("workflow create error = %q, want workspace create hint", err.Error())
	}
}

func TestWorkflowSetupCLIValidateReturnsErrorForCriticalFailure(t *testing.T) {
	withTempWorkingDir(t)
	writeWorkflowSetupCLIWorkspaceConfig(t, "engineering")
	path := writeWorkflowSetupCLIFile(t, "bad.yaml", `
name: issue-triage
integration: github-issues
enable_manual_trigger: true
pipeline_yaml: |
  stages:
    - id: working
`)

	out, err := executeWorkflowCommand(t, "validate", "--workspace", "engineering", path)
	if err == nil {
		t.Fatalf("workflow validate error = nil, want critical failure")
	}
	if !strings.Contains(out, "critical") {
		t.Fatalf("human validation output %q missing critical summary", out)
	}
}

func TestWorkflowSetupCLIValidateJSONUsesValidateResponseShape(t *testing.T) {
	withTempWorkingDir(t)
	writeWorkflowSetupCLIWorkspaceConfig(t, "engineering")
	path := writeWorkflowSetupCLIFile(t, "bad.yaml", `
name: issue-triage
integration: github-issues
enable_manual_trigger: true
pipeline_yaml: |
  stages:
    - id: working
`)
	oldJSONOut := jsonOut
	jsonOut = true
	t.Cleanup(func() {
		jsonOut = oldJSONOut
	})

	out, err := executeWorkflowCommand(t, "validate", "--workspace", "engineering", path)
	if err == nil {
		t.Fatalf("workflow validate error = nil, want critical failure")
	}

	var resp workflowsetup.ValidateResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("decode validation JSON as ValidateResponse: %v\n%s", err, out)
	}
	if resp.OK {
		t.Fatalf("ValidateResponse.OK = true, want false")
	}
	if resp.Summary.Critical == 0 {
		t.Fatalf("ValidateResponse.Summary.Critical = 0, want critical diagnostic: %#v", resp)
	}
}

func executeWorkflowCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()

	cmd := WorkflowCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func withTempWorkingDir(t *testing.T) {
	t.Helper()

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir temp: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldWd); err != nil {
			t.Fatalf("restore working directory: %v", err)
		}
	})
}

func writeWorkflowSetupCLIWorkspaceConfig(t *testing.T, name string) {
	t.Helper()

	dir := filepath.Join(".elasticclaw", "workspaces", name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	data := "schema_version: v1\nname: " + name + "\nprovider: replicated\n"
	if err := os.WriteFile(filepath.Join(dir, "elasticclaw-config.yaml"), []byte(data), 0644); err != nil {
		t.Fatalf("write workspace config: %v", err)
	}
}

func writeWorkflowSetupCLIFile(t *testing.T, name, data string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}
