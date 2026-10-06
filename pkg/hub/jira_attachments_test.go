package hub

import (
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestBuildJiraContextOmitsAttachmentContentsAndPointsToOnDemandWorkflow(t *testing.T) {
	payload := jiraWebhookPayload{}
	payload.Issue.Key = "BUG-42"
	payload.Issue.Fields.Project.Key = "BUG"
	payload.Issue.Fields.Status.Name = "Ready"
	payload.Issue.Fields.Summary = "UI fails after upload"
	payload.Issue.Fields.Description = "Steps to reproduce without embedded binary data."

	context := (&Server{hubCfg: &types.HubConfig{}}).buildJiraContext(payload)
	for _, want := range []string{
		"Issue: BUG-42",
		"Title: UI fails after upload",
		"attachment contents are intentionally omitted",
		"Jira attachment workflow in TOOLS.md",
		"sandbox-provided Jira credentials",
	} {
		if !strings.Contains(context, want) {
			t.Errorf("Jira context missing %q:\n%s", want, context)
		}
	}
	for _, forbidden := range []string{
		"JIRA_API_KEY=",
		"Authorization:",
		"attachment payload",
	} {
		if strings.Contains(context, forbidden) {
			t.Errorf("Jira context contains forbidden attachment or credential material %q:\n%s", forbidden, context)
		}
	}
}
