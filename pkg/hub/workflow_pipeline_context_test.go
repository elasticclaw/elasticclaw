package hub

import (
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestWorkflowPipelineContextUsesWorkspaceIssueTracker(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	workspace := &types.WorkspaceConfig{
		Name: "elasticclaw",
		Files: map[string]string{
			"elasticclaw-config.yaml": "schema_version: v1\nname: elasticclaw\n",
		},
	}
	if err := saveExternalWorkspace(workspace); err != nil {
		t.Fatalf("save workspace: %v", err)
	}
	workflow := &types.WorkflowConfig{
		Name:         "github-issue",
		Integration:  "github-issues",
		PipelineYAML: "stages:\n  - id: working\n    entry: true\n",
	}
	if err := saveExternalWorkflows("elasticclaw", []*types.WorkflowConfig{workflow}); err != nil {
		t.Fatalf("save workflows: %v", err)
	}
	if err := saveWorkspaceIssueTracker("elasticclaw", "github-issues", "default", workspaceIssueTracker{Token: "workspace-token"}); err != nil {
		t.Fatalf("save issue tracker: %v", err)
	}

	loadedWorkspace, loadedWorkflow, ok := loadWorkflowPipelineContext("elasticclaw", "github-issue")
	if !ok {
		t.Fatal("expected workflow pipeline context to load")
	}
	ctx := pipelineContext{
		Workspace: loadedWorkspace,
		Workflow:  loadedWorkflow,
		IssueID:   "elasticclaw/elasticclaw/298",
	}
	if ctx.Factory != nil {
		t.Fatalf("workflow pipeline context synthesized a factory: %#v", ctx.Factory)
	}
	if got := s.resolveGitHubIssuesTokenForPipeline(ctx); got != "workspace-token" {
		t.Fatalf("token = %q, want workspace-token", got)
	}
	if parsePipelineForContext(ctx) == nil {
		t.Fatal("expected workflow pipeline yaml to parse")
	}
}
