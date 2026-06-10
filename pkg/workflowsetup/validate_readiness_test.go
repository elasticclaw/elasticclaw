package workflowsetup

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestValidateReadinessProviderPrecedence(t *testing.T) {
	req := defaultReadinessRequest()
	req.Config = readinessGitHubIssueWorkflowYAML("issue-triage", "replicated", []string{"owner/repo"}, []string{"agent-ready"}, true, map[string]string{
		"WORKFLOW_SECRET": "hub_workflow_secret",
	})
	req.WorkspaceConfig = strings.Replace(validReadinessWorkspaceYAML(), "provider: replicated", "provider: daytona", 1)

	env := defaultReadinessEnv()
	env.snapshot.DefaultProvider = "exedev"
	env.snapshot.Providers = []ProviderRef{configuredProvider("replicated")}

	resp := ValidateReadiness(req, env)

	assertNoDiagnostic(t, resp, "readiness-provider-not-found")
	assertNoDiagnostic(t, resp, "readiness-provider-unconfigured")
	if resp.Summary.Critical != 0 {
		t.Fatalf("critical diagnostics = %d, want 0: %#v", resp.Summary.Critical, resp.Checks)
	}
}

func TestValidateReadinessTemplateProviderPrecedesSnapshotDefault(t *testing.T) {
	req := defaultReadinessRequest()
	req.Config = readinessGitHubIssueWorkflowYAML("issue-triage", "", []string{"owner/repo"}, []string{"agent-ready"}, true, map[string]string{
		"WORKFLOW_SECRET": "hub_workflow_secret",
	})
	req.WorkspaceConfig = strings.Replace(validReadinessWorkspaceYAML(), "provider: replicated", "provider: daytona", 1)

	env := defaultReadinessEnv()
	env.snapshot.DefaultProvider = "replicated"
	env.snapshot.Providers = []ProviderRef{configuredProvider("daytona")}

	resp := ValidateReadiness(req, env)

	assertNoDiagnostic(t, resp, "readiness-provider-not-found")
	assertNoDiagnostic(t, resp, "readiness-provider-unconfigured")
	if resp.Summary.Critical != 0 {
		t.Fatalf("critical diagnostics = %d, want 0: %#v", resp.Summary.Critical, resp.Checks)
	}
}

func TestValidateReadinessProviderRuntimeMatrix(t *testing.T) {
	for _, provider := range []string{"replicated", "daytona", "exedev"} {
		t.Run(provider, func(t *testing.T) {
			req := defaultReadinessRequest()
			req.Config = readinessGitHubIssueWorkflowYAML("issue-triage", provider, []string{"owner/repo"}, []string{"agent-ready"}, true, map[string]string{
				"WORKFLOW_SECRET": "hub_workflow_secret",
			})
			env := defaultReadinessEnv()
			env.snapshot.Providers = []ProviderRef{configuredProvider(provider)}

			resp := ValidateReadiness(req, env)

			assertNoDiagnostic(t, resp, "readiness-provider-runtime-unsupported")
			assertNoDiagnostic(t, resp, "readiness-provider-unconfigured")
			if resp.Summary.Critical != 0 {
				t.Fatalf("critical diagnostics = %d, want 0: %#v", resp.Summary.Critical, resp.Checks)
			}
		})
	}
}

func TestValidateReadinessExedevAllowsDefaultSSHAgent(t *testing.T) {
	req := defaultReadinessRequest()
	req.Config = readinessGitHubIssueWorkflowYAML("issue-triage", "exedev", []string{"owner/repo"}, []string{"agent-ready"}, true, map[string]string{
		"WORKFLOW_SECRET": "hub_workflow_secret",
	})
	env := defaultReadinessEnv()
	env.snapshot.Providers = []ProviderRef{{
		Name:          "exedev",
		Type:          "exedev",
		Provisionable: true,
	}}

	resp := ValidateReadiness(req, env)

	assertNoDiagnostic(t, resp, "readiness-provider-unconfigured")
	if resp.Summary.Critical != 0 {
		t.Fatalf("critical diagnostics = %d, want 0: %#v", resp.Summary.Critical, resp.Checks)
	}
}

func TestValidateReadinessBlocksDockerProviderRuntime(t *testing.T) {
	req := defaultReadinessRequest()
	req.Config = readinessGitHubIssueWorkflowYAML("issue-triage", "docker", []string{"owner/repo"}, []string{"agent-ready"}, true, map[string]string{
		"WORKFLOW_SECRET": "hub_workflow_secret",
	})
	env := defaultReadinessEnv()
	env.snapshot.Providers = []ProviderRef{configuredProvider("docker")}

	resp := ValidateReadiness(req, env)

	assertCriticalDiagnostic(t, resp, "readiness-provider-runtime-unsupported", "workflow.provider")
}

func TestValidateReadinessNoopProviderRequiresExplicitTestMode(t *testing.T) {
	req := ValidateRequest{
		WorkflowName: "manual-task",
		Config:       readinessManualWorkflowYAML("manual-task", "", map[string]string{}),
	}
	env := defaultReadinessEnv()
	env.snapshot.DefaultProvider = "noop"
	env.snapshot.Providers = []ProviderRef{{
		Name:           "noop",
		Type:           "noop",
		Provisionable:  true,
		CredentialsSet: true,
	}}

	resp := ValidateReadiness(req, env)
	assertCriticalDiagnostic(t, resp, "readiness-provider-noop-disabled", "snapshot.defaultProvider")

	resp = ValidateReadinessWithOptions(req, env, ReadinessOptions{AllowNoopProvider: true})
	assertNoDiagnostic(t, resp, "readiness-provider-noop-disabled")
	if resp.Summary.Critical != 0 {
		t.Fatalf("critical diagnostics = %d, want 0: %#v", resp.Summary.Critical, resp.Checks)
	}
}

func TestValidateReadinessRuntimeBlockers(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*readinessFakeEnvironment, *ValidateRequest)
		wantID string
		field  string
	}{
		{
			name: "missing claw token",
			mutate: func(env *readinessFakeEnvironment, req *ValidateRequest) {
				env.snapshot.ClawTokenSet = false
			},
			wantID: "readiness-claw-token-missing",
			field:  "snapshot.clawTokenSet",
		},
		{
			name: "no providers",
			mutate: func(env *readinessFakeEnvironment, req *ValidateRequest) {
				env.snapshot.Providers = nil
			},
			wantID: "readiness-provider-missing",
			field:  "snapshot.providers",
		},
		{
			name: "resolved provider missing",
			mutate: func(env *readinessFakeEnvironment, req *ValidateRequest) {
				env.snapshot.Providers = []ProviderRef{configuredProvider("daytona")}
			},
			wantID: "readiness-provider-not-found",
			field:  "workflow.provider",
		},
		{
			name: "provider not provisionable",
			mutate: func(env *readinessFakeEnvironment, req *ValidateRequest) {
				env.snapshot.Providers = []ProviderRef{{
					Name:           "replicated",
					Type:           "replicated",
					Provisionable:  false,
					CredentialsSet: true,
					TokenSet:       true,
				}}
			},
			wantID: "readiness-provider-unconfigured",
			field:  "workflow.provider",
		},
		{
			name: "no llm key",
			mutate: func(env *readinessFakeEnvironment, req *ValidateRequest) {
				env.snapshot.LLMKeys = nil
			},
			wantID: "readiness-llm-key-missing",
			field:  "snapshot.llmKeys",
		},
		{
			name: "model missing",
			mutate: func(env *readinessFakeEnvironment, req *ValidateRequest) {
				req.WorkspaceConfig = strings.Replace(req.WorkspaceConfig, "default_model: anthropic/claude-sonnet-4-5\n", "", 1)
				env.snapshot.DefaultModel = ""
			},
			wantID: "readiness-model-missing",
			field:  "workspace.default_model",
		},
		{
			name: "concurrency group missing",
			mutate: func(env *readinessFakeEnvironment, req *ValidateRequest) {
				req.Config = readinessGitHubIssueWorkflowYAML("issue-triage", "replicated", []string{"owner/repo"}, []string{"agent-ready"}, true, map[string]string{
					"WORKFLOW_SECRET": "hub_workflow_secret",
				})
				req.Config = strings.Replace(req.Config, "concurrency_group: global", "concurrency_group: repo:owner/repo", 1)
				env.snapshot.ConcurrencyGroups = []ConcurrencyGroupRef{{Name: "other", Limit: 1}}
			},
			wantID: "readiness-concurrency-group-missing",
			field:  "workflow.concurrency_group",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := defaultReadinessRequest()
			env := defaultReadinessEnv()
			tt.mutate(&env, &req)

			resp := ValidateReadiness(req, env)

			assertCriticalDiagnostic(t, resp, tt.wantID, tt.field)
		})
	}
}

func TestValidateReadinessSecretRefsResolve(t *testing.T) {
	tests := []struct {
		name      string
		mutateReq func(*ValidateRequest)
		wantField string
	}{
		{
			name: "workspace env secret",
			mutateReq: func(req *ValidateRequest) {
				req.WorkspaceConfig = strings.Replace(req.WorkspaceConfig, "secret: workspace_secret", "secret: missing_workspace_secret", 1)
			},
			wantField: "workspace.env.FROM_WORKSPACE.secret",
		},
		{
			name: "workspace template secret ref",
			mutateReq: func(req *ValidateRequest) {
				req.WorkspaceConfig = strings.Replace(req.WorkspaceConfig, "TEMPLATE_SECRET: hub_template_secret", "TEMPLATE_SECRET: missing_template_secret", 1)
			},
			wantField: "workspace.secret_refs.TEMPLATE_SECRET",
		},
		{
			name: "workflow secret ref",
			mutateReq: func(req *ValidateRequest) {
				req.Config = strings.Replace(req.Config, "WORKFLOW_SECRET: hub_workflow_secret", "WORKFLOW_SECRET: missing_workflow_secret", 1)
			},
			wantField: "workflow.secret_refs.WORKFLOW_SECRET",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := defaultReadinessRequest()
			tt.mutateReq(&req)

			resp := ValidateReadiness(req, defaultReadinessEnv())

			assertCriticalDiagnostic(t, resp, "readiness-secret-ref-missing", tt.wantField)
		})
	}
}

func TestValidateReadinessSecretRefsIgnoreWorkspaceYAMLSecretDeclarations(t *testing.T) {
	req := defaultReadinessRequest()
	req.WorkspaceConfig = `
name: issue-triage
provider: replicated
default_model: anthropic/claude-sonnet-4-5
secrets:
  - foo
secret_refs:
  X: foo
`
	env := defaultReadinessEnv()
	env.workspaceSecrets = nil

	resp := ValidateReadiness(req, env)

	assertCriticalDiagnostic(t, resp, "readiness-secret-ref-missing", "workspace.secret_refs.X")
}

func TestValidateReadinessWorkspaceSecretLoadFailureStillValidatesRefsAgainstHub(t *testing.T) {
	req := defaultReadinessRequest()
	env := defaultReadinessEnv()
	env.workspaceSecretsErr = fmt.Errorf("workspace secret store unavailable")

	resp := ValidateReadiness(req, env)

	assertWarningDiagnostic(t, resp, "readiness-workspace-secrets-not-checked", "workspace.secrets")
	assertCriticalDiagnostic(t, resp, "readiness-secret-ref-missing", "workspace.env.FROM_WORKSPACE.secret")
}

func TestValidateReadinessIssueTrackerRequiresWorkspaceManagedTracker(t *testing.T) {
	req := defaultReadinessRequest()
	env := defaultReadinessEnv()
	env.snapshot.IssueTrackers = []IssueTrackerRef{{Type: "github-issues", TokenSet: true, WebhookSecretSet: true}}
	env.issueTrackers = nil

	resp := ValidateReadiness(req, env)

	assertCriticalDiagnostic(t, resp, "readiness-issue-tracker-missing", "workflow.trigger.github_issues")
}

func TestValidateReadinessIssueTrackerEmptyTriggerWorkspaceRequiresSingleManagedTracker(t *testing.T) {
	tests := []struct {
		name     string
		trackers []IssueTrackerRef
		wantID   string
	}{
		{
			name:   "missing",
			wantID: "readiness-issue-tracker-missing",
		},
		{
			name: "ambiguous",
			trackers: []IssueTrackerRef{
				{Type: "linear", Workspace: "product", TokenSet: true, WebhookSecretSet: true},
				{Type: "linear", Workspace: "support", TokenSet: true, WebhookSecretSet: true},
			},
			wantID: "readiness-issue-tracker-ambiguous",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := defaultReadinessRequest()
			req.Config = readinessLinearWorkflowYAML("linear-triage", "")
			env := defaultReadinessEnv()
			env.issueTrackers = tt.trackers

			resp := ValidateReadiness(req, env)

			assertCriticalDiagnostic(t, resp, tt.wantID, "workflow.trigger.linear")
		})
	}
}

func TestValidateReadinessIssueTrackerWebhookRequiredForAutomaticWorkflows(t *testing.T) {
	tests := []struct {
		name     string
		config   string
		tracker  IssueTrackerRef
		wantPath string
	}{
		{
			name:     "github issues",
			config:   readinessGitHubIssueWorkflowYAML("issue-triage", "replicated", []string{"owner/repo"}, []string{"agent-ready"}, true, map[string]string{"WORKFLOW_SECRET": "hub_workflow_secret"}),
			tracker:  IssueTrackerRef{Type: "github-issues", TokenSet: true, WebhookSecretSet: false},
			wantPath: "workflow.trigger.github_issues",
		},
		{
			name:     "linear",
			config:   readinessLinearWorkflowYAML("linear-triage", "product"),
			tracker:  IssueTrackerRef{Type: "linear", Workspace: "product", TokenSet: true, WebhookSecretSet: false},
			wantPath: "workflow.trigger.linear",
		},
		{
			name:     "shortcut",
			config:   readinessShortcutWorkflowYAML("shortcut-triage", "engineering"),
			tracker:  IssueTrackerRef{Type: "shortcut", Workspace: "engineering", TokenSet: true, WebhookSecretSet: false},
			wantPath: "workflow.trigger.shortcut",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := defaultReadinessEnv()
			env.snapshot.IssueTrackers = []IssueTrackerRef{tt.tracker}
			env.issueTrackers = []IssueTrackerRef{tt.tracker}
			req := defaultReadinessRequest()
			req.Config = tt.config

			resp := ValidateReadiness(req, env)

			assertCriticalDiagnostic(t, resp, "readiness-webhook-secret-missing", tt.wantPath)
		})
	}
}

func TestValidateReadinessManualOnlyDoesNotRequireWebhookSecret(t *testing.T) {
	req := ValidateRequest{
		WorkflowName: "manual-github",
		Config: readinessManualWorkflowYAML("manual-github", "replicated", map[string]string{
			"WORKFLOW_SECRET": "hub_workflow_secret",
		}),
		WorkspaceConfig: validReadinessWorkspaceYAML(),
	}
	req.Config = strings.Replace(req.Config, "enable_manual_trigger: true", "integration: github-issues\nenable_manual_trigger: true\nrepos:\n  - owner/repo\ninputs:\n  - name: issue_number\n    type: number\n    min: 1", 1)

	env := defaultReadinessEnv()
	env.snapshot.IssueTrackers = []IssueTrackerRef{{Type: "github-issues", TokenSet: true, WebhookSecretSet: false}}
	env.issueTrackers = []IssueTrackerRef{{Type: "github-issues", Workspace: "default", TokenSet: true, WebhookSecretSet: false}}

	resp := ValidateReadiness(req, env)

	assertNoDiagnostic(t, resp, "readiness-webhook-secret-missing")
	if resp.Summary.Critical != 0 {
		t.Fatalf("critical diagnostics = %d, want 0: %#v", resp.Summary.Critical, resp.Checks)
	}
}

func TestValidateReadinessGitHubIssueManualRequiresOneExactRepo(t *testing.T) {
	tests := []struct {
		name  string
		repos []string
	}{
		{name: "missing", repos: nil},
		{name: "wildcard", repos: []string{"owner/*"}},
		{name: "multiple", repos: []string{"owner/repo", "owner/other"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := defaultReadinessRequest()
			req.Config = readinessGitHubIssueWorkflowYAML("issue-triage", "replicated", tt.repos, []string{"agent-ready"}, true, map[string]string{
				"WORKFLOW_SECRET": "hub_workflow_secret",
			})

			resp := ValidateReadiness(req, defaultReadinessEnv())

			assertCriticalDiagnostic(t, resp, "readiness-github-issues-manual-repo", "workflow.trigger.github_issues.repositories")
		})
	}
}

func TestValidateReadinessTriggerOverlapExactGitHubIssuesWarns(t *testing.T) {
	req := defaultReadinessRequest()
	existing := readinessParsedWorkflow(t, readinessGitHubIssueWorkflowYAML("existing-issue-triage", "replicated", []string{"owner/repo"}, []string{"agent-ready"}, false, map[string]string{}))
	env := defaultReadinessEnv()
	env.workspace = &types.WorkspaceConfig{
		Name:      "issue-triage",
		Workflows: []*types.WorkflowConfig{existing},
	}

	resp := ValidateReadiness(req, env)

	assertWarningDiagnostic(t, resp, "readiness-trigger-overlap", "workflow.trigger.github_issues")
}

func TestValidateReadinessTriggerOverlapSkipsDisjointGitHubIssues(t *testing.T) {
	tests := []struct {
		name     string
		existing *types.WorkflowConfig
	}{
		{
			name:     "repo",
			existing: readinessParsedWorkflow(t, readinessGitHubIssueWorkflowYAML("existing-other-repo", "replicated", []string{"owner/other"}, []string{"agent-ready"}, false, map[string]string{})),
		},
		{
			name:     "labels",
			existing: readinessParsedWorkflow(t, readinessGitHubIssueWorkflowYAML("existing-other-label", "replicated", []string{"owner/repo"}, []string{"needs-agent"}, false, map[string]string{})),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := defaultReadinessRequest()
			env := defaultReadinessEnv()
			env.workspace = &types.WorkspaceConfig{
				Name:      "issue-triage",
				Workflows: []*types.WorkflowConfig{tt.existing},
			}

			resp := ValidateReadiness(req, env)

			assertNoDiagnostic(t, resp, "readiness-trigger-overlap")
			if resp.Summary.Critical != 0 {
				t.Fatalf("critical diagnostics = %d, want 0: %#v", resp.Summary.Critical, resp.Checks)
			}
		})
	}
}

func TestValidateReadinessTriggerOverlapSkipsGitHubIssuesDisjointActors(t *testing.T) {
	tests := []struct {
		name     string
		config   string
		existing string
	}{
		{
			name:     "labelers",
			config:   readinessGitHubIssueWorkflowYAMLWithFilters("issue-triage", []string{"owner/repo"}, []string{"agent-ready"}, []string{"alice"}, ""),
			existing: readinessGitHubIssueWorkflowYAMLWithFilters("existing-other-labeler", []string{"owner/repo"}, []string{"agent-ready"}, []string{"bob"}, ""),
		},
		{
			name:     "assigned_to",
			config:   readinessGitHubIssueWorkflowYAMLWithFilters("issue-triage", []string{"owner/repo"}, []string{"agent-ready"}, []string{"*"}, "alice"),
			existing: readinessGitHubIssueWorkflowYAMLWithFilters("existing-other-assignee", []string{"owner/repo"}, []string{"agent-ready"}, []string{"*"}, "bob"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := defaultReadinessRequest()
			req.Config = tt.config
			env := defaultReadinessEnv()
			env.workspace = &types.WorkspaceConfig{
				Name:      "issue-triage",
				Workflows: []*types.WorkflowConfig{readinessParsedWorkflow(t, tt.existing)},
			}

			resp := ValidateReadiness(req, env)

			assertNoDiagnostic(t, resp, "readiness-trigger-overlap")
			if resp.Summary.Critical != 0 {
				t.Fatalf("critical diagnostics = %d, want 0: %#v", resp.Summary.Critical, resp.Checks)
			}
		})
	}
}

func TestValidateReadinessTriggerOverlapGitHubIssuesOpenedIgnoresDisjointLabelers(t *testing.T) {
	req := defaultReadinessRequest()
	req.Config = strings.Replace(
		readinessGitHubIssueWorkflowYAMLWithFilters("issue-triage", []string{"owner/repo"}, []string{"agent-ready"}, []string{"alice"}, ""),
		"    event: issue_labeled\n",
		"    event: issue_opened\n",
		1,
	)
	existing := strings.Replace(
		readinessGitHubIssueWorkflowYAMLWithFilters("existing-other-labeler", []string{"owner/repo"}, []string{"agent-ready"}, []string{"bob"}, ""),
		"    event: issue_labeled\n",
		"    event: issue_opened\n",
		1,
	)
	env := defaultReadinessEnv()
	env.workspace = &types.WorkspaceConfig{
		Name:      "issue-triage",
		Workflows: []*types.WorkflowConfig{readinessParsedWorkflow(t, existing)},
	}

	resp := ValidateReadiness(req, env)

	assertWarningDiagnostic(t, resp, "readiness-trigger-overlap", "workflow.trigger.github_issues")
}

func TestValidateReadinessTriggerOverlapExactLinearWarns(t *testing.T) {
	req := defaultReadinessRequest()
	req.Config = readinessLinearWorkflowYAMLWithFilters("linear-triage", "product", "ENG", []string{"Ready for Agent"}, []string{"agent-ready"}, "marc")
	existing := readinessParsedWorkflow(t, readinessLinearWorkflowYAMLWithFilters("existing-linear-triage", "product", "ENG", []string{"Ready for Agent"}, []string{"agent-ready"}, "marc"))
	env := defaultReadinessEnv()
	env.workspace = &types.WorkspaceConfig{
		Name:      "issue-triage",
		Workflows: []*types.WorkflowConfig{existing},
	}

	resp := ValidateReadiness(req, env)

	assertWarningDiagnostic(t, resp, "readiness-trigger-overlap", "workflow.trigger.linear")
}

func TestValidateReadinessTriggerOverlapLinearStatusAliasWarns(t *testing.T) {
	req := defaultReadinessRequest()
	req.Config = strings.Replace(
		readinessLinearWorkflowYAMLWithFilters("linear-triage", "product", "", []string{"Ready for Agent"}, nil, ""),
		"    event: status_changed\n",
		"    event: status\n",
		1,
	)
	existing := readinessParsedWorkflow(t, readinessLinearWorkflowYAMLWithFilters("existing-linear-triage", "product", "", []string{"Ready for Agent"}, nil, ""))
	env := defaultReadinessEnv()
	env.workspace = &types.WorkspaceConfig{
		Name:      "issue-triage",
		Workflows: []*types.WorkflowConfig{existing},
	}

	resp := ValidateReadiness(req, env)

	assertWarningDiagnostic(t, resp, "readiness-trigger-overlap", "workflow.trigger.linear")
}

func TestValidateReadinessTriggerOverlapExactShortcutWarns(t *testing.T) {
	req := defaultReadinessRequest()
	req.Config = readinessShortcutWorkflowYAMLWithFilters("shortcut-triage", "engineering", []string{"Ready for Agent"}, []string{"agent-ready"}, "marc")
	existing := readinessParsedWorkflow(t, readinessShortcutWorkflowYAMLWithFilters("existing-shortcut-triage", "engineering", []string{"Ready for Agent"}, []string{"agent-ready"}, "marc"))
	env := defaultReadinessEnv()
	env.workspace = &types.WorkspaceConfig{
		Name:      "issue-triage",
		Workflows: []*types.WorkflowConfig{existing},
	}

	resp := ValidateReadiness(req, env)

	assertWarningDiagnostic(t, resp, "readiness-trigger-overlap", "workflow.trigger.shortcut")
}

func TestValidateReadinessTriggerOverlapShortcutStatusAliasWarns(t *testing.T) {
	req := defaultReadinessRequest()
	req.Config = strings.Replace(
		readinessShortcutWorkflowYAMLWithFilters("shortcut-triage", "engineering", []string{"Ready for Agent"}, nil, ""),
		"    event: status_changed\n",
		"    event: status\n",
		1,
	)
	existing := readinessParsedWorkflow(t, readinessShortcutWorkflowYAMLWithFilters("existing-shortcut-triage", "engineering", []string{"Ready for Agent"}, nil, ""))
	env := defaultReadinessEnv()
	env.workspace = &types.WorkspaceConfig{
		Name:      "issue-triage",
		Workflows: []*types.WorkflowConfig{existing},
	}

	resp := ValidateReadiness(req, env)

	assertWarningDiagnostic(t, resp, "readiness-trigger-overlap", "workflow.trigger.shortcut")
}

func TestValidateReadinessTriggerOverlapSkipsDisjointLinearAndShortcut(t *testing.T) {
	tests := []struct {
		name     string
		config   string
		existing string
	}{
		{
			name:     "linear workspace",
			config:   readinessLinearWorkflowYAMLWithFilters("linear-triage", "product", "ENG", []string{"Ready for Agent"}, []string{"agent-ready"}, ""),
			existing: readinessLinearWorkflowYAMLWithFilters("existing-linear-other-workspace", "support", "ENG", []string{"Ready for Agent"}, []string{"agent-ready"}, ""),
		},
		{
			name:     "linear state",
			config:   readinessLinearWorkflowYAMLWithFilters("linear-triage", "product", "ENG", []string{"Ready for Agent"}, []string{"agent-ready"}, ""),
			existing: readinessLinearWorkflowYAMLWithFilters("existing-linear-other-state", "product", "ENG", []string{"Needs Triage"}, []string{"agent-ready"}, ""),
		},
		{
			name:     "linear label",
			config:   readinessLinearWorkflowYAMLWithFilters("linear-triage", "product", "ENG", []string{"Ready for Agent"}, []string{"agent-ready"}, ""),
			existing: readinessLinearWorkflowYAMLWithFilters("existing-linear-other-label", "product", "ENG", []string{"Ready for Agent"}, []string{"agent-review"}, ""),
		},
		{
			name:     "shortcut workspace",
			config:   readinessShortcutWorkflowYAMLWithFilters("shortcut-triage", "engineering", []string{"Ready for Agent"}, []string{"agent-ready"}, ""),
			existing: readinessShortcutWorkflowYAMLWithFilters("existing-shortcut-other-workspace", "product", []string{"Ready for Agent"}, []string{"agent-ready"}, ""),
		},
		{
			name:     "shortcut label",
			config:   readinessShortcutWorkflowYAMLWithFilters("shortcut-triage", "engineering", []string{"Ready for Agent"}, []string{"agent-ready"}, ""),
			existing: readinessShortcutWorkflowYAMLWithFilters("existing-shortcut-other-label", "engineering", []string{"Ready for Agent"}, []string{"agent-review"}, ""),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := defaultReadinessRequest()
			req.Config = tt.config
			env := defaultReadinessEnv()
			env.workspace = &types.WorkspaceConfig{
				Name:      "issue-triage",
				Workflows: []*types.WorkflowConfig{readinessParsedWorkflow(t, tt.existing)},
			}

			resp := ValidateReadiness(req, env)

			assertNoDiagnostic(t, resp, "readiness-trigger-overlap")
			if resp.Summary.Critical != 0 {
				t.Fatalf("critical diagnostics = %d, want 0: %#v", resp.Summary.Critical, resp.Checks)
			}
		})
	}
}

func TestValidateReadinessNetworkChecksAreNotChecked(t *testing.T) {
	resp := ValidateReadiness(defaultReadinessRequest(), defaultReadinessEnv())

	diagnostic := findDiagnostic(resp, "readiness-network-checks")
	if diagnostic == nil {
		t.Fatalf("missing readiness-network-checks diagnostic: %#v", resp.Checks)
	}
	if diagnostic.Status != "not_checked" {
		t.Fatalf("network status = %q, want not_checked", diagnostic.Status)
	}
	if diagnostic.OK {
		t.Fatalf("network OK = true, want false")
	}
	if diagnostic.Severity != SeverityInfo || diagnostic.Blocking {
		t.Fatalf("network diagnostic = %#v, want non-blocking info", *diagnostic)
	}
}

type readinessFakeEnvironment struct {
	snapshot            SetupEnvironmentSnapshot
	workspace           *types.WorkspaceConfig
	workspaceSecrets    []string
	workspaceSecretsErr error
	issueTrackers       []IssueTrackerRef
	issueTrackersErr    error
}

func (e readinessFakeEnvironment) Snapshot() (SetupEnvironmentSnapshot, error) {
	return e.snapshot, nil
}

func (e readinessFakeEnvironment) LoadWorkspace(name string) (*types.WorkspaceConfig, error) {
	return e.workspace, nil
}

func (e readinessFakeEnvironment) LoadWorkflowRaw(workspaceName, workflowName string) (string, error) {
	if e.workspace == nil {
		return "", nil
	}
	for _, workflow := range e.workspace.Workflows {
		if workflow != nil && workflow.Name == workflowName {
			return workflow.RawConfig, nil
		}
	}
	return "", nil
}

func (e readinessFakeEnvironment) WorkspaceSecretNames(string) ([]string, error) {
	if e.workspaceSecretsErr != nil {
		return nil, e.workspaceSecretsErr
	}
	return append([]string(nil), e.workspaceSecrets...), nil
}

func (e readinessFakeEnvironment) WorkspaceIssueTrackers(string) ([]IssueTrackerRef, error) {
	if e.issueTrackersErr != nil {
		return nil, e.issueTrackersErr
	}
	return append([]IssueTrackerRef(nil), e.issueTrackers...), nil
}

func (e readinessFakeEnvironment) WorkspaceGitHubApps(string) ([]GitHubAppRef, error) {
	return nil, nil
}

func (e readinessFakeEnvironment) ListFactories() ([]FactoryRef, error) {
	return nil, nil
}

func (e readinessFakeEnvironment) LoadFactory(string) (*types.FactoryConfig, error) {
	return nil, nil
}

func defaultReadinessEnv() readinessFakeEnvironment {
	return readinessFakeEnvironment{
		snapshot: SetupEnvironmentSnapshot{
			ClawTokenSet: true,
			Providers: []ProviderRef{
				configuredProvider("replicated"),
			},
			DefaultProvider: "replicated",
			DefaultModel:    "anthropic/claude-sonnet-4-5",
			LLMKeys: []LLMKeyRef{
				{Name: "anthropic", Provider: "anthropic", KeySet: true, Default: true, DefaultModel: "anthropic/claude-sonnet-4-5"},
			},
			ConcurrencyGroups: []ConcurrencyGroupRef{
				{Name: "repo:owner/repo", Limit: 1},
			},
			HubSecretNames: []string{
				"hub_template_secret",
				"hub_workflow_secret",
			},
			IssueTrackers: []IssueTrackerRef{
				{Type: "github-issues", TokenSet: true, WebhookSecretSet: true},
				{Type: "linear", Workspace: "product", TokenSet: true, WebhookSecretSet: true},
				{Type: "shortcut", Workspace: "engineering", TokenSet: true, WebhookSecretSet: true},
			},
		},
		workspaceSecrets: []string{"workspace_secret"},
		issueTrackers: []IssueTrackerRef{
			{Type: "github-issues", Workspace: "default", TokenSet: true, WebhookSecretSet: true},
			{Type: "linear", Workspace: "product", TokenSet: true, WebhookSecretSet: true},
			{Type: "shortcut", Workspace: "engineering", TokenSet: true, WebhookSecretSet: true},
		},
	}
}

func configuredProvider(provider string) ProviderRef {
	ref := ProviderRef{
		Name:           provider,
		Type:           provider,
		Provisionable:  true,
		CredentialsSet: true,
	}
	switch provider {
	case "replicated":
		ref.TokenSet = true
	case "daytona":
		ref.APIKeySet = true
	case "exedev":
		ref.SSHKeySet = true
	case "docker":
		ref.CredentialsSet = true
	}
	return ref
}

func defaultReadinessRequest() ValidateRequest {
	return ValidateRequest{
		WorkflowName: "issue-triage",
		Config: readinessGitHubIssueWorkflowYAML("issue-triage", "replicated", []string{"owner/repo"}, []string{"agent-ready"}, true, map[string]string{
			"WORKFLOW_SECRET": "hub_workflow_secret",
		}),
		WorkspaceConfig: validReadinessWorkspaceYAML(),
	}
}

func validReadinessWorkspaceYAML() string {
	return `
name: issue-triage
provider: replicated
default_model: anthropic/claude-sonnet-4-5
secrets:
  - workspace_secret
env:
  FROM_WORKSPACE:
    secret: workspace_secret
secret_refs:
  TEMPLATE_SECRET: hub_template_secret
`
}

func readinessGitHubIssueWorkflowYAML(name, provider string, repos, labels []string, enableManual bool, secretRefs map[string]string) string {
	var b strings.Builder
	b.WriteString("schema_version: v1\n")
	b.WriteString("name: " + name + "\n")
	if provider != "" {
		b.WriteString("provider: " + provider + "\n")
	}
	b.WriteString("concurrency_group: global\n")
	b.WriteString("enable_manual_trigger: " + fmt.Sprintf("%t", enableManual) + "\n")
	if enableManual {
		b.WriteString("inputs:\n")
		b.WriteString("  - name: issue_number\n")
		b.WriteString("    type: number\n")
		b.WriteString("    min: 1\n")
	}
	appendSecretRefsYAML(&b, secretRefs)
	b.WriteString("trigger:\n")
	b.WriteString("  github_issues:\n")
	b.WriteString("    event: issue_labeled\n")
	if len(repos) > 0 {
		b.WriteString("    repositories:\n")
		for _, repo := range repos {
			b.WriteString("      - " + repo + "\n")
		}
	}
	b.WriteString("    states:\n")
	b.WriteString("      - open\n")
	if len(labels) > 0 {
		b.WriteString("    labels:\n")
		for _, label := range labels {
			b.WriteString("      - " + label + "\n")
		}
	}
	b.WriteString("    labelers:\n")
	b.WriteString("      - \"*\"\n")
	b.WriteString("pipeline_yaml: |\n")
	b.WriteString("  stages:\n")
	b.WriteString("    - id: working\n")
	b.WriteString("      entry: true\n")
	b.WriteString("      on_enter:\n")
	b.WriteString("        inject: start\n")
	return b.String()
}

func readinessGitHubIssueWorkflowYAMLWithFilters(name string, repos, labels, labelers []string, assignedTo string) string {
	raw := readinessGitHubIssueWorkflowYAML(name, "replicated", repos, labels, true, map[string]string{
		"WORKFLOW_SECRET": "hub_workflow_secret",
	})
	if len(labelers) > 0 {
		var replacement strings.Builder
		replacement.WriteString("    labelers:\n")
		for _, labeler := range labelers {
			replacement.WriteString("      - " + fmt.Sprintf("%q", labeler) + "\n")
		}
		raw = strings.Replace(raw, "    labelers:\n      - \"*\"\n", replacement.String(), 1)
	}
	if strings.TrimSpace(assignedTo) != "" {
		raw = strings.Replace(raw, "pipeline_yaml: |\n", "    assigned_to: "+assignedTo+"\npipeline_yaml: |\n", 1)
	}
	return raw
}

func readinessLinearWorkflowYAML(name, workspace string) string {
	return readinessLinearWorkflowYAMLWithFilters(name, workspace, "", []string{"Ready for Agent"}, nil, "")
}

func readinessLinearWorkflowYAMLWithFilters(name, workspace, team string, states, labels []string, assignedTo string) string {
	var b strings.Builder
	b.WriteString("schema_version: v1\n")
	b.WriteString("name: " + name + "\n")
	b.WriteString("provider: replicated\n")
	b.WriteString("concurrency_group: global\n")
	b.WriteString("trigger:\n")
	b.WriteString("  linear:\n")
	b.WriteString("    event: status_changed\n")
	if strings.TrimSpace(workspace) != "" {
		b.WriteString("    workspace: " + workspace + "\n")
	}
	if strings.TrimSpace(team) != "" {
		b.WriteString("    team: " + team + "\n")
	}
	b.WriteString("    states:\n")
	for _, state := range states {
		b.WriteString("      - " + state + "\n")
	}
	if len(labels) > 0 {
		b.WriteString("    labels:\n")
		for _, label := range labels {
			b.WriteString("      - " + label + "\n")
		}
	}
	if strings.TrimSpace(assignedTo) != "" {
		b.WriteString("    assigned_to: " + assignedTo + "\n")
	}
	b.WriteString("pipeline_yaml: |\n")
	b.WriteString("  stages:\n")
	b.WriteString("    - id: working\n")
	b.WriteString("      entry: true\n")
	b.WriteString("      on_enter:\n")
	b.WriteString("        move_issue: In Progress\n")
	return b.String()
}

func readinessShortcutWorkflowYAML(name, workspace string) string {
	return readinessShortcutWorkflowYAMLWithFilters(name, workspace, []string{"Ready for Agent"}, nil, "")
}

func readinessShortcutWorkflowYAMLWithFilters(name, workspace string, states, labels []string, assignedTo string) string {
	var b strings.Builder
	b.WriteString("schema_version: v1\n")
	b.WriteString("name: " + name + "\n")
	b.WriteString("provider: replicated\n")
	b.WriteString("concurrency_group: global\n")
	b.WriteString("trigger:\n")
	b.WriteString("  shortcut:\n")
	b.WriteString("    event: status_changed\n")
	if strings.TrimSpace(workspace) != "" {
		b.WriteString("    workspace: " + workspace + "\n")
	}
	b.WriteString("    states:\n")
	for _, state := range states {
		b.WriteString("      - " + state + "\n")
	}
	if len(labels) > 0 {
		b.WriteString("    labels:\n")
		for _, label := range labels {
			b.WriteString("      - " + label + "\n")
		}
	}
	if strings.TrimSpace(assignedTo) != "" {
		b.WriteString("    assigned_to: " + assignedTo + "\n")
	}
	b.WriteString("pipeline_yaml: |\n")
	b.WriteString("  stages:\n")
	b.WriteString("    - id: working\n")
	b.WriteString("      entry: true\n")
	b.WriteString("      on_enter:\n")
	b.WriteString("        move_issue: In Progress\n")
	return b.String()
}

func readinessManualWorkflowYAML(name, provider string, secretRefs map[string]string) string {
	var b strings.Builder
	b.WriteString("schema_version: v1\n")
	b.WriteString("name: " + name + "\n")
	if provider != "" {
		b.WriteString("provider: " + provider + "\n")
	}
	b.WriteString("concurrency_group: global\n")
	b.WriteString("enable_manual_trigger: true\n")
	appendSecretRefsYAML(&b, secretRefs)
	b.WriteString("pipeline_yaml: |\n")
	b.WriteString("  stages:\n")
	b.WriteString("    - id: working\n")
	b.WriteString("      entry: true\n")
	b.WriteString("      on_enter:\n")
	b.WriteString("        inject: start\n")
	return b.String()
}

func appendSecretRefsYAML(b *strings.Builder, secretRefs map[string]string) {
	if len(secretRefs) == 0 {
		return
	}
	b.WriteString("secret_refs:\n")
	for envName, secretName := range secretRefs {
		b.WriteString("  " + envName + ": " + secretName + "\n")
	}
}

func readinessParsedWorkflow(t *testing.T, raw string) *types.WorkflowConfig {
	t.Helper()

	workflow, err := parseWorkflowConfig(raw)
	if err != nil {
		t.Fatalf("parseWorkflowConfig: %v\n%s", err, raw)
	}
	if err := types.NormalizeWorkflowConfig(workflow); err != nil {
		t.Fatalf("NormalizeWorkflowConfig: %v", err)
	}
	return workflow
}

func assertWarningDiagnostic(t *testing.T, resp ValidateResponse, id, fieldPrefix string) {
	t.Helper()

	for _, check := range resp.Checks {
		if check.ID != id {
			continue
		}
		if check.Severity != SeverityWarning {
			t.Fatalf("%s severity = %q, want warning", id, check.Severity)
		}
		if check.Blocking {
			t.Fatalf("%s blocking = true, want false", id)
		}
		if !strings.HasPrefix(check.FieldPath, fieldPrefix) {
			t.Fatalf("%s fieldPath = %q, want prefix %q", id, check.FieldPath, fieldPrefix)
		}
		return
	}
	t.Fatalf("missing warning diagnostic %q with field prefix %q; got %#v", id, fieldPrefix, resp.Checks)
}

func findDiagnostic(resp ValidateResponse, id string) *Diagnostic {
	for i := range resp.Checks {
		if resp.Checks[i].ID == id {
			return &resp.Checks[i]
		}
	}
	return nil
}

func TestValidateReadinessHelpersBuildExpectedWorkflow(t *testing.T) {
	workflow := readinessParsedWorkflow(t, defaultReadinessRequest().Config)
	if workflow.Trigger == nil || workflow.Trigger.GitHubIssues == nil {
		t.Fatalf("github_issues trigger missing: %#v", workflow.Trigger)
	}
	if !reflect.DeepEqual(workflow.Trigger.GitHubIssues.Repositories, []string{"owner/repo"}) {
		t.Fatalf("repositories = %#v, want owner/repo", workflow.Trigger.GitHubIssues.Repositories)
	}
}
