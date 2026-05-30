package hub

import (
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestGitHubIssuesWorkflowTriggerMatchesLabelEvent(t *testing.T) {
	trigger := &types.WorkflowTrigger{
		GitHubIssues: &types.GitHubIssuesWorkflowTrigger{
			Event:        "issue_labeled",
			Repositories: []string{"elasticclaw/elasticclaw"},
			States:       []string{"open"},
			Labels:       []string{"agent-ready"},
			Labelers:     []string{"*"},
		},
	}
	var payload githubIssuesWebhookPayload
	payload.Action = "labeled"
	payload.Sender.Login = "marc"

	if !githubIssuesWorkflowTriggerMatches(trigger, payload, "open", map[string]bool{"agent-ready": true}) {
		t.Fatal("expected trigger to match labeled open issue with required label")
	}
	if githubIssuesWorkflowTriggerMatches(trigger, payload, "closed", map[string]bool{"agent-ready": true}) {
		t.Fatal("expected state filter to reject closed issue")
	}
	payload.Action = "opened"
	if githubIssuesWorkflowTriggerMatches(trigger, payload, "open", map[string]bool{"agent-ready": true}) {
		t.Fatal("expected event filter to reject opened action")
	}
}
