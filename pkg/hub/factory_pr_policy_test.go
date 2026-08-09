package hub

import (
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestDefaultFactoryPRPolicyText(t *testing.T) {
	policy := defaultFactoryPRPolicy

	required := []string{
		"Unless the issue, factory/workflow instructions, template instructions, or a later explicit user instruction says not to create a PR",
		"creating a pull request",
		"Open a pull request for the branch",
		"[DONE] https://github.com/org/repo/pull/N",
		"do not send `[DONE]`",
		"Report the blocker",
	}
	for _, want := range required {
		if !strings.Contains(policy, want) {
			t.Fatalf("defaultFactoryPRPolicy missing %q:\n%s", want, policy)
		}
	}
}

func TestFactoryContextsIncludeDefaultPRPolicy(t *testing.T) {
	var linearPayload linearWebhookPayload
	linearPayload.Data.Identifier = "ELA-128"
	linearPayload.Data.Title = "Create PR by default"
	linearPayload.Data.Description = "Agent should open a PR."

	var githubIssuesPayload githubIssuesWebhookPayload
	githubIssuesPayload.Issue.Number = 128
	githubIssuesPayload.Issue.Title = "Create PR by default"
	githubIssuesPayload.Issue.Body = "Agent should open a PR."
	githubIssuesPayload.Repository.FullName = "elasticclaw/elasticclaw"

	shortcut := shortcutAction{
		Name:        "Create PR by default",
		AppURL:      "https://app.shortcut.com/story/128",
		Description: "Agent should open a PR.",
	}

	var externalPayload externalWebhookPayload
	externalPayload.EventType = "generic"
	externalPayload.Repository.FullName = "elasticclaw/elasticclaw"
	externalFactory := &types.FactoryConfig{
		Name:        "external-release",
		Integration: "external",
		Template:    "elasticclaw",
		ExternalTrigger: &types.ExternalTrigger{
			Source: "generic-webhook",
		},
	}

	manualFactory := &types.FactoryConfig{
		Name:        "manual-fix",
		Integration: "linear",
		Template:    "elasticclaw",
		Inputs: []types.FactoryInput{{
			Name:        "task",
			Type:        "string",
			Description: "Task to complete",
		}},
	}

	manualWorkflow := &types.WorkflowConfig{
		Name:        "manual-workflow",
		Integration: "manual",
		Inputs: []types.FactoryInput{{
			Name:        "task",
			Type:        "string",
			Description: "Task to complete",
		}},
	}

	cases := map[string]string{
		"linear issue":            buildLinearContext(linearPayload, true),
		"github issue":            buildGitHubIssuesContext(githubIssuesPayload, nil, ""),
		"shortcut story":          buildShortcutContext(shortcut, "128"),
		"external event":          buildExternalEventContext(externalPayload, externalFactory),
		"manual factory trigger":  buildManualTriggerContext(manualFactory, map[string]string{"task": "fix issue 128"}),
		"manual workflow trigger": buildWorkflowManualTriggerContext(manualWorkflow, map[string]string{"task": "fix issue 128"}, ""),
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(content, "## PR Completion Policy") {
				t.Fatalf("context missing PR policy:\n%s", content)
			}
			if !strings.Contains(content, "Unless the issue, factory/workflow instructions") {
				t.Fatalf("context missing unless-instructed-otherwise language:\n%s", content)
			}
			if !strings.Contains(content, "[DONE] https://github.com/org/repo/pull/N") {
				t.Fatalf("context missing DONE PR URL contract:\n%s", content)
			}
		})
	}
}

func TestWorkflowManualTriggerContextIncludesTaskAndWorkspaceContext(t *testing.T) {
	workflow := &types.WorkflowConfig{
		Name: "dependency-update-go",
		Stages: []types.WorkflowStage{{
			ID:    "working",
			Entry: true,
			OnEnter: map[string]interface{}{
				"inject": "Workflow task: run the Go dependency update maintenance workflow for vandoor.\n\nReview {{ .Outputs.dependency_updates.files_changed }}.",
			},
		}},
	}

	content := buildWorkflowManualTriggerContext(workflow, map[string]string{}, "Workspace context for vandoor")

	for _, want := range []string{
		"# Manual Workflow Trigger: dependency-update-go",
		"No manual inputs were provided.",
		"Workflow task: run the Go dependency update maintenance workflow for vandoor.",
		"Review {{ .Outputs.dependency_updates.files_changed }}.",
		"## Workspace Context",
		"Workspace context for vandoor",
		"## PR Completion Policy",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("context missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "Read the trigger inputs fully") {
		t.Fatalf("context should not ask for trigger inputs when workflow has none:\n%s", content)
	}
}

func TestAppendWorkflowPreviewContextRequiresDynamicPersistentServer(t *testing.T) {
	content := appendWorkflowPreviewContext(
		"Existing context\n",
		&types.WorkflowPreview{Port: 4173, Label: "QA build"},
	)

	for _, want := range []string{
		"Existing context",
		"## Browser Preview Required",
		"repository's own documented start command and package manager",
		"`0.0.0.0:4173`",
		"tool subprocesses may be reaped",
		"/preview/start",
		"ElasticClaw will run the command outside the tool-call lifecycle",
		"Do not assume the route is `/`",
		"including a visible marker from the change",
		"`path`",
		`{"path":"/setup"}`,
		"/preview/ready",
		"ElasticClaw detaches and stops the AI agent",
		"credential-free preview until its TTL expires",
		"Preview ready: [Open QA preview](PREVIEW_URL)",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("preview context missing %q:\n%s", want, content)
		}
	}
}

func TestGitHubPRContextDoesNotRequestNewPR(t *testing.T) {
	var payload githubPRPayload
	payload.Number = 7
	payload.Repository.FullName = "elasticclaw/elasticclaw"
	payload.PullRequest.HTMLURL = "https://github.com/elasticclaw/elasticclaw/pull/7"
	payload.PullRequest.Title = "Existing PR"
	payload.PullRequest.User.Login = "octocat"
	payload.PullRequest.Head.Ref = "feature"
	payload.PullRequest.Base.Ref = "main"

	content := buildGitHubPRContext(payload)

	if strings.Contains(content, "## PR Completion Policy") {
		t.Fatalf("existing PR context must not request a new PR:\n%s", content)
	}
	if !strings.Contains(content, "gh pr checkout https://github.com/elasticclaw/elasticclaw/pull/7") {
		t.Fatalf("existing PR checkout instruction missing:\n%s", content)
	}
}
