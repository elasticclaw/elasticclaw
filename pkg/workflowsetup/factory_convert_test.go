package workflowsetup

import (
	"reflect"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"gopkg.in/yaml.v3"
)

func TestFactoryConvertSupportedIntegrationsDefaultDisabled(t *testing.T) {
	for _, tt := range []struct {
		name        string
		factory     *types.FactoryConfig
		assert      func(*testing.T, types.WorkflowConfig)
		pipelineYML string
	}{
		{
			name: "github issues",
			factory: &types.FactoryConfig{
				Name:             "issue-triage",
				Integration:      "github-issues",
				Template:         "engineering",
				TriggerStatus:    "agent-ready",
				Labels:           []string{"bug"},
				AssignedTo:       "none",
				AllowedLabelers:  []string{"alice"},
				TriggerRepos:     []string{"owner/repo"},
				SecretRefs:       map[string]string{"WORKFLOW_SECRET": "workspace_secret"},
				ConcurrencyGroup: "repo:owner/repo",
			},
			pipelineYML: validFactoryConvertPipeline("inject: start"),
			assert: func(t *testing.T, workflow types.WorkflowConfig) {
				t.Helper()
				if workflow.Trigger == nil || workflow.Trigger.GitHubIssues == nil {
					t.Fatalf("github_issues trigger missing: %#v", workflow.Trigger)
				}
				trigger := workflow.Trigger.GitHubIssues
				if !reflect.DeepEqual(trigger.Repositories, []string{"owner/repo"}) {
					t.Fatalf("repositories = %#v, want owner/repo", trigger.Repositories)
				}
				if !reflect.DeepEqual(trigger.States, []string{"open"}) {
					t.Fatalf("states = %#v, want open", trigger.States)
				}
				if !reflect.DeepEqual(trigger.Labels, []string{"agent-ready", "bug"}) {
					t.Fatalf("labels = %#v, want trigger label plus factory labels", trigger.Labels)
				}
				if !reflect.DeepEqual(trigger.Labelers, []string{"alice"}) {
					t.Fatalf("labelers = %#v, want alice", trigger.Labelers)
				}
				if trigger.AssignedTo != "none" {
					t.Fatalf("assigned_to = %q, want none", trigger.AssignedTo)
				}
				if workflow.ConcurrencyGroup != "repo:owner/repo" {
					t.Fatalf("concurrency_group = %q, want repo:owner/repo", workflow.ConcurrencyGroup)
				}
			},
		},
		{
			name: "linear",
			factory: &types.FactoryConfig{
				Name:          "linear-triage",
				Integration:   "linear",
				Template:      "engineering",
				Workspace:     "product",
				Team:          "ENG",
				TriggerStatus: "Ready for Agent",
				Labels:        []string{"agent"},
				AssignedTo:    "@alice",
				DoneStatus:    "Done",
			},
			pipelineYML: validFactoryConvertPipeline("move_issue: In Progress"),
			assert: func(t *testing.T, workflow types.WorkflowConfig) {
				t.Helper()
				if workflow.Trigger == nil || workflow.Trigger.Linear == nil {
					t.Fatalf("linear trigger missing: %#v", workflow.Trigger)
				}
				trigger := workflow.Trigger.Linear
				if trigger.Workspace != "product" {
					t.Fatalf("linear workspace = %q, want product", trigger.Workspace)
				}
				if trigger.Team != "ENG" {
					t.Fatalf("linear team = %q, want ENG", trigger.Team)
				}
				if !reflect.DeepEqual(trigger.States, []string{"Ready for Agent"}) {
					t.Fatalf("linear states = %#v, want Ready for Agent", trigger.States)
				}
				if !reflect.DeepEqual(trigger.Labels, []string{"agent"}) {
					t.Fatalf("linear labels = %#v, want agent", trigger.Labels)
				}
				if trigger.AssignedTo != "@alice" {
					t.Fatalf("linear assigned_to = %q, want @alice", trigger.AssignedTo)
				}
				if workflow.FinishedStatus != "Done" {
					t.Fatalf("finished_status = %q, want Done", workflow.FinishedStatus)
				}
			},
		},
		{
			name: "shortcut",
			factory: &types.FactoryConfig{
				Name:           "shortcut-triage",
				Integration:    "shortcut",
				Template:       "engineering",
				Workspace:      "stories",
				TriggerStatus:  "Ready for Agent",
				Labels:         []string{"agent"},
				AssignedTo:     "@bob",
				FinishedStatus: "Complete",
			},
			pipelineYML: validFactoryConvertPipeline("move_issue: In Progress"),
			assert: func(t *testing.T, workflow types.WorkflowConfig) {
				t.Helper()
				if workflow.Trigger == nil || workflow.Trigger.Shortcut == nil {
					t.Fatalf("shortcut trigger missing: %#v", workflow.Trigger)
				}
				trigger := workflow.Trigger.Shortcut
				if trigger.Workspace != "stories" {
					t.Fatalf("shortcut workspace = %q, want stories", trigger.Workspace)
				}
				if !reflect.DeepEqual(trigger.States, []string{"Ready for Agent"}) {
					t.Fatalf("shortcut states = %#v, want Ready for Agent", trigger.States)
				}
				if !reflect.DeepEqual(trigger.Labels, []string{"agent"}) {
					t.Fatalf("shortcut labels = %#v, want agent", trigger.Labels)
				}
				if trigger.AssignedTo != "@bob" {
					t.Fatalf("shortcut assigned_to = %q, want @bob", trigger.AssignedTo)
				}
				if workflow.FinishedStatus != "Complete" {
					t.Fatalf("finished_status = %q, want Complete", workflow.FinishedStatus)
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.factory.PipelineYAML = tt.pipelineYML
			resp := ConvertFactory(FactoryConvertRequest{
				Factory:         tt.factory,
				WorkspaceName:   "engineering",
				WorkspaceConfig: validFactoryConvertWorkspaceConfig(),
				TemplateFiles:   map[string]string{},
				WorkspaceFiles:  validFactoryConvertWorkspaceFiles(),
			})
			if resp.Status != "ready" {
				t.Fatalf("status = %q, want ready; diagnostics: %#v", resp.Status, resp.Diagnostics)
			}
			if resp.Summary.Critical != 0 {
				t.Fatalf("critical diagnostics = %d, want 0: %#v", resp.Summary.Critical, resp.Diagnostics)
			}
			if resp.ConfigHash != ConfigHash(resp.Config) {
				t.Fatalf("configHash = %q, want %q", resp.ConfigHash, ConfigHash(resp.Config))
			}

			workflow := parseConvertedWorkflow(t, resp.Config)
			if workflow.Enabled == nil || *workflow.Enabled {
				t.Fatalf("enabled = %#v, want explicit false", workflow.Enabled)
			}
			if workflow.Name != tt.factory.Name {
				t.Fatalf("workflow name = %q, want %q", workflow.Name, tt.factory.Name)
			}
			if workflow.Integration != tt.factory.Integration {
				t.Fatalf("integration = %q, want %q", workflow.Integration, tt.factory.Integration)
			}
			if workflow.PipelineYAML != tt.pipelineYML {
				t.Fatalf("pipeline_yaml not preserved:\n%s", workflow.PipelineYAML)
			}
			if len(workflow.Stages) != 1 || workflow.Stages[0].ID != "working" {
				t.Fatalf("stages = %#v, want parsed working stage", workflow.Stages)
			}
			tt.assert(t, workflow)
		})
	}
}

func TestFactoryConvertUnsupportedFactoriesAreBlocked(t *testing.T) {
	for _, tt := range []struct {
		name    string
		factory *types.FactoryConfig
		wantID  string
	}{
		{
			name: "github pull request",
			factory: &types.FactoryConfig{
				Name:        "github-pr",
				Integration: "github",
				Template:    "engineering",
				Trigger:     &types.GitHubTrigger{On: "pull_request", Action: "opened"},
			},
			wantID: "factory-convert-unsupported-github-pr",
		},
		{
			name: "external",
			factory: &types.FactoryConfig{
				Name:            "release-webhook",
				Integration:     "external",
				Template:        "engineering",
				ExternalTrigger: &types.ExternalTrigger{Source: "generic-webhook"},
			},
			wantID: "factory-convert-unsupported-external",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.factory.PipelineYAML = validFactoryConvertPipeline("inject: start")
			resp := ConvertFactory(FactoryConvertRequest{
				Factory:         tt.factory,
				WorkspaceName:   "engineering",
				WorkspaceConfig: validFactoryConvertWorkspaceConfig(),
				TemplateFiles:   map[string]string{},
				WorkspaceFiles:  validFactoryConvertWorkspaceFiles(),
			})
			if resp.Status != "blocked" {
				t.Fatalf("status = %q, want blocked", resp.Status)
			}
			assertFactoryConvertCritical(t, resp, tt.wantID)
			if strings.TrimSpace(resp.Config) != "" {
				t.Fatalf("config = %q, want empty for unsupported conversion", resp.Config)
			}
		})
	}
}

func TestFactoryConvertInvalidPipelineIsBlocked(t *testing.T) {
	factory := &types.FactoryConfig{
		Name:          "bad-pipeline",
		Integration:   "linear",
		Template:      "engineering",
		Workspace:     "product",
		TriggerStatus: "Ready for Agent",
		PipelineYAML:  "stages:\n\t- id: bad",
	}

	resp := ConvertFactory(FactoryConvertRequest{
		Factory:         factory,
		WorkspaceName:   "engineering",
		WorkspaceConfig: validFactoryConvertWorkspaceConfig(),
		TemplateFiles:   map[string]string{},
		WorkspaceFiles:  validFactoryConvertWorkspaceFiles(),
	})

	if resp.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", resp.Status)
	}
	assertFactoryConvertCritical(t, resp, "factory-convert-pipeline-invalid")
	if strings.TrimSpace(resp.Config) != "" {
		t.Fatalf("config = %q, want empty when pipeline cannot be parsed", resp.Config)
	}
}

func TestFactoryConvertTemplateWorkspaceMismatchIsBlocked(t *testing.T) {
	factory := &types.FactoryConfig{
		Name:         "legacy-template",
		Integration:  "linear",
		Template:     "legacy-template",
		Workspace:    "product",
		PipelineYAML: validFactoryConvertPipeline("move_issue: In Progress"),
	}

	resp := ConvertFactory(FactoryConvertRequest{
		Factory:         factory,
		WorkspaceName:   "engineering",
		WorkspaceConfig: validFactoryConvertWorkspaceConfig(),
		TemplateFiles:   map[string]string{},
		WorkspaceFiles:  validFactoryConvertWorkspaceFiles(),
	})

	if resp.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", resp.Status)
	}
	assertFactoryConvertCritical(t, resp, "factory-convert-template-workspace-mismatch")
}

func TestFactoryConvertRuntimeParityBlockers(t *testing.T) {
	for _, tt := range []struct {
		name            string
		factory         *types.FactoryConfig
		workspaceConfig string
		wantID          string
	}{
		{
			name: "missing provider",
			factory: &types.FactoryConfig{
				Name:         "no-provider",
				Integration:  "linear",
				Template:     "engineering",
				Workspace:    "product",
				PipelineYAML: validFactoryConvertPipeline("move_issue: In Progress"),
			},
			workspaceConfig: "schema_version: v1\nname: engineering\n",
			wantID:          "factory-convert-provider-unresolved",
		},
		{
			name: "missing secret ref",
			factory: &types.FactoryConfig{
				Name:         "missing-secret",
				Integration:  "linear",
				Template:     "engineering",
				Workspace:    "product",
				SecretRefs:   map[string]string{"LINEAR_API_KEY": "missing_secret"},
				PipelineYAML: validFactoryConvertPipeline("move_issue: In Progress"),
			},
			workspaceConfig: validFactoryConvertWorkspaceConfig(),
			wantID:          "factory-convert-secret-ref-missing",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resp := ConvertFactory(FactoryConvertRequest{
				Factory:         tt.factory,
				WorkspaceName:   "engineering",
				WorkspaceConfig: tt.workspaceConfig,
				TemplateFiles:   map[string]string{},
				WorkspaceFiles:  map[string]string{"elasticclaw-config.yaml": tt.workspaceConfig},
			})
			if resp.Status != "blocked" {
				t.Fatalf("status = %q, want blocked", resp.Status)
			}
			assertFactoryConvertCritical(t, resp, tt.wantID)
		})
	}
}

func TestFactoryConvertTemplateFilesRequireWorkspaceParity(t *testing.T) {
	factory := &types.FactoryConfig{
		Name:          "linear-triage",
		Integration:   "linear",
		Template:      "engineering",
		Workspace:     "product",
		TriggerStatus: "Ready for Agent",
		PipelineYAML:  validFactoryConvertPipeline("move_issue: In Progress"),
	}

	blocked := ConvertFactory(FactoryConvertRequest{
		Factory:         factory,
		WorkspaceName:   "engineering",
		WorkspaceConfig: validFactoryConvertWorkspaceConfig(),
		TemplateFiles: map[string]string{
			"BOOTSTRAP.md": "legacy bootstrap\n",
		},
		WorkspaceFiles: validFactoryConvertWorkspaceFiles(),
	})
	if blocked.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", blocked.Status)
	}
	assertFactoryConvertCritical(t, blocked, "factory-convert-template-file-missing")
	if strings.TrimSpace(blocked.Config) != "" {
		t.Fatalf("config = %q, want empty when template file parity is missing", blocked.Config)
	}

	ready := ConvertFactory(FactoryConvertRequest{
		Factory:         factory,
		WorkspaceName:   "engineering",
		WorkspaceConfig: validFactoryConvertWorkspaceConfig(),
		TemplateFiles: map[string]string{
			"BOOTSTRAP.md": "legacy bootstrap\n",
		},
		WorkspaceFiles: map[string]string{
			"elasticclaw-config.yaml": validFactoryConvertWorkspaceConfig(),
			"BOOTSTRAP.md":            "legacy bootstrap\n",
		},
	})
	if ready.Status != "ready" {
		t.Fatalf("status = %q, want ready; diagnostics: %#v", ready.Status, ready.Diagnostics)
	}
}

func TestFactoryConvertBlocksWithoutTemplateFileEvidence(t *testing.T) {
	factory := &types.FactoryConfig{
		Name:          "linear-triage",
		Integration:   "linear",
		Template:      "engineering",
		Workspace:     "product",
		TriggerStatus: "Ready for Agent",
		PipelineYAML:  validFactoryConvertPipeline("move_issue: In Progress"),
	}

	resp := ConvertFactory(FactoryConvertRequest{
		Factory:         factory,
		WorkspaceName:   "engineering",
		WorkspaceConfig: validFactoryConvertWorkspaceConfig(),
	})

	if resp.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", resp.Status)
	}
	assertFactoryConvertCritical(t, resp, "factory-convert-template-files-unchecked")
}

func TestFactoryConvertSupportedFactoryWithAmbiguousTriggerIsBlocked(t *testing.T) {
	factory := &types.FactoryConfig{
		Name:          "ambiguous-gh-issues",
		Integration:   "github-issues",
		Template:      "engineering",
		TriggerStatus: "agent-ready",
		PipelineYAML:  validFactoryConvertPipeline("inject: start"),
	}

	resp := ConvertFactory(FactoryConvertRequest{
		Factory:         factory,
		WorkspaceName:   "engineering",
		WorkspaceConfig: validFactoryConvertWorkspaceConfig(),
		TemplateFiles:   map[string]string{},
		WorkspaceFiles:  validFactoryConvertWorkspaceFiles(),
	})

	if resp.Status != "blocked" {
		t.Fatalf("status = %q, want blocked", resp.Status)
	}
	assertFactoryConvertCritical(t, resp, "factory-convert-github-issues-repositories-missing")
}

func validFactoryConvertWorkspaceConfig() string {
	return `
schema_version: v1
name: engineering
provider: replicated
default_model: anthropic/claude-sonnet-4-5
secrets:
  - workspace_secret
secret_refs:
  TEMPLATE_SECRET: workspace_secret
`
}

func validFactoryConvertWorkspaceFiles() map[string]string {
	return map[string]string{
		"elasticclaw-config.yaml": validFactoryConvertWorkspaceConfig(),
	}
}

func validFactoryConvertPipeline(action string) string {
	return "stages:\n" +
		"  - id: working\n" +
		"    label: Working\n" +
		"    entry: true\n" +
		"    on_enter:\n" +
		"      " + action + "\n"
}

func parseConvertedWorkflow(t *testing.T, raw string) types.WorkflowConfig {
	t.Helper()

	var workflow types.WorkflowConfig
	if err := yaml.Unmarshal([]byte(raw), &workflow); err != nil {
		t.Fatalf("converted workflow did not parse: %v\n%s", err, raw)
	}
	if err := workflow.Validate(); err != nil {
		t.Fatalf("converted workflow did not validate: %v\n%s", err, raw)
	}
	return workflow
}

func assertFactoryConvertCritical(t *testing.T, resp FactoryConvertResponse, id string) {
	t.Helper()

	for _, diagnostic := range resp.Diagnostics {
		if diagnostic.ID != id {
			continue
		}
		if diagnostic.Severity != SeverityCritical || !diagnostic.Blocking {
			t.Fatalf("diagnostic %s = %#v, want blocking critical", id, diagnostic)
		}
		return
	}
	t.Fatalf("missing critical diagnostic %q in %#v", id, resp.Diagnostics)
}
